package proxy

// Cutting stalled upstream streams loose instead of waiting them out.
//
// The failure this exists for, measured on 2026-08-01: a streaming response
// starts normally (first bytes in seconds), runs for minutes, and then the
// upstream TCP flow dies silently -- no FIN, no RST, no error, just no more
// bytes. Anthropic's SSE stream carries periodic ping events, so a healthy
// stream is never quiet for long; a stream with nothing to say for 90 seconds
// is a dead one. Without this guard the proxy sits on the corpse until the
// client's own stall detector gives up, which took five to nine minutes per
// occurrence -- and the client's eventual retry works, so every stalled stream
// was pure wasted wall-clock.
//
// The guard wraps the claude provider executor and watches the gap between
// stream chunks. When the gap exceeds the configured idle window it emits one
// explicit error chunk -- worded so the failure classifies as a timeout, not a
// cancel -- severs the upstream request, and closes the stream. The client
// gets a definite failure in ~90 seconds instead of an indefinite hang, and
// its automatic retry starts that much sooner.
//
// Two deliberate scope choices:
//
//   - Only the chunk channel is watched. The alternative -- wrapping the HTTP
//     transport via the round-tripper hook -- would replace the executor's
//     internal client, which owns TLS behavior this proxy must not disturb.
//     Chunk arrival is also the correct signal: it measures progress in the
//     unit the client consumes.
//
//   - Only waits ON THE UPSTREAM are timed. A slow *reader* (the client
//     draining its half of the stream slowly) must not trip the guard, so the
//     timer is re-armed at the top of each receive and firings that happen
//     while a forward is in flight are drained before the next arm.
//
// Like the refusal hooks and the log output, the wrap does not survive on its
// own: the upstream service re-registers a fresh ClaudeExecutor on every auth
// update and config reload, silently discarding whatever was installed. A
// ticker reasserts the wrap. The window between replacement and reassertion is
// bounded by stallGuardInterval, and every stream that starts inside one runs
// unguarded until it ends -- streams pin their executor at start, so the cost
// is however many streams begin in that window, not one.
//
// Since 2026-08-06 the pump also keeps the books on how a stream ENDED. The
// upstream SDK's stream reporter publishes a usage record only when it scans a
// line carrying a top-level usage field (the tail message_delta) or hits a
// scanner error; a stream that ended cleanly before that line used to produce
// no usage record and no journal event at all, while the access log recorded a
// 200 -- two such requests were found in two days of logs, findable only by
// cross-auditing the two files. That gap is now covered from both ends. The
// SDK fork under third_party/ carries an EnsurePublished backstop, so such a
// stream at least leaves a zero-token record; and the pump watches the same
// signal the SDK does and self-reports the endings with their stream identity
// attached: "notail" when the stream closed without the tail, "clientdrop"
// when the request side hung up while the upstream was still open. Both arm
// themselves on the claude message_start event, so translated routes -- whose
// usage lines the translators mostly nest or rename -- stay silent instead of
// phantoming. See stallGuardNotes.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/Laurent00TT/slimproxy/i18n"
	"github.com/Laurent00TT/slimproxy/journal"
)

// stallGuardProvider is the one provider wrapped.
//
// Deliberately not "every provider": the wrapper forwards exactly the method
// set ClaudeExecutor has, and wrapping an executor with a wider set (the
// aistudio websocket executor, say) would silently strip the extra interfaces
// -- the same class of bug this repository has had to dig out of upstream
// twice. ensureStallGuard verifies the fit by reflection before wrapping.
const stallGuardProvider = "claude"

// defaultStreamIdle is how long a stream may go silent before it is severed.
//
// Generous on purpose. Anthropic emits SSE pings during thinking pauses, so
// live streams tick over every few dozen seconds at worst; the stalls this
// guard exists for were multi-minute black holes. 90 seconds is several ping
// intervals of margin while still cutting the observed 5-9 minute hangs by
// most of their length.
const defaultStreamIdle = 90 * time.Second

// stallGuardInterval is how long a fresh upstream re-registration can go
// unguarded. A var only so tests can shorten it.
var stallGuardInterval = 5 * time.Second

