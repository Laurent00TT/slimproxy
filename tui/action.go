package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Laurent00TT/slimproxy/credentials"
	"github.com/Laurent00TT/slimproxy/diag"
	"github.com/Laurent00TT/slimproxy/tunnel"
)

// actionSpec is what pressing Enter on a command asks for.
//
// A command decides what to do without doing it. Splitting the decision from
// the work is what lets argument validation and the confirmation prompt happen
// on the render goroutine -- instantly -- while anything touching the network
// or the filesystem runs off it.
type actionSpec struct {
	// Err rejects the invocation outright: a missing argument, an unknown
	// provider. Reported immediately, nothing runs.
	Err error
	// Note is an immediate one-line answer for commands that need no work.
	Note string
	// Quit ends the program after this action.
	Quit bool
	// Do is the work. Nil when the command answered from state it already had.
	Do func(ctx context.Context) actionResult
	// Running is what the status line says while Do is in flight. Required
	// when Do is set: a slow command with no label looks like a freeze.
	Running string
	// Timeout bounds Do. Zero means actionDefaultTimeout.
	Timeout time.Duration
	// RefreshTunnel requests a tunnel poll once Do finishes, so a command that
	// changed the tunnel is not followed by up to 45 seconds of stale display.
	RefreshTunnel bool
	// RefreshCreds is the same for the credential pool.
	RefreshCreds bool
}

// actionResult is what came back.
type actionResult struct {
	// Title heads the output panel. Empty means the result is a one-liner.
	Title string
	// Lines is multi-line output, shown in a scrollable panel.
	Lines []string
	// Note is a one-line result shown on the status line.
	Note string
	// Err reports failure. Rendered in clay, and never merged into Note: a
	// failure that reads like a result is how "隧道已停止" ends up on screen
	// after a kill that did not work.
	Err error
	// Level tints multi-line output when the command has a verdict.
	Level diag.Level
	// HasLevel distinguishes "verdict: Pass" from "no verdict", which share
	// the zero value.
	HasLevel bool
	// Verdict is the one-line tally the status row keeps after the output
	// panel is dismissed. Only meaningful when HasLevel.
	Verdict string
}

// actionDefaultTimeout bounds a command that did not ask for its own.
//
// Generous, because the commands that reach the network are the ones worth
// waiting for: `tunnel up` alone allows 15 seconds for the child to register.
const actionDefaultTimeout = 90 * time.Second

// actionDoneMsg carries a finished action back to Update.
type actionDoneMsg struct {
	res           actionResult
	refreshTunnel bool
	refreshCreds  bool
}

// ---------- tunnel ----------

func runTunnelStatus(m *Model, _ []string) actionSpec {
	deps := m.deps
	if deps.TunnelStatus == nil {
		return actionSpec{Err: errors.New("此构建未接入隧道管理")}
	}
	return actionSpec{
		Running:       "正在查询 Cloudflare…",
		Timeout:       tunnelProbeTimeout,
		RefreshTunnel: true,
		Do: func(ctx context.Context) actionResult {
			st := deps.TunnelStatus(ctx)
			// st.Err is promoted rather than left inside the lines: a status
			// that could not be established must colour the panel as a
			// failure, not read as a calm report that happens to contain the
			// word 错误.
			return actionResult{Title: "隧道状态", Lines: tunnelLines(st), Err: st.Err}
		},
	}
}

func runTunnelUp(m *Model, _ []string) actionSpec {
	deps := m.deps
	if deps.TunnelUp == nil {
		return actionSpec{Err: errors.New("此构建未接入隧道管理")}
	}
	return actionSpec{
		Running:       "正在启动 cloudflared 并等待连接注册…",
		RefreshTunnel: true,
		Do: func(ctx context.Context) actionResult {
			st, err := deps.TunnelUp(ctx)
			if err != nil {
				// The error text carries a log tail on failure, which is the
				// only place the reason exists. Kept as lines rather than
				// squeezed onto the status line.
				return actionResult{
					Title: "启动隧道失败",
					Lines: splitLines(err.Error()),
					Err:   err,
				}
			}
			note := "隧道已启动"
			if st.PID > 0 {
				note = fmt.Sprintf("隧道已启动（PID %d）", st.PID)
			}
			if st.Detail != "" {
				return actionResult{Title: note, Lines: []string{st.Detail}}
			}
			return actionResult{Note: note}
		},
	}
}

