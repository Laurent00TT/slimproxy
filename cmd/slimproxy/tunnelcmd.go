package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/momo/slimproxy/tunnel"
)

// cmdTunnel dispatches the tunnel sub-verbs.
func cmdTunnel(cx *cliContext, args []string) error {
	cmd := lookup("tunnel")
	if len(args) == 0 {
		return fmt.Errorf("%w: tunnel 需要一个子命令\n\n用法: %s", errUsage, cmd.usage)
	}

	verb, rest := args[0], args[1:]
	switch verb {
	case "status":
		return cmdTunnelStatus(cx, rest)
	case "up":
		return cmdTunnelUp(cx, rest)
	case "down":
		return cmdTunnelDown(cx, rest)
	case "-h", "--help", "help":
		fmt.Fprintf(cx.stdout, "用法: %s\n\n%s\n\n子命令:\n"+
			"  status  显示隧道状态（未配置 / 已配置未运行 / 运行中）\n"+
			"  up      启动隧道，默认前台运行；加 -detach 后台运行\n"+
			"  down    停止由 slimproxy 启动的隧道\n", cmd.usage, cmd.summary)
		return nil
	default:
		return fmt.Errorf("%w: 未知的 tunnel 子命令 %q（可用: status / up / down）", errUsage, verb)
	}
}

func tunnelManager(cx *cliContext) *tunnel.Manager {
	return &tunnel.Manager{StateDir: cx.stateDir}
}