// stallGuardMismatchLastWarn throttles the method-set warning.
//
// Rate-limited rather than once-per-process: the refusal means the proxy is
// running with no stall guard at all -- the multi-minute hangs are back -- and
// a single line at startup is a line an operator scrolls past, especially in
// panel mode where logs go to a file. Repeating hourly keeps a permanently
// degraded state visible without becoming noise. Nanoseconds since the epoch,
// zero meaning never.
var stallGuardMismatchLastWarn atomic.Int64

// stallGuardMismatchWarnEvery is how often the refusal repeats itself.
var stallGuardMismatchWarnEvery = time.Hour

// errStreamStalled is the chunk error a severed stream ends with.
//
// This reaches the caller's SSE error frame and nothing else. It does NOT
// reach the usage pipeline: severing works by cancelling the upstream context,
// and what the inner executor's reporter publishes then depends on where the
// cancel lands. Caught blocked on the upstream read, the read error is
// published as "context canceled" and metrics files the request under
// CauseCanceled -- the bucket meaning "the operator pressed Ctrl-C". The
// health tracker skips such a record only when the upstream's first byte had
// already arrived (metrics.abandonedAfterAnswer), which is the mid-stream
// stall this guard exists for; one severed before any byte counts toward an
// unreachable streak like any other canceled wait. Caught parked on its
// channel send, the inner goroutine returns without publishing anything at
// all. An earlier version of this comment asserted the first outcome
// unconditionally; the second is the same silent ending the noTail note
// exists for. Either way a mid-stream stall would be invisible in
// `slimproxy log` and would never contribute to a failure streak.
//
// That is what notes.stalled exists for: the guard reports itself, rather
// than hoping a string survives a pipeline it does not control.
func errStreamStalled(idle time.Duration) error {
	return fmt.Errorf("slimproxy: upstream stream stalled: no data for %s (stream idle timeout); severed so the client can retry", idle)
}

// stallStreamMeta says which stream a note is about, so the resulting journal
// event can answer "which model, which credential, how far in" without a trip
// to the access log. Captured at ExecuteStream, where the request context is
// still on hand -- the pump itself only ever sees chunks.
type stallStreamMeta struct {
	model string
	// auth is the credential's ID, falling back to its label. ID first is a
	// join-key decision, not a readability one: the KindRequest events these
	// health events sit next to carry usage.Record.AuthID (collector.go), and
	// a jq join across the two rows must not depend on whether the credential
	// happens to have a label.
	auth    string
	started time.Time
}

// age is how long the stream had been running when the note fired.
func (m stallStreamMeta) age() time.Duration { return time.Since(m.started) }

// stallGuardNotes carries the guard's self-reports out of the stream wrapper.
//
// A callback set rather than a journal dependency, for the same reason
// earlyFlushNotes is one: the guard stays a pure stream wrapper, and the
// journal wiring lives with Runtime's other dependencies. Every field is
// optional; nil in tests that only exercise the stream mechanics.
type stallGuardNotes struct {
	// stalled fires when the idle window expires and the stream is severed.
	stalled func(meta stallStreamMeta, idle time.Duration)
	// noTail fires when an armed stream -- one that opened with the claude
	// message_start event; see the ledger comment in pump -- closed cleanly,
	// request still alive, without one line carrying a top-level usage field
	// ever passing through. That line (the tail message_delta) is the only
	// publish trigger the upstream SDK's stream reporter has short of an
	// error, so a stream ending before it leaves no usage record and no
	// journal event while the access log records a 200. (Since the SDK fork
	// under third_party/ grew its EnsurePublished backstop the record half of
	// that sentence is history -- a zero-token record now appears -- but this
	// event remains the timeline entry that explains WHY the record is
	// zero-token.)
	noTail func(meta stallStreamMeta)
	// clientDrop fires when the request context ended while an armed stream
	// was still open, or was why it closed. accounted says whether the usage
	// tail or an error had passed through the channel by then -- and false is
	// a hedge, not a verdict: the SDK publishes at scan time, before the
	// event crosses the channel, and a cancel caught on the upstream read is
	// itself published as "context canceled", so an unaccounted drop may in
	// truth be fully recorded, recorded as canceled, or absent, decided by
	// where the cancel landed. The journal wording carries that uncertainty.
	clientDrop func(meta stallStreamMeta, accounted bool)
}

