package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Laurent00TT/slimproxy/credentials"
	"github.com/Laurent00TT/slimproxy/diag"
	"github.com/Laurent00TT/slimproxy/metrics"
	"github.com/Laurent00TT/slimproxy/tunnel"
)

// Model is the dashboard state.
//
// Held by value, as bubbletea requires. Nothing in it is a mutable pointer the
// render goroutine shares with a worker: results arrive as messages, so there
// is no state a running command can write to underneath a frame being drawn.
type Model struct {
	deps Deps
	ctx  context.Context

	width, height int
	// sized reports whether a WindowSizeMsg has arrived. Before it does the
	// terminal dimensions are unknown, and drawing to a guessed width produces
	// one garbled frame on startup.
	sized bool

	started time.Time
	now     time.Time

	stats  metrics.Snapshot
	tunnel tunnelView
	creds  credsView

	// Command mode.
	cmds  []*Command
	entry entryState

	// running is the label of an in-flight action. Non-empty means one is
	// running, and a second cannot be started -- two concurrent `tunnel up`
	// calls would race for the pid record.
	running string

	// quitArmed records that a quit was asked for while an action was running
	// and refused. The next request goes through.
	quitArmed bool

	// result is the last multi-line output, nil when the panel is closed.
	result    *actionResult
	resultTop int // first visible line, for scrolling

	// lastDoctor survives the output panel being closed, because the status
	// row reports it long after the report itself has been dismissed. Nil
	// until doctor has run: the row then says so rather than showing a
	// verdict nothing produced.
	lastDoctor *doctorSummary

	// note is the transient one-line status message.
	note    string
	noteErr bool
	noteAt  time.Time

	quitting bool
}

// entryState is everything about the slash prompt.
//
// Grouped rather than inlined so resetting command mode is one assignment;
// clearing six fields by hand is how a stale completion list survives an Esc.
type entryState struct {
	active bool
	text   string
	hits   []*Command
	args   []string
	sel    int

	// confirming holds a Danger command awaiting y/n. Nil otherwise.
	confirming *Command
	confirmArg []string
}

// tunnelView is the last tunnel answer, plus how it was obtained.
type tunnelView struct {
	// known reports whether any answer has arrived. Distinct from a zero
	// Status, which is a valid reading meaning "not configured".
	known bool
	st    tunnel.Status
	at    time.Time

	// Denormalised for the consequence strings, so they do not have to know
	// how Status spells things. Holds every hostname, joined: "停止后 X 立即
	// 不可达" must name everything that becomes unreachable, not just the
	// first domain.
	hostname  string
	conns     int
	connKnown bool

	inFlight bool
}

// credsView is the last credential-pool reading.
type credsView struct {
	known bool
	creds []credentials.Credential
	err   error
	at    time.Time

	// usable counts credentials that could serve a request right now.
	usable int
	// recoverable counts those the proxy will load and may serve with after a
	// refresh. Tracked separately because it is the number that decides
	// whether the pool is actually empty -- see credentials.Recoverable.
	recoverable int
	// soonest is the nearest expiry among *usable* credentials, and is
	// therefore always in the future. Zero when none recorded one.
	//
	// Deliberately not "among recoverable": an expired-but-refreshable
	// credential has an expiry in the past, and letting it win made the row
	// report how long ago it died as how long the pool has left.
	soonest time.Time
}

// doctorSummary is the last diagnostic verdict, kept for the status row.
type doctorSummary struct {
	level   diag.Level
	summary string
	at      time.Time
}

// noteTTL is how long a one-line result stays on screen.
//
// Long enough to read a sentence, short enough that the line is not still
// claiming "隧道已停止" minutes later when the state has moved on.
const noteTTL = 12 * time.Second

