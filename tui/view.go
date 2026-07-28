package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/momo/slimproxy/diag"
	"github.com/momo/slimproxy/metrics"
	"github.com/momo/slimproxy/tunnel"
)

// Layout constants.
const (
	// minWidth is the narrowest terminal this layout is honest in. Below it
	// the fully-boxed frame spends four columns on borders that the content
	// needs, so the dashboard says so instead of rendering an unreadable
	// version of itself.
	minWidth = 56
	// minHeight leaves room for the header block, one row of each section and
	// the prompt.
	minHeight = 16
	// hpad is the space between the vertical border and content.
	hpad = 1
	// maxRouteRows bounds the route table so a busy proxy does not starve the
	// request stream, which is the section that changes.
	maxRouteRows = 4
	// maxCompletions bounds the completion list.
	maxCompletions = 8
)

func (m Model) View() string {
	if !m.sized {
		// Nothing is known about the terminal yet. Drawing to a guessed width
		// produces one visibly wrong frame before the real size arrives.
		return ""
	}
	if m.quitting {
		return ""
	}
	if m.width < minWidth || m.height < minHeight {
		return sDimO.Render(fmt.Sprintf(
			"终端窗口过小（%d×%d），slimproxy 面板需要至少 %d×%d。\n"+
				"放大窗口，或按 q 退出后使用 slimproxy status / doctor。",
			m.width, m.height, minWidth, minHeight))
	}

	panelW := m.panelWidth()
	inner := panelW - 2 - hpad*2

	// The panel gets its rows first, and what is left over bounds the prompt.
	//
	// The direction matters. When the two together do not fit, something has
	// to give, and it is not the panel: the completion list is a layer the
	// operator just opened and can close, while the status rows are the reason
	// the dashboard exists. Letting the list win instead pushed the header off
	// the top of a 16-row terminal.
	maxBelow := m.height - panelMinRows
	if maxBelow < 1 {
		maxBelow = 1
	}

	below := m.renderBelowPanel(panelW, maxBelow)
	belowH := lipgloss.Height(below)
	if below == "" {
		belowH = 0
	}

	panel := m.renderPanel(inner, m.height-belowH)
	if below == "" {
		return panel
	}
	return panel + "\n" + below
}

// panelMinRows is the shortest honest panel: everything fixed, plus one body
// row.
//
// Below this the panel is no longer reporting anything -- it is a border. The
// prompt is capped so this always fits.
const panelMinRows = panelFixedRows + 1

// panelWidth is the frame plus its content, never wider than the terminal.
//
// Capped so a maximised window does not stretch a layout designed around
// readable line lengths across 200 cells of mostly whitespace.
func (m Model) panelWidth() int {
	if m.width > 100 {
		return 100
	}
	return m.width
}

// renderPanel draws the bordered dashboard to at most height rows.
func (m Model) renderPanel(inner, height int) string {
	var rows []string

	rows = append(rows, m.titleBar(inner))
	rows = append(rows, m.headerRow(inner))
	rows = append(rows, divider(inner, ""))
	rows = append(rows, m.tunnelRow(inner), m.credsRow(inner), m.doctorRow(inner))

	// Rows consumed so far, plus the closing border. Asserted against the
	// constant the paging arithmetic uses, so adding a status row here cannot
	// silently desynchronise scrolling.
	fixed := len(rows) + 1
	if fixed != panelFixedRows {
		panic("tui: panelFixedRows out of sync with renderPanel")
	}
	body := height - fixed
	if body < 1 {
		body = 1
	}

	if m.result != nil {
		rows = append(rows, m.resultSection(inner, body)...)
	} else {
		rows = append(rows, m.streamSections(inner, body)...)
	}

	rows = append(rows, sFrame.Render(boxBL+strings.Repeat(boxH, inner+hpad*2)+boxBR))
	return strings.Join(rows, "\n")
}

// ---------- frame pieces ----------

