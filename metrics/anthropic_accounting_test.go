package metrics

import (
	"testing"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestAnthropicUsageYieldsATrustedCacheRatio settles whether the cache figure
// is actually obtainable from this proxy's own upstream, rather than assumed.
//
// It reproduces the real path rather than describing it. The SDK normalises
// every usage record before publishing it -- UsageReporter.publishWithOutcome
// calls EnsureTokenBreakdownForProvider with the provider and executor -- and
// for anything matching "claude" or "anthropic" that resolves to INDEPENDENT
// accounting, where input_tokens and the cache counters do not overlap. Which
// is exactly Anthropic's wire format: input_tokens excludes the cached prefix,
// and no total_tokens is sent at all.
//
// The numbers are one of this proxy's logged requests, decomposed to match that
// shape. If a future SDK reclassifies Claude as subset accounting, or stops
// vouching for the breakdown, this test fails rather than the panel quietly
// dropping the one metric that is invisible when it breaks.
func TestAnthropicUsageYieldsATrustedCacheRatio(t *testing.T) {
	const (
		uncachedInput = 774
		cacheRead     = 104006
		cacheWrite    = 353
		output        = 500
		inputTotal    = uncachedInput + cacheRead + cacheWrite // 105133
	)

	// As Anthropic sends it: no TotalTokens, cache counted apart from input.
	detail := cliproxyusage.Detail{
		InputTokens:         uncachedInput,
		OutputTokens:        output,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheWrite,
	}
	// As the SDK normalises it before any plugin sees it.
	detail = cliproxyusage.EnsureTokenBreakdownForProvider(detail, "claude", "ClaudeExecutor")

	if q := detail.TokenBreakdown.Quality; q != cliproxyusage.TokenAccountingQualityComplete {
		t.Fatalf("Claude 的记账质量 = %q，不是 complete：缓存率会整个不显示", q)
	}
	if !detail.TokenBreakdown.Valid() {
		t.Fatal("breakdown 未通过 v2 不变量检查：缓存率会整个不显示")
	}

	s := SampleFrom(cliproxyusage.Record{
		Provider: "claude", ExecutorType: "ClaudeExecutor", Detail: detail,
	})

	if s.InputTokens != inputTotal {
		t.Errorf("InputTokens = %d，应为 %d（uncached+cache_read+cache_write）", s.InputTokens, inputTotal)
	}
	got := s.CacheRatio()
	if got < 0.985 || got > 0.995 {
		t.Errorf("缓存占比 = %.4f，应约 0.9893（%d/%d）", got, cacheRead, inputTotal)
	}

	// The legacy total is what the panel's token column shows, and the SDK
	// fills it in from the breakdown when the upstream sends none. Pinned
	// because it settles the question the first cache-ratio bug came from:
	// whether that number already contains the cached prefix. It does -- which
	// is why dividing by it double-counted.
	if detail.TotalTokens != inputTotal+output {
		t.Errorf("TotalTokens = %d，应为 %d：这个字段含缓存，所以不能拿来做缓存率的分母",
			detail.TotalTokens, inputTotal+output)
	}
}

// TestAnthropicFailureStillPublishesCleanly: a failed request carries no tokens,
// and normalisation must not turn that into an accounting inconsistency that
// would poison the window's cache ratio.
func TestAnthropicFailureStillPublishesCleanly(t *testing.T) {
	detail := cliproxyusage.EnsureTokenBreakdownForProvider(
		cliproxyusage.Detail{}, "claude", "ClaudeExecutor")

	s := SampleFrom(cliproxyusage.Record{
		Provider: "claude", ExecutorType: "ClaudeExecutor", Detail: detail, Failed: true,
	})
	if s.InputTokens != 0 {
		t.Errorf("失败请求的 InputTokens = %d，应为 0", s.InputTokens)
	}
	if r := s.CacheRatio(); r >= 0 {
		t.Errorf("没有 token 的请求不该产出缓存占比，实际 %v", r)
	}
}
