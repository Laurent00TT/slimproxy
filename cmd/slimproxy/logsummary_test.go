package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Laurent00TT/slimproxy/journal"
	"github.com/Laurent00TT/slimproxy/metrics"
)

// renderSummary runs events through the same two steps `log -stats` does.
// Chinese output: see TestNetCell for why that is the deterministic language
// under `go test`.
func renderSummary(t *testing.T, events []journal.Event) string {
	t.Helper()
	cx, out, _ := newTestContext()
	writeJournalSummary(cx, journal.Summarise(events), time.Hour)
	return out.String()
}

// TestLogStatsListsCancellationsApart: cancellations are named on the summary
// line but kept out of the failure count and its breakdown, which is what an
// operator reads to decide whether the route is sick.
func TestLogStatsListsCancellationsApart(t *testing.T) {
	no, yes := false, true
	got := renderSummary(t, []journal.Event{
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseConnect},
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseConnect},
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseCanceled, TTFTMs: 4000},
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseCanceled, TTFTMs: 5000},
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseCanceled},
		{Kind: journal.KindRequest, OK: &yes},
	})
	want := "6 个请求，2 个失败（无状态码×2），3 个客户端取消"
	if !strings.Contains(got, want) {
		t.Errorf("汇总行应含 %q，实际:\n%s", want, got)
	}
}

// TestLogStatsCacheLineSaysWhatItCounts: the old label claimed the requests
// "carried cache_control", which the request row cannot know, and counted
// failures that never produced usage at all.
func TestLogStatsCacheLineSaysWhatItCounts(t *testing.T) {
	no, yes := false, true
	got := renderSummary(t, []journal.Event{
		{Kind: journal.KindRequest, OK: &yes, CacheRead: 50_000},
		{Kind: journal.KindRequest, OK: &yes},
		{Kind: journal.KindRequest, OK: &yes},
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseConnect},
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseTimeout},
	})
	want := "成功请求中 2 个未读写缓存"
	if !strings.Contains(got, want) {
		t.Errorf("缓存行应含 %q（失败请求不计入），实际:\n%s", want, got)
	}
	if strings.Contains(got, "cache_control") {
		t.Errorf("缓存行不该再声称请求带了 cache_control，实际:\n%s", got)
	}
}

// TestLogStatsListsCancellationsWithoutFailures: a quiet day with nothing but
// Esc presses still names them. Printing the count only beside a failure count
// would drop it exactly when there are no real failures to sit next to.
func TestLogStatsListsCancellationsWithoutFailures(t *testing.T) {
	no, yes := false, true
	got := renderSummary(t, []journal.Event{
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseCanceled, TTFTMs: 4000},
		{Kind: journal.KindRequest, OK: &no, Cause: metrics.CauseCanceled},
		{Kind: journal.KindRequest, OK: &yes},
	})
	want := "3 个请求，2 个客户端取消"
	if !strings.Contains(got, want) {
		t.Errorf("汇总行应含 %q，实际:\n%s", want, got)
	}
	if strings.Contains(got, "个失败") {
		t.Errorf("没有真正的失败时不该出现失败计数，实际:\n%s", got)
	}
}
