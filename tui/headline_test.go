package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// TestZeroValueSnapshotShowsNoFakeMetrics is the guard on the trap that made
// these two fields carry a Known flag rather than a negative sentinel.
//
// Snapshot is a plain struct that anything can construct, and its zero value
// would otherwise render "配额 0%  缓存 0%" -- two entirely plausible readings.
// The second is not a missing number but the single worst one this dashboard
// can report: caching having completely stopped. A panel that shows it by
// default is a panel whose alarm means nothing.
func TestZeroValueSnapshotShowsNoFakeMetrics(t *testing.T) {
	m := sampleModel(96, 18)
	m.stats.QuotaKnown = false
	m.stats.CacheKnown = false

	out := m.View()
	for _, forbidden := range []string{"配额", "缓存"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("未知时不该出现「%s」:\n%s", forbidden, out)
		}
	}
}

func TestHeadlineMetricsRender(t *testing.T) {
	m := sampleModel(96, 18)
	m.stats.Quota5h, m.stats.QuotaKnown = 0.21, true
	m.stats.QuotaTrend = 1
	m.stats.CacheRatio, m.stats.CacheKnown = 0.9846, true

	out := m.View()
	for _, want := range []string{"配额 21%", "↗", "缓存 98%"} {
		if !strings.Contains(out, want) {
			t.Errorf("顶部缺少 %q:\n%s", want, strings.SplitN(out, "\n", 3)[1])
		}
	}
}

// TestTrendArrowOnlyWhenThereIsATrend: an arrow that is always present is
// decoration, and decoration next to a number reads as part of the reading.
func TestTrendArrowOnlyWhenThereIsATrend(t *testing.T) {
	m := sampleModel(96, 18)
	m.stats.Quota5h, m.stats.QuotaKnown = 0.21, true
	m.stats.QuotaTrend = 0

	out := m.View()
	if strings.Contains(out, "↗") || strings.Contains(out, "↘") {
		t.Errorf("没有趋势时不该画箭头:\n%s", strings.SplitN(out, "\n", 3)[1])
	}
	if !strings.Contains(out, "配额 21%") {
		t.Error("配额本身仍应显示")
	}
}

// TestFailureCausesAreNamed pins the display finally catching up with the
// journal's Cause field.
//
// A transport failure never receives an HTTP status, so these were the larger
// half of all failures and every one of them rendered as a bare "err". A week
// of "err" answers nothing -- which is the same gap that was closed on disk
// months before it was closed on screen.
func TestFailureCausesAreNamed(t *testing.T) {
	cases := []struct {
		cause metrics.Cause
		want  string
	}{
		{metrics.CauseTimeout, "超时"},
		{metrics.CauseConnect, "断连"},
		{metrics.CauseDNS, "DNS"},
		{metrics.CauseTLS, "TLS"},
		{metrics.CauseCanceled, "取消"},
		{metrics.CauseUpstream, "上游"},
		// Unrecognised stays "err": inventing a label for a failure mode nobody
		// has classified would be worse than admitting it is unclassified.
		{metrics.CauseOther, "err"},
	}

	for _, tc := range cases {
		t.Run(string(tc.cause), func(t *testing.T) {
			m := sampleModel(96, 18)
			m.stats.Recent = []metrics.Sample{{
				At: fixedNow, Account: "you@example.com", Target: "claude",
				Model: "claude-opus-5", Failed: true, Cause: tc.cause,
			}}
			if out := m.View(); !strings.Contains(out, tc.want) {
				t.Errorf("cause %q 应显示为 %q:\n%s", tc.cause, tc.want, out)
			}
		})
	}
}

// TestStatusCodeStillWinsOverCause: 401 and 429 call for opposite responses,
// and "上游" says neither. The code is the more specific fact whenever there
// is one.
func TestStatusCodeStillWinsOverCause(t *testing.T) {
	m := sampleModel(96, 18)
	m.stats.Recent = []metrics.Sample{{
		At: fixedNow, Account: "you@example.com", Target: "claude",
		Model: "claude-opus-5", Failed: true, Status: 429,
		Cause: metrics.CauseUpstream,
	}}

	out := m.View()
	if !strings.Contains(out, "429") {
		t.Errorf("有状态码时应显示状态码:\n%s", out)
	}
	if strings.Contains(out, "上游") {
		t.Errorf("有状态码时不该退回到笼统的「上游」:\n%s", out)
	}
}

// TestCauseLabelsKeepTheColumnsApart.
//
// A two-character Chinese label is four cells wide, and colStatus's width
// includes the gap to the next column -- so at the old width of four these
// rendered flush against the route with no space at all. "429 " and "ok  " had
// looked fine throughout, which is exactly why the assumption survived.
func TestCauseLabelsKeepTheColumnsApart(t *testing.T) {
	for _, cause := range []metrics.Cause{
		metrics.CauseTimeout, metrics.CauseConnect, metrics.CauseCanceled, metrics.CauseUpstream,
	} {
		m := sampleModel(96, 18)
		m.stats.Recent = []metrics.Sample{{
			At: fixedNow, Account: "you@example.com", Target: "claude",
			Model: "claude-opus-5", Failed: true, Cause: cause,
		}}

		label := causeLabels[cause].text
		for _, line := range strings.Split(m.View(), "\n") {
			i := strings.Index(line, label)
			if i < 0 || !strings.Contains(line, "you@example.com") {
				continue
			}
			rest := line[i+len(label):]
			if !strings.HasPrefix(rest, " ") {
				t.Errorf("cause %q 与后一列之间没有空格:\n%s", cause, line)
			}
		}
	}
}

// TestHeadlineMetricsDoNotDestabiliseTheFrame extends the flicker guard to the
// new fields: they change on a slower cadence than the elapsed timer, but they
// change, and the same constant-height rule has to hold.
func TestHeadlineMetricsDoNotDestabiliseTheFrame(t *testing.T) {
	build := func(quota float64, trend int, cache float64) string {
		m := sampleModel(96, 20)
		m.now = fixedNow
		m.stats.Quota5h, m.stats.QuotaKnown = quota, true
		m.stats.QuotaTrend = trend
		m.stats.CacheRatio, m.stats.CacheKnown = cache, true
		m.stats.Pending = []metrics.Pending{
			{At: fixedNow.Add(-12 * time.Second), Path: "/v1/messages"},
		}
		return m.View()
	}

	a := strings.Split(build(0.21, 1, 0.98), "\n")
	b := strings.Split(build(0.22, 1, 0.97), "\n")
	if len(a) != len(b) {
		t.Fatalf("指标变化改变了帧高度（%d vs %d）", len(a), len(b))
	}
	changed := 0
	for i := range a {
		if a[i] != b[i] {
			changed++
		}
	}
	if changed > 1 {
		t.Errorf("配额和缓存都在顶部同一行，变化应只影响 1 行，实际 %d 行", changed)
	}
}
