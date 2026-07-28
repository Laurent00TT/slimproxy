package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Laurent00TT/slimproxy/credentials"
	"github.com/Laurent00TT/slimproxy/diag"
	"github.com/Laurent00TT/slimproxy/journal"
	"github.com/Laurent00TT/slimproxy/proxy"
	"github.com/Laurent00TT/slimproxy/translate"
	"github.com/Laurent00TT/slimproxy/tui"
	"github.com/Laurent00TT/slimproxy/tunnel"
)

// proxyStopGrace is how long the dashboard waits for the proxy to finish after
// asking it to stop.
//
// Bounded rather than unlimited: CLIProxyAPI establishes its shutdown deadline
// at startup instead of at signal time, so a long-lived process does not get a
// graceful drain and waiting on one indefinitely would hang the exit.
const proxyStopGrace = 5 * time.Second

// runDashboard serves and renders at the same time.
//
// The two are bound to one context in both directions. A proxy that dies takes
// the dashboard down -- otherwise the panel keeps rendering a proxy that is no
// longer there, with the reason sitting in a log file the terminal is no
// longer showing. And a dashboard the operator quits takes the proxy down,
// which is what pressing q on a foreground process is understood to mean.
func runDashboard(cx *cliContext, cfg *proxy.Config) error {
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	// WithoutStdoutLogs is not optional here: the alternate screen is a single
	// writer surface, and a logrus line written to stdout lands in the middle
	// of a rendered frame. It turns file logging on if it was off, so the
	// lines are redirected rather than dropped.
	rt, err := proxy.Build(*cfg, cx.stateDir, proxy.WithoutStdoutLogs())
	if err != nil {
		return err
	}

	// Checked before the screen is taken, not after.
	//
	// A port already in use is the most common way starting this fails, and
	// CLIProxyAPI reports it late: it prints "API server started successfully",
	// loads every credential, starts the watcher, and only then returns the
	// bind error. In panel mode that sequence renders as a dashboard that
	// appears, sits there for a second, and vanishes -- the error does reach
	// stderr, but after the alternate screen has come and gone, which reads as
	// a crash rather than a diagnosis.
	if err := ensureCanBind(cfg.Addr()); err != nil {
		return err
	}

	// Take the process's stdout before anything can write to it. Silencing
	// logrus is not sufficient: CLIProxyAPI announces the listener and every
	// watcher reload with a bare fmt.Printf, which would land in the middle of
	// a rendered frame and stay there. See proxy.TakeStdout.
	terminal, restoreStdout, err := proxy.TakeStdout(rt.LogDir)
	if err != nil {
		return err
	}
	defer func() { _ = restoreStdout() }()

	deps, err := dashboardDeps(cx, cfg, rt)
	if err != nil {
		return err
	}

	// Written by the panel's goroutine just before it stops, read here after
	// Run returns. Safe without synchronisation because Run returning
	// establishes the ordering -- and it has to be reported here rather than
	// there, since anything written to stderr while the alternate screen is up
	// disappears with it.
	var aborted string
	deps.OnAbort = func(action string) { aborted = action }

	err = supervise(ctx, cancel,
		rt.Run,
		func(ctx context.Context) error { return tui.Run(ctx, deps, terminal) },
		proxyStopGrace, cx.stderr)

	if aborted != "" {
		fmt.Fprintf(cx.stderr,
			"slimproxy: 退出时「%s」仍在执行，已被中止，结果未知\n", aborted)
	}
	return err
}

// ensureCanBind reports whether the listen address is available.
//
// There is a race here -- the port could be taken between this check and the
// real bind -- and it is deliberately accepted. Losing that race lands in
// exactly the behaviour this replaces, so the check can only help; winning it,
// which is the normal case, turns a dashboard that flashes and disappears into
// a sentence naming the problem.
//
// The message says what to do about it, because the overwhelmingly likely cause
// is the operator's own earlier instance still running.
func ensureCanBind(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("无法绑定 %s：%w\n"+
			"很可能已有一个 slimproxy 在运行。用 \"slimproxy status\" 查看，"+
			"停掉它，或用 -port 换一个端口", addr, err)
	}
	return ln.Close()
}

// supervise runs the proxy and the dashboard bound to one context, and decides
// which outcome the process exits on.
//
// Extracted from runDashboard because the decisions here are the ones most
// easily got wrong and least visible when they are: a race between two exit
// paths, and an error precedence that only shows up on the day the proxy fails
// to bind. Neither is observable through a running proxy and a real terminal.
func supervise(
	ctx context.Context,
	cancel context.CancelFunc,
	runProxy func(context.Context) error,
	runUI func(context.Context) error,
	grace time.Duration,
	warn io.Writer,
) error {
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- runProxy(ctx)
		// Whatever happened -- a bind failure, a crash, a clean stop -- the
		// dashboard has nothing left to display. Cancelling here is what makes
		// a failed start visible: the UI exits, the terminal is restored, and
		// the error below prints where it can be read. Without it the panel
		// keeps rendering a proxy that is gone.
		cancel()
	}()

	uiErr := runUI(ctx)
	cancel()

	// The proxy's outcome outranks the dashboard's. A bind failure is the
	// thing the operator needs to see, and the UI exiting cleanly in response
	// to it would otherwise be the only thing reported.
	select {
	case err := <-proxyDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-time.After(grace):
		// Reported rather than ignored: the process is about to exit with the
		// listener possibly still bound, and the next start would fail with a
		// port conflict that has no other explanation.
		fmt.Fprintf(warn, "slimproxy: 代理在 %s 内未停止，进程仍将退出\n", grace)
	}
	return uiErr
}

