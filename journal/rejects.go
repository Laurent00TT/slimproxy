package journal

import (
	"sync"
	"time"
)

// rejectWindow is how long identical rejects are folded together.
//
// One line per probe would make the file that is supposed to explain an
// incident mostly a record of the internet's curiosity. One line per minute per
// distinct (method, path, status, source) keeps the shape of the traffic --
// which is the part worth keeping -- at a size that stays readable.
const rejectWindow = time.Minute

// rejectKey is what makes two rejects "the same".
//
// Deliberately excludes the client address. Counting per-IP would turn a
// distributed scan into thousands of lines, and the question this data answers
// is "what is being probed and how much", not "by whom" -- the latter needs a
// firewall, not a log file.
type rejectKey struct {
	method string
	path   string
	status int
	src    Source
}

// RejectFolder batches rejected inbound requests.
//
// Rejects come from an HTTP middleware rather than from usage records, because
// a request refused at the router never reaches an executor and never produces
// a record. That is exactly the traffic a public endpoint attracts, so leaving
// it out would report a healthy error rate on a proxy being scanned
// continuously.
type RejectFolder struct {
	emit func(Event)

	mu      sync.Mutex
	counts  map[rejectKey]int
	firstAt map[rejectKey]time.Time
	stop    chan struct{}
	once    sync.Once
}

// NewRejectFolder starts a folder that emits batched events.
func NewRejectFolder(emit func(Event)) *RejectFolder {
	f := &RejectFolder{
		emit:    emit,
		counts:  map[rejectKey]int{},
		firstAt: map[rejectKey]time.Time{},
		stop:    make(chan struct{}),
	}
	go f.run()
	return f
}

// Add records one rejected request.
func (f *RejectFolder) Add(method, path string, status int, src Source, at time.Time) {
	if f == nil {
		return
	}
	k := rejectKey{method: method, path: normalisePath(path), status: status, src: src}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts[k] == 0 {
		// The first occurrence dates the batch. Using the flush time instead
		// would place a burst at the end of the window rather than where it
		// started, which is the thing being correlated against.
		f.firstAt[k] = at
	}
	f.counts[k]++
}

// Close flushes and stops the folder.
func (f *RejectFolder) Close() {
	if f == nil {
		return
	}
	f.once.Do(func() {
		close(f.stop)
		f.flush()
	})
}

func (f *RejectFolder) run() {
	t := time.NewTicker(rejectWindow)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
			f.flush()
		}
	}
}

func (f *RejectFolder) flush() {
	f.mu.Lock()
	pending := f.counts
	at := f.firstAt
	f.counts = map[rejectKey]int{}
	f.firstAt = map[rejectKey]time.Time{}
	f.mu.Unlock()

	for k, n := range pending {
		f.emit(Event{
			At:     at[k],
			Kind:   KindReject,
			Method: k.method,
			Path:   k.path,
			Status: k.status,
			Src:    k.src,
			Count:  n,
		})
	}
}
