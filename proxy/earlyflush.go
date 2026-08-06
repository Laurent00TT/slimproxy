package proxy

// Buying streaming requests out of Cloudflare's 100-second guillotine.
//
// The failure this exists for, measured on 2026-08-03/04: a streaming
// /v1/messages request arrives through the tunnel, and the upstream handler
// writes NOTHING -- not a status line, not a header -- until the first chunk
// (or first error) comes back from the executor. Credential selection,
// cooldown waits and the bootstrap read all happen before the first byte.
// Cloudflare's proxy gives an origin ~100 seconds to produce response headers
// (fixed on free/Pro plans), so every request whose pre-first-byte wait
// crossed that line came back to the caller as a 524 -- 129 of them crossed
// 100s on 2026-08-03 alone, while the executor's own TTFT stayed in single
// seconds. The wait is queueing, not upstream slowness, so the request the
// edge kills would almost always have succeeded.
//
// The fix: if a streaming request has produced no output after a threshold,
// commit "200, text/event-stream" early and keep the connection warm with SSE
// keep-alive comments until the handler has something to say. The Cloudflare
// timer stops at the first flushed byte; from there the stream can take as
// long as it needs -- bounded by a silence watchdog, because the same
// heartbeat that feeds the edge's timer also feeds the client's, and a
// request stuck forever upstream must still end somehow.
//
// The price, and why a threshold: once the 200 is on the wire, a late upstream
// failure can no longer be an HTTP 403/429/529 -- it has to travel inside the
// stream as an SSE error event, which clients handle but retry on different
// terms (and without a Retry-After header). Most failures are fast (the
// fake-403s of 2026-08-04 answered in ~1-3s), and fast outcomes deserve their
// real status codes. So nothing changes for any request the handler answers
// inside the threshold; only requests already deep in 524 territory -- where
// the alternative is an HTML error page from the edge -- get the early
// preamble and in-stream errors.
//
// Scope: POST /v1/messages whose body says streaming, judged by the exact
// predicate the upstream handler uses (the field EXISTS and is not literal
// false -- "stream": 1 streams, so sniffing with Bool() would under-cover).
// Non-streaming responses are single JSON documents; committing SSE headers
// for those would corrupt the response.
//
// Concurrency shape, which review found the first draft got wrong in three
// distinct ways: the timer callback and the heartbeat run on their own
// goroutines while the handler owns the request goroutine, and gin recycles
// the underlying writer through a sync.Pool the moment the request ends. So
// (1) finish marks the writer closed under the lock, and closed is the first
// thing every goroutine checks -- a timer that fires into a finished request
// must find a tombstone, not a live writer; (2) the handler never touches the
// real header map: Header() hands out a shadow copy, committed to the real
// map under the lock by whichever call ends up transmitting first, because
// the handler's own header writes are lock-free and a concurrent map write
// from the timer goroutine is a process-fatal panic, not an error; (3) the
// middleware defers finish, so a panicking handler still tears the machinery
// down before gin's recovery writes through the writer.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/Laurent00TT/slimproxy/i18n"
	"github.com/Laurent00TT/slimproxy/journal"
)

// defaultEarlyFlush is how long a streaming request may stay silent before
// the preamble goes out.
//
// Well under Cloudflare's ~100s (the point of the exercise) but above the
// pre-dispatch waits measured under load (median 26s on 2026-08-03): a
// request that will answer at all mostly answers inside this window and
// keeps its real HTTP status; crossing it means the request was already in
// 524 territory.
const defaultEarlyFlush = 30 * time.Second

// earlyFlushHeartbeatEvery is the keep-alive cadence after the preamble.
//
// Cloudflare also cuts a response that stays quiet *between* reads for ~100s,
// and NATs age flows out on their own schedules; 15 seconds keeps every timer
// on the path comfortably fed. A var only so tests can shorten it.
var earlyFlushHeartbeatEvery = 15 * time.Second

// earlyFlushMaxSilence bounds how long the heartbeat will cover for an
// upstream that never says anything at all.
//
// The heartbeat defeats every timeout between here and the caller -- that is
// its job -- so without a bound of our own, one wedged upstream connection
// holds a client open forever. Four minutes is far beyond any observed
// pre-dispatch wait; hitting it means the request is not coming back. The
// watchdog ends the stream with an explicit in-stream error and cancels the
// handler's context so the executor unwinds too. A var only for tests.
var earlyFlushMaxSilence = 4 * time.Minute