func (m Model) titleBar(inner int) string {
	title := " slimproxy "
	if m.deps.Version != "" {
		title = " slimproxy " + m.deps.Version + " "
	}
	total := inner + hpad*2
	left := boxH + title
	fill := total - width(left)
	if fill < 0 {
		left, fill = boxH, total-1
	}
	return sFrame.Render(boxTL + left + strings.Repeat(boxH, fill) + boxTR)
}

// divider draws a section rule, optionally labelled.
//
// Columns headers ride on the rule rather than occupying a row of their own:
// on a 24-row terminal a separate header line per section costs two of the
// rows the request stream is competing for.
func divider(inner int, label string) string {
	total := inner + hpad*2
	if label == "" {
		return sFrame.Render(boxTeeL + strings.Repeat(boxH, total) + boxTeeR)
	}
	// Section labels can carry command output (a result panel's title), so the
	// same rule as renderSegs applies.
	left := boxH + " " + sanitize(label) + " "
	fill := total - width(left)
	if fill < 0 {
		return sFrame.Render(boxTeeL + strings.Repeat(boxH, total) + boxTeeR)
	}
	return sFrame.Render(boxTeeL+left) + sFrame.Render(strings.Repeat(boxH, fill)+boxTeeR)
}

// row wraps already-styled content in the vertical borders.
//
// content must already be exactly inner cells wide as plain text; every helper
// below pads before styling for exactly this reason -- padding afterwards would
// have to measure through escape sequences.
func row(inner int, content string) string {
	pad := sText.Render(strings.Repeat(" ", hpad))
	return sFrame.Render(boxV) + pad + content + pad + sFrame.Render(boxV)
}

// blankRow fills a row with panel background, so the panel is a solid block
// rather than a frame with terminal-coloured gaps.
func blankRow(inner int) string {
	return row(inner, sText.Render(strings.Repeat(" ", inner)))
}

// ---------- header ----------

// headerRow is the always-visible line: where it listens, how long it has been
// up, and the two numbers worth watching.
func (m Model) headerRow(inner int) string {
	listen := m.deps.Listen
	if listen == "" {
		listen = "—"
	}
	// m.now, not time.Now: same rule as the status rows. Before the first
	// stats message arrives Uptime is zero, and reading the clock here made two
	// renders of one identical model disagree.
	uptime := shortDur(m.stats.Uptime)
	if m.stats.Uptime == 0 {
		uptime = shortDur(m.now.Sub(m.started))
	}

	ttft := "—"
	if m.stats.TTFT > 0 {
		ttft = shortDur(m.stats.TTFT)
	}

	// Built as label/value pairs so the whole line can be truncated from the
	// right without splitting a pair.
	segs := []segment{
		{sLabel, "监听 "}, {sText, listen},
		{sText, "  "}, {sLabel, "运行 "}, {sNum, uptime},
		{sText, "  "}, {sLabel, "rpm "}, {sNum, fmt.Sprint(m.stats.RPM)},
		{sText, "  "}, {sLabel, "ttft "}, {sNum, ttft},
	}
	return row(inner, renderSegs(segs, inner))
}

// ---------- status rows ----------

func (m Model) tunnelRow(inner int) string {
	label := segment{sLabel, pad("隧道", 6)}

	if !m.tunnel.known {
		return row(inner, renderSegs([]segment{label, {sLabel, "查询中…"}}, inner))
	}
	st := m.tunnel.st

	// Collapsed, not listed: subjectW is 26 columns and two domains do not
	// fit. The full list lives in the tunnel panel; here the count is enough
	// to show the extra domains exist at all -- which is what the top bar was
	// silently hiding when it printed only the first.
	host := "—"
	if hs := st.Hostnames; len(hs) > 0 {
		host = hs[0]
		if len(hs) > 1 {
			// The hostname is truncated before the count is appended, never
			// after. truncate cuts from the right, so a first hostname of 23+
			// cells would otherwise eat the marker -- and a multi-domain tunnel
			// would render identically to a truncated single-domain one,
			// hiding the extra domains all over again.
			suffix := fmt.Sprintf(" +%d", len(hs)-1)
			host = truncate(host, subjectW-1-width(suffix)) + suffix
		}
	}

	sym, symStyle, detail := tunnelSymbol(m.tunnel)

	segs := []segment{
		label,
		{sText, pad(truncate(host, subjectW-1), subjectW)},
		{symStyle, sym + " "},
		{sText, detail},
		{sLabel, staleness(m.tunnel.at, m.now, tunnelInterval)},
	}
	return row(inner, renderSegs(segs, inner))
}

