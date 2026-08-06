package proxy

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	cliproxyapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	// Registers the built-in translator pairs. sdk/cliproxy pulls these in
	// transitively today, but depending on that is fragile: an unregistered
	// pair does not error, it forwards the untranslated body upstream.
	"github.com/Laurent00TT/slimproxy/fsperm"
	"github.com/Laurent00TT/slimproxy/journal"
	"github.com/Laurent00TT/slimproxy/metrics"
	"github.com/Laurent00TT/slimproxy/outbound"

	// Registers the built-in translator pairs.
	_ "github.com/Laurent00TT/slimproxy/translate"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// EffectiveConfigName returns the file this instance materializes its resolved
// CLIProxyAPI configuration into.
//
// Per-port, and that is a security property rather than tidiness. The name used
// to be a constant, while the state directory defaults to the working
// directory -- so starting a second instance in the same folder overwrote the
// file the first one is serving from. That file is what the upstream watcher
// reloads from, so a second instance configured with
// `allow-unauthenticated: true` would silently turn a running, publicly
// exposed proxy into an open relay. Validate() runs only at startup and would
// never see it.
func EffectiveConfigName(port int) string {
	return fmt.Sprintf("slimproxy.%d.effective.yaml", port)
}

// Version is stamped into the dashboard footer. Override with -ldflags.
var Version = "0.1.0"

// Runtime is an assembled but not yet started proxy.
//
// It exists because the CLI consumes in-process what the deleted dashboard
// consumed over HTTP. Telemetry and the credential pool used to be reachable
// only by serving them; now the caller holds them directly.
type Runtime struct {
	// Service is the assembled CLIProxyAPI service. Call Run to start it.
	Service *cliproxy.Service

	// Stats accumulates completed-request telemetry for the CLI to render.
	Stats *metrics.Collector

	// ConfigPath is where the effective CLIProxyAPI configuration was written.
	ConfigPath string

	// LogDir is the resolved application-log directory, empty when logs go to
	// stdout. Reported because WithoutStdoutLogs can turn file logging on that
	// the operator did not ask for, and a dashboard that silences the logs
	// without saying where they went is one that loses them.
	LogDir string

	// auth is the credential pool, captured during router configuration. Nil
	// until the service has been built. Atomic because the write happens on
	// the server's startup goroutine while the stall-guard ticker reads it
	// from this one.
	auth atomic.Pointer[coreauth.Manager]

	// streamIdle is the resolved stream stall window; zero disables the guard.
	streamIdle time.Duration

	// requireKeys records whether this instance started out demanding inbound
	// authentication. The guard only defends an invariant that existed.
	requireKeys bool

	// Journal records what happened, durably. Nil when journalling is off.
	Journal *journal.Writer
	rejects *journal.RejectFolder

	// relay is the adaptive outbound CONNECT relay, present only when
	// proxy-fallback-direct is on. The engine's proxy address points at it
	// for the life of the process; it decides per connection whether to
	// chain through the configured proxy or dial direct.
	relay *outbound.Relay

	// Health turns runs of failures into something said out loud. Present
	// regardless of whether journalling is on -- see where it is constructed.
	Health *metrics.Health
}

// JournalDirName is the event stream's directory, under the log directory.
const JournalDirName = "events"

// BuildOption adjusts how Build assembles the runtime.
type BuildOption func(*buildSettings)

type buildSettings struct {
	logOpts []logOption
}

// WithoutStdoutLogs keeps application logs off stdout.
//
// For a caller that owns the terminal -- the full-screen dashboard. Turns file
// logging on if it was off, so the lines are redirected rather than dropped;
// Runtime.LogDir then reports where they went.
func WithoutStdoutLogs() BuildOption {
	return func(s *buildSettings) { s.logOpts = append(s.logOpts, withoutStdout()) }
}

// Run starts the service and blocks until ctx is cancelled or it stops.
func (r *Runtime) Run(ctx context.Context) error {
	// The guard shares this Run's lifetime and can end it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.guardInboundAuth(ctx, cancel)
	go keepRefusalHooksInstalled(ctx)
	// The application log is displaced by the same class of event as the
	// refusal hooks -- a config reload -- and just as silently. See logpin.go.
	go keepLogOutputPinned(ctx)
	// The claude executor is replaced by upstream on every auth update, taking
	// the stall guard with it -- same decay, same remedy. See stallguard.go.
	go r.keepStallGuardInstalled(ctx, r.streamIdle)

	// Start and stop go into the same timeline as the requests. Half of
	// retrospective debugging is noticing that the thing being investigated
	// began at a restart.
	r.Note(journal.Event{Kind: journal.KindProxy, State: "start", Detail: r.ConfigPath})
	defer func() {
		r.Note(journal.Event{Kind: journal.KindProxy, State: "stop"})
		r.closeRelay()
		r.closeJournal()
	}()

	return r.Service.Run(ctx)
}

