package tunnel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testManager returns a Manager whose external dependencies are all stubbed, so
// what is exercised is this package's decisions rather than the host's
// cloudflared, network, or process table.
func testManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		StateDir: t.TempDir(),
		Binary:   "cloudflared",
		readConfig: func() (Config, bool, error) {
			return Config{Tunnel: "cfg-tunnel", Hostnames: []string{"h.example.com"}}, true, nil
		},
		connections: func(context.Context, string) (Connectivity, error) {
			return Connectivity{Parsed: true, Count: 0}, nil
		},
		verify: func(record) (identity, error) { return identityGone, nil },
	}
}

func seed(t *testing.T, m *Manager, rec record) {
	t.Helper()
	if rec.PID == 0 {
		rec.PID = 4242
	}
	if rec.Started.IsZero() {
		rec.Started = time.Now()
	}
	if err := writeRecord(m.StateDir, rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func hasRecord(dir string) bool {
	_, err := readRecord(dir)
	return err == nil
}

// ---------- Down: the refusal rules ----------

// TestDownRefusesToKillRecycledPID is the reason identity checking exists. A
// stale record points at a PID the operating system has since handed to
// something else; killing by number alone terminates an unrelated program.
func TestDownRefusesToKillRecycledPID(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 424242, Binary: "cloudflared"})
	m.verify = func(record) (identity, error) {
		return identityMismatch, errors.New(`PID 424242 现在是 "postgres.exe"`)
	}

	_, err := m.Down(context.Background())
	if err == nil {
		t.Fatal("Down reported success on a recycled PID")
	}
	if !strings.Contains(err.Error(), "未终止任何进程") {
		t.Errorf("error does not make clear nothing was killed: %v", err)
	}
	if hasRecord(m.StateDir) {
		t.Error("stale record survived")
	}
}

// TestDownRefusesUnverifiablePID: not being able to check is precisely when
// killing is most dangerous.
func TestDownRefusesUnverifiablePID(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 424243})
	m.verify = func(record) (identity, error) {
		return identityUnknown, errors.New("tasklist 查询失败")
	}

	_, err := m.Down(context.Background())
	if err == nil {
		t.Fatal("Down proceeded despite being unable to verify the process")
	}
	if !strings.Contains(err.Error(), "拒绝终止") {
		t.Errorf("error does not state the refusal: %v", err)
	}
	// Unlike a mismatch, the record is kept: it may still be valid, and
	// discarding it loses the only handle on a running child.
	if !hasRecord(m.StateDir) {
		t.Error("record discarded despite the state being unknown")
	}
}

func TestDownOnAlreadyDeadProcessSucceeds(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 424244})
	m.verify = func(record) (identity, error) { return identityGone, nil }

	if _, err := m.Down(context.Background()); err != nil {
		t.Errorf("Down on a dead process reported failure: %v", err)
	}
	if hasRecord(m.StateDir) {
		t.Error("record for a dead process survived")
	}
}

func TestDownWithoutRecordIsAnError(t *testing.T) {
	m := testManager(t)
	_, err := m.Down(context.Background())
	if err == nil {
		t.Fatal("Down without a record reported success")
	}
	if !strings.Contains(err.Error(), "手动") {
		t.Errorf("error does not mention manually started tunnels: %v", err)
	}
}

// TestDownOnCorruptRecordRefuses: a record that cannot be read is not an absent
// one, and must not be silently treated as "nothing to stop".
func TestDownOnCorruptRecordRefuses(t *testing.T) {
	m := testManager(t)
	if err := os.WriteFile(recordPath(m.StateDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := m.Down(context.Background())
	if err == nil {
		t.Fatal("corrupt record treated as success")
	}
	if !strings.Contains(err.Error(), "损坏") {
		t.Errorf("error does not identify the record as corrupt: %v", err)
	}
	if !strings.Contains(err.Error(), "未终止任何进程") {
		t.Errorf("error does not say nothing was killed: %v", err)
	}
}

// ---------- Status: every identity branch ----------

func TestStatusReportsManagedRunning(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 777, Tunnel: "cfg-tunnel", LogPath: "L"})
	m.verify = func(record) (identity, error) { return identityMatch, nil }

	st := m.Status(context.Background())
	if st.State != Running {
		t.Errorf("State = %v, want Running", st.State)
	}
	// Managed is what decides whether `down` claims it can stop this.
	if !st.Managed {
		t.Error("Managed = false for a process we started")
	}
	if st.PID != 777 {
		t.Errorf("PID = %d, want 777", st.PID)
	}
}

