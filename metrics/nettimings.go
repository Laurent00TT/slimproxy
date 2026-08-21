package metrics

import (
	"context"
	"sync/atomic"
	"time"
)

// NetTimings measures the two network legs of one inbound request that the
// usage record cannot see: how long the client (and the tunnel in front of it)
// took to deliver the request body, and how much time response writes spent
// blocked on the way back. The upstream leg between them is the record's own
// Latency/TTFT.
//
// Written from the request goroutine and read from the usage manager's
// dispatch goroutine, which is why every field is atomic: the publish races
// the request's final client writes, so a snapshot may miss the last write --
// an undercount, never an overcount.
type NetTimings struct {
	started time.Time
	// bodyDoneNs is elapsed nanoseconds from started to the body's EOF,
	// 0 while unseen. Clamped to at least 1 so "instant" stays distinguishable
	// from "never finished".
	bodyDoneNs atomic.Int64
	// writeBlockNs accumulates time spent inside client Write/Flush calls. In
	// a pipelined stream this is the only measurable form of return-path
	// pressure: "upstream done -> client done" is ~0 by construction.
	writeBlockNs atomic.Int64
}

// NewNetTimings starts the clock at started (the middleware's entry, i.e.
// headers parsed, body not yet read).
func NewNetTimings(started time.Time) *NetTimings {
	return &NetTimings{started: started}
}

// MarkBodyDone records the body's EOF. First call wins: a body replayed from
// memory later (earlyflush replaces the drained body with a bytes.Reader)
// must not overwrite the network reading.
func (n *NetTimings) MarkBodyDone(now time.Time) {
	ns := now.Sub(n.started).Nanoseconds()
	if ns < 1 {
		ns = 1
	}
	n.bodyDoneNs.CompareAndSwap(0, ns)
}

// AddWriteBlock accumulates one client-write duration.
func (n *NetTimings) AddWriteBlock(d time.Duration) {
	if d > 0 {
		n.writeBlockNs.Add(int64(d))
	}
}

// Upload is the measured upload leg, 0 when the body never reached EOF.
func (n *NetTimings) Upload() time.Duration {
	return time.Duration(n.bodyDoneNs.Load())
}

// WriteBlock is the accumulated client-write blocking so far.
func (n *NetTimings) WriteBlock() time.Duration {
	return time.Duration(n.writeBlockNs.Load())
}

// netTimingsKey is unexported so only this package mints the context entry.
type netTimingsKey struct{}

// WithNetTimings hangs the timings on a request context. The value reaches
// usage publication because the fork's GetContextWithCancel is patched to
// build the executor ctx on the request ctx (SLIMPROXY_PATCHES.md, the
// reparenting patch) -- upstream builds it on context.Background(), which
// would strand this value in the request ctx. (The fork's ResponseHeaders
// mechanism is NOT a precedent for this: it rides a holder seeded on the new
// ctx, not request-ctx inheritance.) The forkcheck reparent guard and
// proxy's TestNetTimingsSurviveForkContextHop are the sentinels.
func WithNetTimings(ctx context.Context, nt *NetTimings) context.Context {
	return context.WithValue(ctx, netTimingsKey{}, nt)
}

// NetTimingsFrom recovers the timings, nil when the request never passed the
// meter (direct engine tests, health checks).
func NetTimingsFrom(ctx context.Context) *NetTimings {
	nt, _ := ctx.Value(netTimingsKey{}).(*NetTimings)
	return nt
}