// tunnelSymbol maps a tunnel reading onto a symbol and a phrase.
//
// The unknown cases are spelled out rather than folded into the negative one.
// "未确认" and "未运行" lead to different actions, and a dashboard that shows
// the second when it means the first sends the operator to restart something
// that may already be running.
func tunnelSymbol(v tunnelView) (string, lipgloss.Style, string) {
	st := v.st
	switch st.State {
	case tunnel.Running:
		switch {
		case v.connKnown && v.conns == 0:
			// A process that is up with no edge connection is the state that
			// looks healthy in a task list while the public hostname 502s.
			return symBad, sBad, "运行中但 0 连接"
		case v.connKnown && st.EdgeFakeIP != "":
			return symBad, sBad, fmt.Sprintf("%d 连接 · 边缘走 fake-ip", v.conns)
		case v.connKnown:
			owner := ""
			if !st.Managed {
				owner = " · 非本进程启动"
			}
			return symOK, sOK, fmt.Sprintf("%d 连接%s", v.conns, owner)
		case st.ConnectionsErr != nil:
			// The process is running but whether it is serving anyone could not
			// be established. Not green: an unreachable Cloudflare and a tunnel
			// with zero connections look identical from here, and one of those
			// means the public hostname is returning 502.
			return symNA, sLabel, "运行中 · 连接数查询失败"
		default:
			// Unknown, not healthy. doctorRow already renders diag.Unknown as a
			// warning; showing the same uncertainty as a green dot one row
			// above would make the panel disagree with itself.
			owner := ""
			if !st.Managed {
				owner = " · 非本进程启动"
			}
			return symNA, sLabel, "运行中 · 连接数未确认" + owner
		}
	case tunnel.Configured:
		return symNA, sLabel, "已配置，未运行"
	case tunnel.NotConfigured:
		return symNA, sLabel, "未配置"
	default:
		return symBad, sBad, "状态未确认"
	}
}

func (m Model) credsRow(inner int) string {
	label := segment{sLabel, pad("凭据", 6)}

	if !m.creds.known {
		return row(inner, renderSegs([]segment{label, {sLabel, "读取中…"}}, inner))
	}
	if m.creds.err != nil {
		return row(inner, renderSegs([]segment{
			label, {sBad, symBad + " "}, {sText, truncate(m.creds.err.Error(), inner-8)},
		}, inner))
	}

	// m.now, not time.Now: the frame must describe one instant. Reading the
	// clock here instead made "7h24m 后过期" render as whatever the gap
	// happened to be between the poll and the paint.
	who := credSubject(m.creds)
	sym, style, detail := credSymbol(m.creds, m.now)

	segs := []segment{
		label,
		// Truncated one cell short of the column so the status symbol is never
		// flush against an ellipsis.
		{sText, pad(truncate(who, subjectW-1), subjectW)},
		{style, sym + " "},
		{sText, detail},
		{sLabel, staleness(m.creds.at, m.now, credsInterval)},
	}
	return row(inner, renderSegs(segs, inner))
}

// subjectW is the width of the middle column in the three status rows: the
// hostname, the account, the diagnostic verdict.
const subjectW = 26

