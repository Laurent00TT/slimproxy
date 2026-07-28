package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/momo/slimproxy/credentials"
	"github.com/momo/slimproxy/diag"
	"github.com/momo/slimproxy/metrics"
	"github.com/momo/slimproxy/tunnel"
)

// fixedNow anchors every time-dependent string, so a test that renders a
// duration is not comparing against the wall clock.
var fixedNow = time.Date(2026, 7, 26, 12, 41, 7, 0, time.UTC)

// sampleModel builds a dashboard holding the state a healthy deployment
// produces, at a size a real terminal has.
func sampleModel(w, h int) Model {
	m := New(context.Background(), Deps{
		Listen:  "127.0.0.1:8317",
		Version: "0.1.0",
		AuthDir: `C:\Users\x\slimproxy\auths`,
	})
	m.width, m.height, m.sized = w, h, true
	m.now = fixedNow

	m.stats = metrics.Snapshot{
		RPM:    17,
		TTFT:   842 * time.Millisecond,
		Uptime: 4*time.Hour + 12*time.Minute,
		Total:  60,
		Active: []metrics.RouteAgg{
			{Route: "openai → claude", Count: 41, AvgTTFT: 812 * time.Millisecond, Pct: 68},
			{Route: "claude → claude", Count: 17, AvgTTFT: 903 * time.Millisecond, Pct: 28},
			{Route: "gemini → claude", Count: 2, AvgTTFT: 1204 * time.Millisecond, Pct: 4},
		},
		Recent: []metrics.Sample{
			{At: fixedNow, Account: "openai@acct", Target: "claude", Model: "claude-opus-5", TTFT: 812 * time.Millisecond, Tokens: 1900},
			{At: fixedNow.Add(-5 * time.Second), Account: "claude@acct", Target: "claude", Model: "claude-opus-5", TTFT: 903 * time.Millisecond, Tokens: 2100},
			{At: fixedNow.Add(-12 * time.Second), Account: "openai@acct", Target: "claude", Model: "claude-opus-5", TTFT: 21 * time.Second, Failed: true},
			{At: fixedNow.Add(-31 * time.Second), Account: "openai@acct", Target: "claude", Model: "claude-sonnet-5", TTFT: 512 * time.Millisecond, Tokens: 900},
		},
	}

	m.tunnel = tunnelView{
		known: true,
		at:    fixedNow,
		st: tunnel.Status{
			State:            tunnel.Running,
			Managed:          true,
			PID:              4242,
			Hostnames:        []string{"proxy.example.com"},
			TunnelID:         "b7c1e2f0-1111-2222-3333-444455556666",
			Connections:      2,
			ConnectionsKnown: true,
		},
		hostname:  "proxy.example.com",
		conns:     2,
		connKnown: true,
	}

	m.creds = credsView{
		known:       true,
		at:          fixedNow,
		usable:      1,
		recoverable: 1,
		soonest:     fixedNow.Add(7*time.Hour + 24*time.Minute),
		creds: []credentials.Credential{{
			Name:     "claude-user@example.com.json",
			Provider: "claude",
			Account:  "user@example.com",
			Expires:  fixedNow.Add(7*time.Hour + 24*time.Minute),
		}},
	}
	return m
}

// TestRenderPreview prints frames rather than asserting on them.
//
// Not a check -- the assertions live in the tests below. This exists because
// alignment in a boxed, CJK-labelled layout is not something a width assertion
// proves is *right*, only that it is consistent. Run with -v to look at it.
func TestRenderPreview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() Model
	}{
		{"resting", func() Model { return sampleModel(90, 26) }},
		{"narrow", func() Model { return sampleModel(60, 24) }},
		{"command mode", func() Model {
			m := sampleModel(90, 26)
			m.entry.active = true
			m.entry.text = "/tunnel"
			m.refreshHits()
			return m
		}},
		{"confirming", func() Model {
			m := sampleModel(90, 26)
			m.entry.confirming = m.cmds[2] // tunnel down
			return m
		}},
		{"doctor output", func() Model {
			m := sampleModel(90, 26)
			m.lastDoctor = &doctorSummary{level: diag.Warn, summary: "PASS 4 · WARN 1", at: fixedNow}
			m.result = &actionResult{
				Title: "诊断报告（PASS 4 · WARN 1，耗时 6.2s）",
				Lines: []string{
					"PASS     listen-port      127.0.0.1:8317 已被 slimproxy 占用",
					"PASS     credentials      1 个凭据，均可用",
					"WARN     upstream-dns     api.anthropic.com 解析到 198.18.0.7（fake-ip 段）",
					"         → 让该域名绕过本地代理，或在 hosts 中固定真实地址",
					"PASS     tunnel           proxy.example.com，2 个连接",
				},
			}
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("\n" + tc.build().View())
		})
	}
}