// Note records a state event, if journalling is on.
//
// Exported so the commands that change state -- tunnel up, doctor, credential
// removal -- land on the same timeline as the requests they affect. A tunnel
// reconnect next to a burst of failures explains them; in a separate file it
// explains nothing.
func (r *Runtime) Note(e journal.Event) {
	if r == nil || r.Journal == nil {
		return
	}
	r.Journal.Append(e)
}

func (r *Runtime) closeRelay() {
	if r == nil || r.relay == nil {
		return
	}
	_ = r.relay.Close()
}

func (r *Runtime) closeJournal() {
	if r == nil {
		return
	}
	// Folder first: it flushes pending batches into the writer.
	r.rejects.Close()
	if r.Journal != nil {
		_ = r.Journal.Close()
	}
}

// refusalHookReinstallInterval is how often the hooks are put back.
//
// Sized against how long a request stays open rather than how often the hooks
// are displaced: a streaming completion can run for a minute, and the window
// this closes is the part of that minute spent unprotected.
const refusalHookReinstallInterval = 2 * time.Second

// keepRefusalHooksInstalled re-registers the refusal hooks on a timer.
//
// The middleware installs them per request, which covers every request that
// starts with them in place. It does not cover a request already running when
// they are displaced -- and they are displaced regularly:
// syncPluginRuntimeConfigForConfig calls SetPluginHooks, and it runs on auth
// updates as well as config reloads. The credential auto-refresh rewrites the
// auth directory every 15 minutes, so a long streaming response has a real
// chance of losing the hooks mid-flight and delivering a policy refusal as an
// unexplained empty completion -- the exact failure this file exists to fix.
//
// Overwriting whatever the SDK installed is safe here specifically because
// build() pins Plugins.Enabled to false: its pluginHost carries no plugins and
// is a pass-through, so nothing is lost by replacing it. That would not hold in
// a deployment that enabled plugins, which is why it is stated rather than
// assumed.
//
// The registry has no getter, so this cannot check first -- it just reinstalls,
// which is idempotent.
func keepRefusalHooksInstalled(ctx context.Context) {
	ticker := time.NewTicker(refusalHookReinstallInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := installRefusalHooks(); err != nil {
			log.Warnf(i18n.T("slimproxy: 重新安装 refusal hook 失败，上游策略拒绝可能表现为空响应: %v", "slimproxy: reinstalling the refusal hook failed; upstream policy refusals may appear as empty responses: %v"), err)
			return
		}
	}
}

// authGuardInterval is how often the materialized config is re-checked.
//
// Frequent enough that a weakened config is caught in seconds rather than
// discovered in an access log, cheap enough to ignore: one small file read.
const authGuardInterval = 5 * time.Second

// guardInboundAuth stops the service if inbound authentication is removed from
// under it.
//
// Validate() runs once, in Build. After that the upstream watcher reloads the
// materialized file on every change, and its reload path does not re-validate:
// an empty api-keys list unregisters the only access provider, after which
// AuthMiddleware calls c.Next() for every request. A process holding live
// subscription credentials becomes an open relay, and the only trace is a
// debug-level line reading "api-keys count: 1 -> 0".
//
// Nothing here prevents an operator from deliberately configuring
// allow-unauthenticated; the guard only fires when a proxy that started out
// requiring keys stops requiring them. That transition is never intentional --
// applying it requires a restart anyway, since this stops the service.
func (r *Runtime) guardInboundAuth(ctx context.Context, stop context.CancelFunc) {
	if !r.requireKeys || r.ConfigPath == "" {
		// Started without inbound auth by explicit choice. There is no
		// invariant here to protect.
		return
	}

	ticker := time.NewTicker(authGuardInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		body, err := os.ReadFile(r.ConfigPath)
		if err != nil {
			// Unreadable is not the same as weakened, and the watcher will
			// report its own trouble. Do not stop a working proxy over a
			// transient read.
			continue
		}
		var probe struct {
			APIKeys []string `yaml:"api-keys"`
		}
		if err := yaml.Unmarshal(body, &probe); err != nil {
			continue
		}
		if len(probe.APIKeys) > 0 {
			continue
		}

		log.Errorf(i18n.T(
			"slimproxy: %s 中的 api-keys 已变为空。上游会据此注销唯一的入站认证提供者，之后每个请求都会被放行——而这个进程持有可用的上游凭据。正在停止服务。\n若这是另一个 slimproxy 实例覆盖了该文件，请给它单独的 -state 目录后重试。",
			"slimproxy: api-keys in %s has become empty. Upstream deregisters the only inbound auth provider on that, after which every request is admitted -- while this process holds usable upstream credentials. Stopping the service.\nIf another slimproxy instance overwrote the file, give it its own -state directory and retry."),
			r.ConfigPath)
		stop()
		return
	}
}

