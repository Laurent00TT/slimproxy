package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/momo/slimproxy/credentials"
	"github.com/momo/slimproxy/diag"
	"github.com/momo/slimproxy/metrics"
	"github.com/momo/slimproxy/tunnel"
)

// Deps is everything the dashboard needs from the rest of the program.
//
// Functions rather than concrete objects: the panel is then renderable from a
// test with no proxy, no network and no cloudflared, and the slow calls
// (tunnel, doctor) can be stubbed to return instantly. The data types are
// concrete because they are data -- what is injected is how to obtain them.
//
// Every function taking a context is expected to honour it. The dashboard
// cancels on quit, and a call that ignores cancellation would keep the process
// alive after the screen is gone.
type Deps struct {
	// Listen is the address the proxy bound, for the header.
	Listen string
	// Version is stamped into the footer.
	Version string
	// AuthDir is the resolved credential directory, shown when the pool is
	// empty so the operator knows which directory was actually read.
	AuthDir string

	// Stats reads in-process telemetry. Cheap, called every second.
	Stats func(now time.Time) metrics.Snapshot

	// TunnelStatus queries the tunnel, including Cloudflare. Expensive: it
	// makes a network round trip and may take seconds.
	TunnelStatus func(ctx context.Context) tunnel.Status
	// TunnelUp starts the tunnel detached. Blocks until the child registers a
	// connection or fails.
	TunnelUp func(ctx context.Context) (tunnel.Status, error)
	// TunnelDown stops a tunnel this process started.
	TunnelDown func(ctx context.Context) (tunnel.Status, error)

	// Creds lists the credential pool from disk.
	//
	// Takes a context because the directory is not necessarily local: on a
	// network share or a sleeping disk this blocks, and without a deadline the
	// panel's "running" flag never clears -- every later command is then
	// refused with "请等它结束" while the credential row silently keeps showing
	// a reading from minutes ago.
	Creds func(ctx context.Context) ([]credentials.Credential, error)
	// RemoveCred deletes one credential by identifier. force skips the
	// last-credential guard.
	RemoveCred func(ctx context.Context, id string, force bool) (credentials.Credential, error)

	// Doctor runs the full diagnostic set. Slow -- tens of seconds worst case.
	Doctor func(ctx context.Context) diag.Report

	// Routes lists the translator pairs this build serves.
	//
	// Not a function because it is slow or fallible -- it is neither, being a
	// compile-time registry -- but because tui must not import translate to
	// stay a presentation package.
	Routes func() []Route

	// LogDir is where application logs were redirected to, empty when they were
	// not redirected.
	//
	// Panel mode takes stdout and turns on file logging to compensate. An
	// operator whose config says `log-to-file: false` will look for logs on a
	// stdout the panel is occupying, so the panel has to say where they went.
	LogDir string

	// OnAbort is called when the operator quits while an action is still
	// running, naming that action.
	//
	// Invoked from the render goroutine immediately before tea.Quit, and read
	// by the host after Run returns -- writing to stderr any earlier would land
	// inside the alternate screen and disappear with it.
	OnAbort func(action string)
}

// Route is one translator pair, projected to what the panel shows.
type Route struct {
	Client   string
	Provider string
}

// Refresh cadences.
//
// Three rates, not one, because the three sources cost wildly different
// amounts. Polling the tunnel every second would spend most of the process's
// wall clock waiting on Cloudflare; polling metrics every thirty would make the
// headline numbers visibly stale on a dashboard whose whole point is that they
// are live.
const (
	statsInterval  = 1 * time.Second
	credsInterval  = 15 * time.Second
	tunnelInterval = 45 * time.Second
)

// tunnelProbeTimeout bounds one tunnel query from the outside.
//
// tunnel.Manager applies its own 8s deadline per Cloudflare call, but Status
// can make more than one and reads local state in between. This is the backstop
// that keeps a wedged probe from blocking the next refresh forever.
const tunnelProbeTimeout = 20 * time.Second

// credsReadTimeout bounds one credential-directory read.
//
// A local directory answers in microseconds, so this only ever fires when the
// directory is not local -- which is exactly the case where waiting forever
// would wedge the panel.
const credsReadTimeout = 10 * time.Second

// Messages. One type per source so Update can tell them apart without a tag
// field, and so a stray message from a cancelled operation is inert.
//
// Each carries `renew`, which says whether its arrival should arm the next
// timer. It is not derivable from the message: a fetch is started by the poll
// timer, by the operator pressing r, and by any command that changed what it
// reports -- and only the first of those is a link in the polling chain.
//
// Treating every result as a link is what made each manual refresh fork a
// second self-sustaining chain that never merged back. Three presses of r took
// the tunnel from one Cloudflare round trip per interval to seven.
type (
	statsMsg struct {
		snap  metrics.Snapshot
		at    time.Time
		renew bool
	}
	tunnelMsg struct {
		st    tunnel.Status
		at    time.Time
		renew bool
	}
	credsMsg struct {
		creds []credentials.Credential
		err   error
		at    time.Time
		renew bool
	}
	// tickMsg asks for the next poll of one source. Separate from the result
	// messages so a slow fetch delays only itself: the timer for the next round
	// is armed when the previous result lands, never overlapping.
	tickMsg struct{ source int }
)

// Poll sources.
const (
	srcStats = iota
	srcCreds
	srcTunnel
)

func scheduleTick(source int, d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{source: source} })
}

// fireTick asks for one poll right now, through the ordinary tick path.
func fireTick(source int) tea.Cmd {
	return func() tea.Msg { return tickMsg{source: source} }
}

// renewTick arms the next poll only for a result that is part of the chain.
//
// The whole point of the renew flag lives here: exactly one timer per source
// must be outstanding at any moment. A result from a manual refresh or a
// command's follow-up reads fresh data and stops there.
func renewTick(source int, d time.Duration, renew bool) tea.Cmd {
	if !renew {
		return nil
	}
	return scheduleTick(source, d)
}

// The fetches below take `renew`, which travels with the result and decides
// whether its arrival arms the next timer. Poll ticks pass true; a manual
// refresh or a command's follow-up passes false, so it reads fresh data without
// forking a second polling chain.

// fetchStats reads the collector. Synchronous inside the command, but the
// command itself runs off the UI goroutine, so a lock held by a busy request
// path cannot stall rendering.
func (m Model) fetchStats(renew bool) tea.Cmd {
	deps := m.deps
	return func() tea.Msg {
		now := time.Now()
		if deps.Stats == nil {
			return statsMsg{at: now, renew: renew}
		}
		return statsMsg{snap: deps.Stats(now), at: now, renew: renew}
	}
}

func (m Model) fetchCreds(renew bool) tea.Cmd {
	deps := m.deps
	base := m.ctx
	return func() tea.Msg {
		if deps.Creds == nil {
			return credsMsg{at: time.Now(), renew: renew}
		}
		ctx, cancel := context.WithTimeout(base, credsReadTimeout)
		defer cancel()
		creds, err := deps.Creds(ctx)
		return credsMsg{creds: creds, err: err, at: time.Now(), renew: renew}
	}
}

func (m Model) fetchTunnel(renew bool) tea.Cmd {
	deps := m.deps
	base := m.ctx
	return func() tea.Msg {
		if deps.TunnelStatus == nil {
			return tunnelMsg{at: time.Now(), renew: renew}
		}
		ctx, cancel := context.WithTimeout(base, tunnelProbeTimeout)
		defer cancel()
		return tunnelMsg{st: deps.TunnelStatus(ctx), at: time.Now(), renew: renew}
	}
}
