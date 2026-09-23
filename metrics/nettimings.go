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
// Latency/TTFT. Beside the upload leg it counts the body bytes that leg
// carried, so the two together are a throughput rather than just a duration.
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
	// bodyBytes counts the request body bytes the network actually delivered,
	// the numerator bodyDoneNs was missing: without it an upload leg could
	// not be turned into a throughput, and the only size on record was the
	// fidelity probe's once-a-minute sample, capped at its 2 MiB parse limit.
	// Counted at the network side of every body drainer, so a replay from
	// memory never passes it twice (see proxy/netmeter.go) -- and a body
	// abandoned before EOF keeps what was read: undercount, never overcount.
	bodyBytes atomic.Int64
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

// AddBodyBytes counts the b body bytes one Read delivered. A non-positive b
// is ignored: io.Reader forbids it, and adding one would subtract bytes that
// were really delivered.
func (n *NetTimings) AddBodyBytes(b int) {
	if b > 0 {
		n.bodyBytes.Add(int64(b))
	}
}

// BodyBytes is the request body delivered so far, 0 when none was read.
func (n *NetTimings) BodyBytes() int64 {
	return n.bodyBytes.Load()
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