func runTunnelDown(m *Model, _ []string) actionSpec {
	deps := m.deps
	if deps.TunnelDown == nil {
		return actionSpec{Err: errors.New("此构建未接入隧道管理")}
	}
	return actionSpec{
		Running:       "正在停止 cloudflared…",
		RefreshTunnel: true,
		Do: func(ctx context.Context) actionResult {
			st, err := deps.TunnelDown(ctx)
			if err != nil {
				return actionResult{Title: "停止隧道失败", Lines: splitLines(err.Error()), Err: err}
			}
			// Detail carries the case where nothing was actually stopped -- a
			// process that had already died, a second unmanaged connector
			// still serving. Leading with "隧道已停止" and appending that would
			// claim the action succeeded and then quietly contradict it.
			if st.Detail != "" {
				return actionResult{Title: "tunnel down", Lines: []string{st.Detail}}
			}
			return actionResult{Note: "隧道已停止"}
		},
	}
}

// tunnelLines renders a Status for the output panel.
//
// Every field that can be unknown says so rather than showing a zero. A tunnel
// whose connection count could not be read must not display "0 连接" -- that
// is the reading for a tunnel that is up and serving nobody, which is a
// different and much worse situation.
func tunnelLines(st tunnel.Status) []string {
	out := []string{"状态      " + st.State.String()}
	for i, h := range st.Hostnames {
		// One line per hostname, label on the first only. This panel is where
		// the full list lives -- the top bar collapses it to "host +N".
		if i == 0 {
			out = append(out, "主机名    "+h)
		} else {
			out = append(out, "          "+h)
		}
	}
	if st.TunnelID != "" {
		out = append(out, "隧道 ID   "+st.TunnelID)
	}
	if st.ConfiguredTunnelID != "" {
		out = append(out, "配置中的  "+st.ConfiguredTunnelID+"（与运行中的不一致）")
	}
	switch {
	case st.ConnectionsKnown:
		out = append(out, fmt.Sprintf("边缘连接  %d", st.Connections))
	case st.ConnectionsErr != nil:
		// The reason, not just the absence. "未能确认" alone left the operator
		// unable to tell an expired Cloudflare token from a DNS failure.
		out = append(out, "边缘连接  查询失败: "+st.ConnectionsErr.Error())
	case st.State != tunnel.Running:
		// Nothing is running, so there is no count to have failed to read.
		// Saying "未能确认" here reads like a probe that broke.
	default:
		out = append(out, "边缘连接  未能确认")
	}
	if st.EdgeFakeIP != "" {
		out = append(out, "边缘地址  "+st.EdgeFakeIP+"（fake-ip 段，流量经本地代理，长连接易断）")
	}
	if st.PID > 0 {
		managed := "由本进程管理"
		if !st.Managed {
			managed = "非本进程启动"
		}
		out = append(out, fmt.Sprintf("进程      PID %d（%s）", st.PID, managed))
	} else if st.State == tunnel.Running && !st.Managed {
		// Just the fact. The consequence -- that `tunnel down` cannot stop it
		// -- is already in Status.Detail, which is appended below; saying it
		// here too printed the same sentence twice in one panel.
		out = append(out, "进程      非本进程启动")
	}
	if st.ConfigPath != "" {
		out = append(out, "配置文件  "+st.ConfigPath)
	}
	if st.LogPath != "" {
		out = append(out, "日志      "+st.LogPath)
	}
	if st.Detail != "" {
		out = append(out, "", st.Detail)
	}
	if st.Err != nil {
		out = append(out, "", "错误: "+st.Err.Error())
	}
	return out
}

// ---------- credentials ----------