// stallGuard wraps a provider executor and severs streams that stop moving.
type stallGuard struct {
	inner coreauth.ProviderExecutor
	idle  time.Duration
	notes stallGuardNotes
}

func (g *stallGuard) Identifier() string { return g.inner.Identifier() }

func (g *stallGuard) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Non-streaming responses are read to completion inside the executor with
	// the transport's own limits. The stall being guarded is a property of
	// long-lived streams; this path is left alone.
	return g.inner.Execute(ctx, auth, req, opts)
}

func (g *stallGuard) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return g.inner.CountTokens(ctx, auth, req, opts)
}

func (g *stallGuard) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return g.inner.Refresh(ctx, auth)
}

func (g *stallGuard) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return g.inner.HttpRequest(ctx, auth, req)
}

// PrepareRequest forwards the credential-injection hook.
//
// The conductor discovers this method structurally, so the wrapper advertising
// it while the inner executor lacks it would change behavior; ensureStallGuard
// only wraps when the method sets match exactly, which keeps this a plain
// delegation.
func (g *stallGuard) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if p, ok := g.inner.(interface {
		PrepareRequest(*http.Request, *coreauth.Auth) error
	}); ok {
		return p.PrepareRequest(req, auth)
	}
	return nil
}

func (g *stallGuard) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	// The derived context is the sever mechanism: cancelling it is what makes
	// the inner executor abandon the dead connection and close its channel.
	upCtx, cancelUpstream := context.WithCancel(ctx)
	result, err := g.inner.ExecuteStream(upCtx, auth, req, opts)
	if err != nil || result == nil || result.Chunks == nil {
		cancelUpstream()
		return result, err
	}
	meta := stallStreamMeta{model: req.Model, started: time.Now()}
	if auth != nil {
		if meta.auth = auth.ID; meta.auth == "" {
			meta.auth = auth.Label
		}
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go g.pump(ctx, cancelUpstream, meta, result.Chunks, out)
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}, nil
}

