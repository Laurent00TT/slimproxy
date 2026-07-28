package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Laurent00TT/slimproxy/credentials"
	"github.com/Laurent00TT/slimproxy/metrics"
	"github.com/Laurent00TT/slimproxy/tunnel"
)

// Everything here pins a defect that shipped and was found by review. Each test
// names the wrong behaviour, because a test whose only description is what the
// code now does gets deleted the next time the code changes.

// ---------- polling ----------

// runUntilQuiet drives commands until none are left, counting timers still
// pending.
//
// A tea.Tick produces nothing until its deadline while every fetch here answers
// immediately (the stubs return zero values), so "produced nothing inside the
// probe window" identifies a live timer chain.
func runUntilQuiet(t *testing.T, m Model, cmd tea.Cmd) (Model, int) {
	t.Helper()
	timers := 0
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		out := make(chan tea.Msg, 1)
		go func() { out <- c() }()
		select {
		case msg := <-out:
			if batch, ok := msg.(tea.BatchMsg); ok {
				queue = append(queue, batch...)
				continue
			}
			next, nc := m.Update(msg)
			m = next.(Model)
			queue = append(queue, nc)
		case <-time.After(150 * time.Millisecond):
			timers++
		}
	}
	return m, timers
}

// TestPollingChainsNeverMultiply.
//
// Was: every result message armed the next timer, but results come from the
// poll tick, from the r key, and from any command with RefreshTunnel or
// RefreshCreds set. Each manual refresh therefore forked a second
// self-sustaining chain that never merged back -- three presses of r took the
// tunnel from one Cloudflare round trip per interval to seven, and running
// /doctor a few times multiplied it again.
//
// The invariant is one outstanding timer per source, forever.
func TestPollingChainsNeverMultiply(t *testing.T) {
	m := sampleModel(90, 30)

	m, chains := runUntilQuiet(t, m, m.Init())
	if chains != 3 {
		t.Fatalf("Init 之后有 %d 条轮询链，应为 3（stats/creds/tunnel 各一）", chains)
	}

	for i := 1; i <= 3; i++ {
		next, cmd := press(t, m, key("r"))
		var added int
		m, added = runUntilQuiet(t, next, cmd)
		if added != 0 {
			t.Errorf("第 %d 次手动刷新新增了 %d 条轮询链，应为 0", i, added)
		}
	}

	// A command's follow-up refresh goes through the same path.
	next, cmd := m.Update(actionDoneMsg{
		res:           actionResult{Note: "done"},
		refreshTunnel: true,
		refreshCreds:  true,
	})
	_, added := runUntilQuiet(t, next.(Model), cmd)
	if added != 0 {
		t.Errorf("命令附带的刷新新增了 %d 条轮询链，应为 0", added)
	}
}

// TestFirstTunnelProbeIsGuarded.
//
// Was: Init fetched directly. Init has a value receiver and cannot set
// m.tunnel.inFlight, so the very first probe was unguarded -- pressing r during
// it ran two concurrent Cloudflare queries whose results then landed in arrival
// order rather than observation order.
func TestFirstTunnelProbeIsGuarded(t *testing.T) {
	m := sampleModel(90, 30)
	m.tunnel = tunnelView{} // as at startup

	// Init asks for a tick; handling it is what marks the probe in flight.
	next, _ := m.Update(tickMsg{source: srcTunnel})
	if !next.(Model).tunnel.inFlight {
		t.Error("首次隧道探测期间 inFlight 应为真，否则手动刷新会并发第二次查询")
	}
}

// ---------- credential expiry direction ----------

// TestExpiredCredentialIsNotReportedAsFutureExpiry.
//
// Was: soonest ranged over every recoverable credential, so an entry that
// expired three days ago set it to an instant in the past. shortDur takes an
// absolute value, so the row rendered "3d00h 后过期" -- reporting how long ago
// one credential died as how long the pool had left.
func TestExpiredCredentialIsNotReportedAsFutureExpiry(t *testing.T) {
	now := fixedNow
	v := summariseCreds(credsMsg{
		at: now,
		creds: []credentials.Credential{
			{Name: "dead.json", Provider: "claude", Expires: now.Add(-72 * time.Hour)},
			{Name: "live.json", Provider: "claude", Expires: now.Add(8 * time.Hour)},
		},
	}, now)

	if v.soonest.Before(now) {
		t.Fatalf("soonest = %v，早于 now：它只应统计仍可用的凭据", v.soonest)
	}
	if got, want := v.soonest, now.Add(8*time.Hour); !got.Equal(want) {
		t.Errorf("soonest = %v，应为仍可用凭据的过期时间 %v", got, want)
	}

	_, _, detail := credSymbol(v, now)
	if strings.Contains(detail, "72h") || strings.Contains(detail, "3d") {
		t.Errorf("说明 = %q，把已过期凭据的年龄当成了剩余寿命", detail)
	}
	if !strings.Contains(detail, "8h") {
		t.Errorf("说明 = %q，应报告仍可用凭据的剩余寿命", detail)
	}
}