func cmdTunnelStatus(cx *cliContext, args []string) error {
	cmd := lookup("tunnel")
	fs := newFlagSet(cx, cmd)
	fs.StringVar(&cx.stateDir, "state", ".", "directory holding the tunnel pid record")
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := tunnelManager(cx)
	st := m.Status(ctx)

	tw := tabwriter.NewWriter(cx.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  状态\t%s\n", st.State)
	if st.Detail != "" {
		fmt.Fprintf(tw, "  说明\t%s\n", st.Detail)
	}
	if st.Err != nil {
		fmt.Fprintf(tw, "  错误\t%v\n", st.Err)
	}
	if st.TunnelID != "" {
		fmt.Fprintf(tw, "  隧道\t%s\n", st.TunnelID)
	}
	for i, h := range st.Hostnames {
		// One row per hostname, label on the first only: a tunnel serves as
		// many domains as its config has ingress rules, and printing just the
		// first made every additional domain look like it had not taken effect.
		//
		// The continuation label is three ideographic spaces: tabwriter pads
		// cells by rune count while the terminal renders CJK at two cells, so
		// only a stand-in matching "主机名" in both measures -- 3 runes, 6
		// cells -- lands the continuation hostnames in the same column.
		if i == 0 {
			fmt.Fprintf(tw, "  主机名\t%s\n", h)
		} else {
			fmt.Fprintf(tw, "  　　　\t%s\n", h)
		}
	}
	if st.ConfigPath != "" {
		fmt.Fprintf(tw, "  配置\t%s\n", st.ConfigPath)
	}
	if st.PID != 0 {
		owner := "由 slimproxy 启动"
		if !st.Managed {
			owner = "非 slimproxy 启动"
		}
		fmt.Fprintf(tw, "  进程\tPID %d（%s）\n", st.PID, owner)
	}
	if st.LogPath != "" {
		fmt.Fprintf(tw, "  日志\t%s\n", st.LogPath)
	}
	_ = tw.Flush()

	// The connection count is the difference between "a process exists" and
	// "the hostname works". Status already established it -- asking again here
	// would mean two round trips to Cloudflare for a single command.
	switch {
	case st.State != tunnel.Running:
		// Nothing running; a connection count would be meaningless.
	case !st.ConnectionsKnown:
		fmt.Fprintf(cx.stdout, "\n  连接数未知：该查询需要网络和 cert.pem，"+
			"无法查询时不能断定隧道是否正常\n")
	case st.Connections == 0:
		// Looks alive in a task list while the public hostname returns 502.
		fmt.Fprintf(cx.stdout, "\n  ⚠ 进程在运行但活动连接为 0：外网访问此时会返回 502\n")
	default:
		fmt.Fprintf(cx.stdout, "\n  活动连接 %d\n", st.Connections)
	}
	if st.EdgeFakeIP != "" {
		fmt.Fprintf(cx.stdout, "  ⚠ 边缘地址 %s 属于 RFC 2544 保留段：隧道流量经由本机代理，\n"+
			"    长连接可能反复断开；让 *.argotunnel.com 直连可消除\n", st.EdgeFakeIP)
	}
	return nil
}

func cmdTunnelUp(cx *cliContext, args []string) error {
	cmd := lookup("tunnel")
	fs := newFlagSet(cx, cmd)
	fs.StringVar(&cx.stateDir, "state", ".", "directory holding the tunnel pid record")
	detach := fs.Bool("detach", false, "后台运行，命令立即返回；用 tunnel down 停止")
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := tunnelManager(cx)

	if *detach {
		st, err := m.Up(ctx, true, nil, nil)
		if err != nil {
			return tunnelUpError(err)
		}
		fmt.Fprintf(cx.stdout, "隧道已在后台启动：PID %d，隧道 %s\n", st.PID, st.TunnelID)
		if len(st.Hostnames) > 0 {
			fmt.Fprintf(cx.stdout, "对外主机名 %s\n", tunnel.JoinHostnames(st.Hostnames))
		}
		fmt.Fprintf(cx.stdout, "日志 %s\n用 \"slimproxy tunnel down\" 停止\n", st.LogPath)
		return nil
	}

	fmt.Fprintf(cx.stdout, "隧道前台运行中，按 Ctrl-C 停止。\n\n")
	// Foreground: the child is bound to this process through the context, so
	// Ctrl-C takes it down too and nothing is left behind.
	if _, err := m.Up(ctx, false, os.Stdout, os.Stderr); err != nil {
		return tunnelUpError(err)
	}
	return nil
}

// tunnelUpError turns the manager's sentinels into actionable messages.
func tunnelUpError(err error) error {
	switch {
	case errors.Is(err, tunnel.ErrNotConfigured):
		return fmt.Errorf("%w；参见 deploy/TUNNEL.md 完成一次性配置", err)
	case errors.Is(err, tunnel.ErrAlreadyRunning):
		return fmt.Errorf("%w；用 \"slimproxy tunnel status\" 查看，或先 down", err)
	default:
		return err
	}
}

func cmdTunnelDown(cx *cliContext, args []string) error {
	cmd := lookup("tunnel")
	fs := newFlagSet(cx, cmd)
	fs.StringVar(&cx.stateDir, "state", ".", "directory holding the tunnel pid record")
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := tunnelManager(cx)

	// Say what is about to be lost, before losing it.
	//
	// Not a confirmation prompt -- typing the command is already an explicit
	// intent, and an interactive gate would break every script. But the panel
	// tells its user "当前 2 连接 · 停止后 X 立即不可达" before they commit,
	// and there is no reason the command-line user should have strictly less
	// information about the same action.
	if before := m.Status(ctx); before.State == tunnel.Running {
		// Every domain about to go dark, not just the first: with two ingress
		// hostnames, naming only one understates the blast radius by exactly
		// the domains the operator forgot were on it.
		host := tunnel.JoinHostnames(before.Hostnames)
		if host == "" {
			host = "该隧道"
		}
		switch {
		case before.ConnectionsKnown:
			fmt.Fprintf(cx.stdout, "正在停止隧道（%s，当前 %d 个连接）…\n", host, before.Connections)
		default:
			fmt.Fprintf(cx.stdout, "正在停止隧道（%s，连接数未确认）…\n", host)
		}
	}

	st, err := m.Down(ctx)
	if err != nil {
		return err
	}
	// Not the State field: Down re-queries Cloudflare, and that call is slow and
	// frequently times out on a flaky link -- so it would print "未知" right
	// after confirming the process is gone. Termination was verified locally;
	// report that, and only mention what the query did add.
	fmt.Fprintln(cx.stdout, "隧道已停止")
	if st.State == tunnel.Running && !st.Managed {
		fmt.Fprintln(cx.stdout, "注意：仍有非 slimproxy 启动的实例在运行")
	}
	return nil
}
