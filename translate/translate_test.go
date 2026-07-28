package translate

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TestBuiltinPairsReachable pins the number of routes an external module can
// reach. If this drops, sdk/translator/builtin stopped re-exporting some
// internal/translator packages and routes are silently passing through.
func TestBuiltinPairsReachable(t *testing.T) {
	got := Registered()
	if len(got) != 30 {
		t.Errorf("expected 30 registered request pairs, got %d", len(got))
		for _, c := range got {
			t.Logf("  %s", c.Pair)
		}
	}
	for _, c := range got {
		if !c.Complete() {
			t.Errorf("%s has a request transform but no stream response transform", c.Pair)
		}
	}
}

// TestProbeAndTranslateTakeOppositeOrders is the load-bearing test for this
// package: it pins the registry inconsistency the facade exists to hide.
//
// Registration stores responses[client][provider]. The Has* probes read
// responses[from][to] (so: client, provider). TranslateStream reads
// responses[to][from] (so: provider, client). The two conventions are
// opposite, and nothing in the registry's documentation says so.
//
// Uses synthetic formats so the assertion cannot be satisfied by an unrelated
// built-in route -- which is exactly how this asymmetry hides in production:
// for a route whose reverse is also registered, the wrong order resolves to the
// reverse transform instead of missing.
func TestProbeAndTranslateTakeOppositeOrders(t *testing.T) {
	const (
		client   = sdktranslator.Format("slimproxy-test-client")
		provider = sdktranslator.Format("slimproxy-test-provider")
	)
	const sentinel = "SENTINEL"

	sdktranslator.Register(client, provider,
		func(model string, raw []byte, stream bool) []byte { return raw },
		sdktranslator.ResponseTransform{
			Stream: func(ctx context.Context, model string, orig, translated, raw []byte, param *any) [][]byte {
				return [][]byte{[]byte(sentinel)}
			},
		})

	// PROBE convention: (client, provider) hits; the reverse misses.
	if !sdktranslator.HasStreamResponseTransformer(client, provider) {
		t.Error("probe (client, provider) missed; probes no longer use registration order")
	}
	if sdktranslator.HasStreamResponseTransformer(provider, client) {
		t.Error("probe (provider, client) hit; probes changed convention")
	}

	// TRANSLATE convention: (provider, client) hits; the reverse misses.
	var st any
	if got := sdktranslator.TranslateStream(context.Background(), provider, client, "m", nil, nil, []byte("x"), &st); len(got) != 1 || string(got[0]) != sentinel {
		t.Errorf("translate (provider, client) did not reach the transform, got %q", got)
	}
	var st2 any
	got2 := sdktranslator.TranslateStream(context.Background(), client, provider, "m", nil, nil, []byte("x"), &st2)
	if len(got2) == 1 && string(got2[0]) == sentinel {
		t.Error("translate (client, provider) reached the transform; conventions converged, remove the flip in Stream.Line")
	}

	// The facade resolves both directions from one consistent order.
	p := Pair{Client: Format(client), Provider: Format(provider)}
	if !Describe(p).StreamResponse {
		t.Error("Describe failed to see the stream transform under (Client, Provider)")
	}
}

