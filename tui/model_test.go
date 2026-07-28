package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/momo/slimproxy/credentials"
	"github.com/momo/slimproxy/tunnel"
)

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func special(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

// press feeds one key and returns the resulting model, so a test reads as a
// sequence of keystrokes rather than a chain of type assertions.
func press(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(k)
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update 返回了 %T，不是 Model", next)
	}
	return got, cmd
}

// TestDangerousCommandsAskFirst pins the confirmation gate.
//
// These two commands stop a public endpoint and delete a credential. Enter on
// either must not be the last keystroke before it happens.
//
// quit is deliberately not in this list -- see TestQuitBehavesTheSameBothWays
// for why, and for the guard that replaces the prompt there.
func TestDangerousCommandsAskFirst(t *testing.T) {
	for _, name := range []string{"tunnel down", "auth rm"} {
		t.Run(name, func(t *testing.T) {
			var ran bool
			m := sampleModel(90, 30)
			m.deps.TunnelDown = func(context.Context) (tunnel.Status, error) {
				ran = true
				return tunnel.Status{}, nil
			}
			m.deps.RemoveCred = func(context.Context, string, bool) (credentials.Credential, error) {
				ran = true
				return credentials.Credential{}, nil
			}

			m, _ = press(t, m, key("/"))
			for _, r := range name + " x" { // trailing arg keeps `auth rm` valid
				m, _ = press(t, m, key(string(r)))
			}
			m, cmd := press(t, m, special(tea.KeyEnter))

			if m.entry.confirming == nil {
				t.Fatalf("%s 按 Enter 后应进入确认态", name)
			}
			if m.quitting {
				t.Errorf("%s 按 Enter 不应直接退出", name)
			}
			if cmd != nil {
				if _, isDone := cmd().(actionDoneMsg); isDone {
					t.Errorf("%s 按 Enter 不应直接执行", name)
				}
			}
			if ran {
				t.Errorf("%s 在确认前就执行了", name)
			}
		})
	}
}

// TestConfirmRejectsStrayKeys pins that only y confirms.
//
// A confirmation that treats any keypress as yes is not a confirmation, and
// the keys most likely to arrive by accident here -- Enter from the keystroke
// that opened the prompt, a space, an arrow -- are exactly the ones that must
// not act.
func TestConfirmRejectsStrayKeys(t *testing.T) {
	for _, k := range []tea.KeyMsg{
		special(tea.KeyEnter), special(tea.KeySpace), special(tea.KeyDown),
		key("Y "), key("a"), key("1"),
	} {
		t.Run(k.String(), func(t *testing.T) {
			var ran bool
			m := sampleModel(90, 30)
			m.deps.TunnelDown = func(context.Context) (tunnel.Status, error) {
				ran = true
				return tunnel.Status{}, nil
			}
			m.entry.confirming = findCommand(t, m, "tunnel down")

			m, cmd := press(t, m, k)
			if cmd != nil {
				cmd()
			}
			if ran {
				t.Errorf("按键 %q 不应触发执行", k.String())
			}
			if m.running != "" {
				t.Errorf("按键 %q 不应启动动作，running=%q", k.String(), m.running)
			}
		})
	}
}

// TestConfirmYesRuns is the other half: y must actually act, or the gate is a
// wall.
func TestConfirmYesRuns(t *testing.T) {
	var ran bool
	m := sampleModel(90, 30)
	m.deps.TunnelDown = func(context.Context) (tunnel.Status, error) {
		ran = true
		return tunnel.Status{Detail: ""}, nil
	}
	m.entry.confirming = findCommand(t, m, "tunnel down")

	m, cmd := press(t, m, key("y"))
	if cmd == nil {
		t.Fatal("y 应返回一个执行命令")
	}
	cmd()
	if !ran {
		t.Error("y 未触发执行")
	}
	if m.entry.confirming != nil {
		t.Error("执行后应退出确认态")
	}
}