// pump forwards chunks and enforces the idle window on upstream waits only.
func (g *stallGuard) pump(reqCtx context.Context, cancelUpstream context.CancelFunc, meta stallStreamMeta, in <-chan cliproxyexecutor.StreamChunk, out chan<- cliproxyexecutor.StreamChunk) {
	defer close(out)
	defer cancelUpstream()
	timer := time.NewTimer(g.idle)
	defer timer.Stop()
	// The end-of-stream ledger, armed only for streams speaking the claude
	// dialect. accounted becomes true the moment the stream carries something
	// the upstream SDK's reporter publishes on: a line with a top-level usage
	// field (Publish) or a chunk-level error (PublishFailure). An armed
	// stream that ends without either has produced no usage record and never
	// will; the notes below are what make that ending visible.
	//
	// Arming matters because translated routes -- an openai-responses,
	// gemini, or interactions client on this same claude executor -- see
	// POST-translation chunks here while the SDK publishes off the raw claude
	// lines, and three of those four translator families nest or rename the
	// usage field. Mirroring the SDK's predicate against their output would
	// read every successful stream as a missing tail and flood the journal
	// with phantom events. message_start only ever appears in claude-format
	// output, so it is the arming signal: unarmed streams report nothing,
	// trading blindness on routes this deployment does not run for a journal
	// that can be trusted on the one it does.
	//
	// accounted is tracked at receive rather than at forward because the SDK
	// publishes on its side of the channel -- a chunk it managed to send was
	// already published, whether or not the client lived to read it. The
	// converse does not hold: the SDK publishes at scan time, BEFORE the
	// event crosses the channel, so accounted can lag the truth by one
	// in-flight event and false means "unknown", not "lost". The clientDrop
	// wording hedges accordingly.
	armed := false
	accounted := false
	for {
		// Re-arm at the top of every receive. A firing that happened while a
		// forward below was blocked on the reader is stale -- the upstream was
		// not the one being waited on -- and is drained here before it can be
		// mistaken for a stall.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(g.idle)

		select {
		case chunk, ok := <-in:
			if !ok {
				if !armed {
					return
				}
				// End of stream -- but WHOSE ending it was needs the request
				// context consulted first. A hung-up client cancels reqCtx,
				// the cancel unwinds the upstream, and the resulting close
				// can be ready in the same select as reqCtx.Done, which Go
				// picks between at random: without this check, roughly half
				// of routine client drops would be filed under the rare,
				// definitive notail (measured 974/2000 on a probe), drowning
				// the exact signal the event exists to isolate.
				if reqCtx.Err() != nil {
					if g.notes.clientDrop != nil {
						g.notes.clientDrop(meta, accounted)
					}
				} else if !accounted && g.notes.noTail != nil {
					// The request is alive and the upstream ended cleanly
					// without the tail: the silent zero-record ending. Say
					// so, because nothing downstream can.
					g.notes.noTail(meta)
				}
				return
			}
			if !armed || !accounted {
				opens, settles := scanLedgerSignals(chunk.Payload)
				armed = armed || opens
				accounted = accounted || settles || chunk.Err != nil
			}
			select {
			case out <- chunk:
			case <-reqCtx.Done():
				if armed && g.notes.clientDrop != nil {
					g.notes.clientDrop(meta, accounted)
				}
				return
			}
		case <-timer.C:
			log.Warnf(i18n.T(
				"slimproxy: 上游流 %s 无数据，已切断让客户端立即重试（此前这种流会挂住数分钟）",
				"slimproxy: upstream stream sent nothing for %s; severed so the client retries now (these used to hang for minutes)"),
				g.idle)
			if g.notes.stalled != nil {
				g.notes.stalled(meta, g.idle)
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errStreamStalled(g.idle)}:
			case <-reqCtx.Done():
			}
			cancelUpstream()
			// Let the inner stream goroutine observe the cancel and finish;
			// anything it still sends describes a stream already pronounced
			// dead. Neither noTail nor clientDrop is reported on this exit:
			// stalled already named this ending.
			for range in {
			}
			return
		case <-reqCtx.Done():
			if armed && g.notes.clientDrop != nil {
				g.notes.clientDrop(meta, accounted)
			}
			return
		}
	}
}

// scanLedgerSignals reports what one stream chunk means to the ledger: opens
// is the claude stream's opening event (message_start, the arming signal),
// settles is a line the upstream SDK's usage reporter publishes on.
//
// A chunk on the claude passthrough path is one whole SSE event -- an
// "event:" line, one or more "data:" lines, a blank terminator -- so the scan
// is per line, and the per-line predicate mirrors the SDK's
// (helps.ParseClaudeStreamUsage): strip the data: prefix, require valid JSON,
// require a TOP-LEVEL usage field. Top-level is load-bearing: message_start
// carries usage nested under "message", and counting it would settle every
// stream's books at its first event, blinding the check entirely -- which is
// what makes message_start servable as the arming signal and the settlement
// test in the same pass.
func scanLedgerSignals(payload []byte) (opens, settles bool) {
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || line[0] != '{' || !gjson.ValidBytes(line) {
			continue
		}
		if gjson.GetBytes(line, "type").String() == "message_start" {
			opens = true
		}
		if gjson.GetBytes(line, "usage").Exists() {
			settles = true
		}
	}
	return opens, settles
}

