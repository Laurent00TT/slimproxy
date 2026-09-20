package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// State is where a tunnel sits between "nothing set up" and "serving traffic".
//
// Four values, not a boolean. Configured exists because a configured tunnel
// with no process is a normal resting state; Running is separate from it
// because a process with zero connections looks alive in a task list while the
// public hostname returns 502; StateUnknown exists so a failed query is never
// reported as either of the others.
type State int

const (
	// NotConfigured: no cloudflared configuration was found.
	NotConfigured State = iota
	// Configured: a configuration exists but no tunnel process is running.
	Configured
	// Running: a tunnel process is running.
	Running
	// StateUnknown: the state could not be determined.
	StateUnknown
)

func (s State) String() string {
	switch s {
	case NotConfigured:
		return i18n.T("未配置", "not configured")
	case Configured:
		return i18n.T("已配置，未运行", "configured, not running")
	case Running:
		return i18n.T("运行中", "running")
	default:
		return i18n.T("未知", "unknown")
	}
}

// Status describes a tunnel at one instant.
type Status struct {
	State State
	// PID is the running cloudflared, when this process launched it detached.
	PID int
	// Managed is true when a record written by `tunnel up` matches a live
	// process. A tunnel started by hand reports Running with Managed false, and
	// cannot be stopped by `tunnel down`.
	Managed bool
	// TunnelID is the tunnel actually being served: the running child's, when
	// one is running, otherwise the configured one. These differ when the
	// configuration was edited after launch.
	TunnelID string
	// ConfiguredTunnelID is what the configuration currently names. Set only
	// when it differs from TunnelID.
	ConfiguredTunnelID string
	// Hostnames are all public hostnames the configuration routes to an
	// origin, in ingress order. See Config.Hostnames for why this is a list.
	Hostnames  []string
	ConfigPath string
	LogPath    string
	// Connections is the edge connection count, valid only when
	// ConnectionsKnown is true. Carried here so callers do not pay for a second
	// round trip to learn what this call already established.
	Connections      int
	ConnectionsKnown bool
	// ConnectionsErr says why the connection count is unavailable, when it is.
	//
	// Separate from Err, which describes the tunnel's state as a whole: a
	// tunnel can be verifiably Running while the count is unobtainable, and
	// merging the two would turn a partial answer into an unknown state.
	ConnectionsErr error
	// EdgeFakeIP names an edge address in RFC 2544 space, when one was seen.
	// Traffic through such an address is going via a local proxy and long-lived
	// connections tend to drop.
	EdgeFakeIP string
	// Detail carries anything the operator should know that the fields above
	// cannot express. Callers are expected to surface it.
	Detail string
	// Err is set when the state could not be established.
	Err error
}

// probeTimeout bounds one query to Cloudflare. Without it `tunnel status` and
// `tunnel up` block indefinitely on a flaky link, since their contexts carry
// only signal handling and no deadline.
const probeTimeout = 8 * time.Second

// startupGrace is how long a detached child has to register a connection.
//
// Sized for the slow case rather than the typical one: a healthy connect
// usually completes in one to three seconds, but a first connection over a
// congested or proxied link can take considerably longer. Waiting too briefly
// would report a working tunnel as broken -- and then kill it.
const startupGrace = 15 * time.Second

// Manager operates the cloudflared child process.
type Manager struct {
	// StateDir holds the pid record. Defaults to the working directory.
	StateDir string
	// Binary overrides the cloudflared executable. Empty resolves from PATH.
	Binary string

	// The three seams below exist so the decisions in this file can be tested
	// without a cloudflared binary, a network, or a real process. Nil means the
	// production implementation.
	verify      func(rec record) (identity, error)
	readConfig  func() (Config, bool, error)
	connections func(ctx context.Context, id string) (Connectivity, error)
}

func (m *Manager) identityOf(rec record) (identity, error) {
	if m.verify != nil {
		return m.verify(rec)
	}
	return verifyRecord(rec)
}

func (m *Manager) config() (Config, bool, error) {
	if m.readConfig != nil {
		return m.readConfig()
	}
	return ReadConfig()
}

