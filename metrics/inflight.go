package metrics

import (
	"sort"
	"sync"
	"time"
)

// Pending is a request that has started and not yet finished.
//
// Deliberately thin. Everything the dashboard shows about a completed request
// -- which credential served it, which model answered, what it cost -- is only
// known once the upstream has answered, because the executor picks the
// credential downstream of anything that can observe the request arriving. A
// Pending carrying empty fields for all of them would render a row of dashes
// that reads as missing data rather than as a request still running.
type Pending struct {
	// At is when the request entered the proxy. The display derives the elapsed
	// time from it on every frame, which is what makes the row count up without
	// anything having to push an update.
	At time.Time
	// Path is the request path -- the one thing about the request that is free
	// to observe. Learning the model instead would mean reading and copying the
	// whole body on every request, which is the cost that made the fidelity
	// probe sample once a minute rather than measure.
	Path string
}

// InFlight tracks requests between arrival and completion.
//
// Separate from Collector, which accumulates finished ones: the two answer
// opposite questions over opposite lifetimes, and a request is in exactly one
// of them at any instant. Keeping them apart is also what lets this be updated
// from a middleware while the collector stays driven by usage records.
//
// Safe for concurrent use. Begin and End run on request goroutines; Snapshot
// runs on the render loop.
type InFlight struct {
	mu   sync.Mutex
	next int64
	live map[int64]Pending
}

// NewInFlight returns an empty tracker.
func NewInFlight() *InFlight {
	return &InFlight{live: make(map[int64]Pending)}
}

// Begin records a request that just started, returning the token that ends it.
//
// A token rather than the path as a key: two requests to /v1/messages overlap
// constantly, and keying by path would let the first one to finish clear the
// row belonging to the one still running.
func (f *InFlight) Begin(path string, at time.Time) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.live[f.next] = Pending{At: at, Path: path}
	return f.next
}

// End clears the entry Begin returned this token for.
//
// There is no expiry sweep in this type, deliberately. The only thing that
// keeps an entry alive is a Begin whose End has not run, and callers defer End
// -- so an entry that outlives any plausible request is a request that really
// is still running, and ageing it out would hide the single case most worth
// seeing on a dashboard. The price is that a caller who forgets the defer
// leaks a row that never clears, which is a visible failure rather than a
// silent one.
func (f *InFlight) End(token int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, token)
}

// Started counts requests that have entered the tracker since it was created,
// finished ones included.
//
// The live map cannot answer "did anything ever reach this?": a request that
// arrived and completed leaves it exactly as empty as a middleware that was
// never registered at all. That distinction is the whole difference between a
// working feature and a silently dead one -- the panel shows nothing either way
// -- and only a monotonic counter can draw it.
func (f *InFlight) Started() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next
}

// Snapshot lists what is in progress, oldest first.
//
// Oldest first because the oldest is the one worth looking at: it is the
// request closest to being stuck, and the one a height-bounded display should
// keep when it has to drop rows.
func (f *InFlight) Snapshot() []Pending {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.live) == 0 {
		return nil
	}
	out := make([]Pending, 0, len(f.live))
	for _, p := range f.live {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