// staleness notes a reading's age, but only once it is old enough to mislead.
//
// Both status rows carry the instant they were observed and neither showed it,
// so a reading taken 44 seconds ago looked exactly like one taken now. Showing
// the age unconditionally would spend columns on a number that is almost always
// uninteresting; showing it past twice the poll interval means it appears
// precisely when polling has stopped working -- a wedged probe, a fetch that
// never returned -- which is the case where a confident-looking row is a lie.
func staleness(at, now time.Time, interval time.Duration) string {
	if at.IsZero() {
		return ""
	}
	age := now.Sub(at)
	if age < 2*interval {
		return ""
	}
	return " · " + shortDur(age) + "前"
}

// credSubject names the pool: the single account when there is one, a count
// otherwise.
func credSubject(v credsView) string {
	switch {
	case len(v.creds) == 0:
		return "—"
	case len(v.creds) == 1:
		c := v.creds[0]
		if c.Account != "" {
			return c.Provider + " · " + c.Account
		}
		return c.Name
	default:
		return fmt.Sprintf("%d 个凭据", len(v.creds))
	}
}

func credSymbol(v credsView, now time.Time) (string, lipgloss.Style, string) {
	broken := len(v.creds) - v.recoverable

	switch {
	case v.recoverable == 0:
		// The pool is empty in the only sense that matters: the proxy will
		// accept requests and fail every one of them.
		return symBad, sBad, "无可用凭据 · 每个请求都会失败"
	case v.usable == 0:
		return symBad, sBad, fmt.Sprintf("%d 个待刷新 · 当前无可服务凭据", v.recoverable)
	case !v.soonest.IsZero():
		// soonest is the nearest expiry among *usable* credentials, so this
		// duration is forward-looking by construction. It was not always: when
		// it ranged over everything recoverable, a credential that expired
		// three days ago set it to a past instant, and shortDur -- which takes
		// an absolute value -- rendered that as "3d00h 后过期". A pool with one
		// dead entry and one healthy one reported the dead one's age as the
		// healthy one's remaining life.
		d := v.soonest.Sub(now)
		sym, style := symOK, sOK
		if d < 30*time.Minute {
			sym, style = symBad, sBad
		}
		detail := shortDur(d) + " 后过期"
		if broken > 0 {
			sym, style = symBad, sBad
			detail += fmt.Sprintf(" · %d 个无法加载", broken)
		}
		return sym, style, detail
	default:
		detail := fmt.Sprintf("%d 可用 · 未记录过期时间", v.usable)
		if broken > 0 {
			return symBad, sBad, detail + fmt.Sprintf(" · %d 个无法加载", broken)
		}
		return symOK, sOK, detail
	}
}

// doctorRow reports the last diagnostic verdict.
//
// This row showed an upstream reachability probe in the design sketch. It does
// not here, because that probe costs a DNS lookup and a TLS handshake per
// refresh and nothing else on this dashboard needs them -- so the honest
// options were to invent the reading or to show one that is genuinely
// available. The last doctor verdict is the second: real, timestamped, and it
// says so when it has never run.
func (m Model) doctorRow(inner int) string {
	label := segment{sLabel, pad("诊断", 6)}

	if m.lastDoctor == nil {
		return row(inner, renderSegs([]segment{
			label,
			{sLabel, pad("未运行", subjectW)},
			{sLabel, symNA + " "},
			{sLabel, "输入 /doctor 检查部署"},
		}, inner))
	}
	d := m.lastDoctor
	sym, style := symNA, sLabel
	switch d.level {
	case diag.Pass:
		sym, style = symOK, sOK
	case diag.Warn, diag.Unknown, diag.Fail:
		sym, style = symBad, sBad
	}
	when := "刚刚"
	if age := m.now.Sub(d.at); age >= time.Second {
		when = shortDur(age) + "前"
	}
	return row(inner, renderSegs([]segment{
		label,
		{sText, pad(truncate(d.summary, subjectW-1), subjectW)},
		{style, sym + " "},
		{sLabel, when},
	}, inner))
}

// ---------- live sections ----------