// earlyFlushDesperation is how long an unfinished body upload may run before
// the preamble goes out without waiting for the sniff.
//
// Distinct from the ordinary threshold because it is decided on less
// information: the ordinary flush knows the request streams, this one only
// knows the alternative. Sized against the edge's ~100s with enough margin
// for the preamble to cross the tunnel; a request still uploading at 80s
// either gets the preamble now or an HTML 524 shortly. A var only for tests.
var earlyFlushDesperation = 80 * time.Second

// earlyFlushKeepAlive is the exact bytes of one heartbeat.
//
// An SSE comment line, not a ping event: comments are defined by the SSE spec
// to be skipped by every conforming parser, and the upstream SDK itself emits
// this very form between chunks (stream_forwarder.go), so clients demonstrably
// tolerate it. A fabricated Anthropic "ping" event would claim the upstream
// said something it never said.
const earlyFlushKeepAlive = ": keep-alive\n\n"

// earlyFlushMaxSniff caps what the sniff will buffer.
//
// This middleware runs ahead of inbound auth (extraMiddleware precedes the
// route group's key check), so reading the body here hands unauthenticated
// callers a memory amplifier unless it is bounded. Absent or chunked lengths
// skip the wrap entirely rather than trust-and-read: the real clients send
// Content-Length, and a request this cannot see is merely unprotected from
// 524s, not broken.
const earlyFlushMaxSniff = 32 << 20

// earlyFlushNotes carries the self-reports out of the writer.
//
// A callback set rather than a journal dependency, for the same reason the
// stall guard reports through onStall: the writer stays a pure HTTP wrapper,
// and the journal wiring lives with Runtime's other dependencies.
type earlyFlushNotes struct {
	// flushed fires when the preamble goes out: a request that would
	// otherwise have been at the edge's mercy is now safe. verdict says what
	// the body had established about streaming at that moment --
	// verdictStreaming when the sniff (full or prefix) had proven it,
	// verdictUnknown when desperation fired before the stream field arrived
	// and the preamble went out on the odds.
	flushed func(wait time.Duration, verdict streamVerdict)
	// desperationHeld fires when the desperation deadline passed on a body
	// whose prefix had already said stream:false -- the preamble is WITHHELD.
	// SSE headers on a JSON response do not degrade it, they destroy it: the
	// client's non-streaming call parses the body as JSON and reports
	// "empty or malformed response (HTTP 200)", an error it cannot retry
	// around, where the 524 this risks is an honest failure retried
	// automatically. Measured 2026-08-06: Claude Code's non-streaming
	// fallback sends exactly these large stream:false bodies, ~9 per
	// afternoon through the slow tunnel.
	desperationHeld func(wait time.Duration)
	// translated fires when a post-preamble failure is delivered as an
	// in-stream error event instead of its HTTP status.
	translated func(status int)
	// timedOut fires when the silence watchdog ends a stream the upstream
	// never spoke on.
	timedOut func(silence time.Duration)
}

// streamVerdict is what the request body has established about streaming.
type streamVerdict int

const (
	// verdictUnknown: the stream field has not arrived yet (or never will).
	verdictUnknown streamVerdict = iota
	// verdictStreaming: the field exists and is not literal false -- the
	// upstream handler's own predicate.
	verdictStreaming
	// verdictNonStreaming: the field exists and is literal false.
	verdictNonStreaming
)

// sniffStreamField applies the upstream's streaming predicate to however much
// of the body has arrived.
//
// gjson tolerates the truncation: a top-level field that is complete in the
// prefix is found, one that is still in flight is not (verdictUnknown, the
// safe answer). String contents cannot fool it on an intact prefix -- gjson
// parses structure, so a conversation that merely mentions "stream":false
// stays a string. A prefix cut mid-string could in principle misalign the
// scan, but the failure modes are a withheld preamble (a 524 instead of a
// save) or a preamble on the odds (today's behavior), never a crash.
func sniffStreamField(prefix []byte) streamVerdict {
	s := gjson.GetBytes(prefix, "stream")
	switch {
	case !s.Exists():
		return verdictUnknown
	case s.Type == gjson.False:
		return verdictNonStreaming
	default:
		return verdictStreaming
	}
}

