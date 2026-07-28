package proxy

import (
	"context"
	"strings"
	"sync"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func resetUntranslated() { seenUntranslated = sync.Map{} }

// TestUntranslatedPairIsReported.
//
// The registry returns an unregistered pair's body unchanged, so a request
// written in one dialect reaches an upstream speaking another and whatever the
// upstream makes of it comes back as an ordinary error -- with nothing saying
// the translation was skipped. README documented this as a known property, and
// translate/ was built as a "fail-loud facade" over it, but the serving path
// never imports that facade: CLIProxyAPI's executors call sdktranslator
// directly.
//
// These hooks are consulted only when no built-in transformer exists, so being
// called is the signal.
func TestUntranslatedPairIsReported(t *testing.T) {
	resetUntranslated()
	h := &refusalHooks{}

	h.TranslateRequest(context.Background(), "openai", "claude", "m", []byte(`{}`), false)

	pairs := UntranslatedPairs()
	if len(pairs) != 1 {
		t.Fatalf("记录到 %v，应恰好记录一条", pairs)
	}
	if !strings.Contains(pairs[0], "openai->claude") {
		t.Errorf("记录的组合 = %q，应指明是哪一对", pairs[0])
	}
	if s := UntranslatedSummary(); !strings.Contains(s, "原样转发") {
		t.Errorf("摘要应说明报文被原样转发: %q", s)
	}
}

// TestSameDialectIsNotADefect.
//
// A Claude client talking to a Claude upstream needs no translation, and the
// body passing through unchanged is the correct outcome. Reporting it would
// train the operator to ignore this warning -- and claude→claude really does
// reach these hooks, because the registry has no claude→claude entry.
func TestSameDialectIsNotADefect(t *testing.T) {
	resetUntranslated()
	h := &refusalHooks{}

	h.TranslateRequest(context.Background(), "claude", "claude", "m", []byte(`{}`), false)
	h.TranslateResponse(context.Background(), "claude", "Claude", "m", nil, nil, []byte(`{}`), false)

	if pairs := UntranslatedPairs(); len(pairs) != 0 {
		t.Errorf("同方言不需要翻译，不应被记录: %v", pairs)
	}
}

// TestEachPairIsReportedOnce.
//
// A client pointed at the wrong upstream produces a steady stream of these. One
// line per pair for the life of the process; one per request would bury
// everything else in the log.
func TestEachPairIsReportedOnce(t *testing.T) {
	resetUntranslated()
	h := &refusalHooks{}

	for i := 0; i < 500; i++ {
		h.TranslateRequest(context.Background(), "openai", "gemini", "m", []byte(`{}`), false)
	}
	if pairs := UntranslatedPairs(); len(pairs) != 1 {
		t.Errorf("同一组合重复 500 次记录成 %d 条，应去重为 1 条", len(pairs))
	}
}

// TestRequestAndResponseAreTrackedSeparately: one direction can have a
// transformer while the other does not, and the two failures look different to
// the caller.
func TestRequestAndResponseAreTrackedSeparately(t *testing.T) {
	resetUntranslated()
	h := &refusalHooks{}

	h.TranslateRequest(context.Background(), "openai", "claude", "m", []byte(`{}`), false)
	h.TranslateResponse(context.Background(), "openai", "claude", "m", nil, nil, []byte(`{}`), false)

	if pairs := UntranslatedPairs(); len(pairs) != 2 {
		t.Errorf("请求与响应方向应分别记录，实际 %v", pairs)
	}
}

// TestHealthyDeploymentReportsNothing is the premise: every pair slimproxy
// actually serves has a registered transformer, so this must stay silent in
// normal operation or it is just noise.
func TestHealthyDeploymentReportsNothing(t *testing.T) {
	resetUntranslated()
	if s := UntranslatedSummary(); s != "" {
		t.Errorf("没有观察到问题时摘要应为空: %q", s)
	}

	// A registered pair never reaches these hooks -- go through the registry
	// rather than calling them directly, so this asserts the real condition.
	var param any
	sdktranslator.TranslateNonStream(context.Background(), "claude", "openai", "m",
		nil, nil, []byte(`{"content":[{"type":"text","text":"hi"}]}`), &param)

	if pairs := UntranslatedPairs(); len(pairs) != 0 {
		t.Errorf("已注册的组合不应被记录: %v", pairs)
	}
}
