package executor

// slimproxy patch regression tests (see SLIMPROXY_PATCHES.md 第 10-12 条).
//
// What is being pinned: with claude-code.cache-ttl set, every cache_control
// breakpoint a Claude Code client sends reaches Anthropic carrying that
// lifetime, on both executor paths, and nothing else about the request
// changes. The executor-level tests drive the real pipeline against a fake
// upstream that records what arrived: they are the ones that go red when an
// upstream rebase drops the one-line call sites in claude_executor_execute.go
// and claude_executor_stream.go, which no unit test of the function alone can
// notice. Mutation-checked: with either call site commented out, the matching
// executor-level test fails on the 5m the fixture's tool breakpoint keeps.
//
// Run these FIRST after rebasing the patches onto a new upstream version.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const claudeCodeUA = "claude-cli/2.1.63 (external, cli)"

// cacheTTLProbePayload is a Claude Code-shaped request with one breakpoint per
// section, in the shape that defeats a fill-in-the-blanks rewrite: the tool
// breakpoint (evaluated first) says 5m explicitly, the system and message ones
// say nothing. Filling only the blanks would yield 5m,1h,1h -- which the
// ordering normalizer flattens straight back to 5m,5m,5m. The second system
// block carries no cache_control and must not be given one.
func cacheTTLProbePayload(model string, stream bool) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":16,`+
		`"tools":[{"name":"t1","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"5m"}}],`+
		`"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"s2"}],`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
		model, stream))
}

// withUserAgent attaches a gin context whose request carries the given
// User-Agent, the way the SDK handlers hand one to executors (ctx key "gin").
func withUserAgent(ua string) context.Context {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Request.Header.Set("User-Agent", ua)
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func cacheTTLConfig(ttl string) *config.Config {
	cfg := &config.Config{}
	cfg.ClaudeCode.CacheTTL = ttl
	return cfg
}

// breakpointTTLs lists the ttl of every cache_control block in evaluation
// order, "" for a block that has none. Rendered as one comma-joined string so
// a failure prints both count and values at once.
func breakpointTTLs(t *testing.T, payload []byte) string {
	t.Helper()
	if !gjson.ValidBytes(payload) {
		t.Fatalf("payload is not valid JSON: %s", payload)
	}
	var out []string
	visit := func(block gjson.Result) {
		if cc := block.Get("cache_control"); cc.Exists() {
			out = append(out, cc.Get("ttl").String())
		}
	}
	each := func(arr gjson.Result, f func(gjson.Result)) {
		if arr.IsArray() {
			arr.ForEach(func(_, item gjson.Result) bool { f(item); return true })
		}
	}
	each(gjson.GetBytes(payload, "tools"), visit)
	each(gjson.GetBytes(payload, "system"), visit)
	each(gjson.GetBytes(payload, "messages"), func(msg gjson.Result) { each(msg.Get("content"), visit) })
	return strings.Join(out, ",")
}

func TestClaudeCodeCacheTTL_PinsEveryBreakpoint(t *testing.T) {
	in := cacheTTLProbePayload("m", true)
	out := applyClaudeCodeCacheTTL(withUserAgent(claudeCodeUA), cacheTTLConfig("1h"), in)

	if got := breakpointTTLs(t, out); got != "1h,1h,1h" {
		t.Fatalf("breakpoint ttls = %q, want 1h,1h,1h (the explicit 5m must be overwritten, not respected)", got)
	}
	if gjson.GetBytes(out, "system.1.cache_control").Exists() {
		t.Error("a block without cache_control was given one: which prefixes are cached stays the client's call")
	}
	if gjson.GetBytes(out, "system.1.text").String() != "s2" || gjson.GetBytes(out, "tools.0.name").String() != "t1" {
		t.Errorf("content outside cache_control changed:\n%s", out)
	}
}

// The uniform rewrite is what makes the knob survive normalizeCacheControlTTL,
// which runs right after it. Anything less than uniform is flattened back.
func TestClaudeCodeCacheTTL_SurvivesOrderingNormalizer(t *testing.T) {
	in := cacheTTLProbePayload("m", true)
	out := normalizeCacheControlTTL(applyClaudeCodeCacheTTL(withUserAgent(claudeCodeUA), cacheTTLConfig("1h"), in))
	if got := breakpointTTLs(t, out); got != "1h,1h,1h" {
		t.Fatalf("after the normalizer the ttls are %q: the rewrite left a shorter lifetime ahead of a longer one", got)
	}
}

func TestClaudeCodeCacheTTL_LeavesOtherClientsAlone(t *testing.T) {
	in := cacheTTLProbePayload("m", true)
	for _, ua := range []string{"", "curl/8.7.1", "openai-python/1.0"} {
		if out := applyClaudeCodeCacheTTL(withUserAgent(ua), cacheTTLConfig("1h"), in); !bytes.Equal(out, in) {
			t.Fatalf("User-Agent %q was rewritten; only claude-cli/* asks for this:\n%s", ua, out)
		}
	}
}

func TestClaudeCodeCacheTTL_UnsetKnobIsANoOp(t *testing.T) {
	in := cacheTTLProbePayload("m", true)
	for _, cfg := range []*config.Config{nil, cacheTTLConfig(""), cacheTTLConfig("  ")} {
		if out := applyClaudeCodeCacheTTL(withUserAgent(claudeCodeUA), cfg, in); !bytes.Equal(out, in) {
			t.Fatalf("with the knob unset the payload was rewritten:\n%s", out)
		}
	}
}

// Nothing to change means the same bytes back -- the property the rest of
// this pipeline's tests pin for their own passes. The text deliberately
// carries characters a re-marshal would escape.
func TestClaudeCodeCacheTTL_NothingToDoReturnsSameBytes(t *testing.T) {
	in := []byte(`{"system":"<plain & string>","messages":[{"role":"user","content":[{"type":"text","text":"a<b","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	if out := applyClaudeCodeCacheTTL(withUserAgent(claudeCodeUA), cacheTTLConfig("1h"), in); !bytes.Equal(out, in) {
		t.Fatalf("payload already at 1h was rewritten:\n%s", out)
	}
}

// The yaml key is the one slimproxy writes into the effective config; a
// renamed tag would leave the knob silently inert.
func TestClaudeCodeCacheTTLConfigKey(t *testing.T) {
	cfg, err := config.ParseConfigBytes([]byte("port: 0\nclaude-code:\n  cache-ttl: \"1h\"\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if cfg.ClaudeCode.CacheTTL != "1h" {
		t.Fatalf("claude-code.cache-ttl parsed as %q, want 1h", cfg.ClaudeCode.CacheTTL)
	}
}

// --- executor-level: the real pipeline, a fake Anthropic that records ---

type recordingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	last []byte
}

func newRecordingUpstream(t *testing.T, contentType, body string) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		got, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.last = got
		u.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) received() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.last...)
}

func cacheTTLProbeAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         "cache-ttl-probe",
		Attributes: map[string]string{"api_key": "sk-test", "base_url": baseURL},
	}
}

const (
	cacheTTLProbeSSE = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	cacheTTLProbeJSON = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`
)

func runStreamProbe(t *testing.T, cfg *config.Config, ua, model string) []byte {
	t.Helper()
	up := newRecordingUpstream(t, "text/event-stream", cacheTTLProbeSSE)
	e := NewClaudeExecutor(cfg)
	req := cliproxyexecutor.Request{Model: model, Payload: cacheTTLProbePayload(model, true)}
	opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("claude")}
	result, err := e.ExecuteStream(withUserAgent(ua), cacheTTLProbeAuth(up.URL), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-result.Chunks:
			if !ok {
				return up.received()
			}
		case <-deadline:
			t.Fatal("stream did not close within 5s")
		}
	}
}

