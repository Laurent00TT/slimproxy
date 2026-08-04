// Command slimproxy runs a minimal reverse proxy in front of the provider
// executors that CLIProxyAPI ships.
//
// It exists to shrink the operable surface: one small config file, no
// management API, no plugin host, no control panel. The upstream client
// emulation is CLIProxyAPI's, used unmodified.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"gopkg.in/yaml.v3"

	"github.com/Laurent00TT/slimproxy/i18n"
	"github.com/Laurent00TT/slimproxy/proxy"
	"github.com/Laurent00TT/slimproxy/translate"
	"github.com/Laurent00TT/slimproxy/tui"
)

func main() {
	// Language is set twice, and the order matters. The locale guess comes
	// first so that everything printed before a config is loaded -- usage
	// errors, "找不到配置文件", init's output -- is already in the operator's
	// language; the config's lang field then overrides it inside loadConfig,
	// the one gate every config-reading command passes through. Between the two
	// points nothing concurrent is running, so the second Set is safe.
	i18n.Set(i18n.FromLocale(i18n.SystemLocale()))

	cx := newContext()
	err := dispatch(cx, os.Args[1:])
	if err == nil {
		return
	}
	// A command that already printed its findings sets the status without
	// adding a second, redundant error line.
	var silent *silentError
	if errors.As(err, &silent) {
		os.Exit(silent.code)
	}
	// A usage error already carries the command list; printing "slimproxy:"
	// in front of a help screen just adds noise.
	if errors.Is(err, errUsage) {
		fmt.Fprintln(os.Stderr, trimUsagePrefix(err.Error()))
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "slimproxy:", err)
	os.Exit(1)
}

func trimUsagePrefix(s string) string {
	const p = "usage: "
	if len(s) > len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

// cmdServe starts the proxy and blocks.
//
// On a terminal this means the full-screen dashboard, with the proxy running
// inside the same process. Redirected, it means the original line-oriented
// behaviour -- checked rather than assumed, because `slimproxy > log 2>&1`
// under a service manager is an ordinary way to run this and a dashboard piped
// into a file is thousands of escape sequences a minute.
func cmdServe(cx *cliContext, args []string) error {
	cmd := lookup("serve")
	fs := newFlagSet(cx, cmd)
	bindCommon(fs, cx)
	noTUI := fs.Bool("no-tui", false, i18n.T("不启动全屏面板，只输出日志（重定向时自动生效）", "no full-screen panel, logs only (automatic when redirected)"))
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	cfg, err := cx.loadConfigFor()
	if err != nil {
		return err
	}

	if !*noTUI && tui.Supported() {
		return runDashboard(cx, cfg)
	}

	rt, err := proxy.Build(*cfg, cx.stateDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(cx.stdout, i18n.T("slimproxy 正在监听 %s（运行时配置: %s）\n", "slimproxy listening on %s (runtime config: %s)\n"), cfg.Addr(), rt.ConfigPath)

	// Cancel on SIGINT/SIGTERM. Note that CLIProxyAPI's shutdown deadline is
	// established at startup rather than at signal time, so a process that has
	// been up for more than 30s does not get a graceful connection drain --
	// in-flight streams are cut rather than allowed to finish.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return shutdownError(rt.Run(ctx))
}

// cmdCheck validates the config and reports what would run.
func cmdCheck(cx *cliContext, args []string) error {
	cmd := lookup("check")
	fs := newFlagSet(cx, cmd)
	bindCommon(fs, cx)
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}
	cfg, err := cx.loadConfigFor()
	if err != nil {
		return err
	}
	return printCheck(cx.stdout, cfg, cx.stateDir)
}

// cmdInit writes a ready-to-run config.
func cmdInit(cx *cliContext, args []string) error {
	cmd := lookup("init")
	fs := newFlagSet(cx, cmd)
	fs.StringVar(&cx.configPath, "config", defaultConfigPath, "path to write the config to")
	authDir := fs.String("init-auth-dir", "auths", "auth-dir to put in the generated config")
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	if err := initConfig(cx.configPath, *authDir, *force); err != nil {
		return err
	}
	cfg, err := loadConfig(cx.configPath)
	if err != nil {
		return err
	}
	key := ""
	if len(cfg.APIKeys) > 0 {
		key = cfg.APIKeys[0]
	}
	printNextSteps(cx.stdout, cx.configPath, key)
	return nil
}

// cmdRoutes lists the translator pairs this build can serve.
func cmdRoutes(cx *cliContext, args []string) error {
	cmd := lookup("routes")
	fs := newFlagSet(cx, cmd)
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}
	printRoutes(cx.stdout)
	return nil
}

// skipHandled turns the "already printed, exit 0" sentinel back into success.
func skipHandled(err error) error {
	if errors.Is(err, errHandled) {
		return nil
	}
	return err
}