// streamSections renders the route table and the request stream into body
// rows, giving the stream whatever the routes do not need.
func (m Model) streamSections(inner, body int) []string {
	var out []string

	routes := m.stats.Active
	if len(routes) > maxRouteRows {
		routes = routes[:maxRouteRows]
	}

	// Each section costs one row for its rule. Only show the route section
	// when there is both something to put in it and room for the rule plus a
	// line; a header over nothing wastes the row the stream wanted.
	routeRows := 0
	if len(routes) > 0 && body >= len(routes)+3 {
		routeRows = len(routes)
	}

	if routeRows > 0 {
		out = append(out, divider(inner, "活跃路由"))
		for _, r := range routes[:routeRows] {
			out = append(out, m.routeRow(inner, r))
		}
	}

	remaining := body - len(out)
	if remaining < 2 {
		// No room for a second section. Pad so the panel keeps its shape.
		for len(out) < body {
			out = append(out, blankRow(inner))
		}
		return out
	}

	out = append(out, divider(inner, "最近请求"))
	slots := body - len(out)

	recent := m.stats.Recent
	if len(recent) > slots {
		recent = recent[:slots]
	}
	for _, s := range recent {
		out = append(out, m.sampleRow(inner, s))
	}
	if len(recent) == 0 && slots > 0 {
		out = append(out, row(inner, renderSegs([]segment{
			{sLabel, "还没有完成的请求"},
		}, inner)))
	}
	for len(out) < body {
		out = append(out, blankRow(inner))
	}
	return out
}

// Column widths for the route table, from the right.
const (
	colCount = 6
	colAvg   = 9
	colPct   = 6
)

func (m Model) routeRow(inner int, r metrics.RouteAgg) string {
	nameW := inner - colCount - colAvg - colPct
	if nameW < 8 {
		return row(inner, renderSegs([]segment{{sText, truncate(r.Route, inner)}}, inner))
	}
	return row(inner, renderSegs([]segment{
		{sText, pad(r.Route, nameW)},
		{sNum, padLeft(fmt.Sprint(r.Count), colCount)},
		{sNum, padLeft(shortDur(r.AvgTTFT), colAvg)},
		{sLabel, padLeft(fmt.Sprintf("%d%%", r.Pct), colPct)},
	}, inner))
}

// Column widths for the request stream.
const (
	colTime   = 9 // "12:41:07 "
	colStatus = 4 // "200 "
	colTTFT   = 8
	colTokens = 7
)

func (m Model) sampleRow(inner int, s metrics.Sample) string {
	status, statusStyle := sampleStatus(s)

	fixed := colTime + colStatus + colTTFT + colTokens
	rest := inner - fixed
	if rest < 10 {
		// Too narrow for the full row: drop the token column first, since it
		// is the least diagnostic of the four.
		rest = inner - (colTime + colStatus + colTTFT)
		if rest < 6 {
			return row(inner, renderSegs([]segment{{sText, truncate(s.Route(), inner)}}, inner))
		}
		return row(inner, renderSegs([]segment{
			{sLabel, pad(s.At.Format("15:04:05"), colTime)},
			{statusStyle, pad(status, colStatus)},
			{sText, pad(s.Route(), rest)},
			{sNum, padLeft(ttftText(s), colTTFT)},
		}, inner))
	}

	// The route and model share the remaining width, model first to lose
	// characters: two models differ by a suffix, two routes by their whole
	// name.
	routeW := rest * 3 / 5
	modelW := rest - routeW

	return row(inner, renderSegs([]segment{
		{sLabel, pad(s.At.Format("15:04:05"), colTime)},
		{statusStyle, pad(status, colStatus)},
		{sText, pad(s.Route(), routeW)},
		{sLabel, pad(s.Model, modelW)},
		{sNum, padLeft(ttftText(s), colTTFT)},
		{sLabel, padLeft(tokenText(s.Tokens), colTokens)},
	}, inner))
}

