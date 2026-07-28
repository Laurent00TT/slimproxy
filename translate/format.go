// Package translate is a typed, fail-loud facade over CLIProxyAPI's protocol
// translator registry (github.com/router-for-me/CLIProxyAPI/v7/sdk/translator).
//
// The underlying registry is powerful but has three sharp edges that this
// package exists to remove:
//
//  1. Argument order is inconsistent within the registry itself. Registration
//     stores responses[client][provider]. The Has*ResponseTransformer probes
//     read responses[from][to], so they must be called as (client, provider).
//     But TranslateStream, TranslateNonStream and TranslateTokenCount read
//     responses[to][from], so they must be called as (provider, client).
//     Probing and translating the same route take OPPOSITE argument orders.
//
//     Worse, for a route whose reverse is also registered -- which is most of
//     them, since openai->claude and claude->openai are both real routes --
//     the wrong order does not miss and fall back to passthrough. It resolves
//     to the reverse route's transform and silently applies it. This package
//     takes (Client, Provider) everywhere and flips internally where needed.
//
//  2. Streaming state is carried in a caller-owned *any that transforms
//     type-assert without comma-ok. Reusing one *any across two different
//     pairs panics. This package binds state to a Stream handle that is
//     created from exactly one pair.
//
//  3. A missing registration is not an error: the raw body is echoed back.
//     A half-wired provider therefore forwards untranslated bytes upstream
//     with no error anywhere. This package returns ErrNoTransformer unless
//     passthrough is opted into explicitly.
//
// Importing this package registers all built-in translator pairs as a side
// effect (via sdk/translator/builtin).
package translate

import (
	"fmt"
	"sort"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	// Registers the 30 built-in (client, provider) translator pairs. This is
	// the only externally reachable trigger for those init() functions --
	// internal/translator cannot be imported from outside the CLIProxyAPI
	// module.
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

// Format identifies an API dialect. Unlike sdktranslator.Format (a bare string
// alias that accepts any value and silently resolves to "no transformer"),
// values of this type are validated at construction by ParseFormat.
type Format string

// The dialects the built-in registry knows about.
//
// Client dialects are those a downstream caller can speak; provider dialects
// are those an upstream can speak. Several are both. Codex and Antigravity are
// provider-only: nothing translates *from* them.
const (
	// OpenAI is the OpenAI /v1/chat/completions dialect. Client and provider.
	OpenAI Format = "openai"
	// OpenAIResponses is the OpenAI /v1/responses dialect. Client and provider.
	OpenAIResponses Format = "openai-response"
	// Claude is the Anthropic /v1/messages dialect. Client and provider.
	Claude Format = "claude"
	// Gemini is the Google generateContent dialect. Client and provider.
	Gemini Format = "gemini"
	// Interactions is the Gemini Interactions dialect. Client and provider.
	Interactions Format = "interactions"
	// Codex is the Codex backend dialect. Provider only.
	Codex Format = "codex"
	// Antigravity is the Antigravity dialect. Provider only.
	Antigravity Format = "antigravity"
)

var knownFormats = map[Format]struct{}{
	OpenAI:          {},
	OpenAIResponses: {},
	Claude:          {},
	Gemini:          {},
	Interactions:    {},
	Codex:           {},
	Antigravity:     {},
}

// ParseFormat validates s and returns the corresponding Format.
//
// The underlying registry accepts any string and treats an unknown one as
// "no transformer registered", which downstream becomes a silent passthrough.
// Parsing up front turns that class of typo into an error at the edge.
func ParseFormat(s string) (Format, error) {
	f := Format(s)
	if _, ok := knownFormats[f]; !ok {
		return "", fmt.Errorf("translate: unknown format %q (known: %v)", s, Formats())
	}
	return f, nil
}

// Formats returns every known dialect, sorted, for diagnostics and CLI help.
func Formats() []Format {
	out := make([]Format, 0, len(knownFormats))
	for f := range knownFormats {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Valid reports whether f is a known dialect.
func (f Format) Valid() bool {
	_, ok := knownFormats[f]
	return ok
}

func (f Format) String() string { return string(f) }

// sdk converts to the untyped registry format.
func (f Format) sdk() sdktranslator.Format { return sdktranslator.Format(f) }

// Pair is a (client dialect, provider dialect) route. It is the unit the
// registry is keyed on, and the order used consistently throughout this
// package: Client is what the caller speaks, Provider is what the upstream
// speaks.
type Pair struct {
	Client   Format
	Provider Format
}

func (p Pair) String() string { return string(p.Client) + "->" + string(p.Provider) }

// Validate reports whether both halves of the pair are known dialects.
func (p Pair) Validate() error {
	if !p.Client.Valid() {
		return fmt.Errorf("translate: invalid client format %q", p.Client)
	}
	if !p.Provider.Valid() {
		return fmt.Errorf("translate: invalid provider format %q", p.Provider)
	}
	return nil
}

// Capabilities describes what the registry can do for a pair.
//
// There is deliberately no TokenCount field. The registry exposes no probe for
// it: HasResponseTransformer reports whether ANY of the three response
// transforms exists, so using it as a token-count probe reports true for every
// registered pair. Use Translator.TokenCount, which detects absence at call
// time.
type Capabilities struct {
	Pair             Pair
	Request          bool // client request -> provider request
	StreamResponse   bool // provider SSE line -> client frames
	NonStreamRespose bool // provider body -> client body
	AnyResponse      bool // at least one response transform of any kind
}

// Complete reports whether the pair can serve a full streaming round trip.
func (c Capabilities) Complete() bool { return c.Request && c.StreamResponse }

// Describe reports what the registry can do for one pair.
//
// The Has*ResponseTransformer probes read responses[from][to] -- the SAME order
// as registration, i.e. (client, provider). The Translate* functions read
// responses[to][from] -- the OPPOSITE order, i.e. (provider, client). Probing
// and translating the same route therefore require different argument orders;
// see the asymmetry note in the package doc. This function uses the probe
// convention; Translator.Request/NonStreamResponse and Stream.Line use the
// translate convention.
func Describe(p Pair) Capabilities {
	client, provider := p.Client.sdk(), p.Provider.sdk()
	return Capabilities{
		Pair: p,
		// Requests are consistent: probe and translate both use (client, provider).
		Request: sdktranslator.HasRequestTransformer(client, provider),
		// Response PROBES use (client, provider) -- registration order.
		StreamResponse:   sdktranslator.HasStreamResponseTransformer(client, provider),
		NonStreamRespose: sdktranslator.HasNonStreamResponseTransformer(client, provider),
		AnyResponse:      sdktranslator.HasResponseTransformer(client, provider),
	}
}

// Registered returns every pair with a request transform, sorted. Use it to
// verify at startup that the routes a deployment needs actually exist, instead
// of discovering a missing pair as an untranslated body in production.
func Registered() []Capabilities {
	var out []Capabilities
	for _, client := range Formats() {
		for _, provider := range Formats() {
			p := Pair{Client: client, Provider: provider}
			if c := Describe(p); c.Request {
				out = append(out, c)
			}
		}
	}
	return out
}