// shutdownError maps a Service.Run result onto a process exit status.
//
// Service.Run returns ctx.Err() unconditionally, so a planned shutdown arrives
// as context.Canceled. Treating that as failure means every Ctrl-C,
// `systemctl stop`, `docker stop` and pod termination exits non-zero: systemd
// Restart=on-failure restarts after an intentional stop, and Kubernetes records
// Error instead of Completed. An intended stop is not a failure.
//
// context.DeadlineExceeded is deliberately NOT swallowed -- that means a
// shutdown deadline elapsed, which is a real problem worth a non-zero exit.
func shutdownError(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func loadConfig(path string) (*proxy.Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The first thing a new user hits, so it names the fix rather than
			// just the problem.
			return nil, fmt.Errorf(i18n.T("找不到配置文件 %q；运行 \"slimproxy init\" 生成一份", "config file %q not found; run \"slimproxy init\" to generate one"), path)
		}
		return nil, fmt.Errorf(i18n.T("读取配置 %q 失败: %w", "reading config %q failed: %w"), path, err)
	}
	var cfg proxy.Config
	// KnownFields makes a typo in the config an error instead of a silently
	// ignored key, which is how a "why is my setting not applying" hour starts.
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	// The one place a stated language preference takes effect, chosen because
	// every config-reading command funnels through here. A command that errors
	// before this line speaks the locale's language; that is the best available
	// guess about an operator whose config could not be read.
	//
	// Applied even when the config later fails Validate: an operator whose
	// config is broken in some other field still stated which language to
	// explain the breakage in.
	if l, ok := i18n.Parse(cfg.Lang); ok {
		i18n.Set(l)
	}
	return &cfg, nil
}

func printCheck(w io.Writer, cfg *proxy.Config, stateDir string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	fmt.Fprint(w, i18n.T("配置 OK\n\n", "config OK\n\n"))
	fmt.Fprintf(w, i18n.T("  监听      %s\n", "  listen         %s\n"), cfg.Addr())
	fmt.Fprintf(w, i18n.T("  凭据目录  %s\n", "  auth dir       %s\n"), orDefault(cfg.AuthDir, "auths"))
	fmt.Fprintf(w, i18n.T("  状态目录  %s\n", "  state dir      %s\n"), stateDir)
	fmt.Fprintf(w, i18n.T("  入站认证  %s\n", "  inbound auth   %s\n"), authSummary(cfg))
	fmt.Fprintf(w, i18n.T("  应用日志  %s\n", "  app log        %s\n"), cfg.AppLogTarget())
	fmt.Fprintf(w, i18n.T("  请求日志  %s\n", "  request log    %s\n"), cfg.RequestLogTarget())
	fmt.Fprintf(w, i18n.T("  上游重试  request-retry=%d max-retry-interval=%ds max-retry-credentials=%d\n", "  upstream retry request-retry=%d max-retry-interval=%ds max-retry-credentials=%d\n"),
		cfg.RequestRetry, cfg.MaxRetryInterval, cfg.MaxRetryCredentials)
	if cfg.RequestRetry == 0 {
		fmt.Fprint(w, i18n.T("            （request-retry=0 会完全跳过感知冷却的重试循环）\n", "                 (request-retry=0 skips the cooldown-aware retry loop entirely)\n"))
	}
	fmt.Fprintf(w, i18n.T("  流看门狗  %s\n", "  stall guard    %s\n"), cfg.StreamIdleSummary())
	if cfg.StreamIdleTimeout < 0 {
		fmt.Fprint(w, i18n.T("            （关闭后，静默死亡的流会一直挂到客户端自己放弃）\n", "                 (off: a silently dead stream hangs until the client gives up on its own)\n"))
	}
	fmt.Fprintf(w, i18n.T("  早刷流头  %s\n", "  early flush    %s\n"), cfg.StreamEarlyFlushSummary())
	if cfg.StreamEarlyFlush < 0 {
		fmt.Fprint(w, i18n.T("            （关闭后，排队超过 ~100s 的流式请求会被 Cloudflare 斩成 524）\n", "                 (off: streaming requests queued past ~100s get severed as 524s by Cloudflare)\n"))
	}
	if len(cfg.Models) == 0 {
		fmt.Fprint(w, i18n.T("  模型      已加载凭据暴露的全部模型\n", "  models         everything the loaded credentials expose\n"))
	} else {
		fmt.Fprintf(w, i18n.T("  模型      %v（精确匹配）\n", "  models         %v (exact match)\n"), cfg.Models)
	}
	return nil
}

func authSummary(cfg *proxy.Config) string {
	if len(cfg.APIKeys) > 0 {
		return fmt.Sprintf(i18n.T("需要 %d 个 key", "%d keys required"), len(cfg.APIKeys))
	}
	return i18n.T("无 —— 所有请求都会被接受（allow-unauthenticated）", "none -- every request is accepted (allow-unauthenticated)")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func printRoutes(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, i18n.T("客户端协议\t上游协议\t请求\t流式响应\t非流式响应\tToken 计数", "client protocol\tupstream\trequest\tstreaming\tnon-streaming\ttoken counting"))
	for _, c := range translate.Registered() {
		fmt.Fprintf(tw, "%s\t%s\t%v\t%v\t%v\t%v\n",
			c.Pair.Client, c.Pair.Provider,
			c.Request, c.StreamResponse, c.NonStreamRespose, c.AnyResponse)
	}
	_ = tw.Flush()
}