// stateDirOrDot renders the state directory the way an operator can act on it.
func (m *Manager) stateDirOrDot() string {
	if m.StateDir == "" {
		return i18n.T("当前目录", "the current directory")
	}
	if abs, err := filepath.Abs(m.StateDir); err == nil {
		return abs
	}
	return m.StateDir
}

// resolveBinary finds cloudflared, reporting a usable message when it is absent.
func (m *Manager) resolveBinary() (string, error) {
	if m.Binary != "" {
		return m.Binary, nil
	}
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		return "", errors.New(i18n.T("找不到 cloudflared 可执行文件；请安装它或加入 PATH", "cloudflared executable not found; install it or add it to PATH"))
	}
	return path, nil
}

// localStatus answers everything determinable without touching the network.
//
// Separated from Status because Down needs exactly this and nothing more:
// routing it through the full Status made every one of Down's return paths pay
// for a Cloudflare round trip it then discarded.
func (m *Manager) localStatus() Status {
	var st Status

	// The record is read first, and independently of the configuration. A
	// broken config must not hide a running managed child -- doing so lets the
	// next `up` spawn a second one and orphan the first permanently.
	rec, recErr := readRecord(m.StateDir)
	hasRecord := recErr == nil

	cfg, found, cfgErr := m.config()
	switch {
	case cfgErr != nil:
		st.State = StateUnknown
		st.Err = cfgErr
		st.Detail = i18n.T("存在 cloudflared 配置但无法理解", "a cloudflared config exists but could not be understood")
	case !found:
		st.State = NotConfigured
	default:
		st.State = Configured
		st.TunnelID = cfg.Tunnel
		st.Hostnames = cfg.Hostnames
		st.ConfigPath = cfg.Path
	}

	if !hasRecord {
		if !errors.Is(recErr, errNoRecord) {
			// A corrupt record is not an absent one. Say so, or a live child
			// stays invisible while the file sits on disk.
			st.State = StateUnknown
			st.Err = recErr
			st.Detail = i18n.T("PID 记录无法读取，无法确定是否有受管实例在运行", "the PID record is unreadable; whether a managed instance is running cannot be determined")
		}
		return st
	}

	st.LogPath = rec.LogPath
	switch id, verr := m.identityOf(rec); id {
	case identityMatch:
		st.State = Running
		st.PID = rec.PID
		st.Managed = true
		if rec.Tunnel != "" && st.TunnelID != "" && rec.Tunnel != st.TunnelID {
			// The configuration changed after launch. Report the tunnel that is
			// actually being served, or status describes one tunnel while the
			// process serves another.
			st.ConfiguredTunnelID = st.TunnelID
			st.TunnelID = rec.Tunnel
			st.Detail = fmt.Sprintf(i18n.T(
				"配置已改为隧道 %s，但运行中的进程服务的是 %s；需要 down 后重新 up 才会切换",
				"the config now names tunnel %s, but the running process serves %s; down then up again to switch"), st.ConfiguredTunnelID, rec.Tunnel)
		}
	case identityGone:
		removeRecord(m.StateDir)
		st.Detail = fmt.Sprintf(i18n.T("上次启动的 cloudflared（PID %d）已退出，记录已清理", "the previously started cloudflared (PID %d) has exited; record cleaned up"), rec.PID)
	case identityMismatch:
		removeRecord(m.StateDir)
		st.Detail = fmt.Sprintf(i18n.T("记录中的 PID %d 已被其他进程占用，记录已清理", "PID %d from the record now belongs to another process; record cleaned up"), rec.PID)
	default:
		st.State = StateUnknown
		st.Err = verr
		st.Detail = fmt.Sprintf(i18n.T("无法确认 PID %d 的身份", "cannot confirm the identity of PID %d"), rec.PID)
	}
	return st
}