// New builds the dashboard model.
func New(ctx context.Context, deps Deps) Model {
	now := time.Now()
	m := Model{
		deps:    deps,
		ctx:     ctx,
		cmds:    commandSet(),
		started: now,
		now:     now,
	}
	// Said once, at startup, because panel mode may have moved the logs
	// without being asked to: a config with `log-to-file: false` sends the
	// operator looking on a stdout this panel is occupying. Announcing it is
	// the difference between redirected logs and lost ones.
	if deps.LogDir != "" {
		m.setNote("日志已重定向到 "+deps.LogDir, false)
	}
	return m
}

func (m Model) Init() tea.Cmd {
	// Every source is polled immediately rather than after its first interval:
	// waiting would show an empty tunnel row for 45 seconds on a dashboard
	// whose first job is to say whether the tunnel is up.
	//
	// Done by firing the tick rather than the fetch, so the first round goes
	// through exactly the same path as every later one. Init has a value
	// receiver and cannot set m.tunnel.inFlight, so fetching directly here left
	// the first probe unguarded -- pressing r during it ran two concurrent
	// Cloudflare queries whose results then overwrote each other in arrival
	// order rather than in observation order.
	return tea.Batch(
		fireTick(srcStats), fireTick(srcCreds), fireTick(srcTunnel),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sized = true
		m.clampScroll()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		switch msg.source {
		case srcStats:
			return m, m.fetchStats(true)
		case srcCreds:
			return m, m.fetchCreds(true)
		case srcTunnel:
			if m.tunnel.inFlight {
				// A probe is still out. Re-arm rather than stacking a second
				// one: overlapping queries would multiply the load on a link
				// that is already too slow to answer in time. The chain
				// survives because this re-arms it; the in-flight result will
				// carry renew=false and so will not arm a second.
				return m, scheduleTick(srcTunnel, tunnelInterval)
			}
			m.tunnel.inFlight = true
			return m, m.fetchTunnel(true)
		}
		return m, nil

	case statsMsg:
		m.stats = msg.snap
		m.now = msg.at
		m.expireNote()
		return m, renewTick(srcStats, statsInterval, msg.renew)

	case credsMsg:
		// Classified against the instant the directory was read, not the
		// instant the message was handled. A credential expiring during the
		// gap must be reported as it was when observed.
		m.creds = summariseCreds(msg, msg.at)
		return m, renewTick(srcCreds, credsInterval, msg.renew)

	case tunnelMsg:
		m.tunnel = tunnelView{
			known:     true,
			st:        msg.st,
			at:        msg.at,
			hostname:  tunnel.JoinHostnames(msg.st.Hostnames),
			conns:     msg.st.Connections,
			connKnown: msg.st.ConnectionsKnown,
		}
		return m, renewTick(srcTunnel, tunnelInterval, msg.renew)

	case actionDoneMsg:
		m.running = ""
		if msg.res.Err != nil && len(msg.res.Lines) == 0 {
			m.setNote(msg.res.Err.Error(), true)
		} else if msg.res.Note != "" {
			m.setNote(msg.res.Note, false)
		}
		if len(msg.res.Lines) > 0 || msg.res.Title != "" {
			res := msg.res
			m.result = &res
			m.resultTop = 0
		}
		if msg.res.HasLevel {
			m.lastDoctor = &doctorSummary{
				level:   msg.res.Level,
				summary: msg.res.Verdict,
				at:      m.now,
			}
		}
		// renew=false for the same reason as the r key: a command that changed
		// what it reports should refresh the display, not add a polling chain.
		var cmds []tea.Cmd
		if msg.refreshTunnel && !m.tunnel.inFlight {
			m.tunnel.inFlight = true
			cmds = append(cmds, m.fetchTunnel(false))
		}
		if msg.refreshCreds {
			cmds = append(cmds, m.fetchCreds(false))
		}
		return m, tea.Batch(cmds...)
	}

	return m, nil
}