// bodySniffer accumulates the request body while letting the desperation
// timer read a consistent prefix mid-upload.
//
// It exists because the desperation decision used to be made with no body at
// all -- the handler goroutine was parked inside GetRawData, and the timer
// fired blind, betting that every large body streams. Claude Code's
// non-streaming fallback lost that bet (see earlyFlushNotes.desperationHeld);
// buffering the upload here is what gives the timer something to look at.
type bodySniffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// readAll drains r into the buffer and returns the complete body.
func (s *bodySniffer) readAll(r io.Reader) ([]byte, error) {
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			s.buf.Write(chunk[:n])
			s.mu.Unlock()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Bytes(), nil
}

// verdict reports what the bytes so far say about streaming. Called from the
// timer goroutine; the scan runs under the lock, briefly pausing the upload
// copy loop, which at one call per request is noise.
func (s *bodySniffer) verdict() streamVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sniffStreamField(s.buf.Bytes())
}

// EarlyFlushMiddleware wraps streaming /v1/messages requests with the early
// SSE preamble described above.
//
// Registered after ErrorEnvelopeMiddleware, so this writer sits closer to the
// handler: its preamble and passthrough writes flow through the envelope
// writer, which treats a 200 SSE response as the stream it is and stays out
// of the way. Requests the threshold never fires on are byte-for-byte
// untouched.
func EarlyFlushMiddleware(delay time.Duration, notes earlyFlushNotes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if delay <= 0 || c.Request.Method != http.MethodPost || c.Request.URL.Path != "/v1/messages" {
			c.Next()
			return
		}
		if cl := c.Request.ContentLength; cl <= 0 || cl > earlyFlushMaxSniff {
			c.Next()
			return
		}
		// The clock starts BEFORE the body is read, because the edge's does.
		//
		// The body read originally started the clock, on the assumption that
		// the last body byte arriving is roughly when the edge starts
		// counting. Measured on 2026-08-05, it is not: Cloudflare's ~100s
		// window runs from the request's start, and a 1.8MB conversation
		// crawling through the tunnel spent 66-96s of that window in upload
		// alone. Anchoring the threshold at body-complete then added its full
		// delay on top -- one request flushed at arrival+96s and survived by
		// 4 seconds, the next flushed at arrival+115s and came back as the
		// exact 524 this file exists to prevent. The deadline is
		// arrival+delay; a slow upload eats the wait, and a request whose
		// upload alone crossed the threshold flushes the moment the sniff can
		// rule it streaming.
		entered := time.Now()

		// The writer is built before the body read so the desperation timer
		// has something to flush through. It is not installed as c.Writer
		// until the sniff has ruled; until then only the timer goroutine
		// touches it, and the request goroutine is parked inside GetRawData
		// -- the same timer-vs-handler discipline the state machine already
		// enforces, with the handler side silent by construction.
		reqCtx := c.Request.Context()
		ctx, cancel := context.WithCancel(reqCtx)
		w := &earlyFlushWriter{
			ResponseWriter: c.Writer,
			notes:          notes,
			started:        entered,
			reqCtx:         reqCtx,
			cancelHandler:  cancel,
			shadow:         cloneHeader(c.Writer.Header()),
		}
		// Desperation: if the body is STILL uploading this close to the
		// edge's ~100s deadline, commit the preamble without waiting for the
		// full sniff. Same evening, same journal: two uploads outlived the
		// whole window (flush at arrival+105s and +121s -- both into
		// connections the edge had already severed).
		//
		// But no longer blind. The original bet -- "large POST /v1/messages
		// bodies always stream, so SSE headers on a JSON response are just a
		// different spelling of the same loss" -- was measurably wrong on both
		// counts: Claude Code's non-streaming fallback sends large
		// stream:false bodies (9 in one afternoon of 2026-08-06), and SSE
		// headers are not a different spelling of a 524 but strictly worse --
		// the 524 is an honest retryable failure, the SSE-wrapped JSON is
		// "empty or malformed response (HTTP 200)". So the upload is now
		// buffered where the timer can see it, and flushNow consults the
		// prefix: a proven stream:false holds fire.
		sniff := &bodySniffer{}
		w.sniffVerdict = sniff.verdict
		w.timer = time.AfterFunc(earlyFlushDesperation, w.flushNow)

		body, err := sniff.readAll(c.Request.Body)
		if err != nil {
			if !w.tryDisarm() {
				// The preamble is already on the wire for a body that never
				// finished arriving; end the stream honestly rather than
				// handing the handler a broken body behind committed headers.
				w.finish()
				cancel()
				c.Abort()
				return
			}
			cancel()
			c.Next()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		// The upstream's own predicate, exactly (code_handlers.go): the field
		// exists and is not literal false. Bool() would call "stream": 1
		// non-streaming while the upstream streams it, leaving that request
		// unprotected.
		if s := gjson.GetBytes(body, "stream"); !s.Exists() || s.Type == gjson.False {
			if w.tryDisarm() {
				cancel()
				c.Next()
				return
			}
			// Desperation already committed SSE headers for what turned out
			// to be a non-streaming request. There is no un-sending them; the
			// wrapper stays installed so the teardown machinery -- heartbeat,
			// watchdog, finish -- runs, and the response is delivered inside
			// the stream. The alternative this replaced was the edge's 524.
		}

		// The derived context is the watchdog's lever: cancelling it is what
		// makes a wedged executor unwind after the stream has been ended.
		c.Request = c.Request.WithContext(ctx)

		// What is left of the threshold after the upload; zero fires the
		// flush immediately on the timer goroutine, through the same state
		// machine as a normal expiry. A no-op when desperation already fired.
		remaining := delay - time.Since(entered)
		if remaining < 0 {
			remaining = 0
		}
		w.rearm(remaining)
		c.Writer = w
		// Deferred, not sequenced: a panicking handler must still tear the
		// timer and heartbeat down, or they outlive the request and write
		// into a writer gin has already recycled for someone else.
		defer func() {
			w.finish()
			cancel()
		}()
		c.Next()
	}
}