// Status inspects the tunnel, including asking Cloudflare when local state
// cannot answer whether something is serving it.
func (m *Manager) Status(ctx context.Context) Status {
	st := m.localStatus()

	// Only worth asking when local state says nothing is running: a tunnel
	// started by hand, by a service manager, or by an earlier slimproxy with a
	// different state dir is invisible locally, and reporting "not running" on
	// that basis is a false negative.
	if st.State != Configured || st.TunnelID == "" {
		if st.State == Running && st.TunnelID != "" {
			m.fillConnections(ctx, &st)
		}
		return st
	}

	conn, err := m.connectivity(ctx, st.TunnelID)
	switch {
	case err != nil || !conn.Parsed:
		// Not "Configured": that asserts nothing is running, which the failed
		// query did not establish.
		st.State = StateUnknown
		st.Detail = i18n.T("未能确认是否存在非 slimproxy 启动的实例", "could not confirm whether an instance not started by slimproxy exists")
		if err != nil {
			st.Err = err
		}
	case conn.Count > 0:
		st.State = Running
		st.Managed = false
		st.Connections = conn.Count
		st.ConnectionsKnown = true
		st.EdgeFakeIP = conn.FakeIP
		st.Detail = fmt.Sprintf(i18n.T(
			"有 %d 个活动连接，但不是由 slimproxy 启动的；tunnel down 无法停止它",
			"%d active connections, but not started by slimproxy; tunnel down cannot stop it"), conn.Count)
	default:
		st.Connections = 0
		st.ConnectionsKnown = true
	}
	return st
}

// fillConnections adds the edge connection count to a running tunnel's status.
//
// A failed query leaves ConnectionsKnown false and records why. Discarding the
// error -- which this did -- left every caller with "连接数未确认" and no way
// to find out whether the cause was an expired Cloudflare token, a DNS failure,
// or a wrong tunnel ID. The reason existed nowhere in the process, so even the
// deep `tunnel status` command could not report it.
func (m *Manager) fillConnections(ctx context.Context, st *Status) {
	conn, err := m.connectivity(ctx, st.TunnelID)
	switch {
	case err != nil:
		st.ConnectionsErr = err
		return
	case !conn.Parsed:
		st.ConnectionsErr = errors.New(i18n.T("cloudflared 输出无法解析，可能是版本差异", "cloudflared output could not be parsed; possibly a version difference"))
		return
	}
	st.Connections = conn.Count
	st.ConnectionsKnown = true
	st.EdgeFakeIP = conn.FakeIP
}

func (m *Manager) connectivity(ctx context.Context, id string) (Connectivity, error) {
	if m.connections != nil {
		return m.connections(ctx, id)
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return m.queryConnectivity(cctx, id)
}

// ErrAlreadyRunning is returned by Up when a managed tunnel is already up.
var ErrAlreadyRunning = i18n.NewError("隧道已在运行", "the tunnel is already running")

// ErrNotConfigured is returned when there is no cloudflared configuration.
var ErrNotConfigured = i18n.NewError("未找到 cloudflared 配置", "no cloudflared configuration found")

// Up starts cloudflared.
//
// When detach is false the child runs in the foreground and this blocks until
// ctx is cancelled or the child exits -- the child is bound to this process, so
// there is nothing to leak. When detach is true the child outlives the command,
// a record is written, and the launch is confirmed to have survived before
// success is reported.
func (m *Manager) Up(ctx context.Context, detach bool, stdout, stderr *os.File) (Status, error) {
	st := m.Status(ctx)
	switch st.State {
	case NotConfigured:
		return st, ErrNotConfigured
	case Running:
		if st.Managed {
			return st, ErrAlreadyRunning
		}
		// Something else serves this tunnel. Not an error: cloudflared supports
		// several connectors per tunnel as high availability. Proceed, but the
		// caller must surface this.
		st.Detail = i18n.T("已有非 slimproxy 启动的实例在运行；将额外启动一个连接器（cloudflared 支持多连接器）", "an instance not started by slimproxy is already running; starting an additional connector (cloudflared supports several)")
	case StateUnknown:
		// A failed *remote* query is not evidence that something is running,
		// and refusing here would make a flaky link render the tunnel
		// unstartable. A failed *local* read is different: st.TunnelID is then
		// empty, which the guard below catches.
		st.Detail = i18n.T("未能确认是否已有实例在运行，仍继续启动", "could not confirm whether an instance is already running; starting anyway")
	}

	// Without this, a config that failed to parse yields an empty tunnel ID and
	// `cloudflared tunnel run ""` falls back to reading that same unparseable
	// file -- discarding the diagnosis this process already made.
	if st.TunnelID == "" {
		if st.Err != nil {
			return st, fmt.Errorf(i18n.T("无法确定隧道 ID: %w", "cannot determine the tunnel ID: %w"), st.Err)
		}
		return st, errors.New(i18n.T("无法确定隧道 ID：cloudflared 配置中没有 tunnel 字段", "cannot determine the tunnel ID: the cloudflared config has no tunnel field"))
	}

	bin, err := m.resolveBinary()
	if err != nil {
		return st, err
	}

	if !detach {
		cmd := exec.CommandContext(ctx, bin, "tunnel", "run", st.TunnelID)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		// CommandContext kills the child when ctx is cancelled, so Ctrl-C takes
		// the tunnel down with it. No record is written: there is nothing to
		// find later.
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return st, nil // cancelled by the operator, not a failure
			}
			return st, fmt.Errorf(i18n.T("cloudflared 退出: %w", "cloudflared exited: %w"), err)
		}
		return st, nil
	}

	return m.startDetached(ctx, st, bin)
}