// sampleStatus renders what is actually known about a request's outcome.
//
// Never "200". The usage record reports whether a request failed and, when it
// did, the upstream status -- it never reports the status of a success. Writing
// 200 there displayed a number the process had not observed, and 201 or 204
// would have been shown as 200 too.
//
// The failure side is the one that matters: 401 means replace the credential,
// 429 means wait, 500 means the upstream is broken. Collapsing all three into
// "err" hides the only distinction an operator can act on.
func sampleStatus(s metrics.Sample) (string, lipgloss.Style) {
	switch {
	case !s.Failed:
		return "ok", sOK
	case s.Status > 0:
		return fmt.Sprint(s.Status), sBad
	default:
		return "err", sBad
	}
}

// ttftText renders a sample's time to first token, distinguishing "not
// recorded" from "instant".
func ttftText(s metrics.Sample) string {
	if s.TTFT <= 0 {
		return "—"
	}
	return shortDur(s.TTFT)
}

func tokenText(n int64) string {
	switch {
	case n <= 0:
		return "—"
	case n < 1000:
		return fmt.Sprint(n)
	case n < 100_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%dk", n/1000)
	}
}

// ---------- result panel ----------

// resultSection replaces the live sections with a command's output.
//
// It takes over the lower half only: the header and the three status rows stay
// visible, so a doctor report is read against the state it describes rather
// than instead of it.
func (m Model) resultSection(inner, body int) []string {
	res := m.result
	title := res.Title
	if title == "" {
		title = "输出"
	}

	out := []string{divider(inner, truncate(title, inner-4))}

	lines := trimTrailingBlank(res.Lines)
	slots := body - len(out)
	if slots < 1 {
		return out
	}

	view := visibleResultRows(slots, len(lines))
	scrollable := view < len(lines)

	top := m.resultTop
	if top > len(lines)-view {
		top = max(0, len(lines)-view)
	}
	end := min(len(lines), top+view)

	style := sText
	if res.Err != nil {
		style = sBad
	}
	for _, l := range lines[top:end] {
		out = append(out, row(inner, renderSegs([]segment{{style, pad(truncate(l, inner), inner)}}, inner)))
	}

	if scrollable {
		hint := fmt.Sprintf("%d–%d / %d 行   ↑↓ 滚动 · Esc 关闭", top+1, end, len(lines))
		out = append(out, row(inner, renderSegs([]segment{{sLabel, hint}}, inner)))
	}
	for len(out) < body {
		out = append(out, blankRow(inner))
	}
	return out
}

// visibleResultRows is how many content lines the output panel actually shows,
// given the rows available to it and how much there is to display.
//
// The subtlety is the scroll indicator: when the content does not fit, the last
// row goes to "24–39 / 40 行" instead of content, so one fewer line is visible.
// resultSection and the paging arithmetic must agree on this exactly. They did
// not -- resultSection reserved the row and resultViewHeight did not -- so
// maxScroll was one too small and the page step one too large. End never
// reached the last line of a doctor report, and paging from the top skipped a
// line per screen with nothing to indicate it had.
func visibleResultRows(slots, total int) int {
	if slots < 1 {
		return 0
	}
	if total <= slots {
		return total
	}
	// Content exceeds the space, so the indicator row is needed.
	if slots == 1 {
		// Only room for the indicator; showing it alone is more honest than
		// showing one line and no way to know there are more.
		return 0
	}
	return slots - 1
}

// resultViewHeight is how many content lines are visible, for paging.
//
// Derived from the same function the renderer uses rather than recomputed, so
// the two cannot drift apart again.
func (m Model) resultViewHeight() int {
	slots := m.resultSlots()
	total := 0
	if m.result != nil {
		total = len(trimTrailingBlank(m.result.Lines))
	}
	if v := visibleResultRows(slots, total); v > 0 {
		return v
	}
	return 1
}

// resultSlots is the row budget resultSection receives, mirroring View's
// arithmetic.
func (m Model) resultSlots() int {
	body := m.height - m.belowHeight() - panelFixedRows
	// One row of the body goes to the section rule above the output.
	return body - 1
}