// TestRoundTripOpenAIClaude drives a real request and a real streaming response
// through the facade to prove the wiring works end to end, not just that the
// lookups resolve.
func TestRoundTripOpenAIClaude(t *testing.T) {
	tr, err := New(Pair{Client: OpenAI, Provider: Claude}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	clientBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	upstream, err := tr.Request("claude-sonnet-4", clientBody, true)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	got := string(upstream)
	// The Anthropic body is built from a template, so assert on shape rather
	// than on exact bytes.
	for _, want := range []string{`"messages"`, `"stream":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("translated request missing %s\ngot: %s", want, got)
		}
	}
	if strings.Contains(got, `"stream":false`) {
		t.Errorf("stream flag not honoured\ngot: %s", got)
	}

	// Now a streaming response. Transforms run per RAW LINE and every
	// Claude-upstream transform drops lines without a "data: " prefix, so feed
	// the wire form.
	stream, err := tr.NewStream("claude-sonnet-4", clientBody, upstream)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer stream.Finish()

	ctx := context.Background()
	lines := []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":5,"output_tokens":0}}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
	}

	var frames [][]byte
	for _, l := range lines {
		frames = append(frames, stream.Line(ctx, []byte(l))...)
	}
	if len(frames) == 0 {
		t.Fatal("no client frames produced from a valid Claude SSE prefix")
	}

	joined := strings.Join(func() []string {
		out := make([]string, len(frames))
		for i, f := range frames {
			out[i] = string(f)
		}
		return out
	}(), "\n")

	// OpenAI-chat targets emit BARE JSON chunks; the "data: " wrapper is added
	// by the HTTP layer, not the translator.
	if !strings.Contains(joined, "chat.completion.chunk") {
		t.Errorf("expected OpenAI chunk objects, got:\n%s", joined)
	}
	if !strings.Contains(joined, "hello") {
		t.Errorf("text delta did not survive translation, got:\n%s", joined)
	}
}

// TestIdentityPairRequiresOptIn documents that claude->claude has no
// registration: the executors short-circuit it instead. Strict construction
// must refuse it rather than silently echoing bodies.
func TestIdentityPairRequiresOptIn(t *testing.T) {
	p := Pair{Client: Claude, Provider: Claude}

	if _, err := New(p, Options{}); err == nil {
		t.Fatal("expected claude->claude to be refused under strict options")
	} else if !strings.Contains(err.Error(), "no transformer") {
		t.Errorf("unexpected error: %v", err)
	}

	tr, err := New(p, Options{AllowRequestPassthrough: true, AllowResponsePassthrough: true})
	if err != nil {
		t.Fatalf("New with passthrough: %v", err)
	}
	body := []byte(`{"model":"claude-sonnet-4"}`)
	out, err := tr.Request("claude-sonnet-4", body, false)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("passthrough altered the body:\n in: %s\nout: %s", body, out)
	}
}

// TestTokenCountOnlyForClaudeAndGeminiClients pins which routes can express a
// token count, and guards the reason Capabilities has no TokenCount field: the
// registry's generic HasResponseTransformer probe reports true for all 30
// pairs, so it cannot be used here.
func TestTokenCountOnlyForClaudeAndGeminiClients(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"upstream":"ignored"}`)

	// The exact set, one entry per init.go that populates
	// interfaces.TranslateResponse.TokenCount. A client dialect of Claude or
	// Gemini is necessary but NOT sufficient: interactions-as-provider has no
	// token-count transform, so claude->interactions and gemini->interactions
	// are absent.
	want := map[string]bool{
		"claude->antigravity": true,
		"claude->codex":       true,
		"claude->gemini":      true,
		"claude->openai":      true,
		"gemini->antigravity": true,
		"gemini->claude":      true,
		"gemini->codex":       true,
		"gemini->gemini":      true,
		"gemini->openai":      true,
	}

	got := map[string]bool{}
	for _, c := range Registered() {
		tr, err := New(c.Pair, Options{})
		if err != nil {
			t.Fatalf("New(%s): %v", c.Pair, err)
		}
		out, err := tr.TokenCount(ctx, 42, body)
		if err != nil {
			if !errors.Is(err, ErrNoTransformer) {
				t.Errorf("%s: unexpected error %v", c.Pair, err)
			}
			continue
		}
		if !strings.Contains(string(out), "42") {
			t.Errorf("%s: count missing from %s", c.Pair, out)
		}
		got[c.Pair.String()] = true
	}

	for p := range want {
		if !got[p] {
			t.Errorf("%s lost its token-count transform", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("%s gained a token-count transform; update the pinned set", p)
		}
	}
}

func TestParseFormatRejectsUnknown(t *testing.T) {
	if _, err := ParseFormat("openai"); err != nil {
		t.Errorf("ParseFormat(openai): %v", err)
	}
	// The raw registry accepts this and resolves it to a silent passthrough.
	if _, err := ParseFormat("openai-responses"); err == nil {
		t.Error("expected typo 'openai-responses' to be rejected")
	}
}

// TestProviderOnlyFormats pins that Codex and Antigravity are never a client
// dialect. If that changes, routing tables that assume it need revisiting.
func TestProviderOnlyFormats(t *testing.T) {
	for _, providerOnly := range []Format{Codex, Antigravity} {
		for _, provider := range Formats() {
			p := Pair{Client: providerOnly, Provider: provider}
			if Describe(p).Request {
				t.Errorf("%s unexpectedly usable as a client dialect (pair %s)", providerOnly, p)
			}
		}
	}
}
