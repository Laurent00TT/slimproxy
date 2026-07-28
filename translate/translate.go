package translate

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// ErrNoTransformer is returned when the registry has no transform for a pair
// and passthrough was not opted into.
//
// The underlying registry treats this case as success and echoes the raw body.
// That is what makes a half-wired provider forward untranslated bytes to an
// upstream that expects a different schema, with no error logged anywhere.
var ErrNoTransformer = errors.New("translate: no transformer registered for pair")

// Options controls how a Translator handles pairs the registry cannot serve.
type Options struct {
	// AllowRequestPassthrough permits TranslateRequest to return the body
	// unchanged when no request transform is registered.
	//
	// This is legitimate for identity routes: the built-in registry has no
	// claude->claude or openai-response->openai-response entry because those
	// are short-circuited by the executor rather than translated. Enable it
	// when the caller deliberately relies on that.
	AllowRequestPassthrough bool

	// AllowResponsePassthrough permits response translation to echo upstream
	// frames unchanged when no response transform is registered. Same
	// rationale as AllowRequestPassthrough.
	AllowResponsePassthrough bool
}

// Translator serves one (client, provider) pair. Construct it once per route
// at startup so a missing registration surfaces there rather than on the first
// request that needs it.
type Translator struct {
	pair Pair
	caps Capabilities
	opts Options
}

// New returns a Translator for the pair, or an error if the pair is invalid or
// the registry cannot serve it under opts.
func New(p Pair, opts Options) (*Translator, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	caps := Describe(p)
	if !caps.Request && !opts.AllowRequestPassthrough {
		return nil, fmt.Errorf("%w: %s has no request transform (set AllowRequestPassthrough if this is an identity route)", ErrNoTransformer, p)
	}
	if !caps.StreamResponse && !caps.NonStreamRespose && !opts.AllowResponsePassthrough {
		return nil, fmt.Errorf("%w: %s has no response transform (set AllowResponsePassthrough if this is an identity route)", ErrNoTransformer, p)
	}
	return &Translator{pair: p, caps: caps, opts: opts}, nil
}

// MustNew is New with strict options, panicking on failure. Intended for
// package-level vars in programs whose routes are fixed at compile time.
func MustNew(p Pair) *Translator {
	t, err := New(p, Options{})
	if err != nil {
		panic(err)
	}
	return t
}

// Pair returns the route this Translator serves.
func (t *Translator) Pair() Pair { return t.pair }

// Capabilities returns what the registry can do for this route.
func (t *Translator) Capabilities() Capabilities { return t.caps }

// Request converts a client-dialect request body into the provider dialect.
//
// stream must reflect what will actually be requested upstream. Several
// request transforms bake the flag into the emitted body (Claude->Codex writes
// "stream": true unconditionally, for example), so a mismatch here produces a
// body whose stream flag disagrees with the transport.
func (t *Translator) Request(model string, body []byte, stream bool) ([]byte, error) {
	if !t.caps.Request {
		if !t.opts.AllowRequestPassthrough {
			return nil, fmt.Errorf("%w: %s", ErrNoTransformer, t.pair)
		}
		return body, nil
	}
	// Request lookups take (client, provider) -- the same order as registration.
	out := sdktranslator.TranslateRequest(t.pair.Client.sdk(), t.pair.Provider.sdk(), model, body, stream)
	if out == nil {
		return nil, fmt.Errorf("translate: %s request transform returned nil", t.pair)
	}
	return out, nil
}

// NonStreamResponse converts a complete provider response into the client
// dialect.
//
// What "complete" means is provider-specific and this package cannot normalize
// it: a Claude upstream feeds the entire SSE transcript, a Codex upstream feeds
// only the terminal response.completed event, and Gemini feeds a single JSON
// object. Feed whatever the matching executor would have fed.
func (t *Translator) NonStreamResponse(ctx context.Context, model string, originalRequest, translatedRequest, body []byte) ([]byte, error) {
	if !t.caps.NonStreamRespose {
		if !t.opts.AllowResponsePassthrough {
			return nil, fmt.Errorf("%w: %s (non-stream)", ErrNoTransformer, t.pair)
		}
		return body, nil
	}
	var state any
	// Response lookups take (provider, client) -- flipped relative to registration.
	out := sdktranslator.TranslateNonStream(ctx,
		t.pair.Provider.sdk(), t.pair.Client.sdk(),
		model, originalRequest, translatedRequest, body, &state)
	return out, nil
}

// TokenCount renders an upstream token count in the client's native shape.
//
// Only pairs whose CLIENT dialect is Claude or Gemini have a token-count
// transform; the rest cannot express a count and return ErrNoTransformer.
//
// Absence is detected at call time rather than probed: the registry has no
// HasTokenCountTransformer, and its generic HasResponseTransformer reports
// true whenever any response transform exists, which is every registered pair.
// TranslateTokenCount echoes its input when unregistered, so an unchanged
// result means "not supported". Registered transforms discard the input body
// entirely and synthesize a fresh document, so they cannot collide with this.
func (t *Translator) TokenCount(ctx context.Context, count int64, body []byte) ([]byte, error) {
	out := sdktranslator.TranslateTokenCount(ctx,
		t.pair.Provider.sdk(), t.pair.Client.sdk(), count, body)
	if bytes.Equal(out, body) {
		return nil, fmt.Errorf("%w: %s (token count)", ErrNoTransformer, t.pair)
	}
	return out, nil
}
