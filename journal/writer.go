package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Laurent00TT/slimproxy/fsperm"
	"github.com/Laurent00TT/slimproxy/metrics"
)

// DefaultRetentionDays is how long daily event files are kept.
const DefaultRetentionDays = 7

// queueSize bounds how many events may be waiting to be written.
//
// The queue exists so Append never blocks a request. Bounded rather than
// unbounded because an unbounded queue does not remove the failure, it converts
// a slow disk into unbounded memory growth and defers the crash to a worse
// moment. When it fills, events are dropped and the drop is counted -- see
// Dropped, which the diagnostics report, because a journal that quietly loses
// events is worse than one that admits to it.
const queueSize = 4096

// flushInterval bounds how much is lost if the process dies.
//
// Buffered writes are what keep this off the request path, and the cost of
// buffering is exactly this window. One second is short enough that a crash
// loses almost nothing and long enough that a busy proxy is not doing a
// syscall per request.
const flushInterval = time.Second

// Writer appends events to a daily file and prunes old ones.
//
// Safe for concurrent use. Append is non-blocking by design: it runs on the
// usage manager's dispatch goroutine, where a slow write would back up every
// other plugin behind it.
type Writer struct {
	dir       string
	retention time.Duration

	events chan Event
	done   chan struct{}
	closed sync.Once

	mu      sync.Mutex
	file    *os.File
	day     string
	dropped int64
	written int64
	lastErr error
}

// Open prepares a writer over dir, creating it if needed.
func Open(dir string, retentionDays int) (*Writer, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("journal: 未指定目录")
	}
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: 创建目录 %s 失败: %w", dir, err)
	}
	// Same reasoning as everywhere else the project writes: the mode argument
	// is not access control on Windows. The journal names credentials and
	// hostnames even though it holds no secrets outright.
	_ = fsperm.Restrict(dir)

	w := &Writer{
		dir:       dir,
		retention: time.Duration(retentionDays) * 24 * time.Hour,
		events:    make(chan Event, queueSize),
		done:      make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// Append queues an event. Never blocks; drops and counts when the queue is
// full.
func (w *Writer) Append(e Event) {
	if w == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	select {
	case w.events <- e:
	default:
		w.mu.Lock()
		w.dropped++
		w.mu.Unlock()
	}
}

// Stats reports what the writer has done and failed to do.
type Stats struct {
	Written int64
	Dropped int64
	// LastErr is the most recent write failure. A journal that cannot write is
	// a diagnostic tool that is lying by omission, so this is surfaced rather
	// than merely logged.
	LastErr error
	Dir     string
}

func (w *Writer) Stats() Stats {
	if w == nil {
		return Stats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return Stats{Written: w.written, Dropped: w.dropped, LastErr: w.lastErr, Dir: w.dir}
}

// Close flushes and stops the writer.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.closed.Do(func() {
		close(w.events)
		<-w.done
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// run is the single writer goroutine.
func (w *Writer) run() {
	defer close(w.done)

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	// Prune on a much slower cadence than flushing: it lists a directory, and
	// the answer cannot change more than once a day.
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	w.prune(time.Now())

	pending := false
	for {
		select {
		case e, ok := <-w.events:
			if !ok {
				w.flush()
				return
			}
			w.write(e)
			pending = true
		case <-ticker.C:
			if pending {
				w.flush()
				pending = false
			}
		case <-prune.C:
			w.prune(time.Now())
		}
	}
}

// write appends one event, rotating to a new day's file when needed.
func (w *Writer) write(e Event) {
	line, err := json.Marshal(e)
	if err != nil {
		w.mu.Lock()
		w.lastErr = err
		w.mu.Unlock()
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	day := e.At.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if w.file != nil {
			_ = w.file.Close()
		}
		path := filepath.Join(w.dir, day+".jsonl")
		f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if ferr != nil {
			w.lastErr = ferr
			w.file = nil
			return
		}
		_ = fsperm.Restrict(path)
		w.file, w.day = f, day
	}

	if _, werr := w.file.Write(append(line, '\n')); werr != nil {
		w.lastErr = werr
		return
	}
	w.written++
}

func (w *Writer) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_ = w.file.Sync()
	}
}

// prune deletes day files older than the retention window.
//
// By filename rather than modification time: a file's mtime changes when it is
// written, so the current day's file would look newest and a file restored from
// a backup would look new regardless of what is in it. The name is the day the
// events belong to, which is the thing being retained.
func (w *Writer) prune(now time.Time) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-w.retention)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		day, perr := time.ParseInLocation("2006-01-02", strings.TrimSuffix(e.Name(), ".jsonl"), time.Local)
		if perr != nil {
			// Not one of ours. Leaving it alone is the safe default: this
			// deletes files, and a parser that guesses would eventually guess
			// about something it did not write.
			continue
		}
		// End of that day, so a file is kept for the whole retention window
		// rather than expiring at whatever hour it happens to be.
		if day.AddDate(0, 0, 1).Before(cutoff) {
			_ = os.Remove(filepath.Join(w.dir, e.Name()))
		}
	}
}

// Days lists the day files present, oldest first.
func Days(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	sort.Strings(out)
	return out, nil
}

// FromSample turns a completed request into an event.
//
// The projection lives here rather than in metrics because it is a journal
// concern: which of the sample's fields are worth a week of disk, and how they
// are named on the wire.
func FromSample(s metrics.Sample) Event {
	e := Event{
		At:            s.At,
		Kind:          KindRequest,
		Route:         s.Route(),
		Model:         s.Model,
		OK:            boolPtr(!s.Failed),
		Tokens:        s.Tokens,
		CacheRead:     s.CacheRead,
		CacheCreation: s.CacheCreation,
		Auth:          s.Auth,
		Exec:          s.Executor,
	}
	if s.Failed {
		e.Status = s.Status
		// Recorded even when a status is present: the two answer different
		// questions, and "upstream" next to a 429 is what distinguishes a
		// refusal that arrived from one that never got that far.
		e.Cause = s.Cause
	}
	if s.TTFT > 0 {
		e.TTFTMs = s.TTFT.Milliseconds()
	}
	if s.Latency > 0 {
		e.LatencyMs = s.Latency.Milliseconds()
	}
	if s.HasQuota() {
		e.Quota5h = f64Ptr(s.Quota5h)
	}
	return e
}