// Credentials returns the live credential pool, or nil if the service has not
// finished assembling.
func (r *Runtime) Credentials() *coreauth.Manager { return r.auth.Load() }

// Run starts the proxy and blocks until ctx is cancelled or the server stops.
//
// stateDir receives the materialized effective configuration. It must be
// writable: CLIProxyAPI's Builder requires a config path that exists, and its
// file watcher re-reads that path on every change, so the file has to hold the
// complete effective configuration rather than a fragment.
func Run(ctx context.Context, c Config, stateDir string) error {
	rt, err := Build(c, stateDir)
	if err != nil {
		return err
	}
	return rt.Run(ctx)
}

// Build resolves the configuration, materializes it, and assembles the service
// without starting it.
//
// Separated from Run so callers can inspect what was resolved before the
// service is running. It does briefly bind the listen address to check it is
// free -- see ensureCanBind for why that has to happen here rather than in the
// callers.
func Build(c Config, stateDir string, opts ...BuildOption) (*Runtime, error) {
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("slimproxy: invalid config: %w", err)
	}

	var set buildSettings
	for _, o := range opts {
		o(&set)
	}

	// Before anything else logs: neither Builder nor Service.Run configures
	// logrus, so without this the Debug and LogToFile knobs are inert and every
	// line goes to stderr with the stock formatter.
	// The log file handle is intentionally not closed here: it must outlive
	// Build and stay open for the life of the process. Tests that start and
	// stop the proxy call setupLogging directly and close it themselves.
	logDir, _, err := setupLogging(&c, set.logOpts...)
	if err != nil {
		return nil, fmt.Errorf("slimproxy: configure logging: %w", err)
	}

	cfg := c.build()

	// The watcher fsnotify-Adds both the config path and the auth dir and
	// fails to start if either is missing; Service.Run treats that failure as
	// fatal. Create the auth dir rather than letting startup die on it.
	if err := os.MkdirAll(cfg.AuthDir, 0o700); err != nil {
		return nil, fmt.Errorf("slimproxy: cannot create auth dir %q: %w", cfg.AuthDir, err)
	}
	// Holds OAuth access and refresh tokens in plaintext -- the single most
	// sensitive directory this program owns.
	tighten(cfg.AuthDir)

	// The request log holds request and response bodies verbatim, so it gets
	// the same treatment as the credential directory. Creating it ourselves is
	// what lets the permission be ours: the upstream logger would create it
	// with 0755, and MkdirAll leaves an existing directory's mode alone.
	//
	// The tighten in the else branch is not redundant. Builds before
	// gatedRequestLogger wrote error dumps here with request logging off, so
	// this directory can already exist, full of other people's prompts, having
	// been created by upstream and never hardened. MkdirAll would not fix it
	// and nothing else visits it. On Windows the exposure is an inherited ACE
	// rather than a mode -- see the fsperm package doc, which measured exactly
	// that on this project's own directories.
	//
	// It is deliberately not created when request logging is off: the gate
	// guarantees nothing will write there, and an empty directory would only
	// suggest to an operator that logs exist.
	if dir, derr := c.resolvedRequestLogDir(); derr == nil {
		switch _, statErr := os.Stat(dir); {
		case c.RequestLog:
			if err := os.MkdirAll(dir, 0o700); err != nil {
				// Fail loud. The upstream middleware discards write errors
				// entirely (two `// Log error but continue` sites), so a
				// request log that cannot be written produces no output and no
				// complaint -- while `check` prints the path as though it were
				// working.
				return nil, fmt.Errorf(i18n.T("slimproxy: request-log 目录 %q 无法创建: %w", "slimproxy: request-log directory %q cannot be created: %w"), dir, err)
			}
			tighten(dir)
			if err := probeWritable(dir); err != nil {
				return nil, fmt.Errorf(i18n.T(
					"slimproxy: request-log 目录 %q 不可写: %w\n上游的请求日志中间件会丢弃写入错误，所以这必须在启动时拦下——否则代理会正常运行但一个字节都不记录",
					"slimproxy: request-log directory %q is not writable: %w\nupstream's request-log middleware discards write errors, so this has to be caught at startup -- otherwise the proxy runs normally and records not one byte"), dir, err)
			}
		case statErr == nil:
			tighten(dir)
		}
	}

	// Before materialize, never after: writing the effective config is what
	// makes this instance's settings live for whoever is already serving.
	if err := ensureCanBind(c.Addr()); err != nil {
		return nil, err
	}

	// Constructed before the relay so its flip callback can close over rt.
	// ConfigPath is filled in after materialize. Flips only happen once
	// connections flow, which is after Build has finished and the journal
	// (when on) has been attached below.
	rt := &Runtime{LogDir: logDir, Stats: metrics.NewCollector(),
		requireKeys: len(c.APIKeys) > 0, streamIdle: c.streamIdle()}

	// The adaptive outbound relay, when asked for. Before materialize, so the
	// engine -- which resolves its proxy exactly once -- is handed the relay's
	// address instead of the raw proxy-url, and the per-connection choice
	// between chaining and dialing direct happens behind an address that never
	// changes. See the outbound package for why.
	if c.ProxyURL != "" && c.ProxyFallbackDirect {
		relay, rerr := outbound.Start(c.ProxyURL, outbound.Options{OnFlip: func(via bool, reason string) {
			// The reason is the relay's actual failure text, not an
			// assumption: a port can be listening and still not be an HTTP
			// proxy, and a journal that says "not listening" for that case
			// sends the reader to check the wrong thing.
			detail := fmt.Sprintf(i18n.T("直连（%s：%s）", "direct (%s: %s)"), c.ProxyURL, reason)
			if via {
				detail = fmt.Sprintf(i18n.T("经代理 %s（端口在监听）", "via proxy %s (its port is listening)"), c.ProxyURL)
			}
			// Both the log and the journal, deliberately: a path switch that
			// nobody is told about would recreate the exact blind spot this
			// feature exists to close.
			log.Infof(i18n.T("slimproxy: 出站路径: %s", "slimproxy: outbound path: %s"), detail)
			rt.Note(journal.Event{Kind: journal.KindProxy, State: "outbound", Detail: detail})
		}})
		if rerr != nil {
			return nil, fmt.Errorf(i18n.T("slimproxy: 启动自适应出站中继失败: %w", "slimproxy: cannot start the adaptive outbound relay: %w"), rerr)
		}
		rt.relay = relay
		cfg.ProxyURL = relay.URL()
	}

	path, err := materialize(cfg, stateDir)
	if err != nil {
		rt.closeRelay()
		return nil, err
	}
	rt.ConfigPath = path

	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(path).
		WithServerOptions(
			// Owns where request logs land -- see newRequestLoggerFactory.
			cliproxyapi.WithRequestLoggerFactory(newRequestLoggerFactory(c)),
			// Narrows who may state a request's origin. gin trusts every proxy
			// by default, which made the client IP in the access log a value
			// the caller picked -- see trustedproxy.go. Runs before middleware
			// setup, so ClientIP() is already constrained by the time anything
			// reads it.
			cliproxyapi.WithEngineConfigurator(engineConfigurator()),
			// First, and it has to stay first: it wraps the response writer, so
			// every middleware and handler registered after it writes through
			// the wrapper. Registered ahead of the journal middlewares
			// deliberately -- those read the status, which the wrapper reports
			// faithfully, while the body they never see is the part it rewrites.
			//
			// The redaction is one-directional by construction: upstream stores
			// the unredacted body in the gin context (appendAPIResponse) before
			// handing it to the writer, so the request log and the journal keep
			// the full text and only the caller's copy is replaced. That split
			// is the whole design -- see errorenvelope.go.
			cliproxyapi.WithMiddleware(ErrorEnvelopeMiddleware()),
			// Keeps upstream policy refusals from reaching callers as an
			// unexplained empty completion -- see RefusalHookMiddleware.
			cliproxyapi.WithMiddleware(RefusalHookMiddleware()),
		)

	// The credential pool is only reachable through BaseAPIHandler.AuthManager,
	// which a router configurator is the one place that hands it out. Service
	// exposes no getter for it, so anything needing to enumerate or refresh
	// credentials has to be captured here.
	// The journal resolves its own directory rather than riding on the
	// application log's.
	//
	// It used to require file logging, which coupled two unrelated decisions:
	// `log-to-file: false` -- a perfectly ordinary setting -- silently produced
	// no event stream at all. Panel mode turns file logging on, so the journal
	// worked there and nowhere else, and the resulting gap in the record reads
	// exactly like a quiet period rather than like a feature that never ran.
	if jdir, jderr := c.resolvedJournalDir(); jderr != nil {
		log.Warnf(i18n.T("slimproxy: 无法解析事件日志目录，本次运行不会记录可回溯事件: %v", "slimproxy: cannot resolve the event journal directory; this run will record no reviewable events: %v"), jderr)
	} else if jdir != "" {
		jw, jerr := journal.Open(jdir, c.JournalDays)
		switch {
		case jerr != nil:
			// Not fatal. A proxy that refuses to serve because it cannot keep a
			// diary is worse than one that serves and says the diary is
			// missing -- but it must say so, or the absence of events later
			// reads as an absence of problems.
			log.Warnf(i18n.T("slimproxy: 无法打开事件日志，本次运行不会记录可回溯事件: %v", "slimproxy: cannot open the event journal; this run will record no reviewable events: %v"), jerr)
		default:
			rt.Journal = jw
			// Completed requests arrive through the collector, so the field
			// mapping has exactly one implementation.
			// Sample observation is registered once, below and
			// unconditionally -- health tracking must not depend on
			// journalling being on.
			// Refused requests never reach an executor and never produce a
			// usage record; they are only visible to a middleware.
			rt.rejects = journal.NewRejectFolder(jw.Append)
			builder = builder.WithServerOptions(
				cliproxyapi.WithMiddleware(AccessJournalMiddleware(rt.rejects)),
				// Records what the client asked for, which the usage record
				// cannot: it describes what the upstream did.
				cliproxyapi.WithMiddleware(FidelityProbeMiddleware(jw)),
			)
		}
	}
	// Health tracking, and the one observer both it and the journal ride on.
	//
	// Registered outside the journal branch on purpose. Everything that watches
	// requests used to live in there, so `log-to-file: false` bought silence
	// from the diary and from the alarm at once -- the same coupling the
	// journal's own directory resolution was changed to avoid, one level up.
	//
	// What this catches: a run of failures nobody is looking at. The collector
	// saw all 107 failures of a two-day upstream outage and filed every one of
	// them, and the first anyone knew of it was reading the files afterwards.
	//
	// What it cannot catch: anything that fails before reaching an executor,
	// because that produces no usage record. Refused and misrouted requests are
	// the reject folder's half of the picture, not this one's.
	rt.Health = metrics.NewHealth(func(ev metrics.HealthEvent) {
		log.Warnf("slimproxy: %s", ev.Text())
		rt.Note(journal.Event{Kind: journal.KindHealth, State: ev.State(), Detail: ev.Text()})
	})
	// Note is nil-safe, so this closure is correct with or without a journal.
	rt.Stats.Observe(func(sm metrics.Sample) {
		rt.Note(journal.FromSample(sm))
		rt.Health.Observe(sm)
	})

	builder = builder.WithServerOptions(
		cliproxyapi.WithRouterConfigurator(func(_ *gin.Engine, h *handlers.BaseAPIHandler, _ *cliproxyconfig.Config) {
			rt.auth.Store(h.AuthManager)
			// Here rather than only from the ticker: this runs while the
			// router is being assembled, so the guard is in place before the
			// first request can start a stream. A stream pins its executor
			// when it starts, so one that begins unguarded stays unguarded
			// for its whole life -- exactly the multi-minute hang the guard
			// exists to prevent.
			ensureStallGuard(h.AuthManager, rt.streamIdle, rt.stallNotes())
		}),
		// Registered here rather than in the journal branch above, for the same
		// reason the health tracker is: what is running right now is a property
		// of the display, not of the diary, and putting it up there would mean
		// `log-to-file: false` silently emptied the dashboard's only live
		// section.
		cliproxyapi.WithMiddleware(InFlightMiddleware(rt.Stats.InFlight())),
		// Last on purpose, so its writer sits closest to the handler: the
		// journal middlewares behind it read Status() through the wrapper,
		// which reports the handler's intended status even after the early
		// 200 is on the wire -- see earlyflush.go.
		cliproxyapi.WithMiddleware(EarlyFlushMiddleware(c.streamEarlyFlush(), earlyFlushNotes{
			flushed:         rt.noteEarlyFlush,
			desperationHeld: rt.noteDesperationHeld,
			translated:      rt.noteEarlyFlushTranslated,
			timedOut:        rt.noteEarlyFlushTimeout,
		})),
	)

	svc, err := builder.Build()
	if err != nil {
		rt.closeRelay()
		return nil, fmt.Errorf("slimproxy: build service: %w", err)
	}
	rt.Service = svc
	svc.RegisterUsagePlugin(rt.Stats)

	return rt, nil
}