// summariseCreds derives the counts the panel shows from one listing.
//
// usable and recoverable are counted separately and deliberately: a pool of
// one expired-but-refreshable credential has zero usable and one recoverable,
// and reporting the first as the pool size would say "0 凭据" about a proxy
// that is about to work fine.
func summariseCreds(msg credsMsg, now time.Time) credsView {
	v := credsView{known: true, creds: msg.creds, err: msg.err, at: msg.at}
	for _, c := range msg.creds {
		if c.Recoverable(now) {
			v.recoverable++
		}
		if !c.Usable(now) {
			continue
		}
		v.usable++
		// Only usable credentials contribute to soonest, which is what keeps
		// the value in the future -- an expired entry's timestamp is in the
		// past and would be rendered as a remaining lifetime.
		if c.Expires.IsZero() {
			continue
		}
		if v.soonest.IsZero() || c.Expires.Before(v.soonest) {
			v.soonest = c.Expires
		}
	}
	return v
}

// Every time comparison in this model goes through m.now, and every renderer
// reads it rather than the clock. One frame then describes one instant --
// which is what let "7h24m 后过期" render as the gap between the poll and the
// paint instead of the value that was polled.
//
// m.now advances on each stats message, so it is at most one second stale.

func (m *Model) setNote(s string, isErr bool) {
	m.note, m.noteErr, m.noteAt = s, isErr, m.now
}

func (m *Model) expireNote() {
	if m.note != "" && m.now.Sub(m.noteAt) > noteTTL {
		m.note, m.noteErr = "", false
	}
}

// requestQuit ends the session, or refuses once when an action is in flight.
//
// Quitting cancels the context every running action holds, and for `tunnel up`
// that is not a no-op: confirmStarted takes the cancellation as "the child
// never connected" and kills the cloudflared it just launched, then deletes the
// pid record. The operator asked to start a tunnel, pressed q while waiting,
// and got a killed tunnel and an empty screen -- the actionDoneMsg explaining
// it is delivered to a program that has already stopped.
//
// So the first request during an action reports what would be interrupted, and
// a second goes through. Not a y/n prompt: q on a foreground process means
// stop, and turning the ordinary case into a dialogue to cover the rare one
// trains the operator to dismiss it.
//
// Both q and /quit route here, so the two spellings cannot behave differently.
func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	if m.running != "" && !m.quitArmed {
		m.quitArmed = true
		m.setNote("「"+m.running+"」仍在执行；再按一次 q 会中止它并退出", true)
		return m, nil
	}
	if m.running != "" && m.deps.OnAbort != nil {
		// Reported after the screen is released -- writing to stderr now would
		// land inside the alternate screen and vanish with it.
		m.deps.OnAbort(m.running)
	}
	m.quitting = true
	return m, tea.Quit
}

// ---------- keyboard ----------

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl-C always quits, in every mode, and without the second-press guard
	// that q has. A dashboard that argues with Ctrl-C is one the operator can
	// only escape by killing the process from another terminal.
	if k.Type == tea.KeyCtrlC {
		if m.running != "" && m.deps.OnAbort != nil {
			m.deps.OnAbort(m.running)
		}
		m.quitting = true
		return m, tea.Quit
	}

	if m.entry.confirming != nil {
		return m.handleConfirmKey(k)
	}
	if m.entry.active {
		return m.handleEntryKey(k)
	}
	return m.handleNormalKey(k)
}