func TestStatusClearsStaleRecord(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 424245})
	m.verify = func(record) (identity, error) { return identityGone, nil }

	st := m.Status(context.Background())
	if hasRecord(m.StateDir) {
		t.Error("record for an exited process was kept")
	}
	if st.State == Running {
		t.Error("a dead child was reported as running")
	}
}

func TestStatusClearsRecycledRecord(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 424246})
	m.verify = func(record) (identity, error) { return identityMismatch, errors.New("是 chrome.exe") }

	st := m.Status(context.Background())
	if hasRecord(m.StateDir) {
		t.Error("record pointing at a recycled PID was kept")
	}
	if st.State == Running {
		t.Error("a recycled PID was reported as a running tunnel")
	}
}

// TestStatusKeepsRecordWhenIdentityUnknown: discarding it would lose the only
// handle on a child that may well be alive.
func TestStatusKeepsRecordWhenIdentityUnknown(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 424247})
	m.verify = func(record) (identity, error) { return identityUnknown, errors.New("查询失败") }

	st := m.Status(context.Background())
	if !hasRecord(m.StateDir) {
		t.Error("record discarded on an unverifiable identity")
	}
	if st.State != StateUnknown {
		t.Errorf("State = %v, want StateUnknown", st.State)
	}
}

// TestStatusSurvivesBrokenConfig: a running managed child must remain visible
// even when the cloudflared configuration cannot be parsed. Otherwise the next
// `up` spawns a second one and orphans the first permanently.
func TestStatusSurvivesBrokenConfig(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 888, Tunnel: "running-tunnel"})
	m.readConfig = func() (Config, bool, error) { return Config{}, false, errors.New("YAML 解析失败") }
	m.verify = func(record) (identity, error) { return identityMatch, nil }

	st := m.Status(context.Background())
	if st.PID != 888 {
		t.Errorf("a broken config hid the running child (PID = %d)", st.PID)
	}
	if !st.Managed {
		t.Error("running managed child not reported as managed")
	}
}

// TestStatusDetectsUnmanagedTunnel: no local record does not mean nothing is
// running -- a tunnel started by hand is invisible locally.
func TestStatusDetectsUnmanagedTunnel(t *testing.T) {
	m := testManager(t)
	m.connections = func(context.Context, string) (Connectivity, error) {
		return Connectivity{Parsed: true, Count: 2}, nil
	}
	st := m.Status(context.Background())
	if st.State != Running {
		t.Errorf("State = %v, want Running for an externally started tunnel", st.State)
	}
	if st.Managed {
		t.Error("an externally started tunnel was reported as managed")
	}
	if st.Connections != 2 || !st.ConnectionsKnown {
		t.Errorf("connection count not carried: %d (known=%v)", st.Connections, st.ConnectionsKnown)
	}
}

// TestStatusUnknownWhenProbeFails: a failed query is not evidence that nothing
// is running.
func TestStatusUnknownWhenProbeFails(t *testing.T) {
	m := testManager(t)
	m.connections = func(context.Context, string) (Connectivity, error) {
		return Connectivity{}, errors.New("网络不可达")
	}
	st := m.Status(context.Background())
	if st.State != StateUnknown {
		t.Errorf("State = %v, want StateUnknown when the probe failed", st.State)
	}
}

// TestStatusReportsTunnelDrift: after the config is edited, status must name
// the tunnel actually being served, not the one now configured.
func TestStatusReportsTunnelDrift(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 999, Tunnel: "old-tunnel"})
	m.verify = func(record) (identity, error) { return identityMatch, nil }

	st := m.Status(context.Background())
	if st.TunnelID != "old-tunnel" {
		t.Errorf("TunnelID = %q, want the running tunnel %q", st.TunnelID, "old-tunnel")
	}
	if st.ConfiguredTunnelID != "cfg-tunnel" {
		t.Errorf("ConfiguredTunnelID = %q, want %q", st.ConfiguredTunnelID, "cfg-tunnel")
	}
	if st.Detail == "" {
		t.Error("drift not explained to the operator")
	}
}

// ---------- Up ----------

// TestUpRefusesWithoutTunnelID is C2: a config that failed to parse yields an
// empty ID, and `cloudflared tunnel run ""` would fall back to reading that
// same unparseable file -- discarding the diagnosis already made.
func TestUpRefusesWithoutTunnelID(t *testing.T) {
	m := testManager(t)
	m.readConfig = func() (Config, bool, error) { return Config{}, false, errors.New("YAML 解析失败") }

	_, err := m.Up(context.Background(), true, nil, nil)
	if err == nil {
		t.Fatal("Up proceeded with no tunnel ID")
	}
	if !strings.Contains(err.Error(), "无法确定隧道 ID") {
		t.Errorf("error does not name the cause: %v", err)
	}
	// The original diagnosis must survive, not be replaced by a generic message.
	if !strings.Contains(err.Error(), "YAML") {
		t.Errorf("the underlying config error was discarded: %v", err)
	}
}