// panelFixedRows is what renderPanel spends before the body: title bar, header,
// divider, three status rows, and the closing border.
const panelFixedRows = 7

// belowHeight measures what sits under the panel.
//
// It renders rather than predicts. The prompt's height depends on the
// completion count, the selection, whether a consequence applies and how much
// room is left -- reimplementing that here is how the paging step and the
// actual layout drift apart, and the symptom would be a page-down that skips
// or repeats a line.
func (m Model) belowHeight() int {
	maxBelow := m.height - panelMinRows
	if maxBelow < 1 {
		maxBelow = 1
	}
	below := m.renderBelowPanel(m.panelWidth(), maxBelow)
	if below == "" {
		return 0
	}
	return lipgloss.Height(below)
}

// ---------- below the panel ----------

// renderBelowPanel draws the prompt and, in command mode, the completion list
// growing upward from it.
//
// Deliberately outside the frame. The panel is the thing that is always true;
// the prompt is a layer over it, and keeping it outside means invoking a
// command never reflows the dashboard being read.
//
// maxH is a hard ceiling, not a hint: exceeding it pushes the top of the panel
// off the alternate screen, where it cannot be scrolled back to.
func (m Model) renderBelowPanel(panelW, maxH int) string {
	switch {
	case m.entry.confirming != nil:
		return m.renderConfirm(panelW, maxH)
	case m.entry.active:
		return m.renderEntry(panelW, maxH)
	default:
		return m.renderStatusLine(panelW)
	}
}

func (m Model) renderEntry(panelW, maxH int) string {
	var b strings.Builder

	// One row is always the prompt itself -- without it there is no command
	// mode, only a list. Whatever remains is the list's.
	listRows := maxH - 1
	if listRows < 0 {
		listRows = 0
	}

	// The consequence line, when one will be shown, costs a row from the list
	// rather than being added on top of it.
	sel := m.selected()
	showConsequence := false
	if sel != nil && sel.Consequence != nil && listRows > 1 {
		if sel.Consequence(&m) != "" {
			showConsequence = true
			listRows--
		}
	}

	window := min(maxCompletions, listRows)

	hits := m.entry.hits
	shown := hits
	start := 0
	if window <= 0 {
		shown = nil
	} else if len(shown) > window {
		// Keep the selection visible when the list is longer than the window.
		if m.entry.sel >= window {
			start = m.entry.sel - window + 1
		}
		shown = hits[start : start+window]
	}

	// Widest name, so summaries line up.
	nameW := 0
	for _, c := range shown {
		w := width(c.Name)
		if c.Args != "" {
			w += 1 + width(c.Args)
		}
		if w > nameW {
			nameW = w
		}
	}
	nameW = min(nameW, 28)

	for i, c := range shown {
		isSelected := start+i == m.entry.sel

		name := c.Name
		if c.Args != "" {
			name += " " + c.Args
		}
		label := " " + pad(name, nameW) + " "

		var line string
		if isSelected {
			line = "  " + sSel.Render(label) + " " + sAmbO.Render(c.Summary)
		} else {
			line = "  " + sDimO.Render(label+" "+c.Summary)
		}
		b.WriteString(truncateStyled(line, panelW) + "\n")

		// The consequence rides under the selected row only. Showing it for
		// every row would turn the list into prose; showing it for none would
		// drop the sentence that stops a mistake.
		if isSelected && showConsequence {
			style := sDimO
			if c.Danger {
				style = lipgloss.NewStyle().Foreground(colClay)
			}
			b.WriteString(truncateStyled("     "+style.Render(c.Consequence(&m)), panelW) + "\n")
		}
	}

	if len(hits) == 0 && listRows > 0 {
		b.WriteString("  " + sDimO.Render("没有匹配的命令") + "\n")
	}

	// Truncated like every other line below the panel. lipgloss.Height counts
	// newlines, not wrapped rows, so an over-wide line reports height 1 while
	// occupying two physical rows -- and the panel, sized against that lie,
	// pushes its own header off the top of the alternate screen where it
	// cannot be scrolled back to. Reachable by typing a long path into
	// `/auth rm`.
	prompt := sAmbO.Render(" › ") + sPlain.Render(sanitize(m.entry.text)) + sSel.Render(" ")
	b.WriteString(truncateStyled(prompt, panelW))
	return b.String()
}