// cloneHeader deep-copies a header map, value slices included.
func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// earlyFlushWriter is the response writer that does the work.
//
// States, entered in order and never left backwards:
//
//	untouched  -- the handler wrote inside the threshold; every call is a
//	              plain forward and the wrapper might as well not exist.
//	preflushed -- the timer fired first: 200 + SSE headers are on the wire
//	              and a heartbeat keeps the connection warm.
//	streamed   -- the handler's own bytes joined the preflushed stream; from
//	              here the handler owns the wire, including its own SSE error
//	              frames (WriteTerminalError), which pass through verbatim.
//	translate  -- after the preamble but before any stream bytes, the handler
//	              tried to write an HTTP failure; its status and body are
//	              captured and re-delivered as one SSE error event in finish.
//	terminated -- the silence watchdog ended the stream; later writes are
//	              swallowed.
//	closed     -- finish ran. The tombstone every goroutine checks first.
type earlyFlushWriter struct {
	gin.ResponseWriter

	notes         earlyFlushNotes
	started       time.Time
	reqCtx        context.Context
	cancelHandler context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	// sniffVerdict asks the in-flight body what it has said about streaming.
	// Set for the desperation window only; rearm clears it once the full
	// sniff has ruled the request streaming, after which a firing needs no
	// second opinion.
	sniffVerdict func() streamVerdict
	// shadow is the only header map the handler ever sees. Its contents move
	// to the real map under mu by whichever call transmits first; after the
	// preamble it is a decoy, absorbing header writes that can no longer
	// matter. This is what keeps the timer goroutine and the handler's
	// lock-free header writes off the same map -- a concurrent map write is
	// a runtime-fatal crash of the whole proxy, not a request failure.
	shadow    http.Header
	committed bool
	// decided means the handler has expressed an outcome itself (a write, a
	// status, or a bare flush); it is what the timer checks to know it lost.
	decided     bool
	preflushed  bool
	streamed    bool
	translating bool
	terminated  bool
	closed      bool
	// intended is the status the handler meant to send after the preamble
	// made that impossible. Status() reports it so the journal records the
	// truth rather than the wire's unconditional 200.
	intended int
	errBuf   bytes.Buffer

	hbStop chan struct{}
	hbDone chan struct{}
}