// materialize writes the effective configuration to stateDir and returns its
// path.
//
// The file is generated, not user-authored: editing it works, because the
// watcher reloads from it, but the next start overwrites it. It is written
// 0600 because it contains inbound API keys and any provider keys set in
// Config.
func materialize(cfg *cliproxyconfig.Config, stateDir string) (string, error) {
	if stateDir == "" {
		stateDir = "."
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("slimproxy: resolve state dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("slimproxy: create state dir %q: %w", abs, err)
	}

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("slimproxy: marshal effective config: %w", err)
	}

	header := []byte("# Generated by slimproxy. Regenerated on every start; edit the\n" +
		"# slimproxy config instead. Contains secrets -- keep mode 0600.\n" +
		"#\n# Subsystems pinned off:" +
		indentComment(hardeningNotes) + "\n")

	path := filepath.Join(abs, EffectiveConfigName(cfg.Port))
	if err := os.WriteFile(path, append(header, body...), 0o600); err != nil {
		return "", fmt.Errorf("slimproxy: write %q: %w", path, err)
	}
	// The 0600 above is not enough on Windows, where it sets the read-only
	// attribute and leaves the parent's inherited ACEs in place -- measured on
	// this project's own tree, a local group held read access over the inbound
	// API keys in this very file.
	tighten(path)
	return path, nil
}