func (m *Manager) startDetached(ctx context.Context, st Status, bin string) (Status, error) {
	// Claim the record file before spawning. This is the mutual exclusion
	// between two concurrent `up` calls: without it both see "no record", both
	// spawn, and the second write orphans the first child.
	handle, err := claimRecord(m.StateDir)
	if err != nil {
		return st, err
	}
	claimed := true
	defer func() {
		_ = handle.Close()
		if claimed {
			return
		}
		removeRecord(m.StateDir)
	}()

	logPath, logFile, logStart, err := m.openLog()
	if err != nil {
		claimed = false
		return st, err
	}
	defer func() { _ = logFile.Close() }()

	// Deliberately not CommandContext: this must outlive the command.
	cmd := exec.Command(bin, "tunnel", "run", st.TunnelID)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd) // platform-specific: leave the terminal's signal group
	if err := cmd.Start(); err != nil {
		claimed = false
		return st, fmt.Errorf(i18n.T("启动 cloudflared 失败: %w", "starting cloudflared failed: %w"), err)
	}

	rec := record{
		PID:     cmd.Process.Pid,
		Started: time.Now(),
		Tunnel:  st.TunnelID,
		Binary:  bin,
		LogPath: logPath,
	}
	if err := writeRecordTo(handle, rec); err != nil {
		// A child we cannot record is a child we cannot stop. Kill it -- and
		// verify the kill rather than asserting it, because claiming to have
		// terminated a process that survived is worse than admitting the mess.
		claimed = false
		reap(cmd)
		if kerr := killAndConfirm(ctx, m, rec); kerr != nil {
			return st, fmt.Errorf(i18n.T(
				"无法写入 PID 记录（%w），且未能终止刚启动的 cloudflared（PID %d）：%v。请手动结束该进程",
				"cannot write the PID record (%w), and terminating the cloudflared just started (PID %d) also failed: %v. End that process by hand"), err, rec.PID, kerr)
		}
		return st, fmt.Errorf(i18n.T("无法写入 PID 记录，已终止刚启动的 cloudflared: %w", "cannot write the PID record; the cloudflared just started has been terminated: %w"), err)
	}

	// cmd.Start only proves a process was created, and liveness alone is not
	// enough either: given a bad tunnel ID or no network, cloudflared retries
	// forever rather than exiting. Success means a connection was registered.
	//
	// On failure the child is killed: leaving a process that will never serve
	// traffic, with a record saying the tunnel is up, is the failure this whole
	// check exists to prevent.
	if err := m.confirmStarted(ctx, rec, logStart); err != nil {
		claimed = false
		reap(cmd)
		if kerr := killAndConfirm(ctx, m, rec); kerr != nil {
			return st, fmt.Errorf(i18n.T(
				"%w\n此外未能终止该进程（PID %d）：%v，请手动结束",
				"%w\nadditionally, terminating the process (PID %d) failed: %v -- end it by hand"), err, rec.PID, kerr)
		}
		return st, err
	}

	go func() { _ = cmd.Wait() }() // reap, so it is not a zombie on Unix

	st.State = Running
	st.PID = rec.PID
	st.Managed = true
	st.LogPath = logPath
	return st, nil
}