// TestEveryRowIsExactlyPanelWidth is the alignment check.
//
// A boxed layout fails in exactly one way: some row is a cell wider or
// narrower than the others and the right border walks. That is invisible in a
// unit test asserting on content and glaring on screen, so it is asserted
// directly -- on rendered output, measured the way a terminal measures it.
//
// CJK is what makes this worth pinning. Every label here is double-width, so
// any place that reached for len() instead of a display-width function shows up
// as a border two cells off per Chinese character.
func TestEveryRowIsExactlyPanelWidth(t *testing.T) {
	for _, size := range [][2]int{{90, 26}, {60, 24}, {56, 16}, {120, 40}, {100, 30}} {
		w, h := size[0], size[1]
		t.Run(sizeName(w, h), func(t *testing.T) {
			m := sampleModel(w, h)
			panelW := w
			if panelW > 100 {
				panelW = 100
			}

			frame := m.renderPanel(panelW-2-hpad*2, h-1)
			for i, line := range strings.Split(frame, "\n") {
				if got := lipgloss.Width(line); got != panelW {
					t.Errorf("第 %d 行宽度 %d，应为 %d\n%q", i+1, got, panelW, line)
				}
			}
		})
	}
}

// TestPanelNeverExceedsTerminalHeight pins the other direction: a frame taller
// than the terminal scrolls, which on the alternate screen means the top of
// the panel -- the header, the status rows -- silently walks off the top.
func TestPanelNeverExceedsTerminalHeight(t *testing.T) {
	for _, size := range [][2]int{{90, 26}, {60, 24}, {56, 16}, {120, 40}} {
		w, h := size[0], size[1]
		t.Run(sizeName(w, h), func(t *testing.T) {
			for _, mode := range []struct {
				name  string
				build func() Model
			}{
				{"resting", func() Model { return sampleModel(w, h) }},
				{"command mode", func() Model {
					m := sampleModel(w, h)
					m.entry.active = true
					m.entry.text = ""
					m.refreshHits()
					return m
				}},
				{"result panel", func() Model {
					m := sampleModel(w, h)
					m.result = &actionResult{Title: "输出", Lines: longLines(80)}
					return m
				}},
			} {
				t.Run(mode.name, func(t *testing.T) {
					got := lipgloss.Height(mode.build().View())
					if got > h {
						t.Errorf("渲染出 %d 行，终端只有 %d 行", got, h)
					}
				})
			}
		})
	}
}

func longLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "第 " + string(rune('0'+i%10)) + " 行：一些足够长的中文内容用来测试截断与滚动行为"
	}
	return out
}

func sizeName(w, h int) string {
	return string(rune('0'+w/100%10)) + string(rune('0'+w/10%10)) + string(rune('0'+w%10)) +
		"x" + string(rune('0'+h/10%10)) + string(rune('0'+h%10))
}

// TestTooSmallTerminalSaysSo pins the degradation. Rendering a boxed layout
// into 40 columns produces something unreadable; saying the window is too
// small is the honest outcome.
func TestTooSmallTerminalSaysSo(t *testing.T) {
	m := sampleModel(40, 20)
	out := m.View()
	if !strings.Contains(out, "过小") {
		t.Errorf("窄终端应给出提示，实际输出:\n%s", out)
	}
	if strings.Contains(out, boxTL) {
		t.Error("窄终端不应该尝试绘制面板边框")
	}
}

// TestUnsizedRendersNothing pins that the first frame waits for the terminal
// dimensions. Guessing produces one visibly wrong frame at every startup.
func TestUnsizedRendersNothing(t *testing.T) {
	m := New(context.Background(), Deps{})
	if out := m.View(); out != "" {
		t.Errorf("尺寸未知时应渲染空，实际:\n%q", out)
	}
}
