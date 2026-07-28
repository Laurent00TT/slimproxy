package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/momo/slimproxy/diag"
	"github.com/momo/slimproxy/proxy"
	"github.com/momo/slimproxy/tunnel"
)

// loadConfigForDiagnosis parses the config without rejecting unknown fields.
//
// The strict loader every other command uses refuses to decode a config
// carrying a field this binary does not know, which is right for running the
// proxy and exactly wrong here: it makes doctor fail to start on the very
// problem the config-fields check exists to report. Diagnosis has to survive a
// config that running would refuse.
//
// Only a YAML *syntax* error is fatal, because then there is nothing to
// inspect. A type error is not: yaml.v3 decodes every field it can and reports
// the rest, so a config with `port: "8317"` still yields a usable host,
// auth-dir and api-keys. Discarding all of that because one field has the wrong
// type would repeat the mistake this function exists to avoid.
//
// Type errors are returned alongside the config so config-fields can report
// them; they are not swallowed.
func (cx *cliContext) loadConfigForDiagnosis() (*proxy.Config, []string, error) {
	body, err := os.ReadFile(cx.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Still not fatal: on a fresh machine this is exactly when doctor is
			// most useful, and the port, DNS, upstream and tunnel checks do not
			// need a config at all.
			return &proxy.Config{}, []string{fmt.Sprintf("配置文件 %s 不存在", cx.configPath)}, nil
		}
		return nil, nil, fmt.Errorf("read config %q: %w", cx.configPath, err)
	}

	var cfg proxy.Config
	var typeErrs []string
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		var terr *yaml.TypeError
		if !errors.As(err, &terr) {
			return nil, nil, fmt.Errorf("parse config %q: %w", cx.configPath, err)
		}
		typeErrs = terr.Errors
	}
	if cx.port != 0 {
		cfg.Port = cx.port
	}
	return &cfg, typeErrs, nil
}

// targetFor builds the inspection target from the loaded config, filling in the
// tunnel from cloudflared's own configuration when one exists.
func targetFor(cfg *proxy.Config, configPath string, typeErrs []string) diag.Target {
	// Resolved, not as written: the proxy expands ~ and absolutizes before
	// loading credentials, so a check reading the raw string would inspect a
	// different directory than the one that matters.
	authDir, authErr := cfg.ResolveAuthDir()

	t := diag.Target{
		Host:       cfg.Host,
		Port:       cfg.Port,
		AuthDir:    authDir,
		AuthDirErr: authErr,
		ConfigPath: configPath,
		ConfigErrs: typeErrs,
	}
	tc, found, err := diag.DetectTunnel()
	switch {
	case err != nil:
		t.TunnelErr = err
	case found:
		t.TunnelName = tc.Tunnel
		t.TunnelHostname = tunnel.JoinHostnames(tc.Hostnames)
	}
	return t
}

// cmdStatus reports what is observable right now.
func cmdStatus(cx *cliContext, args []string) error {
	cmd := lookup("status")
	fs := newFlagSet(cx, cmd)
	bindCommon(fs, cx)
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}
	cfg, typeErrs, err := cx.loadConfigForDiagnosis()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	target := targetFor(cfg, cx.configPath, typeErrs)
	report := diag.Run(ctx, diag.Checks(target))

	fmt.Fprintf(cx.stdout, "slimproxy 状态\n\n")

	tw := tabwriter.NewWriter(cx.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  监听\t%s\n", cfg.Addr())
	if cfg.Host == "" {
		// Say which address was tested. Printing 0.0.0.0 while probing loopback
		// is how a check passes on one address and the proxy fails to bind
		// another.
		fmt.Fprintf(tw, "  \t（配置为绑定全部网卡，检查在 %s 上探测）\n", target.ProbeAddr())
	}
	fmt.Fprintf(tw, "  配置\t%s\n", cx.configPath)
	// The resolved directory, which is what the checks read and what the proxy
	// loads from -- not the raw config string, which may contain an unexpanded ~.
	fmt.Fprintf(tw, "  凭据目录\t%s\n", target.AuthDir)
	if target.TunnelName != "" {
		host := target.TunnelHostname
		if host == "" {
			host = "（未在 ingress 中找到主机名）"
		}
		fmt.Fprintf(tw, "  隧道\t%s → %s\n", target.TunnelName, host)
	} else {
		fmt.Fprintf(tw, "  隧道\t未配置\n")
	}
	_ = tw.Flush()

	fmt.Fprintf(cx.stdout, "\n检查项\n\n")
	writeResults(cx.stdout, report, false)
	writeSummary(cx.stdout, report)

	// status reports; it does not judge. A failing check is information, not a
	// reason to exit non-zero -- scripts poll this.
	return nil
}

