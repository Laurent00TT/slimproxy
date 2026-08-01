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

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"

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
// reach the usage pipeline, and an earlier version of this comment claiming
// otherwise was wrong: severing works by cancelling the upstream context, so
// the inner executor's own reporter publishes the resulting "context
// canceled", and metrics files the request under CauseCanceled -- the bucket
// meaning "the operator pressed Ctrl-C", deliberately excluded from health
// alerting. A stall would therefore be invisible in `slimproxy log` and would
// never contribute to a failure streak.
//
// That is what onStall exists for: the guard reports itself, rather than
// hoping a string survives a pipeline it does not control.
func errStreamStalled(idle time.Duration) error {
	return fmt.Errorf("slimproxy: upstream stream stalled: no data for %s (stream idle timeout); severed so the client can retry", idle)
}

// stallGuard wraps a provider executor and severs streams that stop moving.
type stallGuard struct {
	inner coreauth.ProviderExecutor
	idle  time.Duration
	// onStall records a firing. Optional; nil in tests that only exercise the
	// stream mechanics.
	onStall func(time.Duration)
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
	out := make(chan cliproxyexecutor.StreamChunk)
	go g.pump(ctx, cancelUpstream, result.Chunks, out)
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}, nil
}

// pump forwards chunks and enforces the idle window on upstream waits only.
func (g *stallGuard) pump(reqCtx context.Context, cancelUpstream context.CancelFunc, in <-chan cliproxyexecutor.StreamChunk, out chan<- cliproxyexecutor.StreamChunk) {
	defer close(out)
	defer cancelUpstream()
	timer := time.NewTimer(g.idle)
	defer timer.Stop()
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
				return
			}
			select {
			case out <- chunk:
			case <-reqCtx.Done():
				return
			}
		case <-timer.C:
			log.Warnf(i18n.T(
				"slimproxy: 上游流 %s 无数据，已切断让客户端立即重试（此前这种流会挂住数分钟）",
				"slimproxy: upstream stream sent nothing for %s; severed so the client retries now (these used to hang for minutes)"),
				g.idle)
			if g.onStall != nil {
				g.onStall(g.idle)
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errStreamStalled(g.idle)}:
			case <-reqCtx.Done():
			}
			cancelUpstream()
			// Let the inner stream goroutine observe the cancel and finish;
			// anything it still sends describes a stream already pronounced
			// dead.
			for range in {
			}
			return
		case <-reqCtx.Done():
			return
		}
	}
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
func ensureStallGuard(mgr *coreauth.Manager, idle time.Duration, onStall func(time.Duration)) {
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
	mgr.RegisterExecutor(&stallGuard{inner: exec, idle: idle, onStall: onStall})
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

// noteStall records a firing on the request timeline.
//
// Kept here rather than inside the guard so the guard stays a pure stream
// wrapper, and so the journal dependency lives with the rest of Runtime's.
func (r *Runtime) noteStall(idle time.Duration) {
	if r == nil {
		return
	}
	r.Note(journal.Event{
		Kind:  journal.KindHealth,
		State: "stall",
		Detail: fmt.Sprintf(i18n.T(
			"上游流 %s 无数据，已切断（客户端会立即重试）",
			"upstream stream sent nothing for %s; severed (the client retries immediately)"), idle),
	})
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
	ensureStallGuard(r.Credentials(), idle, r.noteStall)
	ticker := time.NewTicker(stallGuardInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ensureStallGuard(r.Credentials(), idle, r.noteStall)
	}
}