func runExecuteProbe(t *testing.T, cfg *config.Config, ua, model string) []byte {
	t.Helper()
	up := newRecordingUpstream(t, "application/json", cacheTTLProbeJSON)
	e := NewClaudeExecutor(cfg)
	req := cliproxyexecutor.Request{Model: model, Payload: cacheTTLProbePayload(model, false)}
	opts := cliproxyexecutor.Options{Stream: false, SourceFormat: sdktranslator.FromString("claude")}
	if _, err := e.Execute(withUserAgent(ua), cacheTTLProbeAuth(up.URL), req, opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return up.received()
}

// TestClaudeStreamCarriesConfiguredCacheTTLUpstream is the patch's reason to
// exist on the path Claude Code actually uses.
func TestClaudeStreamCarriesConfiguredCacheTTLUpstream(t *testing.T) {
	body := runStreamProbe(t, cacheTTLConfig("1h"), claudeCodeUA, "claude-probe-ttl-stream")
	if got := breakpointTTLs(t, body); got != "1h,1h,1h" {
		t.Fatalf("upstream received breakpoint ttls %q, want 1h,1h,1h -- the call site in claude_executor_stream.go is gone or sits after normalizeCacheControlTTL", got)
	}
}

func TestClaudeExecuteCarriesConfiguredCacheTTLUpstream(t *testing.T) {
	body := runExecuteProbe(t, cacheTTLConfig("1h"), claudeCodeUA, "claude-probe-ttl-execute")
	if got := breakpointTTLs(t, body); got != "1h,1h,1h" {
		t.Fatalf("upstream received breakpoint ttls %q, want 1h,1h,1h -- the call site in claude_executor_execute.go is gone or sits after normalizeCacheControlTTL", got)
	}
}

// Control: with the knob unset the breakpoints arrive as written, so the two
// assertions above are on the patch and not on something the pipeline would
// have done anyway.
func TestClaudeStreamWithoutKnobSendsBreakpointsAsWritten(t *testing.T) {
	body := runStreamProbe(t, &config.Config{}, claudeCodeUA, "claude-probe-ttl-asis")
	if got := breakpointTTLs(t, body); got != "5m,," {
		t.Fatalf("upstream received breakpoint ttls %q with the knob unset, want them as the client wrote them (5m,,)", got)
	}
}