// cmdDoctor runs the same checks and explains how to fix what it finds.
func cmdDoctor(cx *cliContext, args []string) error {
	cmd := lookup("doctor")
	fs := newFlagSet(cx, cmd)
	bindCommon(fs, cx)
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}
	cfg, typeErrs, err := cx.loadConfigForDiagnosis()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	report := diag.Run(ctx, diag.Checks(targetFor(cfg, cx.configPath, typeErrs)))

	fmt.Fprintf(cx.stdout, "slimproxy doctor\n\n")
	writeResults(cx.stdout, report, true)
	writeSummary(cx.stdout, report)

	return exitForReport(report)
}

// exitForReport maps a report onto a process exit status.
//
// Separated from cmdDoctor so the mapping can be tested without running real
// checks. It is the machine-readable half of the command: CI gates on it, so a
// silent change here is a change to a contract.
//
// UNKNOWN counts as failure. An unanswered question about a process holding
// live subscription credentials is not a pass.
func exitForReport(r diag.Report) error {
	switch r.Worst() {
	case diag.Fail, diag.Unknown:
		return errDoctorFoundProblems
	default:
		return nil
	}
}

// errDoctorFoundProblems carries a non-zero exit without printing a second
// error line -- the report above already said everything.
var errDoctorFoundProblems = &silentError{code: 1}

type silentError struct{ code int }

func (e *silentError) Error() string { return "" }

func writeResults(w io.Writer, r diag.Report, withRemedy bool) {
	for _, res := range r.Results {
		fmt.Fprintf(w, "  %-8s %-16s %s\n", res.Level, res.Name, res.Detail)
		if withRemedy && res.ShowRemedy() {
			for _, line := range wrapRemedy(res.Remedy, 68) {
				fmt.Fprintf(w, "  %-8s %-16s → %s\n", "", "", line)
			}
		}
		// Err is where some failures record what actually went wrong: a command
		// killed by a timeout produces no output at all, so without this the
		// operator reads "查询失败: " with nothing after the colon.
		if extra := res.ExtraErr(); withRemedy && extra != "" {
			fmt.Fprintf(w, "  %-8s %-16s 因: %s\n", "", "", extra)
		}
	}
}

func writeSummary(w io.Writer, r diag.Report) {
	c := r.Counts()
	parts := []string{}
	// Most severe first, from the package that defines severity. The order was
	// written out here as {Pass, Warn, Fail, Unknown} -- declaration order --
	// which put Unknown last, reading as least important for the level that
	// exists because an unanswered question is not benign.
	for _, l := range diag.LevelsBySeverity() {
		if c[l] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c[l], l))
		}
	}
	fmt.Fprintf(w, "\n  %s，用时 %s\n", strings.Join(parts, " · "), r.Took.Round(time.Millisecond))
}

// wrapRemedy breaks a remedy onto lines that fit beside the indent.
func wrapRemedy(s string, width int) []string {
	runes := []rune(s)
	if len(runes) <= width {
		return []string{s}
	}
	var out []string
	for len(runes) > width {
		out = append(out, string(runes[:width]))
		runes = runes[width:]
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}