func (m Model) handleNormalKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "/":
		// The slash is kept in the text, not stripped and re-added at render
		// time. match trims it, the prompt shows it, and backspacing over it
		// leaves command mode -- one representation, so the three cannot
		// disagree about whether it is there.
		m.entry.active = true
		m.entry.text = "/"
		m.entry.hits, m.entry.args = match(m.cmds, "/")
		m.entry.sel = 0
		return m, nil
	case "q":
		return m.requestQuit()
	case "esc":
		// Closes the output panel. Not a quit: Esc meaning "exit the program"
		// in a dashboard that also uses Esc to dismiss a panel is how a doctor
		// report gets closed and the proxy stopped in one keystroke.
		m.result = nil
		m.resultTop = 0
		return m, nil
	case "up", "k":
		m.scroll(-1)
		return m, nil
	case "down", "j":
		m.scroll(1)
		return m, nil
	case "pgup":
		m.scroll(-m.resultViewHeight())
		return m, nil
	case "pgdown", " ":
		m.scroll(m.resultViewHeight())
		return m, nil
	case "home":
		m.resultTop = 0
		return m, nil
	case "end":
		m.resultTop = m.maxScroll()
		return m, nil
	case "r":
		// Manual refresh, for the operator who just changed something outside
		// this process and does not want to wait out the poll interval.
		//
		// renew=false: this reads fresh data without becoming a link in the
		// polling chain. Arming a timer here is what made every press add a
		// permanent extra round of polling.
		var cmds []tea.Cmd
		cmds = append(cmds, m.fetchCreds(false))
		if !m.tunnel.inFlight {
			m.tunnel.inFlight = true
			cmds = append(cmds, m.fetchTunnel(false))
		}
		m.setNote("正在刷新…", false)
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) handleEntryKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		m.entry = entryState{}
		return m, nil

	case tea.KeyEnter:
		return m.submit()

	case tea.KeyUp:
		if m.entry.sel > 0 {
			m.entry.sel--
		}
		return m, nil

	case tea.KeyDown:
		if m.entry.sel < len(m.entry.hits)-1 {
			m.entry.sel++
		}
		return m, nil

	case tea.KeyTab:
		// Completes to the selected command, leaving the cursor ready for
		// arguments. Adds the trailing space only for commands that take them,
		// so Enter on a no-argument command does not have to delete it.
		if c := m.selected(); c != nil {
			m.entry.text = "/" + c.Name
			if c.Args != "" {
				m.entry.text += " "
			}
			m.refreshHits()
		}
		return m, nil

	case tea.KeyBackspace:
		r := []rune(m.entry.text)
		if len(r) > 1 {
			m.entry.text = string(r[:len(r)-1])
			m.refreshHits()
			return m, nil
		}
		// Deleting the slash itself leaves command mode, which is what the
		// keystroke means: there is nothing left to delete.
		m.entry = entryState{}
		return m, nil

	case tea.KeySpace:
		m.entry.text += " "
		m.refreshHits()
		return m, nil

	case tea.KeyRunes:
		m.entry.text += string(k.Runes)
		m.refreshHits()
		return m, nil
	}
	return m, nil
}

func (m Model) handleConfirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "y", "Y":
		c, args := m.entry.confirming, m.entry.confirmArg
		m.entry = entryState{}
		return m.execute(c, args)
	case "n", "N", "esc", "enter":
		m.entry = entryState{}
		m.setNote("已取消", false)
		return m, nil
	}
	// Anything else is ignored on purpose. A confirmation that accepts any
	// keypress as yes is not a confirmation.
	return m, nil
}

// refreshHits recomputes completions and resets the selection.
//
// Reset, not clamped. Clamping only pulled the index down when the list got
// shorter, so a selection made by arrowing down survived into a completely
// different list: pressing / then arrowing to `quit` then typing one character
// left the highlight sitting on `tunnel down` -- a destructive command the
// operator never navigated to. Editing the text means choosing again.
func (m *Model) refreshHits() {
	m.entry.hits, m.entry.args = match(m.cmds, m.entry.text)
	m.entry.sel = 0
}

func (m Model) selected() *Command {
	if m.entry.sel < 0 || m.entry.sel >= len(m.entry.hits) {
		return nil
	}
	return m.entry.hits[m.entry.sel]
}