// TestBrokenCredentialsAreVisibleOnTheStatusRow.
//
// Was: credSubject counted every file and credSymbol looked only at
// usable/recoverable, so a pool of one healthy and two unparseable credentials
// rendered as a green "3 个凭据". credentials.List keeps broken files
// deliberately -- "a file the proxy will fail to load is exactly what an
// operator needs to see" -- and the panel threw that away.
func TestBrokenCredentialsAreVisibleOnTheStatusRow(t *testing.T) {
	now := fixedNow
	v := summariseCreds(credsMsg{
		at: now,
		creds: []credentials.Credential{
			{Name: "good.json", Provider: "claude", Expires: now.Add(5 * time.Hour)},
			{Name: "bad1.json", Err: errBroken},
			{Name: "bad2.json", Err: errBroken},
		},
	}, now)

	sym, _, detail := credSymbol(v, now)
	if sym != symBad {
		t.Errorf("符号 = %q，池中有无法加载的凭据时应告警", sym)
	}
	if !strings.Contains(detail, "无法加载") {
		t.Errorf("说明 = %q，应指出有凭据无法加载", detail)
	}
}

var errBroken = &stringError{"不是合法 JSON"}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

// ---------- tunnel honesty ----------

// TestUnreadableConnectionCountIsNotGreen.
//
// Was: a running tunnel whose connection count could not be read rendered as a
// green ●. An unreachable Cloudflare and a tunnel serving nobody look identical
// from there, and one of them means the public hostname returns 502. The
// operator scans three rows, sees green, walks away.
func TestUnreadableConnectionCountIsNotGreen(t *testing.T) {
	for _, tc := range []struct {
		name string
		view tunnelView
	}{
		{"查询报错", tunnelView{
			known: true,
			st: tunnel.Status{
				State: tunnel.Running, Managed: true,
				ConnectionsErr: errBroken,
			},
		}},
		{"未确认", tunnelView{
			known: true,
			st:    tunnel.Status{State: tunnel.Running, Managed: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sym, _, detail := tunnelSymbol(tc.view)
			if sym == symOK {
				t.Errorf("符号 = %q（健康），但连接数并未确认: %q", sym, detail)
			}
		})
	}
}

// TestConnectionQueryFailureCarriesItsReason.
//
// Was: fillConnections discarded the error, so the reason a count was
// unavailable existed nowhere in the process. Even the deep `tunnel status`
// could only say "未能确认" -- indistinguishable between an expired Cloudflare
// token, a DNS failure, and a wrong tunnel ID.
func TestConnectionQueryFailureCarriesItsReason(t *testing.T) {
	lines := tunnelLines(tunnel.Status{
		State: tunnel.Running, Managed: true, PID: 42,
		Hostnames:      []string{"proxy.example.com"},
		ConnectionsErr: &stringError{"cert.pem 已过期"},
	})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "cert.pem 已过期") {
		t.Errorf("输出未包含查询失败的原因:\n%s", joined)
	}
}

// TestNothingRunningDoesNotClaimAFailedProbe.
//
// "未能确认" about a tunnel that is not configured reads like a probe that
// broke. There is no count to have failed to read.
func TestNothingRunningDoesNotClaimAFailedProbe(t *testing.T) {
	lines := tunnelLines(tunnel.Status{State: tunnel.NotConfigured})
	if joined := strings.Join(lines, "\n"); strings.Contains(joined, "未能确认") {
		t.Errorf("未配置隧道时不应报告连接数未能确认:\n%s", joined)
	}
}