// ensureStallGuard wraps the claude executor if it is present and unwrapped.
//
// Safe to call at any moment: wrapping is idempotent (a guard is never
// re-wrapped) and RegisterExecutor replaces atomically under the manager's
// lock. When the inner executor's exported method set is wider than the
// wrapper forwards, the wrap is refused and reported -- an unguarded stream
// beats an executor with capabilities silently amputated.
//
// The read-then-register pair is NOT atomic, and upstream offers no
// compare-and-swap to make it so. If a re-registration lands between the two,
// this overwrites it with a wrap of the executor read a moment earlier; when
// the discarded one carried a fresh *config.Config (a reload rather than an
// auth refresh), that config stays shadowed until the next re-registration.
// The collision window is microseconds against an event that happens minutes
// apart, and the alternative -- not wrapping at all -- costs every stalled
// stream, so this is an accepted trade rather than an unseen one.
func ensureStallGuard(mgr *coreauth.Manager, idle time.Duration, notes stallGuardNotes) {
	if mgr == nil || idle <= 0 {
		return
	}
	exec, ok := mgr.Executor(stallGuardProvider)
	if !ok || exec == nil {
		return
	}
	if _, already := exec.(*stallGuard); already {
		return
	}
	if missing := uncoveredMethods(exec, &stallGuard{}); len(missing) > 0 {
		if shouldWarnMismatch() {
			log.Warnf(i18n.T(
				"slimproxy: 流空闲看门狗未安装：上游 executor 有本包装未转发的方法 %v。上游版本升级后需要同步扩展 stallGuard。",
				"slimproxy: stream stall guard NOT installed: upstream executor has methods this wrapper does not forward: %v. The stallGuard needs extending after the upstream version bump."),
				missing)
		}
		return
	}
	mgr.RegisterExecutor(&stallGuard{inner: exec, idle: idle, notes: notes})
}

// shouldWarnMismatch reports whether the refusal is due to be said again.
func shouldWarnMismatch() bool {
	now := time.Now().UnixNano()
	last := stallGuardMismatchLastWarn.Load()
	if last != 0 && now-last < int64(stallGuardMismatchWarnEvery) {
		return false
	}
	// A lost race means two lines at once, which is harmless; the alternative
	// (a lock) buys nothing for a log line.
	return stallGuardMismatchLastWarn.CompareAndSwap(last, now)
}

// uncoveredMethods lists exported methods of inner that wrapper lacks or
// declares with a different signature.
//
// One-directional by design, and the direction is the safe one: methods the
// inner executor has and the wrapper does not are capabilities that would be
// amputated, so they block the wrap. The reverse -- the wrapper declaring
// PrepareRequest when a future inner executor drops it -- would turn that
// method into a silent no-op; the delegation guards against it by type-
// asserting the inner and returning nil only when it genuinely has no such
// method, which is the same answer the conductor would get from the bare
// executor.
func uncoveredMethods(inner, wrapper any) []string {
	it := reflect.TypeOf(inner)
	wt := reflect.TypeOf(wrapper)
	var missing []string
	for i := 0; i < it.NumMethod(); i++ {
		m := it.Method(i)
		wm, ok := wt.MethodByName(m.Name)
		if !ok || !sameSignatureIgnoringReceiver(m.Type, wm.Type) {
			missing = append(missing, m.Name)
		}
	}
	return missing
}

func sameSignatureIgnoringReceiver(a, b reflect.Type) bool {
	if a.NumIn() != b.NumIn() || a.NumOut() != b.NumOut() {
		return false
	}
	for i := 1; i < a.NumIn(); i++ {
		if a.In(i) != b.In(i) {
			return false
		}
	}
	for i := 0; i < a.NumOut(); i++ {
		if a.Out(i) != b.Out(i) {
			return false
		}
	}
	return true
}

// healthEventFor stamps a guard note's stream identity onto a journal event.
//
// KindHealth events have always been free to carry the request fields; these
// are the first to use that. Model, credential and age answer the first three
// questions an operator asks of any of these events, and answering them here
// saves the trip to the access log that used to be the only way.
func healthEventFor(meta stallStreamMeta, state, detail string) journal.Event {
	return journal.Event{
		Kind:      journal.KindHealth,
		State:     state,
		Model:     meta.model,
		Auth:      meta.auth,
		LatencyMs: meta.age().Milliseconds(),
		Detail:    detail,
	}
}