// submit runs the selected command, or asks first when it is dangerous.
func (m Model) submit() (tea.Model, tea.Cmd) {
	c := m.selected()
	if c == nil {
		m.setNote("没有匹配的命令", true)
		return m, nil
	}
	args := m.entry.args
	if m.running != "" {
		m.setNote("正在执行「"+m.running+"」，请等它结束", true)
		return m, nil
	}

	if c.Danger {
		m.entry.active = false
		m.entry.confirming = c
		m.entry.confirmArg = args
		return m, nil
	}
	m.entry = entryState{}
	return m.execute(c, args)
}

// execute turns a command into work, or reports why it cannot run.
func (m Model) execute(c *Command, args []string) (tea.Model, tea.Cmd) {
	spec := c.Run(&m, args)

	switch {
	case spec.Err != nil:
		m.setNote(spec.Err.Error(), true)
		return m, nil
	case spec.Quit:
		// Same path as the q key, including the in-flight guard.
		return m.requestQuit()
	case spec.Do == nil:
		if spec.Note != "" {
			m.setNote(spec.Note, false)
		}
		return m, nil
	}

	m.running = spec.Running
	m.result = nil
	m.resultTop = 0

	base := m.ctx
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = actionDefaultTimeout
	}
	do := spec.Do
	refreshT, refreshC := spec.RefreshTunnel, spec.RefreshCreds

	label := c.Name

	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()

		// Run on a goroutine and race the deadline, rather than calling do(ctx)
		// and inspecting ctx afterwards.
		//
		// The difference is whether the timeout is enforced or merely reported.
		// Some of what Do reaches -- os.ReadDir on a network share, a sleeping
		// disk -- cannot be interrupted by a context at all, so a synchronous
		// call returns when the operation feels like it. The panel would then
		// sit with m.running set forever, refusing every later command with
		// "请等它结束". Racing the deadline costs a leaked goroutine in that
		// case, and buys back a usable dashboard.
		done := make(chan actionResult, 1)
		go func() { done <- do(ctx) }()

		select {
		case res := <-done:
			// Finishing after the deadline is not success. A command that ran
			// out of time may have produced partial output, and presenting
			// that as a complete answer is the shape of bug this file exists
			// to avoid.
			if res.Err == nil && ctx.Err() != nil {
				res.Err = ctx.Err()
				res.Title = timeoutTitle(label, res.Title)
				res.Lines = append([]string{timeoutLine(label, timeout)}, res.Lines...)
				res.Note = ""
			}
			return actionDoneMsg{res: res, refreshTunnel: refreshT, refreshCreds: refreshC}
		case <-ctx.Done():
			return actionDoneMsg{res: actionResult{
				Title: timeoutTitle(label, ""),
				Lines: []string{timeoutLine(label, timeout)},
				Err:   ctx.Err(),
			}}
		}
	}
}

// timeoutTitle heads a timed-out result, keeping whatever the command had
// already called its output.
func timeoutTitle(cmd, existing string) string {
	if existing == "" {
		return cmd + " 超时"
	}
	return existing + "（超时）"
}

// timeoutLine says what timed out and for how long, in the interface's own
// language.
//
// Without it the status line showed a bare `context deadline exceeded`: English
// in an otherwise Chinese panel, and silent about which command produced it.
func timeoutLine(cmd string, d time.Duration) string {
	return fmt.Sprintf("「%s」在 %s 内未完成，以下结果可能不完整。", cmd, shortDur(d))
}

// ---------- scrolling ----------

func (m *Model) scroll(delta int) {
	if m.result == nil {
		return
	}
	m.resultTop += delta
	m.clampScroll()
}

func (m *Model) clampScroll() {
	if m.result == nil {
		m.resultTop = 0
		return
	}
	if top := m.maxScroll(); m.resultTop > top {
		m.resultTop = top
	}
	if m.resultTop < 0 {
		m.resultTop = 0
	}
}

func (m Model) maxScroll() int {
	if m.result == nil {
		return 0
	}
	return max(0, len(m.result.Lines)-m.resultViewHeight())
}

// trimTrailingBlank removes trailing empty lines so a panel does not reserve
// rows for nothing.
func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