// TestUnmanagedTunnelSaysItOnlyOnce.
//
// Was: tunnelLines synthesised "非本进程启动，tunnel down 无法停止它" and then
// appended Status.Detail, which the tunnel package had already filled with the
// same sentence. The panel printed it twice.
func TestUnmanagedTunnelSaysItOnlyOnce(t *testing.T) {
	lines := tunnelLines(tunnel.Status{
		State: tunnel.Running, Managed: false,
		Hostnames: []string{"proxy.example.com"},
		Detail:    "有 2 个活动连接，但不是由 slimproxy 启动的；tunnel down 无法停止它",
	})
	if n := strings.Count(strings.Join(lines, "\n"), "tunnel down 无法停止它"); n != 1 {
		t.Errorf("同一句话出现 %d 次，应为 1 次:\n%s", n, strings.Join(lines, "\n"))
	}
}

// TestSecondHostnameIsVisible.
//
// Was: Status carried a single Hostname -- the first ingress rule that routed
// somewhere -- so a second domain served by the same tunnel appeared nowhere.
// The operator who had just added one read the dashboard as "还是显示一个" and
// concluded the new domain had not taken effect, when it was routing fine.
func TestSecondHostnameIsVisible(t *testing.T) {
	hs := []string{"proxy.example.com", "proxy.example.org"}

	// The top bar collapses to a count: subjectW cannot hold two domains, but
	// it must say the second one exists.
	m := sampleModel(90, 26)
	m.tunnel.st.Hostnames = hs
	if out := m.View(); !strings.Contains(out, "proxy.example.com +1") {
		t.Errorf("顶栏没有显示额外域名的数量:\n%s", out)
	}

	// The panel is where the full list lives.
	joined := strings.Join(tunnelLines(tunnel.Status{State: tunnel.Running, Hostnames: hs}), "\n")
	for _, h := range hs {
		if !strings.Contains(joined, h) {
			t.Errorf("面板缺少主机名 %s:\n%s", h, joined)
		}
	}
}

// TestExtraHostnameCountSurvivesTruncation.
//
// Was: the "+N" suffix was appended before truncation, and truncate cuts from
// the right -- so any first hostname of 23+ display cells lost the marker, and
// a multi-domain tunnel rendered identically to a truncated single-domain one.
// The exact hidden-extra-domains failure the marker exists to prevent, back
// for every hostname long enough to matter.
func TestExtraHostnameCountSurvivesTruncation(t *testing.T) {
	m := sampleModel(90, 26)
	m.tunnel.st.Hostnames = []string{"gateway.internal.example.com", "proxy.example.org"}
	if out := m.View(); !strings.Contains(out, "+1") {
		t.Errorf("长主机名截断后 +1 标记消失:\n%s", out)
	}
}

// ---------- request stream ----------

// TestStatusColumnNeverInventsTwoHundred.
//
// Was: successes were hardcoded to "200" -- a value the usage record never
// carries, so 201 and 204 were displayed as 200 too -- while every failure
// collapsed to "err", hiding the difference between 401 (replace the
// credential) and 429 (wait).
func TestStatusColumnNeverInventsTwoHundred(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sample metrics.Sample
		want   string
	}{
		{"成功", metrics.Sample{}, "ok"},
		{"限流", metrics.Sample{Failed: true, Status: 429}, "429"},
		{"凭据失效", metrics.Sample{Failed: true, Status: 401}, "401"},
		{"失败但无状态码", metrics.Sample{Failed: true}, "err"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := sampleStatus(tc.sample)
			if got != tc.want {
				t.Errorf("状态列 = %q，应为 %q", got, tc.want)
			}
			if got == "200" {
				t.Error("面板不应显示进程从未观测到的 200")
			}
		})
	}
}

// ---------- quit ----------

// TestQuitBehavesTheSameBothWays.
//
// q and /quit are one action. Marking only the typed spelling Danger gave it a
// confirmation the key press did not have.
func TestQuitBehavesTheSameBothWays(t *testing.T) {
	viaKey := sampleModel(90, 30)
	viaKey, _ = press(t, viaKey, key("q"))

	viaCmd := sampleModel(90, 30)
	viaCmd, _ = press(t, viaCmd, key("/"))
	for _, r := range "quit" {
		viaCmd, _ = press(t, viaCmd, key(string(r)))
	}
	viaCmd, _ = press(t, viaCmd, special(tea.KeyEnter))

	if viaKey.quitting != viaCmd.quitting {
		t.Errorf("q 退出=%v，/quit 退出=%v，两者应一致", viaKey.quitting, viaCmd.quitting)
	}
	if !viaKey.quitting {
		t.Error("空闲状态下 q 应直接退出")
	}
}