// noteStall records a firing on the request timeline.
//
// Kept here rather than inside the guard so the guard stays a pure stream
// wrapper, and so the journal dependency lives with the rest of Runtime's.
func (r *Runtime) noteStall(meta stallStreamMeta, idle time.Duration) {
	if r == nil {
		return
	}
	r.Note(healthEventFor(meta, "stall", fmt.Sprintf(i18n.T(
		"上游流 %s 无数据，已切断（客户端会立即重试）",
		"upstream stream sent nothing for %s; severed (the client retries immediately)"), idle)))
}

// noteNoTail records a stream that ended without the usage tail.
//
// Since the SDK fork under third_party/ grew its EnsurePublished backstop,
// such a stream also leaves a zero-token success record in the books; this
// event is the timeline entry that explains why that record is zero-token.
// The once-considered alternative -- synthesizing a KindRequest event here --
// was decided against: it could never carry tokens or a true outcome, and a
// ledger entry that admits neither is worse than a health event that says so.
func (r *Runtime) noteNoTail(meta stallStreamMeta) {
	if r == nil {
		return
	}
	r.Note(healthEventFor(meta, "notail", i18n.T(
		"上游流在 usage 尾行（message_delta）出现前干净结束：SDK 兜底会补一条零 token 记录，这条事件解释它为何是零——回复可能被截断，也可能客户端已完整收到",
		"upstream stream ended cleanly before the usage tail line (message_delta): the SDK backstop files a zero-token record, and this event is why it is zero -- the reply may have been truncated, or the client may have received it whole")))
}

// noteClientDrop records the request side hanging up mid-stream.
//
// "Request side" rather than "client": the same cancel also arrives when the
// early-flush silence watchdog or a shutdown tears the handler context down.
// The distinction the event does draw is whether the books were already
// settled when the drop happened, because an unaccounted drop is the same
// zero-record ending noteNoTail describes, minus the certainty.
func (r *Runtime) noteClientDrop(meta stallStreamMeta, accounted bool) {
	if r == nil {
		return
	}
	detail := i18n.T(
		"请求侧在上游流结束前取消（多为客户端挂断）；usage 尾行未过流——取决于取消落在上游哪一步，这条请求可能被记成 canceled、可能其实已记全、也可能只剩 SDK 兜底的零 token 记录",
		"request side cancelled before the upstream stream ended (usually the client hanging up); the usage tail had not come through -- depending on where the cancel landed, the request may be filed as canceled, may in fact be fully recorded, or may leave only the SDK backstop's zero-token record")
	if accounted {
		detail = i18n.T(
			"请求侧在上游流结束前取消（多为客户端挂断）；usage 尾行或错误已经过流，账应已记全",
			"request side cancelled before the upstream stream ended (usually the client hanging up); the usage tail or an error had already passed through, so the books should be complete")
	}
	r.Note(healthEventFor(meta, "clientdrop", detail))
}

// stallNotes bundles the guard's reporters against this Runtime. Note is
// nil-safe, so the bundle is correct with or without a journal.
func (r *Runtime) stallNotes() stallGuardNotes {
	return stallGuardNotes{stalled: r.noteStall, noTail: r.noteNoTail, clientDrop: r.noteClientDrop}
}

// keepStallGuardInstalled reasserts the wrap for the run's lifetime.
//
// The eager call matters: a ticker's first tick is a full interval away, and
// a stream pins its executor when it starts, so without this every stream
// accepted in the first interval would run unguarded for its whole life. The
// startup case is covered earlier still -- Build installs the guard from the
// router configurator, before the server serves -- and this is the belt to
// that suspenders, for the case where the credential pool arrives late.
//
// Windows after each upstream re-registration remain: streams that start in
// one run unguarded until they end. Plural, not "at most one" -- several
// clients can start streams inside the same window, and re-registration
// happens on every auth refresh.
func (r *Runtime) keepStallGuardInstalled(ctx context.Context, idle time.Duration) {
	if idle <= 0 {
		return
	}
	ensureStallGuard(r.Credentials(), idle, r.stallNotes())
	ticker := time.NewTicker(stallGuardInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ensureStallGuard(r.Credentials(), idle, r.stallNotes())
	}
}
