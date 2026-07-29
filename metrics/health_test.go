package metrics

import (
	"testing"
	"time"
)

func fail(status int) Sample { return Sample{Failed: true, Status: status, At: time.Now()} }
func ok() Sample             { return Sample{At: time.Now()} }

// collect drives n samples through a tracker and returns everything it said.
func collect(samples ...Sample) []HealthEvent {
	var got []HealthEvent
	h := NewHealth(func(ev HealthEvent) { got = append(got, ev) })
	for _, s := range samples {
		h.Observe(s)
	}
	return got
}

// TestHealthSaysNothingAboutIsolatedFailures.
//
// The bar has to be above the noise floor or the alarm becomes the noise. One
// or two failures are what a client retries past without the operator ever
// needing to know.
func TestHealthSaysNothingAboutIsolatedFailures(t *testing.T) {
	if got := collect(fail(0), ok(), fail(0), fail(0), ok()); len(got) != 0 {
		t.Errorf("零星失败不该发声，却发了 %d 条: %+v", len(got), got)
	}
}

// TestHealthAnnouncesASustainedRun pins the case that went unreported for two
// days: the same transport failure, over and over, with nobody told.
func TestHealthAnnouncesASustainedRun(t *testing.T) {
	got := collect(fail(0), fail(0), fail(0))
	if len(got) != 1 {
		t.Fatalf("连续 3 次失败应恰好发一条，实际 %d 条: %+v", len(got), got)
	}
	if !got[0].Degraded {
		t.Error("事件应标记为 degraded")
	}
	if got[0].Class != ClassUnreachable {
		t.Errorf("class = %v，want ClassUnreachable（无 HTTP 状态码=传输层失败）", got[0].Class)
	}
	if got[0].Streak != 3 {
		t.Errorf("streak = %d, want 3", got[0].Streak)
	}
}

// TestHealthQuietensDownAsTroubleContinues.
//
// A fixed interval would produce the same volume at minute one and hour three.
// The ladder says less the longer it lasts, so a long outage does not bury the
// line that announced it.
func TestHealthQuietensDownAsTroubleContinues(t *testing.T) {
	var samples []Sample
	for i := 0; i < 30; i++ {
		samples = append(samples, fail(0))
	}
	got := collect(samples...)

	var streaks []int
	for _, ev := range got {
		streaks = append(streaks, ev.Streak)
	}
	want := []int{3, 10, 30}
	if len(streaks) != len(want) {
		t.Fatalf("30 次连续失败应发 %v，实际 %v", want, streaks)
	}
	for i := range want {
		if streaks[i] != want[i] {
			t.Errorf("第 %d 条在 streak=%d 发出，want %d", i+1, streaks[i], want[i])
		}
	}
}

// TestHealthReportsRecovery.
//
// An alarm that never clears is one an operator learns to ignore -- and worse,
// it leaves them believing the outage is still running.
func TestHealthReportsRecovery(t *testing.T) {
	got := collect(fail(0), fail(0), fail(0), ok())
	if len(got) != 2 {
		t.Fatalf("应有 degraded + recovered 两条，实际 %d 条: %+v", len(got), got)
	}
	rec := got[1]
	if rec.Degraded {
		t.Error("第二条应是恢复事件")
	}
	if rec.State() != "recovered" {
		t.Errorf("State() = %q, want recovered", rec.State())
	}
	if rec.Streak != 3 {
		t.Errorf("恢复事件应带上先前的连败长度，got %d", rec.Streak)
	}
}

// TestHealthDoesNotAnnounceRecoveryItNeverWarnedAbout: otherwise every success
// following a couple of stray failures produces an all-clear for trouble the
// operator was never told about.
func TestHealthDoesNotAnnounceRecoveryItNeverWarnedAbout(t *testing.T) {
	if got := collect(fail(0), fail(0), ok()); len(got) != 0 {
		t.Errorf("没有发过告警就不该发恢复，实际 %+v", got)
	}
}

// TestHealthTreatsADifferentFailureAsANewRun.
//
// "Cannot reach the upstream" and "the upstream is rate-limiting us" call for
// opposite actions -- there is nothing to do about the first but wait for the
// network, and waiting is exactly right for the second. Summing them into one
// streak would describe neither, and would report whichever came first.
func TestHealthTreatsADifferentFailureAsANewRun(t *testing.T) {
	got := collect(fail(0), fail(0), fail(429), fail(429), fail(429))
	if len(got) != 1 {
		t.Fatalf("应只在 429 连续 3 次时发一条，实际 %d 条: %+v", len(got), got)
	}
	if got[0].Class != ClassQuota {
		t.Errorf("class = %v, want ClassQuota", got[0].Class)
	}
	if got[0].Streak != 3 {
		t.Errorf("streak = %d, want 3（换了失败类型要重新计数）", got[0].Streak)
	}
}

// TestClassifySplitsOnWhatTheOperatorWouldDo pins the mapping itself. Status is
// zero exactly when a failure carried no HTTP code, which is the transport
// layer -- 108 of 110 real failures landed there.
func TestClassifySplitsOnWhatTheOperatorWouldDo(t *testing.T) {
	cases := []struct {
		name string
		s    Sample
		want FailureClass
	}{
		{"success", ok(), ClassNone},
		{"dial timeout carries no status", fail(0), ClassUnreachable},
		{"rate limited", fail(429), ClassQuota},
		{"upstream 500", fail(500), ClassUpstream},
		{"bad credential", fail(401), ClassUpstream},
		// A successful request with a status set must not be read as a failure.
		{"success with status", Sample{Status: 200}, ClassNone},
	}
	for _, c := range cases {
		if got := classify(c.s); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestHealthStateTracksTheCurrentRun covers what the panel would read.
func TestHealthStateTracksTheCurrentRun(t *testing.T) {
	h := NewHealth(func(HealthEvent) {})
	for i := 0; i < 3; i++ {
		h.Observe(fail(0))
	}
	if st := h.State(); !st.Degraded || st.Streak != 3 {
		t.Errorf("State() = %+v，want degraded 且 streak=3", st)
	}
	h.Observe(ok())
	if st := h.State(); st.Degraded || st.Streak != 0 {
		t.Errorf("恢复后 State() = %+v，want 非 degraded 且 streak=0", st)
	}
}

// TestHealthNilIsUsable: the tracker is reachable through Runtime, and a nil
// one must not turn an outage into a panic.
func TestHealthNilIsUsable(t *testing.T) {
	var h *Health
	h.Observe(fail(0))
	if st := h.State(); st.Degraded {
		t.Error("nil tracker 不该报告 degraded")
	}
}