func TestUpRefusesWhenAlreadyManaged(t *testing.T) {
	m := testManager(t)
	seed(t, m, record{PID: 555, Tunnel: "cfg-tunnel"})
	m.verify = func(record) (identity, error) { return identityMatch, nil }

	_, err := m.Up(context.Background(), true, nil, nil)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("err = %v, want ErrAlreadyRunning", err)
	}
}

// TestUpProceedsWhenUnmanagedInstanceExists: cloudflared supports several
// connectors per tunnel as high availability, so this is a warning rather than
// a refusal -- but the warning must be carried back to the caller.
func TestUpProceedsWhenUnmanagedInstanceExists(t *testing.T) {
	m := testManager(t)
	m.connections = func(context.Context, string) (Connectivity, error) {
		return Connectivity{Parsed: true, Count: 1}, nil
	}
	m.Binary = filepath.Join(t.TempDir(), "definitely-not-here")

	st, err := m.Up(context.Background(), true, nil, nil)
	if errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("refused to start alongside an unmanaged connector")
	}
	if st.Detail == "" {
		t.Error("proceeded without telling the caller a second connector is being added")
	}
}

func TestUpWithoutConfigIsNotConfigured(t *testing.T) {
	m := testManager(t)
	m.readConfig = func() (Config, bool, error) { return Config{}, false, nil }

	_, err := m.Up(context.Background(), true, nil, nil)
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

// TestUpRefusesConcurrentLaunch: the record file is the mutual exclusion
// between two `up` calls sharing a state directory. Without it both spawn and
// the second write orphans the first child.
func TestUpRefusesConcurrentLaunch(t *testing.T) {
	m := testManager(t)
	// A record whose process cannot be verified: Status leaves it in place, so
	// the claim below must find the file already there.
	seed(t, m, record{PID: 12345})
	m.verify = func(record) (identity, error) { return identityUnknown, errors.New("查询失败") }

	if _, err := claimRecord(m.StateDir); err == nil {
		t.Fatal("claimRecord succeeded while a record already exists")
	}
}

// ---------- records ----------

func TestRecordRoundTripAndCorruption(t *testing.T) {
	dir := t.TempDir()
	in := record{PID: 1234, Started: time.Now().Truncate(time.Second), Tunnel: "t", Binary: "b", LogPath: "l"}
	if err := writeRecord(dir, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := readRecord(dir)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if out.PID != in.PID || out.Tunnel != in.Tunnel || out.LogPath != in.LogPath {
		t.Errorf("round trip lost data: %+v vs %+v", out, in)
	}

	// Absent and corrupt must be distinguishable; conflating them hides a live
	// child behind "no record".
	if _, err := readRecord(t.TempDir()); !errors.Is(err, errNoRecord) {
		t.Errorf("absent record error = %v, want errNoRecord", err)
	}
	if err := os.WriteFile(recordPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = readRecordErr(dir)
	if err == nil || errors.Is(err, errNoRecord) {
		t.Errorf("corrupt record error = %v, want a distinct error", err)
	}
	if err := os.WriteFile(recordPath(dir), []byte(`{"pid":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readRecordErr(dir); err == nil {
		t.Error("record with PID 0 accepted")
	}
}

func readRecordErr(dir string) error {
	_, err := readRecord(dir)
	return err
}

func TestClaimRecordIsExclusive(t *testing.T) {
	dir := t.TempDir()
	f, err := claimRecord(dir)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := claimRecord(dir); err == nil {
		t.Error("second claim succeeded; two concurrent launches would both proceed")
	}
}

// ---------- identity ----------

// TestVerifyRecordRejectsWrongBinary: substring matching would authorise
// killing "cloudflared-old.exe" or "my-cloudflared-wrapper.exe".
func TestVerifyRecordRejectsWrongBinary(t *testing.T) {
	// Exercised through the real query against this test binary, which is
	// certainly alive and certainly not cloudflared.
	id, err := verifyRecord(record{PID: os.Getpid(), Binary: "cloudflared.exe", Started: time.Now()})
	if err != nil && id == identityUnknown {
		t.Skipf("process query unavailable in this environment: %v", err)
	}
	if id != identityMismatch {
		t.Errorf("verifyRecord(self) = %v, want identityMismatch", id)
	}
}

func TestVerifyRecordOnAbsentPID(t *testing.T) {
	const absent = 999999
	id, err := verifyRecord(record{PID: absent, Binary: "cloudflared", Started: time.Now()})
	if err != nil && id == identityUnknown {
		t.Skipf("process query unavailable: %v", err)
	}
	if id == identityMatch {
		t.Skip("PID 999999 happens to be cloudflared here")
	}
	if id != identityGone {
		t.Errorf("verifyRecord(absent) = %v, want identityGone", id)
	}
}

func TestStateStringsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []State{NotConfigured, Configured, Running, StateUnknown} {
		if seen[s.String()] {
			t.Errorf("duplicate description for %v", s)
		}
		seen[s.String()] = true
	}
}

// TestBinaryMatchIsExactNotSubstring is I6: a substring test would authorise
// killing a differently-named program when a recycled PID lands on one.
func TestBinaryMatchIsExactNotSubstring(t *testing.T) {
	if !binaryMatches("cloudflared.exe", "cloudflared.exe") {
		t.Error("identical names did not match")
	}
	// Windows image names are case-insensitive.
	if !binaryMatches("CloudflareD.EXE", "cloudflared.exe") {
		t.Error("case difference rejected")
	}
	// Each of these contains "cloudflared" and would pass a substring test.
	for _, running := range []string{
		"cloudflared-old.exe",
		"my-cloudflared-wrapper.exe",
		"cloudflaredX.exe",
		"not-cloudflared.exe",
	} {
		if binaryMatches(running, "cloudflared.exe") {
			t.Errorf("%q matched %q; a recycled PID on this process would be killed",
				running, "cloudflared.exe")
		}
	}
}

func TestExpectedImageNameFallsBack(t *testing.T) {
	if got := expectedImageName(`C:\tools\cloudflared.exe`); got != "cloudflared.exe" {
		t.Errorf("expectedImageName = %q, want the basename", got)
	}
	if got := expectedImageName(""); got == "" {
		t.Error("empty binary path produced no expected name")
	}
}

// TestConfirmAliveDetectsImmediateExit is C1, the worst failure this package
// could have: cmd.Start only proves a process was created, and cloudflared
// exits within a second or two on a bad credentials file or an unknown tunnel.
// Reporting success there announces a launch that already failed.
func TestConfirmAliveDetectsImmediateExit(t *testing.T) {
	m := testManager(t)
	logPath := filepath.Join(m.StateDir, "cloudflared.log")
	if err := os.WriteFile(logPath, []byte("ERR failed to unmarshal credentials\nERR exiting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.verify = func(record) (identity, error) { return identityGone, nil }

	// Offset 0: this log holds only the current run's output.
	err := m.confirmStarted(context.Background(), record{PID: 1234, LogPath: logPath}, 0)
	if err == nil {
		t.Fatal("a child that exited immediately was reported as started")
	}
	// The reason is only in the log; without it the operator gets "it failed"
	// and nothing else.
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("the log tail was not included: %v", err)
	}
}

// TestConfirmStartedAcceptsConnectedChild: success is a registered connection,
// not mere liveness.
func TestConfirmStartedAcceptsConnectedChild(t *testing.T) {
	m := testManager(t)
	logPath := filepath.Join(m.StateDir, "connected.log")
	connectedLog := `INF Starting tunnel
INF Registered tunnel connection connIndex=0 location=lax01
`
	if err := os.WriteFile(logPath, []byte(connectedLog), 0o600); err != nil {
		t.Fatal(err)
	}
	m.verify = func(record) (identity, error) { return identityMatch, nil }
	if err := m.confirmStarted(context.Background(), record{PID: 1234, LogPath: logPath}, 0); err != nil {
		t.Errorf("a connected child was rejected: %v", err)
	}
}

// TestConfirmStartedRejectsLiveButUnconnected is the case liveness checking
// misses entirely: given an unknown tunnel ID cloudflared never exits, it
// retries forever. Reporting that as a successful launch is exactly the
// failure this check exists to prevent.
func TestConfirmStartedRejectsLiveButUnconnected(t *testing.T) {
	m := testManager(t)
	logPath := filepath.Join(m.StateDir, "retrying.log")
	// Verbatim shape of a real failure: the process stays up and keeps retrying,
	// so nothing here ever says "connected".
	retryingLog := `ERR Serve tunnel error: control stream encountered a failure
INF Retrying connection in up to 2s
ERR Serve tunnel error: control stream encountered a failure
INF Retrying connection in up to 4s
`
	if err := os.WriteFile(logPath, []byte(retryingLog), 0o600); err != nil {
		t.Fatal(err)
	}
	m.verify = func(record) (identity, error) { return identityMatch, nil } // alive throughout

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := m.confirmStarted(ctx, record{PID: 1234, LogPath: logPath}, 0)
	if err == nil {
		t.Fatal("a live but never-connected child was reported as started")
	}
}