// TestEscapeClosesPanelWithoutQuitting.
//
// Esc dismisses the output panel and Esc leaves command mode, so it is the key
// most likely to be pressed reflexively. Wiring it to quit would stop a proxy
// serving live traffic on a keystroke meaning "close this".
func TestEscapeClosesPanelWithoutQuitting(t *testing.T) {
	m := sampleModel(90, 30)
	m.result = &actionResult{Title: "输出", Lines: []string{"a", "b"}}

	m, _ = press(t, m, special(tea.KeyEsc))
	if m.quitting {
		t.Error("Esc 不应退出程序")
	}
	if m.result != nil {
		t.Error("Esc 应关闭输出面板")
	}

	// A second Esc, with no panel open, must still not quit.
	m, _ = press(t, m, special(tea.KeyEsc))
	if m.quitting {
		t.Error("面板已关闭时 Esc 仍不应退出程序")
	}
}

// TestTypingInCommandModeDoesNotQuit pins that q is a character once the
// prompt is open. Typing "/quit" would otherwise quit on its first letter.
func TestTypingInCommandModeDoesNotQuit(t *testing.T) {
	m := sampleModel(90, 30)
	m, _ = press(t, m, key("/"))
	for _, r := range "quit" {
		m, _ = press(t, m, key(string(r)))
		if m.quitting {
			t.Fatalf("输入 %q 时退出了程序", r)
		}
	}
	if m.entry.text != "/quit" {
		t.Errorf("输入内容 = %q，应为 %q", m.entry.text, "/quit")
	}
}

// TestSecondCommandRefusedWhileRunning.
//
// Two concurrent `tunnel up` calls race for the same pid record; the loser
// spawns a child nothing will ever be able to stop. The interface refuses
// instead.
func TestSecondCommandRefusedWhileRunning(t *testing.T) {
	m := sampleModel(90, 30)
	m.running = "正在启动 cloudflared…"

	var ran bool
	m.deps.Doctor = nil // would be a different failure
	m.deps.TunnelStatus = func(context.Context) tunnel.Status {
		ran = true
		return tunnel.Status{}
	}

	m, _ = press(t, m, key("/"))
	for _, r := range "tunnel status" {
		m, _ = press(t, m, key(string(r)))
	}
	m, cmd := press(t, m, special(tea.KeyEnter))
	if cmd != nil {
		cmd()
	}
	if ran {
		t.Error("已有动作在执行时不应启动第二个")
	}
	if m.note == "" {
		t.Error("应告知操作者为何没有执行")
	}
}

// TestTunnelPollsDoNotOverlap.
//
// The tunnel probe can take 20 seconds. Stacking a second one on each tick
// would multiply load on a link already too slow to answer -- and the tick
// fires whether or not the previous answer arrived.
func TestTunnelPollsDoNotOverlap(t *testing.T) {
	m := sampleModel(90, 30)
	m.tunnel.inFlight = true

	var probed bool
	m.deps.TunnelStatus = func(context.Context) tunnel.Status {
		probed = true
		return tunnel.Status{}
	}

	next, cmd := m.Update(tickMsg{source: srcTunnel})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("tick 必须重新排期，否则隧道轮询就此停止")
	}

	// The command is run on a goroutine with a deadline rather than called
	// directly. Both outcomes are commands: the correct one is a re-armed
	// tea.Tick, which sleeps for the full 45s poll interval before producing
	// anything -- calling it inline would make this test take 45 seconds. The
	// wrong one is a fetch, which returns immediately because the probe here
	// is a stub. So "produced nothing quickly" is the assertion.
	produced := make(chan tea.Msg, 1)
	go func() { produced <- cmd() }()
	select {
	case msg := <-produced:
		t.Errorf("上一次查询未返回时不应立即发起新查询，收到 %T", msg)
	case <-time.After(250 * time.Millisecond):
	}
	if probed {
		t.Error("上一次查询未返回时不应发起新查询")
	}

	// The answer landing must clear the flag, or the dashboard never polls the
	// tunnel again.
	next, _ = m.Update(tunnelMsg{st: tunnel.Status{}, at: fixedNow})
	if next.(Model).tunnel.inFlight {
		t.Error("查询返回后应清除 inFlight，否则之后永不再查")
	}
}

