package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/momo/slimproxy/fsperm"
	"github.com/momo/slimproxy/journal"
	"github.com/momo/slimproxy/metrics"

	// Registers the built-in translator pairs.
	_ "github.com/momo/slimproxy/translate"
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
	// until the service has been built.
	auth *coreauth.Manager

	// requireKeys records whether this instance started out demanding inbound
	// authentication. The guard only defends an invariant that existed.
	requireKeys bool

	// Journal records what happened, durably. Nil when journalling is off.
	Journal *journal.Writer
	rejects *journal.RejectFolder
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

	// Start and stop go into the same timeline as the requests. Half of
	// retrospective debugging is noticing that the thing being investigated
	// began at a restart.
	r.Note(journal.Event{Kind: journal.KindProxy, State: "start", Detail: r.ConfigPath})
	defer func() {
		r.Note(journal.Event{Kind: journal.KindProxy, State: "stop"})
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
			log.Warnf("slimproxy: 重新安装 refusal hook 失败，上游策略拒绝可能表现为空响应: %v", err)
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

		log.Errorf("slimproxy: %s 中的 api-keys 已变为空。"+
			"上游会据此注销唯一的入站认证提供者，之后每个请求都会被放行——"+
			"而这个进程持有可用的上游凭据。正在停止服务。\n"+
			"若这是另一个 slimproxy 实例覆盖了该文件，请给它单独的 -state 目录后重试。",
			r.ConfigPath)
		stop()
		return
	}
}

// Credentials returns the live credential pool, or nil if the service has not
// finished assembling.
func (r *Runtime) Credentials() *coreauth.Manager { return r.auth }

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
// Separated from Run so callers can inspect what was resolved before binding a
// port, and so tests can exercise the mapping without a listener.
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

	// The request log is created by the upstream logger, which uses 0755 for
	// the directory and 0644 for the files -- the one place in this deployment
	// that breaks the 0700/0600 rule, and it holds request and response bodies
	// verbatim. Creating it first is what lets the permission be ours:
	// MkdirAll leaves an existing directory's mode alone.
	if c.RequestLog {
		if dir, derr := c.resolvedRequestLogDir(); derr == nil {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				// Fail loud. The upstream middleware discards write errors
				// entirely (two `// Log error but continue` sites), so a
				// request log that cannot be written produces no output and no
				// complaint -- while `check` prints the path as though it were
				// working.
				return nil, fmt.Errorf("slimproxy: request-log 目录 %q 无法创建: %w", dir, err)
			}
			tighten(dir)
			if err := probeWritable(dir); err != nil {
				return nil, fmt.Errorf("slimproxy: request-log 目录 %q 不可写: %w\n"+
					"上游的请求日志中间件会丢弃写入错误，所以这必须在启动时拦下——"+
					"否则代理会正常运行但一个字节都不记录", dir, err)
			}
		}
	}

	path, err := materialize(cfg, stateDir)
	if err != nil {
		return nil, err
	}

	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(path).
		WithServerOptions(
			// Owns where request logs land -- see newRequestLoggerFactory.
			cliproxyapi.WithRequestLoggerFactory(newRequestLoggerFactory(c)),
			// Keeps upstream policy refusals from reaching callers as an
			// unexplained empty completion -- see RefusalHookMiddleware.
			cliproxyapi.WithMiddleware(RefusalHookMiddleware()),
		)

	// The credential pool is only reachable through BaseAPIHandler.AuthManager,
	// which a router configurator is the one place that hands it out. Service
	// exposes no getter for it, so anything needing to enumerate or refresh
	// credentials has to be captured here.
	rt := &Runtime{ConfigPath: path, LogDir: logDir, Stats: metrics.NewCollector(),
		requireKeys: len(c.APIKeys) > 0}

	// The journal resolves its own directory rather than riding on the
	// application log's.
	//
	// It used to require file logging, which coupled two unrelated decisions:
	// `log-to-file: false` -- a perfectly ordinary setting -- silently produced
	// no event stream at all. Panel mode turns file logging on, so the journal
	// worked there and nowhere else, and the resulting gap in the record reads
	// exactly like a quiet period rather than like a feature that never ran.
	if jdir, jderr := c.resolvedJournalDir(); jderr != nil {
		log.Warnf("slimproxy: 无法解析事件日志目录，本次运行不会记录可回溯事件: %v", jderr)
	} else if jdir != "" {
		jw, jerr := journal.Open(jdir, c.JournalDays)
		switch {
		case jerr != nil:
			// Not fatal. A proxy that refuses to serve because it cannot keep a
			// diary is worse than one that serves and says the diary is
			// missing -- but it must say so, or the absence of events later
			// reads as an absence of problems.
			log.Warnf("slimproxy: 无法打开事件日志，本次运行不会记录可回溯事件: %v", jerr)
		default:
			rt.Journal = jw
			// Completed requests arrive through the collector, so the field
			// mapping has exactly one implementation.
			rt.Stats.Observe(func(sm metrics.Sample) { jw.Append(journal.FromSample(sm)) })
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
	builder = builder.WithServerOptions(
		cliproxyapi.WithRouterConfigurator(func(_ *gin.Engine, h *handlers.BaseAPIHandler, _ *cliproxyconfig.Config) {
			rt.auth = h.AuthManager
		}),
	)

	svc, err := builder.Build()
	if err != nil {
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
func tighten(path string) {
	if err := fsperm.Restrict(path); err != nil {
		log.Warnf("slimproxy: 未能收紧 %s 的访问权限（本机其他用户可能可读）: %v", path, err)
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
