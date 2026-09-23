package metrics

import (
	"fmt"
	"sync"
	"time"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// FailureClass groups failures by what an operator would do about them.
//
// The grouping is the point: a run of ClassUnreachable means the machine cannot
// reach the upstream at all and no amount of waiting or credential swapping
// helps, while ClassQuota means waiting is exactly right. Reporting both as
// "failed" is what let two days of the former pass unremarked.
type FailureClass int

const (
	// ClassNone is a successful request.
	ClassNone FailureClass = iota
	// ClassUnreachable is a failure that never got an HTTP status: DNS, dial,
	// TLS, or a connection dropped mid-flight.
	ClassUnreachable
	// ClassQuota is a 429.
	ClassQuota
	// ClassUpstream is any other failure the upstream put a status on.
	ClassUpstream
)

func (c FailureClass) String() string {
	switch c {
	case ClassUnreachable:
		return i18n.T("上游不可达", "upstream unreachable")
	case ClassQuota:
		return i18n.T("配额限流", "quota limited")
	case ClassUpstream:
		return i18n.T("上游报错", "upstream error")
	default:
		return i18n.T("正常", "healthy")
	}
}

// classify buckets a completed request.
//
// Status is zero when a failure carried no HTTP code, which is precisely the
// transport-layer case -- see Sample.Status. In three days of real traffic 108
// of 110 failures landed here, all of them dial timeouts. A client cancellation
// also carries no status; the ones that came after the upstream had answered
// never get here -- see abandonedAfterAnswer.
func classify(s Sample) FailureClass {
	if !s.Failed {
		return ClassNone
	}
	switch {
	case s.Status == 0:
		return ClassUnreachable
	case s.Status == 429:
		return ClassQuota
	default:
		return ClassUpstream
	}
}

// alertSteps are the streak lengths worth saying something about.
//
// A ladder rather than a time window: it adapts to both a short burst and a
// long outage without consulting a clock, and it says less the longer trouble
// lasts, which is the opposite of what a fixed interval does. The first rung is
// 3 because a single failure is something the caller retries past without
// noticing, while three consecutive dial timeouts is already a minute of total
// unavailability.
var alertSteps = []int{3, 10, 30, 100, 300, 1000}

func isAlertStep(n int) bool {
	for _, s := range alertSteps {
		if n == s {
			return true
		}
	}
	return false
}

// HealthEvent is something worth telling the operator about.
type HealthEvent struct {
	// Degraded distinguishes "it is still broken" from "it recovered".
	Degraded bool
	Class    FailureClass
	Streak   int
	// For a recovery, how long the run of failures lasted.
	Lasted time.Duration
}

// State renders the event for the journal's State field.
func (e HealthEvent) State() string {
	if e.Degraded {
		return "degraded"
	}
	return "recovered"
}

// Text is the one line a human reads.
func (e HealthEvent) Text() string {
	if e.Degraded {
		return fmt.Sprintf(i18n.T(
			"连续 %d 次请求失败（%s）——上游持续不可用",
			"%d consecutive request failures (%s) -- upstream persistently unavailable"),
			e.Streak, e.Class)
	}
	return fmt.Sprintf(i18n.T(
		"已恢复：先前连续 %d 次失败（%s），持续 %s",
		"recovered: previously %d consecutive failures (%s), lasting %s"),
		e.Streak, e.Class, e.Lasted.Round(time.Second))
}

// HealthState is what the panel would show.
type HealthState struct {
	Degraded bool
	Class    FailureClass
	Streak   int
	Since    time.Time
}

// Health turns a stream of samples into the few statements worth making.
//
// It exists because nothing was making any. The collector saw every one of the
// failures in a two-day outage and handed them all to the journal, where they
// sat until somebody went looking. Recording is not reporting.
type Health struct {
	mu     sync.Mutex
	streak int
	class  FailureClass
	since  time.Time
	// alerted records whether the current run has been announced, so that a
	// recovery is only reported when a degradation was.
	alerted bool

	notify func(HealthEvent)
}

// NewHealth returns a tracker that calls notify on state changes worth
// reporting.
//
// notify must be cheap. It runs inside Collector.HandleUsage while the
// collector's lock is held, so anything slow here also stalls Snapshot -- which
// the panel calls once a second. Writing a log line is acceptable only because
// the ladder in alertSteps bounds how often that happens: a few lines per
// outage, not one per failed request.
func NewHealth(notify func(HealthEvent)) *Health {
	return &Health{notify: notify}
}

// abandonedAfterAnswer reports a request the client canceled after the
// upstream had already sent its first byte.
//
// Such a sample says nothing about the upstream's health either way, so Observe
// skips it rather than classifying it. It is not unreachable: the first byte
// arrived, which classify's Status==0 rule cannot see -- and on 2026-09-23 that
// blindness turned three bursts of abandoned streams (every one with a first
// byte at 3-11s) into "上游不可达" alerts and matching all-clears. Nor is it a
// success: a record is published when a request ends, not when it begins, so
// a stream that got its first token before an outage and was abandoned during
// it proves only that the upstream was reachable back then. Read as ClassNone
// it would end the run and announce a recovery in the middle of the outage.
//
// A cancellation with no first byte keeps counting as unreachable. During a
// real outage a client that gives up waiting is the outage's symptom, and
// skipping those too would let a dead upstream plus an impatient user read as
// silence.
func abandonedAfterAnswer(s Sample) bool {
	return s.Cause == CauseCanceled && s.TTFT > 0
}

// Observe folds one completed request into the current state.
func (h *Health) Observe(s Sample) {
	if h == nil || abandonedAfterAnswer(s) {
		return
	}
	class := classify(s)

	h.mu.Lock()
	var ev HealthEvent
	var fire bool

	switch {
	case class == ClassNone:
		// A success ends the run. Report it only if the run was reported --
		// otherwise every third request would produce an all-clear for trouble
		// nobody was told about.
		if h.alerted {
			ev = HealthEvent{Class: h.class, Streak: h.streak, Lasted: time.Since(h.since)}
			fire = true
		}
		h.streak, h.class, h.alerted = 0, ClassNone, false

	case class != h.class:
		// The kind of trouble changed. Treat it as a new run: "cannot reach the
		// upstream" and "the upstream is rate-limiting us" call for opposite
		// actions, and averaging them into one streak describes neither.
		h.streak, h.class, h.since, h.alerted = 1, class, time.Now(), false

	default:
		h.streak++
	}

	if class != ClassNone && isAlertStep(h.streak) {
		ev = HealthEvent{Degraded: true, Class: h.class, Streak: h.streak}
		fire = true
		h.alerted = true
	}
	notify := h.notify
	h.mu.Unlock()

	if fire && notify != nil {
		notify(ev)
	}
}

// State reports the current health for display.
func (h *Health) State() HealthState {
	if h == nil {
		return HealthState{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return HealthState{
		Degraded: h.alerted,
		Class:    h.class,
		Streak:   h.streak,
		Since:    h.since,
	}
}