// tighten restricts a path to the current user, reporting rather than failing.
//
// Best-effort on purpose: refusing to start because a log directory could not
// be locked down helps nobody, and the proxy is still no worse off than it was.
// But it is reported, because tightening nothing and saying nothing is how the
// exposure above went unnoticed.
// ensureCanBind reports whether the listen address is available.
//
// It lives in Build, ahead of materialize, because the damage a doomed second
// instance does happens before it ever tries to listen. materialize rewrites
// the effective configuration, and that file is what the upstream watcher
// reloads -- so an instance that is about to fail on a busy port first hands
// its own settings to the instance already serving on it. Per-port file naming
// narrowed this to same-port collisions; a same-port collision is exactly the
// case that cannot bind, so checking here closes what is left.
//
// There is a race -- the port could be taken between this check and the real
// bind -- and it is deliberately accepted. Losing it lands in the old
// behaviour, so the check can only help. Winning it, the normal case, also
// turns a late, confusing failure into a sentence: CLIProxyAPI prints "API
// server started successfully", loads every credential, starts the watcher,
// and only then returns the bind error, which in panel mode reads as a
// dashboard that appears and vanishes.
//
// The message names the likely cause because it usually is one: the operator's
// own earlier instance, still running.
func ensureCanBind(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// No "slimproxy:" prefix -- main already adds one, and this message is
		// the most common startup failure there is, so a doubled prefix would
		// be the thing operators see most often.
		return fmt.Errorf(i18n.T(
			"无法绑定 %s：%w\n很可能已有一个 slimproxy 在运行。用 \"slimproxy status\" 查看，停掉它，或用 -port 换一个端口",
			"cannot bind %s: %w\nmost likely another slimproxy is already running. Check with \"slimproxy status\", stop it, or pick another port with -port"), addr, err)
	}
	return ln.Close()
}

func tighten(path string) {
	if err := fsperm.Restrict(path); err != nil {
		log.Warnf(i18n.T("slimproxy: 未能收紧 %s 的访问权限（本机其他用户可能可读）: %v", "slimproxy: could not tighten access on %s (other local users may be able to read it): %v"), path, err)
	}
}

// indentComment turns hardeningNotes into YAML comment lines.
func indentComment(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		if line == "" {
			continue
		}
		out += "\n#   " + line
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// probeWritable confirms a directory can actually be written to.
//
// os.MkdirAll succeeding is not the same answer: the directory may already
// exist and be read-only, or live on a mount that rejects writes. The upstream
// request-logging middleware discards its write errors ("// Log error but
// continue" at both of its failure sites), so this is the last point where the
// problem is still observable.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".slimproxy-write-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