// connectedMarker is what cloudflared logs once an edge connection is up.
//
// Process liveness is not a usable success signal on its own: given an unknown
// tunnel ID, an expired certificate, or no network, cloudflared does not exit
// -- it retries forever, logging "Retrying connection in up to Ns". A launch
// that will never serve traffic therefore looks identical to a healthy one if
// all you check is that the process exists.
const connectedMarker = "Registered tunnel connection"

// confirmStarted waits until the child has actually connected, or explains why
// it did not.
//
// Three outcomes, each reported differently: the process died (its log says
// why), it is alive but never connected (also in the log, usually a repeating
// retry), or a connection was registered.
//
// Both failure paths go through startupFailure, which names the cause whenever
// the log evidences one. "Here are twelve lines of cloudflared output, work it
// out" is what this function used to return, and it is the difference between
// an operator reading a fake-ip edge address and spending an afternoon on it.
func (m *Manager) confirmStarted(ctx context.Context, rec record, logStart int64) error {
	deadline := time.Now().Add(startupGrace)
	// Whether the last check actually saw our process. Only identityMatch
	// counts: identityUnknown means the query itself failed, and "it never
	// exited, it kept retrying" would then be a statement about a process
	// nobody managed to look at. Not-gone is not the same as observed-alive.
	confirmedAlive := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}

		id, _ := m.identityOf(rec)
		if id == identityGone || id == identityMismatch {
			return startupFailure(i18n.T(
				"cloudflared 启动后随即退出",
				"cloudflared exited immediately after starting"), rec.LogPath, logStart, false)
		}
		confirmedAlive = id == identityMatch
		if logContains(rec.LogPath, connectedMarker, logStart) {
			return nil
		}
	}

	return startupFailure(startupTimeoutHeadline(confirmedAlive), rec.LogPath, logStart, confirmedAlive)
}

// startupTimeoutHeadline is the first line for a launch that ran out of grace.
//
// Not phrased in the present tense ("the process is still running"): the caller
// kills the child as soon as confirmStarted returns, so by the time an operator
// reads the message that claim is already false. What stays true is what was
// observed inside the window -- and when the liveness query never answered,
// what was observed is only that no connection registered.
func startupTimeoutHeadline(confirmedAlive bool) string {
	if !confirmedAlive {
		return fmt.Sprintf(i18n.T(
			"cloudflared 在 %s 内未能建立到 Cloudflare 的连接（进程状态无法确认）",
			"cloudflared did not establish a Cloudflare connection within %s (its process state could not be confirmed)"),
			startupGrace)
	}
	return fmt.Sprintf(i18n.T(
		"cloudflared 在 %s 内未能建立到 Cloudflare 的连接：它没有退出，而是一直在重试",
		"cloudflared did not establish a Cloudflare connection within %s: it never exited, it kept retrying"),
		startupGrace)
}

// startupFailure builds the message for a launch that did not connect: the
// named cause first, then what to do, then the log tail as evidence.
//
// Order is the whole point. The TUI renders this into a panel that truncates
// every line to the panel width and shows a bounded number of rows (see
// tui/view.go), so a twelve-line dump of timestamped cloudflared output is a
// dump with the decisive field cut off the right-hand edge. The tail stays
// because it is the raw evidence for the line above it -- and because when
// nothing classifies, it is still the only thing there is.
//
// alive is passed through to the classifier, which needs it to tell "still
// retrying" from "already dead".
func startupFailure(headline, logPath string, offset int64, alive bool) error {
	lines := []string{headline}
	if d := classifyStartupLog(readLogFrom(logPath, offset), alive); d.known() {
		lines = append(lines, d.Lines...)
	}
	lines = append(lines, i18n.T("日志末尾：", "log tail:"), logTail(logPath, offset, 12))
	return errors.New(strings.Join(lines, "\n"))
}

// startupLogScan caps how much of this launch's output is classified.
//
// One launch writes a few dozen lines inside the startup grace, so the cap is
// never reached in practice; it exists because the log is append-only and
// shared with every previous run, and an unbounded read of a file that grows
// forever is a bug waiting for the one machine where cloudflared loops on a
// chatty error.
const startupLogScan = 256 * 1024

