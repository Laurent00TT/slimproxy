package tui

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"

	"github.com/Laurent00TT/slimproxy/i18n"
	"github.com/Laurent00TT/slimproxy/metrics"
)

// TestEnglishFrameHasNoChineseLeft is the smoke test for the whole conversion:
// switch to English, render the busiest frame this dashboard has, and demand
// that not one Han character survives.
//
// The lint test proves every literal is wrapped; this proves the wrapping is
// actually consulted on the render path -- a var that froze its text at init
// passes the lint and fails here.
func TestEnglishFrameHasNoChineseLeft(t *testing.T) {
	i18n.Set(i18n.En)
	t.Cleanup(func() { i18n.Set(i18n.Zh) })

	m := sampleModel(96, 24)
	// Load every section: metrics, pending, failures with causes, and a note.
	m.stats.Quota5h, m.stats.QuotaKnown = 0.21, true
	m.stats.QuotaTrend = 1
	m.stats.CacheRatio, m.stats.CacheKnown = 0.98, true
	m.stats.Pending = []metrics.Pending{
		{At: fixedNow.Add(-12 * time.Second), Path: "/v1/messages"},
	}
	m.stats.Recent = append(m.stats.Recent, metrics.Sample{
		At: fixedNow.Add(-40 * time.Second), Account: "you@example.com",
		Target: "claude", Model: "claude-opus-5", Failed: true, Cause: metrics.CauseTimeout,
	})
	m.setNote("note", false)

	out := m.View()
	for i, line := range strings.Split(out, "\n") {
		for _, r := range line {
			if unicode.Is(unicode.Han, r) {
				t.Fatalf("英文模式下第 %d 行仍有中文:\n%s", i, line)
			}
		}
	}
}

// TestEnglishFrameKeepsItsShape: the English strings are longer, and the frame
// arithmetic is all display-cell based. Every line must still be exactly the
// panel's width -- an overflowing label would push the right border out on one
// row and shear the box.
func TestEnglishFrameKeepsItsShape(t *testing.T) {
	i18n.Set(i18n.En)
	t.Cleanup(func() { i18n.Set(i18n.Zh) })

	for _, size := range []struct{ w, h int }{{96, 24}, {60, 18}, {56, 16}} {
		m := sampleModel(size.w, size.h)
		m.stats.Quota5h, m.stats.QuotaKnown = 0.87, true
		m.stats.CacheRatio, m.stats.CacheKnown = 0.04, true
		m.stats.Pending = []metrics.Pending{
			{At: fixedNow.Add(-8 * time.Second), Path: "/v1/messages"},
		}

		lines := strings.Split(m.View(), "\n")
		panelW := lipgloss.Width(lines[0])
		for i, line := range lines[:len(lines)-1] { // last line is the help bar
			if w := lipgloss.Width(line); w != panelW && i < len(lines)-1 {
				t.Errorf("%dx%d: 第 %d 行宽 %d，首行宽 %d——英文文案把布局撑破了:\n%s",
					size.w, size.h, i, w, panelW, line)
			}
		}
	}
}

// TestCommandListSpeaksBothLanguages pins the registry pattern: commandSet is
// built per Model, after the language is set, so an English session must get
// English summaries -- and switching back must get Chinese ones again.
func TestCommandListSpeaksBothLanguages(t *testing.T) {
	i18n.Set(i18n.En)
	t.Cleanup(func() { i18n.Set(i18n.Zh) })
	for _, c := range commandSet() {
		for _, r := range c.Summary {
			if unicode.Is(unicode.Han, r) {
				t.Fatalf("英文模式下命令 %s 的摘要仍是中文: %s", c.Name, c.Summary)
			}
		}
	}

	i18n.Set(i18n.Zh)
	var hasHan bool
	for _, c := range commandSet() {
		for _, r := range c.Summary {
			if unicode.Is(unicode.Han, r) {
				hasHan = true
			}
		}
	}
	if !hasHan {
		t.Error("切回中文后命令摘要里没有任何中文——语言切换只生效了一个方向")
	}
}