// dashboardDeps wires the dashboard to the running proxy and the local
// tooling.
//
// Everything slow or failure-prone is a function the dashboard calls when it
// wants an answer, never a value captured now: a tunnel status read at startup
// would be stale by the first frame, and one that failed would be stale
// forever.
func dashboardDeps(cx *cliContext, cfg *proxy.Config, rt *proxy.Runtime) (tui.Deps, error) {
	authDir, authErr := cfg.ResolveAuthDir()
	if authErr != nil {
		// Not fatal: the proxy resolved the same path and started. But the
		// dashboard would be reading a different directory than the one being
		// served, and every credential row would be about the wrong place.
		return tui.Deps{}, fmt.Errorf("无法解析凭据目录: %w", authErr)
	}

	mgr := &tunnel.Manager{StateDir: cx.stateDir}

	// The diagnostic target is rebuilt per run rather than captured, so
	// `/doctor` reports on the config as it is on disk now. The config can
	// change under a running proxy -- CLIProxyAPI watches and reloads it --
	// and a doctor pinned to the startup snapshot would diagnose a deployment
	// that no longer exists.
	doctor := func(ctx context.Context) diag.Report {
		liveCfg, typeErrs, err := cx.loadConfigForDiagnosis()
		if err != nil {
			// The file became unreadable since startup. Diagnose what is
			// running instead of refusing: the in-memory config is the one
			// actually serving requests.
			liveCfg, typeErrs = cfg, []string{"配置文件当前无法读取: " + err.Error()}
		}
		target := targetFor(liveCfg, cx.configPath, typeErrs)
		// Only reachable from inside the process that served the requests --
		// the finding comes from the translation hooks at request time, not
		// from anything on disk. The CLI's own `doctor` reports Unknown for
		// this check, which is the honest answer from another process.
		target.FromRunningInstance = true
		target.Untranslated = proxy.UntranslatedPairs()
		js := rt.Journal.Stats()
		target.JournalDir, target.JournalDropped, target.JournalErr = js.Dir, js.Dropped, js.LastErr
		rep := diag.Run(ctx, diag.Checks(target))
		// The verdict, not the whole report: a week of full reports would bury
		// the requests, and the verdict is what a later question needs -- "was
		// it already unhealthy at 14:30".
		rt.Note(journal.Event{
			Kind: journal.KindDoctor, Level: rep.Worst().String(),
			Detail: doctorNote(rep),
		})
		return rep
	}

	return tui.Deps{
		Listen:  cfg.Addr(),
		Version: proxy.Version,
		AuthDir: authDir,

		Stats: rt.Stats.Snapshot,

		TunnelStatus: mgr.Status,
		TunnelUp: func(ctx context.Context) (tunnel.Status, error) {
			// Detached, always. A foreground child would be bound to this
			// process's stdio, which the alternate screen owns.
			st, err := mgr.Up(ctx, true, nil, nil)
			// Onto the same timeline as the requests: a burst of failures next
			// to a tunnel transition explains itself, and in a separate file
			// explains nothing.
			rt.Note(journal.Event{
				Kind: journal.KindTunnel, State: "up",
				Detail: tunnelNote(st, err),
			})
			return st, err
		},
		TunnelDown: func(ctx context.Context) (tunnel.Status, error) {
			st, err := mgr.Down(ctx)
			rt.Note(journal.Event{
				Kind: journal.KindTunnel, State: "down",
				Detail: tunnelNote(st, err),
			})
			return st, err
		},

		// The context is accepted and not consulted: credentials.List is
		// os.ReadDir, which no context can interrupt. The deadline is still
		// enforced -- tui.execute races Do against it -- so a hung network
		// share costs a leaked goroutine rather than a wedged panel. Taking
		// the parameter keeps that contract visible here instead of making
		// the caller guess.
		Creds: func(context.Context) ([]credentials.Credential, error) {
			return credentials.List(authDir)
		},
		RemoveCred: func(_ context.Context, id string, force bool) (credentials.Credential, error) {
			c, err := credentials.Remove(authDir, id, force, time.Now())
			if err == nil {
				rt.Note(journal.Event{
					Kind: journal.KindCred, State: "removed", Detail: c.Name,
				})
			}
			return c, err
		},

		Doctor: doctor,
		Routes: registeredRoutes,
		LogDir: rt.LogDir,
	}, nil
}

// registeredRoutes projects the translator registry onto what the panel shows.
func registeredRoutes() []tui.Route {
	regs := translate.Registered()
	out := make([]tui.Route, 0, len(regs))
	for _, c := range regs {
		out = append(out, tui.Route{Client: string(c.Pair.Client), Provider: string(c.Pair.Provider)})
	}
	return out
}

// tunnelNote condenses a tunnel transition into one line for the journal.
func tunnelNote(st tunnel.Status, err error) string {
	if err != nil {
		return "失败: " + err.Error()
	}
	parts := []string{st.State.String()}
	if len(st.Hostnames) > 0 {
		parts = append(parts, tunnel.JoinHostnames(st.Hostnames))
	}
	if st.ConnectionsKnown {
		parts = append(parts, fmt.Sprintf("%d 连接", st.Connections))
	}
	return strings.Join(parts, " · ")
}

// doctorNote condenses a report into its tally.
func doctorNote(rep diag.Report) string {
	counts := rep.Counts()
	var parts []string
	for _, l := range diag.LevelsBySeverity() {
		if n := counts[l]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", l, n))
		}
	}
	return strings.Join(parts, " · ")
}