// TestQuitRefusesOnceWhileAnActionRuns.
//
// Was: q quit immediately regardless. Quitting cancels the context every
// running action holds, and for `tunnel up` that is not inert -- confirmStarted
// reads the cancellation as "never connected", kills the cloudflared it just
// launched and deletes the pid record. The operator asked to start a tunnel,
// pressed q while waiting, and got a killed tunnel and a blank screen, because
// the message explaining it was delivered to a program that had already
// stopped.
func TestQuitRefusesOnceWhileAnActionRuns(t *testing.T) {
	var aborted string
	m := sampleModel(90, 30)
	m.running = "正在启动 cloudflared 并等待连接注册…"
	m.deps.OnAbort = func(a string) { aborted = a }

	m, _ = press(t, m, key("q"))
	if m.quitting {
		t.Fatal("动作执行中第一次按 q 不应立即退出")
	}
	if m.note == "" {
		t.Error("应说明为何没有退出")
	}

	m, _ = press(t, m, key("q"))
	if !m.quitting {
		t.Error("第二次按 q 应退出")
	}
	if aborted != "正在启动 cloudflared 并等待连接注册…" {
		t.Errorf("中止的动作 = %q，应报告给宿主以便退出后打印", aborted)
	}
}

// TestCtrlCAlwaysQuitsImmediately.
//
// The escape hatch has no second-press guard on purpose: a dashboard that
// argues with Ctrl-C can only be escaped by killing the process from another
// terminal. It still reports what it interrupted.
func TestCtrlCAlwaysQuitsImmediately(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m Model) Model
	}{
		{"普通模式", func(m Model) Model { return m }},
		{"命令模式", func(m Model) Model {
			m.entry.active = true
			m.entry.text = "/tunnel"
			m.refreshHits()
			return m
		}},
		{"确认态", func(m Model) Model {
			m.entry.confirming = m.cmds[2]
			return m
		}},
		{"执行中", func(m Model) Model { m.running = "正在停止 cloudflared…"; return m }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.setup(sampleModel(90, 30))
			m, _ = press(t, m, special(tea.KeyCtrlC))
			if !m.quitting {
				t.Error("Ctrl-C 应在任何模式下立即退出")
			}
		})
	}
}

// ---------- completion selection ----------

// TestEditingTheQueryResetsTheSelection.
//
// Was: refreshHits only clamped downward, so a selection made by arrowing
// survived into a completely different list. Pressing / then arrowing to quit
// then typing one character left the highlight on `tunnel down` -- a
// destructive command the operator never navigated to.
func TestEditingTheQueryResetsTheSelection(t *testing.T) {
	m := sampleModel(90, 30)
	m, _ = press(t, m, key("/"))
	for i := 0; i < 8; i++ {
		m, _ = press(t, m, special(tea.KeyDown))
	}
	before := m.selected()
	if before == nil || before.Name != "quit" {
		t.Fatalf("方向键之后选中 %v，预期 quit", before)
	}

	m, _ = press(t, m, key("t"))
	if got := m.selected(); got == nil || got.Danger {
		t.Errorf("输入后选中 %v，编辑查询不应把高亮移到破坏性命令上", got)
	}
	if m.entry.sel != 0 {
		t.Errorf("sel = %d，编辑查询后应回到 0", m.entry.sel)
	}
}

// ---------- timeouts ----------

// TestTimeoutFiresEvenWhenTheWorkIgnoresContext.
//
// Was: execute called do(ctx) synchronously and inspected ctx afterwards. Some
// of what Do reaches -- os.ReadDir on a network share -- cannot be interrupted
// by a context, so the deadline was advisory: m.running stayed set forever and
// every later command was refused with "请等它结束".
func TestTimeoutFiresEvenWhenTheWorkIgnoresContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	m := sampleModel(90, 30)
	stuck := &Command{
		Name: "stuck",
		Run: func(*Model, []string) actionSpec {
			return actionSpec{
				Running: "正在卡住…",
				Timeout: 100 * time.Millisecond,
				Do: func(context.Context) actionResult {
					<-release // ignores ctx entirely
					return actionResult{Note: "never"}
				},
			}
		},
	}

	next, cmd := m.execute(stuck, nil)
	if cmd == nil {
		t.Fatal("应返回执行命令")
	}
	if next.(Model).running == "" {
		t.Error("执行期间 running 应被设置")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		res := msg.(actionDoneMsg).res
		if res.Err == nil {
			t.Error("超时应作为错误上报")
		}
		if !strings.Contains(strings.Join(res.Lines, "\n"), "stuck") {
			t.Errorf("超时说明应点名是哪条命令: %v", res.Lines)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Do 忽略 ctx 时超时未生效，面板会永久卡在执行中")
	}
}

