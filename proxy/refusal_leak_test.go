package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// pendingCount counts entries, which sync.Map does not expose.
func pendingCount(h *refusalHooks) int {
	n := 0
	h.pending.Range(func(_, _ any) bool { n++; return true })
	return n
}

const refusedBody = `{"stop_reason":"refusal","stop_details":{"category":"cyber","explanation":"blocked"}}`

// streamFrame is a translated SSE-ish frame carrying a finish_reason, which is
// what markContentFiltered rewrites.
const terminalFrame = `{"choices":[{"finish_reason":"stop","delta":{}}]}`
const midFrame = `{"choices":[{"delta":{"content":"x"}}]}`

// TestStreamingRefusalDoesNotLeak.
//
// Was: the only Delete sat under `if !stream`, and streaming is the branch that
// fires -- the SDK calls NormalizeResponseAfter once per output frame. Every
// refused stream therefore leaked a map entry plus the entire context chain it
// keys on, permanently, in a process built to run for weeks.
func TestStreamingRefusalDoesNotLeak(t *testing.T) {
	h := &refusalHooks{}

	for i := 0; i < 200; i++ {
		ctx := context.WithValue(context.Background(), testKey{}, i)
		h.NormalizeResponseBefore(ctx, "claude", "openai", "m", nil, nil, []byte(refusedBody), true)
		// Frames before the terminal one carry no finish_reason.
		h.NormalizeResponseAfter(ctx, "claude", "openai", "m", nil, nil, []byte(midFrame), true)
		// The terminal frame is where finish_reason is rewritten.
		h.NormalizeResponseAfter(ctx, "claude", "openai", "m", nil, nil, []byte(terminalFrame), true)
	}

	if got := pendingCount(h); got != 0 {
		t.Errorf("200 次流式拒绝之后残留 %d 条记录，应为 0", got)
	}
}

type testKey struct{}

// TestSuccessfulRetryIsNotMarkedAsFiltered.
//
// CLIProxyAPI's retry loop reuses one context across attempts. A first attempt
// that was refused left a finding behind, and the *successful* retry was then
// rewritten with it -- the caller received a complete answer labelled
// content_filter, which is worse than the bug this file exists to fix.
func TestSuccessfulRetryIsNotMarkedAsFiltered(t *testing.T) {
	h := &refusalHooks{}
	ctx := context.Background()

	// Attempt 1: refused.
	h.NormalizeResponseBefore(ctx, "claude", "openai", "m", nil, nil, []byte(refusedBody), true)

	// Attempt 2, same ctx: upstream answers normally.
	clean := `{"content":[{"type":"text","text":"here you go"}],"stop_reason":"end_turn"}`
	h.NormalizeResponseBefore(ctx, "claude", "openai", "m", nil, nil, []byte(clean), true)

	out := h.NormalizeResponseAfter(ctx, "claude", "openai", "m", nil, nil, []byte(terminalFrame), true)

	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("输出无法解析: %v", err)
	}
	if len(parsed.Choices) == 0 {
		t.Fatal("输出没有 choices")
	}
	if got := parsed.Choices[0].FinishReason; got == "content_filter" {
		t.Error("重试成功的响应被标记成了内容过滤：上一次尝试的判定污染了这一次")
	}
	if got := pendingCount(h); got != 0 {
		t.Errorf("干净响应之后仍残留 %d 条记录", got)
	}
}

// TestNonStreamingStillCleansUp guards the path that always worked, so the
// rework above cannot quietly break it.
func TestNonStreamingStillCleansUp(t *testing.T) {
	h := &refusalHooks{}
	ctx := context.Background()

	h.NormalizeResponseBefore(ctx, "claude", "openai", "m", nil, nil, []byte(refusedBody), false)
	h.NormalizeResponseAfter(ctx, "claude", "openai", "m", nil, nil,
		[]byte(`{"choices":[{"finish_reason":"stop"}]}`), false)

	if got := pendingCount(h); got != 0 {
		t.Errorf("非流式路径残留 %d 条记录", got)
	}
}