func runAuthList(m *Model, _ []string) actionSpec {
	deps := m.deps
	if deps.Creds == nil {
		return actionSpec{Err: errors.New("此构建未接入凭据管理")}
	}
	authDir := deps.AuthDir
	return actionSpec{
		Running:      "正在读取凭据目录…",
		Timeout:      credsReadTimeout,
		RefreshCreds: true,
		Do: func(ctx context.Context) actionResult {
			creds, err := deps.Creds(ctx)
			if err != nil {
				return actionResult{Title: "读取凭据失败", Lines: splitLines(err.Error()), Err: err}
			}
			if len(creds) == 0 {
				return actionResult{
					Title: "凭据池为空",
					Lines: []string{
						"目录  " + authDir,
						"",
						"代理会启动并接受请求，但每一个都会因为没有可用凭据而失败。",
						"退出后运行 slimproxy auth add <provider> 添加。",
					},
				}
			}
			return actionResult{Title: fmt.Sprintf("凭据池（%d）", len(creds)), Lines: credLines(creds, time.Now())}
		},
	}
}

// credLines renders the pool as aligned rows.
//
// Column widths are computed from the content rather than fixed, so a long
// account address does not push the status column off the panel on one row
// while the others sit narrow.
func credLines(creds []credentials.Credential, now time.Time) []string {
	nameW, acctW := 0, 0
	for _, c := range creds {
		if w := width(c.Name); w > nameW {
			nameW = w
		}
		if w := width(c.Account); w > acctW {
			acctW = w
		}
	}
	// Bounded: past this the panel is better off truncating one long name than
	// squeezing every other column.
	nameW = min(nameW, 34)
	acctW = min(acctW, 30)

	out := make([]string, 0, len(creds))
	for _, c := range creds {
		st := c.Status(now)
		row := pad(c.Name, nameW) + "  " + pad(c.Account, acctW) + "  " + pad(st.String(), 8)
		switch {
		case c.Err != nil:
			row += "  " + c.Err.Error()
		case st == credentials.StatusUnknown:
			row += "  文件中没有过期时间"
		case !c.Expires.IsZero():
			row += "  " + relTime(c.Expires, now)
		}
		out = append(out, row)
	}
	return out
}

// relTime renders an expiry relative to now, in the direction that reads
// correctly on both sides of the boundary.
func relTime(t, now time.Time) string {
	d := t.Sub(now)
	if d < 0 {
		return "已过期 " + shortDur(-d)
	}
	return shortDur(d) + " 后过期"
}

func runAuthRemove(m *Model, args []string) actionSpec {
	deps := m.deps
	if deps.RemoveCred == nil {
		return actionSpec{Err: errors.New("此构建未接入凭据管理")}
	}
	switch len(args) {
	case 0:
		return actionSpec{Err: errors.New("需要一个参数：/auth rm <名称或邮箱>")}
	case 1:
	default:
		return actionSpec{Err: fmt.Errorf("只接受一个参数，收到 %d 个：%s",
			len(args), strings.Join(args, " "))}
	}
	id := args[0]
	return actionSpec{
		Running:      "正在删除 " + truncate(id, 40) + "…",
		Timeout:      credsReadTimeout,
		RefreshCreds: true,
		Do: func(ctx context.Context) actionResult {
			c, err := deps.RemoveCred(ctx, id, false)
			if err != nil {
				// The last-credential guard is the error most worth explaining
				// rather than just reporting, since the way past it is a flag
				// this interface does not expose.
				if errors.Is(err, credentials.ErrLastUsable) {
					return actionResult{
						Title: "已拒绝删除",
						Lines: []string{
							err.Error(),
							"",
							"确实要删除的话，退出后运行:",
							"  slimproxy auth rm " + id + " -force",
						},
						Err: err,
					}
				}
				return actionResult{Title: "删除失败", Lines: splitLines(err.Error()), Err: err}
			}
			return actionResult{Note: "已删除 " + c.Name}
		},
	}
}

// runAuthAdd declines, and says where to go instead.
//
// The OAuth flows open a browser and wait for a pasted code. Driving that from
// inside an alternate-screen TUI means taking over the terminal for a
// multi-step external interaction whose every failure mode -- no browser, wrong
// account signed in, expired code -- would surface as a frozen dashboard.
// Refusing costs one line; pretending to support it costs the operator a
// wedged screen and no way to see why.
func runAuthAdd(m *Model, args []string) actionSpec {
	if len(args) == 0 {
		return actionSpec{
			Note: "登录需要浏览器交互，请按 q 退出后运行: slimproxy auth add <" +
				strings.Join(credentials.ProviderNames(), "|") + ">",
		}
	}
	// Validated here rather than echoed back. The CLI checks the provider name
	// before doing anything, and a panel that repeats a typo inside an
	// otherwise-correct command line sends the operator to run it and hit the
	// same error one step later.
	if !credentials.KnownProvider(args[0]) {
		return actionSpec{Err: fmt.Errorf("不支持的 provider %q（可用: %s）",
			args[0], strings.Join(credentials.ProviderNames(), ", "))}
	}
	return actionSpec{
		Note: "登录需要浏览器交互，请按 q 退出后运行: slimproxy auth add " + args[0],
	}
}