// ---------- scrolling ----------

// TestEveryLineIsReachableByPaging.
//
// Was: resultSection reserved the last row for the scroll indicator and
// resultViewHeight did not, so maxScroll was one too small and the page step
// one too large. End never reached the last line of a doctor report, and paging
// from the top skipped a line per screen with nothing to say it had.
func TestEveryLineIsReachableByPaging(t *testing.T) {
	for _, size := range [][2]int{{90, 26}, {60, 24}, {100, 40}} {
		for _, n := range []int{5, 40, 200} {
			t.Run(sizeName(size[0], size[1])+"/"+itoa(n), func(t *testing.T) {
				m := sampleModel(size[0], size[1])
				m.result = &actionResult{Title: "输出", Lines: numberedLines(n)}

				seen := map[string]bool{}
				record := func(m Model) {
					for _, line := range strings.Split(m.View(), "\n") {
						for i := 0; i < n; i++ {
							if strings.Contains(line, marker(i)) {
								seen[marker(i)] = true
							}
						}
					}
				}

				record(m)
				for i := 0; i < n+5; i++ {
					m, _ = press(t, m, special(tea.KeyPgDown))
					record(m)
				}

				var missing []string
				for i := 0; i < n; i++ {
					if !seen[marker(i)] {
						missing = append(missing, marker(i))
					}
				}
				if len(missing) > 0 {
					t.Errorf("翻页过程中有 %d 行从未显示: %v", len(missing), missing)
				}
			})
		}
	}
}

// TestEndShowsTheLastLine pins the specific symptom: the final line of a doctor
// report is the verdict most worth reading.
func TestEndShowsTheLastLine(t *testing.T) {
	m := sampleModel(90, 26)
	m.result = &actionResult{Title: "输出", Lines: numberedLines(60)}

	m, _ = press(t, m, special(tea.KeyEnd))
	if !strings.Contains(m.View(), marker(59)) {
		t.Error("按 End 之后最后一行仍不可见")
	}
}

func numberedLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = marker(i) + " 一些中文内容"
	}
	return out
}

func marker(i int) string { return "L<" + itoa(i) + ">" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ---------- hostile content ----------

// TestControlCharactersCannotBreakTheFrame.
//
// Everything the panel displays that is not its own text arrives from a config
// file, a provider's response, or a filename on disk. A newline splits one row
// into two and shifts every row below it; an ESC is executed by the terminal
// rather than shown, so a model name containing \x1b[2J clears the screen.
func TestControlCharactersCannotBreakTheFrame(t *testing.T) {
	hostile := []string{
		"claude\nopus",
		"claude\topus",
		"claude\x1b[2J\x1b[Hopus",
		"claude\x07opus",
		"claude\ropus",
	}

	for _, s := range hostile {
		t.Run(escapeForName(s), func(t *testing.T) {
			m := sampleModel(90, 26)
			m.stats.Recent = []metrics.Sample{{
				At: fixedNow, Account: "openai@acct", Target: "claude", Model: s,
				TTFT: time.Second, Tokens: 100,
			}}
			m.stats.Active = []metrics.RouteAgg{{Route: s, Count: 1, AvgTTFT: time.Second, Pct: 100}}
			m.tunnel.hostname = s
			m.tunnel.st.Hostnames = []string{s}

			out := m.View()
			for i, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > m.width {
					t.Errorf("第 %d 行宽 %d，超出终端宽度 %d: %q", i+1, w, m.width, line)
				}
			}
			for _, r := range out {
				if r != '\n' && isControl(r) {
					t.Errorf("渲染结果里出现控制字符 %q，会被终端执行而非显示", r)
					break
				}
			}
		})
	}
}

