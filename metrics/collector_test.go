package metrics

import (
	"net/http"
	"strings"
	"testing"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestAccountIsNotADialect.
//
// Sample.Client claimed to be the inbound protocol dialect for most of this
// project's life. usage.Record.Source is Auth.AccountInfo(), which is an email
// for OAuth credentials -- so the dashboard's "活跃路由" column, and the
// "openai → claude" examples in the README, were showing an account the whole
// time. The inbound dialect is not available from a usage record at all; the
// fidelity probe reads it from the request path instead.
func TestAccountIsNotADialect(t *testing.T) {
	s := SampleFrom(cliproxyusage.Record{
		Source: "you@example.com", Provider: "claude", Model: "claude-opus-5",
	})
	if s.Account != "you@example.com" {
		t.Errorf("Account = %q，应保留邮箱标识", s.Account)
	}
	if !strings.Contains(s.Route(), "@") {
		t.Errorf("Route = %q，应表明左侧是账号而非协议", s.Route())
	}
}

// TestApiKeyAccountIsRedacted.
//
// Auth.AccountInfo() returns the API KEY ITSELF for key-based credentials, and
// this value reaches the panel, the on-disk journal, and anything reporting on
// traffic. An email is an identifier; a key is a credential, and they arrive
// through the same field.
func TestApiKeyAccountIsRedacted(t *testing.T) {
	const secret = "sk-ant-api03-VERY-SECRET-KEY-MATERIAL-9f8e7d6c"
	s := SampleFrom(cliproxyusage.Record{Source: secret, Provider: "claude"})

	if strings.Contains(s.Account, "SECRET") || s.Account == secret {
		t.Errorf("API key 原样进入了遥测: %q", s.Account)
	}
	if len(s.Account) > 12 {
		t.Errorf("脱敏后仍然过长: %q", s.Account)
	}
	// Still has to distinguish two credentials in a listing.
	other := SampleFrom(cliproxyusage.Record{Source: "sk-ant-api03-DIFFERENT-KEY", Provider: "claude"})
	if s.Account == other.Account {
		t.Error("两个不同的密钥脱敏成了同一个值，无法区分凭据")
	}
}

// TestQuotaExtraction pins the five-hour window reading, including the
// distinction between a fresh window and a provider that reports nothing.
func TestQuotaExtraction(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "62")
	if got := quotaFromHeaders(h); got < 0.61 || got > 0.63 {
		t.Errorf("62 应解析为 0.62，实际 %v", got)
	}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.62")
	if got := quotaFromHeaders(h); got < 0.61 || got > 0.63 {
		t.Errorf("0.62 应解析为 0.62，实际 %v", got)
	}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0")
	if got := quotaFromHeaders(h); got != 0 {
		t.Errorf("刚重置的窗口是 0，不是未知，实际 %v", got)
	}
	if got := quotaFromHeaders(http.Header{}); got >= 0 {
		t.Errorf("上游没报告时应为负，实际 %v", got)
	}
}

// TestCacheTokensSurvive: the pair that answers whether prompt caching works
// at all was being dropped entirely.
func TestCacheTokensSurvive(t *testing.T) {
	s := SampleFrom(cliproxyusage.Record{
		Detail: cliproxyusage.Detail{
			TotalTokens: 200, CacheReadTokens: 1358, CacheCreationTokens: 0,
		},
	})
	if s.CacheRead != 1358 {
		t.Errorf("cache_read 丢失: %d", s.CacheRead)
	}
	if !s.Cached() {
		t.Error("有缓存读取却报告未命中")
	}
	// The ratio is deliberately NOT derived from these fields any more. This
	// record carries no canonical breakdown, so how much of TotalTokens the
	// 1358 cache reads already account for is unknown -- and the old answer
	// (1358/(1358+200)) assumed they did not overlap at all. On this proxy's
	// real traffic they do, and that assumption reported 49% where the upstream
	// meant 98%.
	if r := s.CacheRatio(); r >= 0 {
		t.Errorf("没有权威 breakdown 时应报告未知，实际算出了 %v", r)
	}
	// No cacheable prefix at all is not the same as a miss.
	if r := (Sample{}).CacheRatio(); r >= 0 {
		t.Errorf("没有可分母时应返回负数，实际 %v", r)
	}
}

// TestCacheRatioUsesTheCanonicalBreakdown is the other half: given accounting
// the upstream vouches for, the ratio must be right.
//
// The numbers are one of this proxy's own logged requests -- 104006 of 105633
// input tokens served from cache. That request is the reason the old formula
// was caught: it reported 49%, which on a healthy proxy would have looked like
// caching half-broken.
func TestCacheRatioUsesTheCanonicalBreakdown(t *testing.T) {
	const (
		uncached   = 1274
		cacheRead  = 104006
		cacheWrite = 353
		inputTotal = uncached + cacheRead + cacheWrite // 105633
		output     = 500
	)
	s := SampleFrom(cliproxyusage.Record{
		Detail: cliproxyusage.Detail{
			TotalTokens:         inputTotal + output,
			CacheReadTokens:     cacheRead,
			CacheCreationTokens: cacheWrite,
			TokenBreakdown: cliproxyusage.NewSubsetTokenBreakdown(
				inputTotal, cacheRead, cacheWrite, output, 0, inputTotal+output),
		},
	})

	if s.InputTokens != inputTotal {
		t.Fatalf("InputTokens = %d，应为 %d（breakdown 没被采信）", s.InputTokens, inputTotal)
	}
	got := s.CacheRatio()
	if got < 0.98 || got > 0.99 {
		t.Errorf("缓存占比 = %.4f，应约 0.9846（%d/%d）", got, cacheRead, inputTotal)
	}
}

// TestUntrustworthyAccountingIsNotGuessedAt pins the conservative gate.
//
// A breakdown the upstream could not fully classify still carries plausible
// numbers, and dividing them produces a plausible ratio. Showing it would put a
// figure on screen that the source itself declined to stand behind -- and this
// number exists to be believed when it says caching stopped working.
func TestUntrustworthyAccountingIsNotGuessedAt(t *testing.T) {
	s := SampleFrom(cliproxyusage.Record{
		Detail: cliproxyusage.Detail{
			TotalTokens:     2000,
			CacheReadTokens: 1500,
			// Unclassified accounting: the SDK says it could not attribute the
			// total to buckets.
			TokenBreakdown: cliproxyusage.NewUnclassifiedTokenBreakdown(2000),
		},
	})
	if s.InputTokens != 0 {
		t.Errorf("InputTokens = %d，无法归类的记账不该被当成输入总量", s.InputTokens)
	}
	if r := s.CacheRatio(); r >= 0 {
		t.Errorf("缓存占比 = %v，应报告未知", r)
	}
}