// ---------- diagnostics ----------

func runDoctor(m *Model, _ []string) actionSpec {
	deps := m.deps
	if deps.Doctor == nil {
		return actionSpec{Err: errors.New("此构建未接入诊断")}
	}
	return actionSpec{
		Running:       "正在运行诊断（可能需要数十秒）…",
		RefreshTunnel: true,
		RefreshCreds:  true,
		Do: func(ctx context.Context) actionResult {
			rep := deps.Doctor(ctx)
			v := verdict(rep)
			return actionResult{
				Title:    fmt.Sprintf("诊断报告（%s，耗时 %s）", v, shortDur(rep.Took)),
				Lines:    reportLines(rep),
				Level:    rep.Worst(),
				HasLevel: true,
				Verdict:  v,
			}
		},
	}
}

func verdict(rep diag.Report) string {
	counts := rep.Counts()
	parts := []string{}
	for _, l := range diag.LevelsBySeverity() {
		if n := counts[l]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", l, n))
		}
	}
	if len(parts) == 0 {
		return "无检查项"
	}
	return strings.Join(parts, " · ")
}

// reportLines renders a report for the output panel.
//
// The layout is this package's business; the two judgements are not.
// ShowRemedy and ExtraErr come from diag because the CLI renderer makes the
// same calls with a different layout, and writing them out again here is
// exactly how this once printed remedies under passing checks and repeated an
// error the detail line already carried.
func reportLines(rep diag.Report) []string {
	out := make([]string, 0, len(rep.Results)*3)
	for _, r := range rep.Results {
		// pad, not %-8s: those count bytes, so the day a check gets a Chinese
		// name the columns silently shift.
		out = append(out, pad(r.Level.String(), 8)+" "+pad(r.Name, 16)+" "+r.Detail)
		if r.ShowRemedy() {
			out = append(out, "         → "+r.Remedy)
		}
		if extra := r.ExtraErr(); extra != "" {
			out = append(out, "         因: "+extra)
		}
	}
	return out
}

// ---------- misc ----------

func runRoutes(m *Model, _ []string) actionSpec {
	deps := m.deps
	if deps.Routes == nil {
		return actionSpec{Err: errors.New("此构建未接入路由清单")}
	}
	return actionSpec{
		Running: "…",
		Timeout: 5 * time.Second,
		Do: func(context.Context) actionResult {
			routes := deps.Routes()
			if len(routes) == 0 {
				return actionResult{Title: "没有注册任何翻译路由", Lines: []string{
					"未注册的协议对不会报错，请求体会原样转发到上游。",
				}}
			}
			byClient := map[string][]string{}
			for _, r := range routes {
				byClient[r.Client] = append(byClient[r.Client], r.Provider)
			}
			clients := make([]string, 0, len(byClient))
			for c := range byClient {
				clients = append(clients, c)
			}
			sort.Strings(clients)

			lines := make([]string, 0, len(clients))
			for _, c := range clients {
				providers := byClient[c]
				sort.Strings(providers)
				lines = append(lines, pad(c, 14)+"→  "+strings.Join(providers, ", "))
			}
			return actionResult{
				Title: fmt.Sprintf("翻译路由（%d 条）", len(routes)),
				Lines: lines,
			}
		},
	}
}

// runQuit is unreachable as a Do: execute handles spec.Quit before dispatch.
// It exists so /quit and the q key are the same command rather than two
// spellings that can drift.
func runQuit(m *Model, _ []string) actionSpec {
	return actionSpec{Quit: true}
}

// splitLines turns a multi-line error into panel rows, dropping the trailing
// blank a message ending in \n would otherwise contribute.
func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\r\n"), "\n")
}

// shortDur renders a duration the way an operator reads it: the two largest
// units, never nanoseconds.
func shortDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