// readLogFrom returns what this launch wrote, from offset onwards.
//
// The same region confirmStarted's success check scans, and for the same
// reason: the file is append-only, so a previous run's pre-check rows and dial
// failures are still in it, and classifying those would diagnose the last
// launch instead of this one. An unreadable log yields "", which classifies as
// nothing and leaves the caller with the tail alone.
func readLogFrom(path string, offset int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(f, startupLogScan))
	if err != nil {
		return ""
	}
	return string(body)
}

// logContains reports whether the log holds a marker past offset.
//
// The offset is what makes this a check rather than a formality. The log is
// opened O_APPEND and never truncated, so a successful launch leaves
// "Registered tunnel connection" in it forever -- and scanning the whole file
// meant the next launch matched that line on its first 300ms poll and declared
// success before cloudflared had done anything.
//
// This defeated the entire point of the marker. Given a bad tunnel ID
// cloudflared does not exit, it retries forever; the check existed precisely to
// catch that, and reading stale content made it weaker than the liveness test
// it replaced. The repository's own cloudflared.log carried six of these
// markers against three launches.
func logContains(path, marker string, offset int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	// Scanned rather than read whole: this runs every 300ms for up to the
	// startup grace, against a file that grows without bound.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.Contains(sc.Text(), marker) {
			return true
		}
	}
	return false
}

// logTail returns the last n lines this launch wrote, for reporting a failure
// whose only explanation is in there.
//
// Bounded to offset for the same reason classifyStartupLog is. The log is
// append-only and shared with every previous run, so a launch that died after
// three lines used to have its tail padded out with nine lines from an earlier
// one -- on the exit path, often a "Registered tunnel connection" that flatly
// contradicts the headline above it. Evidence from a run that is over is not
// evidence about this one, and the tail is printed as evidence.
//
// offset outside the file means it shrank underneath us (rotated, or replaced);
// there is then no earlier run left to confuse this one with, so the whole file
// is the tail.
func logTail(path string, offset int64, n int) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf(i18n.T("（无法读取日志 %s: %v）", "(cannot read log %s: %v)"), path, err)
	}
	if offset > 0 && offset <= int64(len(body)) {
		body = body[offset:]
	}
	text := strings.TrimRight(string(body), "\r\n")
	if text == "" {
		// Stated rather than left as an empty block: that cloudflared wrote
		// nothing at all is the finding. It never reached the point of
		// logging, which points at the binary or its arguments, not the
		// network -- and an empty block reads as a rendering bug instead.
		return "  " + i18n.T("（本次启动没有写入任何日志）", "(this launch wrote nothing to the log)")
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "  " + strings.Join(lines, "\n  ")
}

// openLog creates the file a detached child writes to.
func (m *Manager) openLog() (string, *os.File, int64, error) {
	dir := m.StateDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, 0, fmt.Errorf(i18n.T("创建状态目录失败: %w", "creating the state directory failed: %w"), err)
	}
	// Absolute: a relative path recorded here would point somewhere else when
	// status is run from another directory.
	abs, err := filepath.Abs(filepath.Join(dir, "cloudflared.log"))
	if err != nil {
		abs = filepath.Join(dir, "cloudflared.log")
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, 0, fmt.Errorf(i18n.T("打开日志文件失败: %w", "opening the log file failed: %w"), err)
	}

	// Where this launch's output will begin. Appending keeps the history of
	// previous runs, which is useful; scanning it for a success marker is not.
	// Everything confirmStarted reads must start here.
	size := int64(0)
	if st, serr := f.Stat(); serr == nil {
		size = st.Size()
	}
	return abs, f, size, nil
}