// commitShadowLocked moves the handler's header state onto the real map.
//
// Replace, not merge: the shadow started as a clone, so a key the handler
// deleted is absent here and must become absent there. Only ever called with
// mu held and only before anything has been transmitted.
func (w *earlyFlushWriter) commitShadowLocked() {
	if w.committed {
		return
	}
	w.committed = true
	dst := w.ResponseWriter.Header()
	for k := range dst {
		if _, ok := w.shadow[k]; !ok {
			delete(dst, k)
		}
	}
	for k, v := range w.shadow {
		dst[k] = append([]string(nil), v...)
	}
}

// Header hands the handler the shadow map, always.
//
// gin re-fetches this on every c.Header call, so nothing caches the real map
// across the preamble boundary.
func (w *earlyFlushWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.shadow
}

// tryDisarm retires a writer that was never installed: the timer is stopped
// and the tombstone set, so a concurrent fire finds nothing to do. Returns
// false when the preamble is already out -- committed headers cannot be
// retired, and the caller must keep the wrapper on the request.
func (w *earlyFlushWriter) tryDisarm() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.preflushed {
		return false
	}
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
	}
	return true
}

// rearm replaces the desperation deadline with what is left of the real
// threshold, once the sniff has ruled the request streaming. A no-op when
// the preamble already went out -- there is nothing left to schedule.
func (w *earlyFlushWriter) rearm(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.preflushed {
		return
	}
	// The full body has been sniffed and ruled streaming; a later firing
	// needs no prefix consultation, and the sniffer can be collected.
	w.sniffVerdict = nil
	w.timer.Stop()
	w.timer = time.AfterFunc(d, w.flushNow)
}

// flushNow is the timer callback: commit the preamble if the handler has not
// spoken and the request is still alive.
func (w *earlyFlushWriter) flushNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.decided || w.preflushed {
		return
	}
	verdict := verdictStreaming
	if w.sniffVerdict != nil {
		verdict = w.sniffVerdict()
	}
	if verdict == verdictNonStreaming {
		// The prefix has already said stream:false. Hold fire: the preamble
		// would turn this request's JSON response into the client's
		// unretryable "malformed response (HTTP 200)", while doing nothing
		// leaves at worst an honest, retryable 524. The timer is spent; the
		// finished sniff will disarm the wrapper through the ordinary path.
		if w.notes.desperationHeld != nil {
			w.notes.desperationHeld(time.Since(w.started))
		}
		return
	}
	w.preflushed = true
	// From here the shadow is a decoy; the real map belongs to this side of
	// the lock alone.
	w.committed = true

	h := w.ResponseWriter.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Tells nginx-compatible relays not to buffer; harmless elsewhere.
	h.Set("X-Accel-Buffering", "no")
	w.ResponseWriter.WriteHeader(http.StatusOK)
	w.ResponseWriter.Flush()

	if w.notes.flushed != nil {
		w.notes.flushed(time.Since(w.started), verdict)
	}

	w.hbStop = make(chan struct{})
	w.hbDone = make(chan struct{})
	go w.heartbeat(w.hbStop, w.hbDone)
}

// heartbeat keeps the committed-but-silent stream visibly alive, and ends it
// when the silence outlives any plausible upstream.
func (w *earlyFlushWriter) heartbeat(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	deadline := time.Now().Add(earlyFlushMaxSilence)
	ticker := time.NewTicker(earlyFlushHeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		w.mu.Lock()
		if w.closed || w.decided || w.terminated {
			// The handler is writing its own bytes now, or the request is
			// over; either way the silence this covers has ended.
			w.mu.Unlock()
			return
		}
		if time.Now().After(deadline) {
			w.terminated = true
			w.intended = http.StatusGatewayTimeout
			_, _ = w.ResponseWriter.Write(sseErrorFrame(http.StatusGatewayTimeout, nil))
			w.ResponseWriter.Flush()
			cancel, notes := w.cancelHandler, w.notes
			w.mu.Unlock()
			// Outside the lock: cancel unwinds the executor, which re-enters
			// this writer on its way out.
			if cancel != nil {
				cancel()
			}
			if notes.timedOut != nil {
				notes.timedOut(earlyFlushMaxSilence)
			}
			return
		}
		_, _ = w.ResponseWriter.Write([]byte(earlyFlushKeepAlive))
		w.ResponseWriter.Flush()
		w.mu.Unlock()
	}
}