func escapeForName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isControl(r) {
			b.WriteString("^")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// TestNoLineEverExceedsTerminalWidth.
//
// Was: the prompt, the confirmation question and the running line skipped
// truncation. lipgloss.Height counts newlines rather than wrapped rows, so an
// over-wide line reported height 1 while occupying two physical rows -- and the
// panel, sized against that, pushed its own header off the alternate screen
// where it cannot be scrolled back to. Reachable by typing a long path into
// `/auth rm`.
func TestNoLineEverExceedsTerminalWidth(t *testing.T) {
	long := strings.Repeat("很长的路径/", 60)

	for _, tc := range []struct {
		name  string
		build func() Model
	}{
		{"输入行", func() Model {
			m := sampleModel(90, 26)
			m.entry.active = true
			m.entry.text = "/auth rm " + long
			m.refreshHits()
			return m
		}},
		{"确认行", func() Model {
			m := sampleModel(90, 26)
			m.entry.confirming = findCommand(t, m, "auth rm")
			m.entry.confirmArg = []string{long}
			return m
		}},
		{"执行中", func() Model {
			m := sampleModel(90, 26)
			m.running = "正在删除 " + long + "…"
			return m
		}},
		{"提示行", func() Model {
			m := sampleModel(90, 26)
			m.setNote(long, true)
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.build()
			for i, line := range strings.Split(m.View(), "\n") {
				if w := lipgloss.Width(line); w > m.width {
					t.Errorf("第 %d 行宽 %d，超出终端宽度 %d", i+1, w, m.width)
				}
			}
			if h := lipgloss.Height(m.View()); h > m.height {
				t.Errorf("渲染 %d 行，终端只有 %d 行", h, m.height)
			}
		})
	}
}

// ---------- determinism ----------

// TestRenderingDoesNotReadTheWallClock.
//
// Every renderer must read m.now. headerRow did not, so two renders of one
// identical model disagreed about uptime -- and the same slip elsewhere made
// the credential row report the gap between polling and painting as the time
// left before expiry.
func TestRenderingDoesNotReadTheWallClock(t *testing.T) {
	m := sampleModel(90, 26)
	m.stats.Uptime = 0 // the branch that used time.Since
	m.lastDoctor = &doctorSummary{summary: "PASS 5", at: fixedNow}

	first := m.View()
	time.Sleep(30 * time.Millisecond)
	if second := m.View(); first != second {
		t.Error("同一个 model 渲染两次结果不同：某处读了墙上时钟而非 m.now")
	}
}

// ---------- argument handling ----------

// TestAuthAddRejectsUnknownProvider.
//
// Was: the panel echoed whatever was typed into a command line for the operator
// to run, so a typo was reported one step later by the CLI instead of here.
func TestAuthAddRejectsUnknownProvider(t *testing.T) {
	m := sampleModel(90, 30)

	if spec := runAuthAdd(&m, []string{"clude"}); spec.Err == nil {
		t.Errorf("未知 provider 应被拒绝，实际返回 Note=%q", spec.Note)
	}
	if spec := runAuthAdd(&m, []string{"claude"}); spec.Err != nil {
		t.Errorf("已知 provider 不应被拒绝: %v", spec.Err)
	}
}

// TestEmptyPoolConsequenceDoesNotInventACredential.
//
// Was: the zero case fell into a branch whose message hardcoded "仅剩 1 个",
// counting a credential that does not exist.
func TestEmptyPoolConsequenceDoesNotInventACredential(t *testing.T) {
	m := sampleModel(90, 30)
	m.creds = credsView{known: true, recoverable: 0}

	c := findCommand(t, m, "auth rm")
	got := c.Consequence(&m)
	if strings.Contains(got, "1 个") {
		t.Errorf("后果说明 = %q，池中并没有凭据", got)
	}
}

// TestQuitWarningIsActuallyVisible.
//
// Was: renderStatusLine checked m.running before m.note, and both are set when
// a quit is refused. The panel showed "正在启动 cloudflared…" while the warning
// that a second q would abort it went undisplayed -- so the second press came
// with no warning ever seen, which is the entire failure the guard exists to
// prevent.
func TestQuitWarningIsActuallyVisible(t *testing.T) {
	m := sampleModel(90, 22)
	m.running = "正在启动 cloudflared 并等待连接注册…"

	m, _ = press(t, m, key("q"))
	if !strings.Contains(m.View(), "再按一次") {
		t.Errorf("拒绝退出的警告未显示在界面上:\n%s", m.View())
	}
}
