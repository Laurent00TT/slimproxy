package journal

import (
	"testing"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// TestCacheMissCountsOnlySuccesses: a failed request never produced usage, so
// its zero cache fields are "not measured". Counting them made 2026-09-23's
// cache line read 148 misses when 143 of those rows were failures.
func TestCacheMissCountsOnlySuccesses(t *testing.T) {
	s := Summarise([]Event{
		{Kind: KindRequest, OK: boolPtr(false), Cause: metrics.CauseConnect},
		{Kind: KindRequest, OK: boolPtr(false), Cause: metrics.CauseCanceled, TTFTMs: 4000},
		{Kind: KindRequest, OK: boolPtr(false), Status: 529, Cause: metrics.CauseUpstream},
		// No OK at all is "not known", and not known is not a success.
		{Kind: KindRequest},
		{Kind: KindRequest, OK: boolPtr(true), CacheRead: 90_000},
		{Kind: KindRequest, OK: boolPtr(true), CacheCreation: 3_000},
		{Kind: KindRequest, OK: boolPtr(true)},
	})
	if s.CacheMissed != 1 {
		t.Errorf("CacheMissed = %d, want 1（只有那个成功且未读写缓存的请求）", s.CacheMissed)
	}
}

// TestSummaryCountsCancellationsApart: an Esc press is not the path breaking,
// but during an outage a client giving up is a symptom -- so it is counted
// beside the failures, never inside them and never dropped.
func TestSummaryCountsCancellationsApart(t *testing.T) {
	s := Summarise([]Event{
		{Kind: KindRequest, OK: boolPtr(false), Cause: metrics.CauseTLS},
		{Kind: KindRequest, OK: boolPtr(false), Status: 429, Cause: metrics.CauseUpstream},
		{Kind: KindRequest, OK: boolPtr(false), Cause: metrics.CauseCanceled, TTFTMs: 4000},
		{Kind: KindRequest, OK: boolPtr(false), Cause: metrics.CauseCanceled},
		{Kind: KindRequest, OK: boolPtr(true), CacheRead: 1},
	})
	if s.Requests != 5 {
		t.Errorf("Requests = %d, want 5（取消的请求仍是请求）", s.Requests)
	}
	if s.Failed != 2 || s.Canceled != 2 {
		t.Errorf("Failed = %d, Canceled = %d, want 2 和 2", s.Failed, s.Canceled)
	}
	// Status 0 is also where a cancellation would land; one entry, not three,
	// is what shows it stayed out.
	if s.ByStatus[0] != 1 || s.ByStatus[429] != 1 || len(s.ByStatus) != 2 {
		t.Errorf("ByStatus = %v, want map[0:1 429:1]", s.ByStatus)
	}
}