// TestPendingIsBounded.
//
// A stream abandoned mid-response -- client disconnect, upstream reset --
// produces neither a clean body nor a terminal frame, so nothing removes its
// entry. Rare, but unbounded; the backstop trades dropped findings (degrading
// to the pre-fix behaviour) for a bound, and says so in the log.
func TestPendingIsBounded(t *testing.T) {
	h := &refusalHooks{}

	for i := 0; i < maxPendingRefusals+50; i++ {
		ctx := context.WithValue(context.Background(), testKey{}, i)
		// Before only: the stream never finishes.
		h.NormalizeResponseBefore(ctx, "claude", "openai", "m", nil, nil, []byte(refusedBody), true)
	}

	if got := pendingCount(h); got > maxPendingRefusals {
		t.Errorf("残留 %d 条，超过上限 %d：映射无界增长", got, maxPendingRefusals)
	}
}

// TestClaudeClientGetsNoFalseWarning.
//
// claude→claude non-streaming has no registered transformer, so the body passes
// through untouched and the caller receives the upstream's own stop_reason
// "refusal" with its stop_details -- nothing was lost. The code warned that the
// caller "sees an empty completion", which was simply untrue.
func TestClaudeClientGetsNoFalseWarning(t *testing.T) {
	if !dialectCarriesRefusal(sdktranslator.Format("claude")) {
		t.Error("claude 方言自带 refusal 语义，不应被视为需要改写")
	}
	for _, f := range []string{"openai", "gemini", "codex"} {
		if dialectCarriesRefusal(sdktranslator.Format(f)) {
			t.Errorf("%s 没有等价的 refusal 表示，改写失败时应当告警", f)
		}
	}
	// Case-insensitive, since the SDK's format strings are not normalised here.
	if !dialectCarriesRefusal(sdktranslator.Format("Claude")) {
		t.Error("方言比较应当不分大小写")
	}
}

// TestRefusalIsStillMarked is the premise for all of the above: the rewrite
// must still happen, or these tests would pass on a no-op.
func TestRefusalIsStillMarked(t *testing.T) {
	h := &refusalHooks{}
	ctx := context.Background()

	h.NormalizeResponseBefore(ctx, "claude", "openai", "m", nil, nil, []byte(refusedBody), true)
	out := h.NormalizeResponseAfter(ctx, "claude", "openai", "m", nil, nil, []byte(terminalFrame), true)

	if !strings.Contains(string(out), "content_filter") {
		t.Errorf("拒绝未被标记为 content_filter: %s", out)
	}
}

// TestHooksSurviveDisplacement.
//
// The middleware installs hooks per request, which covers requests that start
// with them in place. It does not cover a request already running when they are
// displaced -- and syncPluginRuntimeConfigForConfig calls SetPluginHooks on auth
// updates, which the credential auto-refresh triggers every 15 minutes. A long
// streaming response could therefore lose the hooks mid-flight and deliver a
// policy refusal as an unexplained empty completion.
func TestHooksSurviveDisplacement(t *testing.T) {
	// Something else takes the registry, exactly as the SDK does on reload.
	sdktranslator.SetPluginHooks(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 6*refusalHookReinstallInterval)
	defer cancel()
	go keepRefusalHooksInstalled(ctx)

	// Give the loop a couple of ticks to put them back.
	deadline := time.Now().Add(4 * refusalHookReinstallInterval)
	for time.Now().Before(deadline) {
		if refusalHooksActive() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("hooks 被外部覆盖后没有被重新安装：进行中的流式请求会失去策略拒绝的标注")
}

// refusalHooksActive reports whether our hooks are the ones the registry will
// call.
//
// Observed through behaviour rather than read back: the registry exposes no
// getter, which is also why keepRefusalHooksInstalled reinstalls blindly
// instead of checking first. TranslateNonStream runs the hooks around whatever
// transform is registered, so a refusal that comes out marked content_filter
// proves ours ran.
func refusalHooksActive() bool {
	var param any
	out := sdktranslator.TranslateNonStream(context.Background(),
		"claude", "openai", "m", nil, nil, []byte(refusedBody), &param)
	return strings.Contains(string(out), "content_filter")
}