func (w *earlyFlushWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer.Stop()
	if !w.preflushed {
		w.decided = true
		w.commitShadowLocked()
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.terminated || w.closed {
		return
	}
	w.decided = true
	w.intended = code
	// Only a failure BEFORE any stream bytes enters translation. Once the
	// handler has streamed, a status write is mid-stream bookkeeping
	// (WriteTerminalError calls c.Status before its own SSE error frame, and
	// that frame must pass through verbatim -- capturing it here would
	// re-wrap it and rewrite its error type, destroying the client's retry
	// signal).
	if code >= http.StatusBadRequest && !w.streamed {
		w.translating = true
	}
}

func (w *earlyFlushWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer.Stop()
	if !w.preflushed {
		w.decided = true
		w.commitShadowLocked()
		return w.ResponseWriter.Write(b)
	}
	if w.terminated || w.closed {
		// The stream was already ended on the wire; these bytes have nowhere
		// to go. Report success so the handler finishes its own unwind.
		return len(b), nil
	}
	w.decided = true
	if w.translating {
		// Captured for the error frame. Past the envelope bound it cannot be
		// a failure body anyone meant to parse; the head is enough for
		// isUpstreamFailure/replacementEnvelope to work with.
		if w.errBuf.Len() < maxEnvelopeBody {
			w.errBuf.Write(b[:min(len(b), maxEnvelopeBody-w.errBuf.Len())])
		}
		return len(b), nil
	}
	w.streamed = true
	return w.ResponseWriter.Write(b)
}

func (w *earlyFlushWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Flush counts as the handler speaking: the upstream's empty-stream success
// path sets its headers and flushes without a single Write, and that is a
// completed response, not a silent one.
func (w *earlyFlushWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.preflushed {
		w.timer.Stop()
		w.decided = true
		w.commitShadowLocked()
		w.ResponseWriter.Flush()
		return
	}
	if w.terminated || w.closed || w.translating {
		return
	}
	w.decided = true
	w.ResponseWriter.Flush()
}

// Written reports true once the preamble is out: upstream's
// `if !c.Writer.Written()` guards must not try to re-send headers that are
// already on the wire.
func (w *earlyFlushWriter) Written() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.preflushed {
		return true
	}
	return w.ResponseWriter.Written()
}

// Status reports the handler's intended status when there is one, so the
// journal middlewares behind this wrapper file the request under the outcome
// it actually had -- the wire's 200 is a delivery detail.
func (w *earlyFlushWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.intended != 0 {
		return w.intended
	}
	return w.ResponseWriter.Status()
}

// finish stops the machinery and, when a preflushed stream ended without the
// handler concluding it, delivers the one closing SSE error event.
func (w *earlyFlushWriter) finish() {
	w.mu.Lock()
	// The tombstone. From this moment a timer callback or heartbeat tick
	// that was already in flight finds closed=true and does nothing -- the
	// alternative, observed under test, is a preamble committed to a writer
	// gin has already recycled and a heartbeat goroutine nothing ever stops.
	w.closed = true
	w.timer.Stop()
	stop, done := w.hbStop, w.hbDone
	w.hbStop, w.hbDone = nil, nil
	w.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.preflushed || w.terminated || w.streamed {
		return
	}
	if w.decided && !w.translating {
		return
	}
	// A cancelled request reaching here is the client hanging up during the
	// wait: the upstream returns without writing (its ctx.Done branch), and
	// there is neither anyone to send an error frame to nor an upstream
	// failure to report -- fabricating a 502 here would file voluntary
	// cancels as upstream faults, the exact confusion the failure taxonomy
	// exists to prevent.
	if w.reqCtx != nil && w.reqCtx.Err() != nil {
		return
	}

	status := w.intended
	if status == 0 {
		// The handler returned without writing anything on a live request;
		// the stream must still end explicitly or the client holds a silent
		// open connection.
		status = http.StatusBadGateway
		w.intended = status
	}
	_, _ = w.ResponseWriter.Write(sseErrorFrame(status, w.errBuf.Bytes()))
	w.ResponseWriter.Flush()

	if w.notes.translated != nil {
		w.notes.translated(status)
	}
}

// sseErrorFrame renders one terminal SSE error event.
//
// SSE frames are line-oriented: a payload with a newline would smear across
// data: fields. replacementEnvelope marshals to a single line and its message
// is bounded ASCII, so anything multi-line -- or anything that fails the
// upstream-envelope allowlist -- goes through it.
func sseErrorFrame(status int, body []byte) []byte {
	body = bytes.TrimSpace(body)
	if bytes.ContainsAny(body, "\r\n") || !isUpstreamFailure(body) {
		body = replacementEnvelope(status, body)
	}
	var frame bytes.Buffer
	frame.WriteString("event: error\ndata: ")
	frame.Write(body)
	frame.WriteString("\n\n")
	return frame.Bytes()
}

// noteEarlyFlush records a preamble commit on the request timeline.
//
// Kept here rather than inside the writer so the writer stays a pure HTTP
// wrapper, and so the journal dependency lives with the rest of Runtime's --
// the same split noteStall uses, and for the same reason: the save must be
// self-reported, because nothing downstream knows it happened.
func (r *Runtime) noteEarlyFlush(wait time.Duration, verdict streamVerdict) {
	if r == nil {
		return
	}
	detail := fmt.Sprintf(i18n.T(
		"流式请求 %s 无输出，已提前发送 SSE 响应头保住连接（否则会被边缘按 ~100s 斩成 524）",
		"streaming request silent for %s; SSE preamble sent early to keep the connection (the edge would sever it as a 524 at ~100s)"), wait.Round(time.Second))
	if verdict == verdictUnknown {
		// The distinction matters for the audit trail: an unknown-verdict
		// flush onto what turns out non-streaming is the one residual way a
		// JSON response can still end up behind SSE headers, and finding
		// those cases is how the residue gets sized.
		detail = fmt.Sprintf(i18n.T(
			"请求上传 %s 仍未完成，desperation 预发 SSE 头（前缀尚未出现 stream 字段，按流式赌——若实为非流式，客户端会报 malformed 200）",
			"request still uploading after %s; desperation preamble sent (no stream field in the prefix yet, betting on streaming -- a non-streaming request would surface as the client's malformed 200)"), wait.Round(time.Second))
	}
	r.Note(journal.Event{Kind: journal.KindHealth, State: "earlyflush", Detail: detail})
}

// noteDesperationHeld records a withheld desperation preamble.
func (r *Runtime) noteDesperationHeld(wait time.Duration) {
	if r == nil {
		return
	}
	r.Note(journal.Event{
		Kind:  journal.KindHealth,
		State: "earlyflush",
		Detail: fmt.Sprintf(i18n.T(
			"非流式请求上传 %s 仍未完成；前缀已见 stream:false，按兵不动（SSE 预发头会把 JSON 响应毁成客户端不可重试的 malformed 200，宁可让它冒 524 的险）",
			"non-streaming request still uploading after %s; the prefix says stream:false, so the preamble is withheld (SSE headers would destroy the JSON response into the client's unretryable malformed 200 -- risking a 524 is the better loss)"), wait.Round(time.Second)),
	})
}

// noteEarlyFlushTranslated records a failure that had to travel in-stream.
func (r *Runtime) noteEarlyFlushTranslated(status int) {
	if r == nil {
		return
	}
	r.Note(journal.Event{
		Kind:  journal.KindHealth,
		State: "earlyflush",
		Detail: fmt.Sprintf(i18n.T(
			"预发响应头之后上游失败（HTTP %d），已转为流内 error 事件送达",
			"upstream failed after the preamble (HTTP %d); delivered as an in-stream error event"), status),
	})
}

// noteEarlyFlushTimeout records the silence watchdog ending a stream.
func (r *Runtime) noteEarlyFlushTimeout(silence time.Duration) {
	if r == nil {
		return
	}
	r.Note(journal.Event{
		Kind:  journal.KindHealth,
		State: "earlyflush",
		Detail: fmt.Sprintf(i18n.T(
			"预发响应头后上游沉默超过 %s，已发流内超时错误并结束请求",
			"upstream stayed silent for over %s after the preamble; stream ended with an in-stream timeout error"), silence.Round(time.Second)),
	})
}