// Down stops a tunnel this package started.
//
// It refuses to act on an unverifiable PID. Killing by number alone is how a
// stale record ends up terminating whatever unrelated program inherited it.
func (m *Manager) Down(ctx context.Context) (Status, error) {
	rec, err := readRecord(m.StateDir)
	if err != nil {
		if errors.Is(err, errNoRecord) {
			// The record lives in the state directory, which defaults to the
			// working directory. Starting with -detach in one directory and
			// stopping from another is an easy and invisible way to land here,
			// so the likely cause is named rather than left to be deduced from
			// a message about a missing record.
			return m.localStatus(), fmt.Errorf(i18n.T(
				"在 %s 下没有找到由 slimproxy 启动的隧道记录。\n若当初是在别的目录用 -detach 启动的，请回到那个目录运行，或用 -state 指向记录所在目录；若 cloudflared 是手动启动的，请手动停止它",
				"no tunnel record started by slimproxy found under %s.\nIf it was started with -detach from another directory, run from there or point -state at the record's directory; if cloudflared was started by hand, stop it by hand"),
				m.stateDirOrDot())
		}
		return m.localStatus(), fmt.Errorf(i18n.T("%w；未终止任何进程，请检查该文件", "%w; no process was terminated -- inspect that file"), err)
	}

	// Opened before verifying: on Windows this is an OpenProcess handle, and
	// holding one prevents the PID from being recycled for the duration of the
	// check, closing the verify-then-kill window entirely. On Unix it is inert.
	proc, ferr := os.FindProcess(rec.PID)

	switch id, verr := m.identityOf(rec); id {
	case identityGone:
		removeRecord(m.StateDir)
		// Cleaning up a stale record is success, but it is not the same event
		// as stopping a tunnel, and the caller must be able to tell them
		// apart. Reporting both as "隧道已停止" told an operator they had just
		// closed the public entry point when the process had died on its own
		// some time earlier -- and, if a second unmanaged cloudflared was
		// serving the same hostname, while it was still up.
		//
		// Built before localStatus, whose reading is taken after the record is
		// gone and therefore has nothing left to describe.
		st := m.localStatus()
		st.Detail = fmt.Sprintf(i18n.T(
			"记录中的 cloudflared（PID %d）此前已自行退出，本次未终止任何进程；记录已清理",
			"the recorded cloudflared (PID %d) had already exited on its own; nothing was terminated this time and the record is cleaned up"), rec.PID)
		return st, nil
	case identityMismatch:
		removeRecord(m.StateDir)
		return m.localStatus(), fmt.Errorf(i18n.T("未终止任何进程：%w（记录已清理）", "no process was terminated: %w (record cleaned up)"), verr)
	case identityUnknown:
		return m.localStatus(), fmt.Errorf(i18n.T(
			"无法确认 PID %d 的身份，拒绝终止以免杀错进程: %w",
			"cannot confirm the identity of PID %d; refusing to terminate lest the wrong process be killed: %w"),
			rec.PID, verr)
	}

	if ferr != nil {
		return m.localStatus(), fmt.Errorf(i18n.T("无法获取进程 %d: %w", "cannot find process %d: %w"), rec.PID, ferr)
	}
	if err := proc.Kill(); err != nil {
		return m.localStatus(), fmt.Errorf(i18n.T("终止 cloudflared（PID %d）失败: %w", "terminating cloudflared (PID %d) failed: %w"), rec.PID, err)
	}
	if err := killAndConfirm(ctx, m, rec); err != nil {
		return m.localStatus(), err
	}
	removeRecord(m.StateDir)
	return m.localStatus(), nil
}

// reap consumes a child's exit status.
//
// Only meaningful on Unix, where an unreaped terminated child becomes a zombie
// -- and a zombie is exactly what breaks the identity check: it keeps a process
// table entry with its original name, so queryProcessUnix reports it as present
// and verifyRecord returns identityMatch. killAndConfirm then polls for five
// seconds and reports "已发送终止信号，但 PID N 仍在运行", sending the operator
// to hunt a `cloudflared <defunct>` that no kill can remove.
//
// The success path always did this; the two failure paths did not, which is how
// the wrong diagnosis ended up attached to the failure cases specifically.
func reap(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}

// killAndConfirm waits until the process is verifiably gone.
//
// Reporting success on a signal that was merely sent is not the same as the
// process having stopped.
func killAndConfirm(ctx context.Context, m *Manager, rec record) error {
	if proc, err := os.FindProcess(rec.PID); err == nil {
		_ = proc.Kill()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// A recycled PID also means our process is gone: the number now belongs
		// to something else, so waiting for identityGone alone would spin until
		// the timeout and then report a still-running process that is dead.
		if id, _ := m.identityOf(rec); id == identityGone || id == identityMismatch {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return fmt.Errorf(i18n.T("已发送终止信号，但 PID %d 仍在运行", "termination signal sent, but PID %d is still running"), rec.PID)
}
