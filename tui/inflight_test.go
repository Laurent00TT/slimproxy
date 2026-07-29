package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// TestFrameDeltaStaysSmall is the flicker guard, stated as something a test can
// check.
//
// bubbletea repaints only the lines that differ from the previous frame, so
// "does the dashboard flicker" reduces to two measurable properties: the frame
// must not change height between ticks, and only a handful of lines may differ.
// A frame that grows or shrinks forces a repaint of everything below the change,
// which at one frame per second is exactly what a terminal renders as flicker.
//
// The in-flight row is what makes this worth pinning: it is the first element
// on this panel that changes on every single tick by design.
func TestFrameDeltaStaysSmall(t *testing.T) {
	build := func(offset time.Duration) string {
		m := sampleModel(90, 22)
		m.now = fixedNow.Add(offset)
		m.stats.Pending = []metrics.Pending{
			{At: fixedNow.Add(-12 * time.Second), Path: "/v1/messages"},
		}
		return m.View()
	}

	a := strings.Split(build(0), "\n")
	b := strings.Split(build(time.Second), "\n")

	if len(a) != len(b) {
		t.Fatalf("相隔一秒的两帧高度不同（%d vs %d）。变高变矮会强制重绘下方全部内容，"+
			"这正是终端上看得见的闪烁", len(a), len(b))
	}

	var changed []int
	for i := range a {
		if a[i] != b[i] {
			changed = append(changed, i)
		}
	}
	// Four leaves room for the clock-driven rows that legitimately tick (the
	// credential countdown, the in-flight elapsed time) without leaving room for
	// a redesign that repaints the panel.
	if len(changed) > 4 {
		t.Errorf("一秒内有 %d 行发生变化（行号 %v），超过 4 行就不再是局部重绘了",
			len(changed), changed)
	}
	if len(changed) == 0 {
		t.Error("一秒过去没有任何一行变化——进行中的耗时应该在走")
	}
}

// TestPendingRowLeadsTheStream pins where the row goes and what it says.
//
// Above the completed requests, because when the upstream answers this row is
// replaced by a sampleRow in the same position: matching columns and a shared
// place make that read as one request changing state, not as one row vanishing
// while an unrelated one appears.
func TestPendingRowLeadsTheStream(t *testing.T) {
	m := sampleModel(90, 22)
	m.stats.Pending = []metrics.Pending{
		{At: fixedNow.Add(-12 * time.Second), Path: "/v1/messages"},
	}

	lines := strings.Split(m.View(), "\n")
	var pendingAt, firstDoneAt = -1, -1
	for i, l := range lines {
		if pendingAt < 0 && strings.Contains(l, pendingMark) && strings.Contains(l, "/v1/messages") {
			pendingAt = i
		}
		if firstDoneAt < 0 && strings.Contains(l, " ok  ") {
			firstDoneAt = i
		}
	}
	if pendingAt < 0 {
		t.Fatalf("没有渲染进行中的行:\n%s", m.View())
	}
	if firstDoneAt >= 0 && pendingAt > firstDoneAt {
		t.Errorf("进行中的行在第 %d 行，已完成的在第 %d 行——进行中的应该在上面", pendingAt, firstDoneAt)
	}
	if !strings.Contains(lines[pendingAt], "12.0s") {
		t.Errorf("进行中的行没有显示已耗时:\n%s", lines[pendingAt])
	}
	// The count in the section label is what survives when the panel is too
	// short to keep the rows themselves.
	if !strings.Contains(m.View(), "1 进行中") {
		t.Error("分区标题没有显示进行中的数量")
	}
}

// TestNoEmptyClaimWhileSomethingRuns covers a contradiction the panel could
// otherwise print: "还没有完成的请求" sitting directly under a row proving a
// request is on its way. Both statements are true, and together they read as a
// bug.
func TestNoEmptyClaimWhileSomethingRuns(t *testing.T) {
	m := sampleModel(90, 22)
	m.stats.Recent = nil
	m.stats.Active = nil
	m.stats.Pending = []metrics.Pending{
		{At: fixedNow.Add(-3 * time.Second), Path: "/v1/messages"},
	}

	out := m.View()
	if strings.Contains(out, "还没有完成的请求") {
		t.Errorf("有请求进行中时不该说「还没有完成的请求」:\n%s", out)
	}
	if !strings.Contains(out, pendingMark) {
		t.Errorf("进行中的行没渲染出来:\n%s", out)
	}
}

// TestHostilePathCannotBreakTheFrame pins the sanitize line for the newest
// externally-controlled field on the panel.
//
// Pending.Path is whatever the client sent, and this proxy hangs off a public
// tunnel that is being scanned as a matter of routine. A path carrying an
// escape sequence or a newline reaches the renderer verbatim -- the defence is
// renderSegs sanitising at the single point every row passes through, and this
// test exists so that a future pendingRow "optimisation" bypassing renderSegs
// fails here instead of handing scanners a way to scramble the operator's
// terminal.
func TestHostilePathCannotBreakTheFrame(t *testing.T) {
	m := sampleModel(90, 22)
	m.stats.Pending = []metrics.Pending{
		{At: fixedNow.Add(-3 * time.Second), Path: "/v1/\x1b[2J\x07evil\npath"},
	}

	out := m.View()
	for _, forbidden := range []string{"\x1b[2J", "\x07"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("控制序列原样进了帧: %q", forbidden)
		}
	}
	// The frame must also keep its shape: an injected newline that survived
	// would add a row and shear every border below it. lipgloss.Width is what
	// the panel's own width tests measure with -- it ignores the colour codes
	// styling adds.
	for i, line := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(line); lw > 90 {
			t.Errorf("第 %d 行宽 %d，超出面板", i, lw)
		}
	}
}

// TestPendingTruncationKeepsTheOldest pins which rows survive a burst.
//
// The longest-running request is the one closest to being stuck, and therefore
// the one worth a row. Keeping the newest instead would drop it precisely when
// it started to matter.
func TestPendingTruncationKeepsTheOldest(t *testing.T) {
	m := sampleModel(90, 22)
	// oldest first, as InFlight.Snapshot returns them
	for _, age := range []int{35, 28, 21, 14, 7} {
		m.stats.Pending = append(m.stats.Pending, metrics.Pending{
			At:   fixedNow.Add(-time.Duration(age) * time.Second),
			Path: "/v1/messages",
		})
	}

	out := m.View()
	for _, want := range []string{"35.0s", "28.0s", "21.0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("最久的三个之一 %s 没有显示:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"14.0s", "7.0s"} {
		if strings.Contains(out, notWant) {
			t.Errorf("超出上限的 %s 不该占行", notWant)
		}
	}
	if !strings.Contains(out, "另有 2 个进行中") {
		t.Error("被截断的数量没有说明——静默丢弃会让人以为只有三个在跑")
	}
}