func (m Model) renderConfirm(panelW, maxH int) string {
	c := m.entry.confirming
	name := c.Name
	if len(m.entry.confirmArg) > 0 {
		name += " " + strings.Join(m.entry.confirmArg, " ")
	}

	var b strings.Builder
	// The prompt row is mandatory; the consequence gets a row only if there is
	// one to spare. On a terminal too short for both, the question the
	// operator is answering wins over the reason -- an unanswerable prompt is
	// worse than an unexplained one.
	if c.Consequence != nil && maxH > 1 {
		if s := c.Consequence(&m); s != "" {
			b.WriteString(truncateStyled("  "+lipgloss.NewStyle().Foreground(colClay).Render(s), panelW) + "\n")
		}
	}
	b.WriteString(truncateStyled(" "+sSel.Render(" 确认 ")+" "+
		sPlain.Render(sanitize(name))+sDimO.Render("   y 执行 · n 取消"), panelW))
	return b.String()
}

// renderStatusLine is the resting state: the last result, or the key hints.
func (m Model) renderStatusLine(panelW int) string {
	switch {
	// A note outranks the running label. Both can be set at once, and the note
	// is the newer information -- it is the answer to something the operator
	// just did. Showing the running label instead swallowed the one message
	// that had to be read: pressing q during `tunnel up` set the warning that a
	// second press would abort it, and the panel displayed "正在启动
	// cloudflared…" instead, so the second press came with no warning seen.
	//
	// Notes expire, and the running label reappears underneath when they do.
	case m.note != "":
		style := sDimO
		if m.noteErr {
			style = lipgloss.NewStyle().Foreground(colClay)
		}
		return truncateStyled(" "+style.Render(sanitize(m.note)), panelW)
	case m.running != "":
		return truncateStyled(" "+sAmbO.Render("⋯ ")+sPlain.Render(sanitize(m.running)), panelW)
	default:
		hint := "/ 命令"
		if m.result != nil {
			hint = "↑↓ 滚动 · Esc 关闭 · / 命令"
		}
		return " " + sDimO.Render(hint+"   r 刷新   q 退出")
	}
}

// ---------- segment plumbing ----------

// segment is one styled run of text. Content is plain, already padded to its
// final width by the caller; styling happens here, after every width decision
// has been made on text with no escape sequences in it.
type segment struct {
	style lipgloss.Style
	text  string
}

// renderSegs styles the segments and pads the result to exactly inner cells.
//
// Truncation happens on the plain text, segment by segment: cutting a rendered
// string would slice through an escape sequence and spray the rest of the
// frame with garbage.
func renderSegs(segs []segment, inner int) string {
	var b strings.Builder
	used := 0
	for _, s := range segs {
		if used >= inner {
			break
		}
		// Sanitised here, at the single point every panel row passes through,
		// rather than at each of the dozen places that build a segment. A
		// newline or an escape sequence reaching this far would break the
		// frame no matter which field it came from.
		text := sanitize(s.text)
		if used+width(text) > inner {
			text = truncate(text, inner-used)
		}
		if text == "" {
			continue
		}
		b.WriteString(s.style.Render(text))
		used += width(text)
	}
	if used < inner {
		b.WriteString(sText.Render(strings.Repeat(" ", inner-used)))
	}
	return b.String()
}

// truncateStyled cuts an already-styled line to w cells.
//
// Used only outside the panel, where lines are assembled from independent
// Render calls and re-measuring is cheaper than restructuring. lipgloss.Width
// measures through escape sequences; when the line fits -- the normal case --
// nothing is cut at all.
func truncateStyled(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}
