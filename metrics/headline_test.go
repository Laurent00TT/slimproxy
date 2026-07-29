package metrics

import (
	"testing"
	"time"
)

// withSamples builds a collector holding exactly these samples.
//
// Set directly rather than fed through HandleUsage: what is under test is how
// Snapshot aggregates a window, and routing every case through a usage record
// would make each one carry a synthetic set of response headers whose parsing
// is somebody else's test.
func withSamples(t *testing.T, samples ...Sample) *Collector {
	t.Helper()
	c := NewCollector()
	c.samples = samples
	return c
}

// TestQuotaIsTheLatestReadingNotAnAverage pins which reading wins.
//
// The five-hour window is a level, not a rate. Averaging it across the sample
// window would report where consumption has been rather than where it is, and
// the entire value of the number is answering "is there anything left".
func TestQuotaIsTheLatestReadingNotAnAverage(t *testing.T) {
	now := time.Now()
	c := withSamples(t,
		Sample{At: now.Add(-30 * time.Minute), Quota5h: 0.10},
		Sample{At: now.Add(-20 * time.Minute), Quota5h: 0.30},
		Sample{At: now.Add(-1 * time.Minute), Quota5h: 0.62},
	)

	snap := c.Snapshot(now)
	if !snap.QuotaKnown {
		t.Fatal("有读数却报告未知")
	}
	if snap.Quota5h != 0.62 {
		t.Errorf("配额 = %v，应为最新的 0.62（不是平均值 0.34）", snap.Quota5h)
	}
	if snap.QuotaTrend != 1 {
		t.Errorf("趋势 = %d，0.10 → 0.62 应为上升", snap.QuotaTrend)
	}
}

// TestQuotaFallingIsReported: a rolling window releases as it fills, so down is
// a real direction and not just the absence of up.
func TestQuotaFallingIsReported(t *testing.T) {
	now := time.Now()
	c := withSamples(t,
		Sample{At: now.Add(-30 * time.Minute), Quota5h: 0.80},
		Sample{At: now.Add(-1 * time.Minute), Quota5h: 0.55},
	)
	if got := c.Snapshot(now).QuotaTrend; got != -1 {
		t.Errorf("趋势 = %d，0.80 → 0.55 应为下降", got)
	}
}

// TestQuotaTrendNeedsTwoDistinctReadings.
//
// One reading repeated is not a flat trend, it is no trend -- and an arrow
// derived from a single observation would be pure decoration.
func TestQuotaTrendNeedsTwoDistinctReadings(t *testing.T) {
	now := time.Now()
	c := withSamples(t, Sample{At: now.Add(-time.Minute), Quota5h: 0.42})

	snap := c.Snapshot(now)
	if !snap.QuotaKnown || snap.Quota5h != 0.42 {
		t.Fatalf("单个读数本身应当报告出来，实际 known=%v 值=%v", snap.QuotaKnown, snap.Quota5h)
	}
	if snap.QuotaTrend != 0 {
		t.Errorf("趋势 = %d，只有一个读数时不该声称方向", snap.QuotaTrend)
	}
}

// TestUnreportedQuotaIsNotZero is the distinction the whole QuotaKnown flag
// exists for: an empty window and an unreported one are different facts, and
// zero is a real reading of the first.
func TestUnreportedQuotaIsNotZero(t *testing.T) {
	now := time.Now()
	c := withSamples(t, Sample{At: now.Add(-time.Minute), Quota5h: -1})

	if snap := c.Snapshot(now); snap.QuotaKnown {
		t.Errorf("上游没报告配额，却报告 known（值 %v）——面板会显示一个上游从未说过的百分比",
			snap.Quota5h)
	}
}

// TestCacheRatioIsWeightedByTokens.
//
// One enormous cached conversation and one tiny uncached probe are not two
// equal votes on whether caching works. Averaging the per-request ratios would
// make them exactly that, and a single small miss would drag a healthy
// proxy's headline number down by half.
func TestCacheRatioIsWeightedByTokens(t *testing.T) {
	now := time.Now()
	c := withSamples(t,
		// 100k input, almost all cached
		Sample{At: now.Add(-2 * time.Minute), InputTokens: 100_000, CacheRead: 99_000},
		// 100 input, nothing cached
		Sample{At: now.Add(-time.Minute), InputTokens: 100, CacheRead: 0},
	)

	snap := c.Snapshot(now)
	if !snap.CacheKnown {
		t.Fatal("有可信记账却报告未知")
	}
	// Weighted: 99000/100100 ≈ 0.989. Averaged it would be (0.99+0)/2 = 0.495.
	if snap.CacheRatio < 0.98 {
		t.Errorf("缓存占比 = %.4f，按 token 加权应约 0.989；0.495 说明是按请求平均的",
			snap.CacheRatio)
	}
}

// TestUntrustedSamplesStayOutOfBothSides.
//
// A sample whose accounting could not be trusted contributes to neither the
// numerator nor the denominator. Letting its cache reads in while dropping its
// input total would inflate the ratio; the reverse would deflate it. Both would
// be a number nobody can act on.
func TestUntrustedSamplesStayOutOfBothSides(t *testing.T) {
	now := time.Now()
	c := withSamples(t,
		Sample{At: now.Add(-2 * time.Minute), InputTokens: 1000, CacheRead: 500},
		// InputTokens == 0 means the breakdown was rejected; CacheRead is still
		// populated from the legacy field and must not be counted.
		Sample{At: now.Add(-time.Minute), InputTokens: 0, CacheRead: 9_000_000},
	)

	if got := c.Snapshot(now).CacheRatio; got != 0.5 {
		t.Errorf("缓存占比 = %v，应恰为 0.5——不可信样本的 CacheRead 混进了分子", got)
	}
}

// TestNoTrustworthyAccountingMeansUnknown: a proxy talking to an upstream that
// reports no breakdown at all must show nothing, not zero. Zero is the reading
// for caching having completely stopped, which is the alarm this exists to
// raise.
func TestNoTrustworthyAccountingMeansUnknown(t *testing.T) {
	now := time.Now()
	c := withSamples(t, Sample{At: now.Add(-time.Minute), Tokens: 500, CacheRead: 200})

	if snap := c.Snapshot(now); snap.CacheKnown {
		t.Errorf("没有任何可信记账，却报告缓存占比 %v", snap.CacheRatio)
	}
}