// TestExpiredButRefreshableIsNotAnEmptyPool.
//
// The proxy renews expired access tokens on its own, so a pool of one expired
// credential is a working deployment. Counting only usable ones reports it as
// "无可用凭据 · 每个请求都会失败" -- an alarm about a proxy that is fine.
func TestExpiredButRefreshableIsNotAnEmptyPool(t *testing.T) {
	now := fixedNow
	v := summariseCreds(credsMsg{
		at: now,
		creds: []credentials.Credential{{
			Name: "claude.json", Provider: "claude", Expires: now.Add(-time.Hour),
		}},
	}, now)

	if v.usable != 0 {
		t.Errorf("usable = %d，已过期的凭据此刻不能服务请求", v.usable)
	}
	if v.recoverable != 1 {
		t.Errorf("recoverable = %d，应为 1：代理会加载它并刷新", v.recoverable)
	}

	_, _, detail := credSymbol(v, now)
	if strings.Contains(detail, "无可用凭据") {
		t.Errorf("不应报告为空池: %q", detail)
	}
}

// TestGenuinelyEmptyPoolIsReported is the other direction: a pool the proxy
// cannot serve from must say so, because the proxy will start, accept requests
// and fail every one -- healthy from the outside.
func TestGenuinelyEmptyPoolIsReported(t *testing.T) {
	now := fixedNow
	for _, tc := range []struct {
		name string
		cred credentials.Credential
	}{
		{"broken", credentials.Credential{Name: "x.json", Err: errors.New("不是合法 JSON")}},
		{"disabled", credentials.Credential{Name: "x.json", Disabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := summariseCreds(credsMsg{at: now, creds: []credentials.Credential{tc.cred}}, now)
			if v.recoverable != 0 {
				t.Fatalf("recoverable = %d，应为 0", v.recoverable)
			}
			sym, _, detail := credSymbol(v, now)
			if sym != symBad {
				t.Errorf("符号 = %q，应为告警符号", sym)
			}
			if !strings.Contains(detail, "每个请求都会失败") {
				t.Errorf("说明 = %q，应指出请求会全部失败", detail)
			}
		})
	}
}

// TestRunningTunnelWithZeroConnectionsIsAWarning.
//
// A cloudflared process with no edge connection looks alive in a task list
// while the public hostname returns 502. Showing it as healthy points the
// operator away from the one thing that is wrong.
func TestRunningTunnelWithZeroConnectionsIsAWarning(t *testing.T) {
	v := tunnelView{
		known:     true,
		st:        tunnel.Status{State: tunnel.Running, Managed: true},
		connKnown: true,
		conns:     0,
	}
	sym, _, detail := tunnelSymbol(v)
	if sym != symBad {
		t.Errorf("符号 = %q，0 连接应告警", sym)
	}
	if !strings.Contains(detail, "0 连接") {
		t.Errorf("说明 = %q，应点明连接数为 0", detail)
	}
}

// TestUnknownConnectionCountIsNotZero.
//
// "could not ask" and "asked, and the answer is none" are different states
// with different remedies. Rendering the first as the second sends the
// operator to restart a tunnel that may be serving traffic.
func TestUnknownConnectionCountIsNotZero(t *testing.T) {
	v := tunnelView{
		known:     true,
		st:        tunnel.Status{State: tunnel.Running, Managed: true},
		connKnown: false,
	}
	sym, _, detail := tunnelSymbol(v)
	if sym == symBad {
		t.Errorf("连接数未知不应等同于 0 连接的告警: %q", detail)
	}
	if strings.Contains(detail, "0 连接") {
		t.Errorf("说明 = %q，未确认的连接数不应显示为 0", detail)
	}
}

// ---------- completion ----------

