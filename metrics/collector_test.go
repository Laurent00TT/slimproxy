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
	if r := s.CacheRatio(); r < 0.8 {
		t.Errorf("缓存占比 = %v，1358/(1358+200) 应约 0.87", r)
	}
	// No cacheable prefix at all is not the same as a miss.
	if r := (Sample{}).CacheRatio(); r >= 0 {
		t.Errorf("没有可分母时应返回负数，实际 %v", r)
	}
}