func TestMatch(t *testing.T) {
	cmds := commandSet()
	for _, tc := range []struct {
		input    string
		wantOne  string   // when exactly one command should match
		wantMany []string // when a family should match, in order
		wantArgs []string
	}{
		{input: "/tunnel", wantMany: []string{"tunnel status", "tunnel up", "tunnel down"}},
		{input: "/tunnel d", wantOne: "tunnel down"},
		{input: "/doc", wantOne: "doctor"},
		{input: "/auth rm claude.json", wantOne: "auth rm", wantArgs: []string{"claude.json"}},
		{input: "/auth rm  claude.json ", wantOne: "auth rm", wantArgs: []string{"claude.json"}},
		{input: "/quit", wantOne: "quit"},
		// Substring fallback: nothing starts with "own", but tunnel down
		// contains it.
		{input: "/own", wantOne: "tunnel down"},
		{input: "/zzz"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			hits, args := match(cmds, tc.input)

			switch {
			case tc.wantOne != "":
				if len(hits) != 1 || hits[0].Name != tc.wantOne {
					t.Fatalf("匹配到 %v，应为 [%s]", names(hits), tc.wantOne)
				}
			case len(tc.wantMany) > 0:
				if len(hits) != len(tc.wantMany) {
					t.Fatalf("匹配到 %v，应为 %v", names(hits), tc.wantMany)
				}
				for i, want := range tc.wantMany {
					if hits[i].Name != want {
						t.Errorf("第 %d 项 = %q，应为 %q", i, hits[i].Name, want)
					}
				}
			default:
				if len(hits) != 0 {
					t.Fatalf("匹配到 %v，应为空", names(hits))
				}
			}

			if len(args) != len(tc.wantArgs) {
				t.Fatalf("参数 = %v，应为 %v", args, tc.wantArgs)
			}
			for i, want := range tc.wantArgs {
				if args[i] != want {
					t.Errorf("参数 %d = %q，应为 %q", i, args[i], want)
				}
			}
		})
	}
}

// TestEmptyInputOffersEverything pins that pressing / alone is a menu.
func TestEmptyInputOffersEverything(t *testing.T) {
	cmds := commandSet()
	hits, _ := match(cmds, "/")
	if len(hits) != len(cmds) {
		t.Errorf("空输入匹配 %d 条，应为全部 %d 条", len(hits), len(cmds))
	}
}

// ---------- argument validation ----------

// TestAuthRemoveRequiresExactlyOneArgument.
//
// Zero arguments must not delete anything, and two must not delete the first:
// this is the command that removes the file the proxy authenticates with.
func TestAuthRemoveRequiresExactlyOneArgument(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"a", "b"}} {
		var called bool
		m := sampleModel(90, 30)
		m.deps.RemoveCred = func(context.Context, string, bool) (credentials.Credential, error) {
			called = true
			return credentials.Credential{}, nil
		}
		spec := runAuthRemove(&m, args)
		if spec.Err == nil {
			t.Errorf("参数 %v 应被拒绝", args)
		}
		if spec.Do != nil {
			spec.Do(context.Background())
		}
		if called {
			t.Errorf("参数 %v 不应触发删除", args)
		}
	}
}

// TestAuthRemoveNeverForces.
//
// The dashboard has no way to type a flag, so the guard against deleting the
// last credential must never be bypassed from here -- the command that can is
// the CLI one, where the operator has to write -force by hand.
func TestAuthRemoveNeverForces(t *testing.T) {
	var gotForce bool
	m := sampleModel(90, 30)
	m.deps.RemoveCred = func(_ context.Context, _ string, force bool) (credentials.Credential, error) {
		gotForce = force
		return credentials.Credential{Name: "x.json"}, nil
	}
	spec := runAuthRemove(&m, []string{"x.json"})
	if spec.Do == nil {
		t.Fatal("合法参数应产生执行体")
	}
	spec.Do(context.Background())
	if gotForce {
		t.Error("面板不应以 force 删除凭据")
	}
}

// TestMissingDependencyIsReportedNotIgnored pins that a command wired to
// nothing says so, rather than appearing to succeed.
func TestMissingDependencyIsReportedNotIgnored(t *testing.T) {
	m := New(context.Background(), Deps{}) // every hook nil
	for _, c := range m.cmds {
		if c.Name == "quit" || c.Name == "auth add" {
			continue // answer without dependencies by design
		}
		t.Run(c.Name, func(t *testing.T) {
			spec := c.Run(&m, []string{"x"})
			if spec.Err == nil && spec.Do == nil && spec.Note == "" {
				t.Error("依赖缺失时既未报错也未产生任何输出")
			}
			if spec.Do != nil {
				t.Error("依赖缺失时不应产生执行体")
			}
		})
	}
}

// ---------- helpers ----------

func findCommand(t *testing.T, m Model, name string) *Command {
	t.Helper()
	for _, c := range m.cmds {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("找不到命令 %q", name)
	return nil
}

func names(cs []*Command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}
