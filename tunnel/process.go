// Package tunnel manages the cloudflared child process that exposes slimproxy.
//
// It owns three things: knowing whether a tunnel is running, starting one, and
// stopping it. Everything it does to a process is guarded by an identity check
// -- see verifyRecord for why a bare PID is not enough to justify a kill.
package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/Laurent00TT/slimproxy/fsperm"
)

// pidFileName is where a detached tunnel records itself, inside the state dir.
const pidFileName = "cloudflared.pid.json"

// record is what a detached launch writes down.
//
// The PID alone is not enough to stop it later: PIDs are reused, so a stale
// record can point at an unrelated process that the operating system handed the
// same number. Binary and Started exist to detect exactly that, and both are
// read by verifyRecord.
type record struct {
	PID int `json:"pid"`
	// Started is when this process was launched. A process whose creation time
	// predates this cannot be the one we started, however matching its PID.
	Started time.Time `json:"started"`
	// Tunnel names which tunnel it serves. Compared against the configuration
	// so a config edited after launch does not make status describe the wrong
	// tunnel.
	Tunnel string `json:"tunnel"`
	// Binary is the cloudflared path that was launched, matched exactly against
	// the running image name rather than by substring.
	Binary string `json:"binary"`
	// LogPath is where the child's output goes. Absolute, so a status run from
	// another directory prints somewhere real.
	LogPath string `json:"log_path"`
}

func recordPath(stateDir string) string {
	if stateDir == "" {
		stateDir = "."
	}
	return filepath.Join(stateDir, pidFileName)
}

// errNoRecord means no record file exists -- a legitimate state.
var errNoRecord = errors.New("no pid record")

// readRecord returns the record, or an error distinguishing "absent" from
// "unreadable".
//
// Three outcomes, not two. Collapsing a corrupt record into "absent" would hide
// a live managed child: status would fall through to the remote probe and call
// it unmanaged, and down would say "no record" while the file sits right there.
// tunnel/config.go argues the same point for the cloudflared configuration; the
// discipline has to hold here too.
func readRecord(stateDir string) (record, error) {
	body, err := os.ReadFile(recordPath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return record{}, errNoRecord
		}
		return record{}, fmt.Errorf("无法读取 PID 记录: %w", err)
	}
	var r record
	if err := json.Unmarshal(body, &r); err != nil {
		return record{}, fmt.Errorf("PID 记录已损坏: %w", err)
	}
	if r.PID <= 0 {
		return record{}, fmt.Errorf("PID 记录中的 PID 非法: %d", r.PID)
	}
	return r, nil
}

// claimRecord creates the record file exclusively, reserving the right to run.
//
// O_EXCL makes this the mutual exclusion between two concurrent `up` calls
// sharing a state directory. Without it both see "no record", both spawn, and
// the second write orphans the first child permanently.
func claimRecord(stateDir string) (*os.File, error) {
	if stateDir == "" {
		stateDir = "."
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建状态目录失败: %w", err)
	}
	path := recordPath(stateDir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("已存在 PID 记录，可能有另一个实例正在启动或运行")
		}
		return nil, fmt.Errorf("无法创建 PID 记录: %w", err)
	}
	// The record names the tunnel and the binary, and `down` trusts it before
	// terminating a process. On Windows the 0600 above sets an attribute and
	// leaves the parent's inherited ACEs, so another local account could
	// rewrite it.
	if rerr := fsperm.Restrict(path); rerr != nil {
		log.Warnf("slimproxy: 未能收紧 PID 记录 %s 的权限: %v", path, rerr)
	}
	return f, nil
}

func writeRecordTo(f *os.File, r record) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		return err
	}
	return f.Sync()
}

// writeRecord replaces the record. Used by tests and by recovery paths.
func writeRecord(stateDir string, r record) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(recordPath(stateDir), body, 0o600)
}

func removeRecord(stateDir string) {
	_ = os.Remove(recordPath(stateDir))
}

// identity is the outcome of checking whether a PID is still our process.
type identity int

const (
	// identityMatch: the PID is alive and is the process we started.
	identityMatch identity = iota
	// identityGone: nothing is running under that PID.
	identityGone
	// identityMismatch: something is running under that PID, but it is not our
	// process. The PID was recycled.
	identityMismatch
	// identityUnknown: the check could not be performed. Callers must not kill
	// on this outcome -- an unverifiable PID is exactly when killing is most
	// likely to hit the wrong process.
	identityUnknown
)

// procInfo is what the operating system reports about a live process.
type procInfo struct {
	Name string
	// Created is the process start time. Zero when the platform query could not
	// supply it, which downgrades the reuse check rather than failing it.
	Created time.Time
}

// verifyRecord reports whether rec.PID is still the process rec describes.
//
// A PID is not a handle. Between writing the record and reading it back the
// process may have exited and the number been reassigned, so stopping a tunnel
// by PID alone can terminate an unrelated program. Two independent facts are
// checked: the image name must equal the binary we launched, and the process
// must not predate the launch we recorded.
func verifyRecord(rec record) (identity, error) {
	info, present, err := queryProcess(rec.PID)
	switch {
	case err != nil:
		return identityUnknown, err
	case !present:
		return identityGone, nil
	}

	want := expectedImageName(rec.Binary)
	if !binaryMatches(info.Name, want) {
		return identityMismatch, fmt.Errorf("PID %d 现在是 %q，不是 %q", rec.PID, info.Name, want)
	}

	// A process that started before we launched ours cannot be ours, whatever
	// its name. This is what actually closes the reuse window; the name check
	// alone cannot, because the replacement may happen to be cloudflared too.
	if !info.Created.IsZero() && !rec.Started.IsZero() {
		if info.Created.Before(rec.Started.Add(-30 * time.Second)) {
			return identityMismatch, fmt.Errorf(
				"PID %d 的启动时间(%s)早于记录(%s)，是被复用的 PID",
				rec.PID, info.Created.Format(time.RFC3339), rec.Started.Format(time.RFC3339))
		}
	}
	return identityMatch, nil
}

// expectedImageName is the process image we expect for a recorded binary path.
func expectedImageName(binary string) string {
	if name := filepath.Base(binary); name != "" && name != "." && name != string(filepath.Separator) {
		return name
	}
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

// binaryMatches reports whether a running image is the one we launched.
//
// Exact comparison, not substring. "cloudflared-old.exe",
// "my-cloudflared-wrapper.exe" and "cloudflaredX.exe" are all different
// programs, and a substring test would authorise killing any of them when a
// recycled PID happens to land on one. Case-insensitive because Windows image
// names are.
func binaryMatches(running, want string) bool {
	return strings.EqualFold(running, want)
}

// queryProcess asks the operating system about a PID.
//
// present=false means no such process. An error means the question could not be
// asked, which callers must not treat as either answer.
//
// Implemented by shelling out because Go's standard library has no portable
// process query: os.FindProcess succeeds unconditionally on Windows.
func queryProcess(pid int) (procInfo, bool, error) {
	if runtime.GOOS == "windows" {
		return queryProcessWindows(pid)
	}
	return queryProcessUnix(pid)
}

func queryProcessWindows(pid int) (procInfo, bool, error) {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return procInfo{}, false, fmt.Errorf("tasklist 查询失败: %w", err)
	}
	name, present := parseTasklistRow(string(out), pid)
	if !present {
		return procInfo{}, false, nil
	}
	// tasklist does not report a start time. Best effort via WMI; its absence
	// only downgrades the reuse check, so a failure here is not an error.
	return procInfo{Name: name, Created: windowsCreationTime(pid)}, true, nil
}

// parseTasklistRow extracts the image name for pid from tasklist CSV output.
//
// Language-independent by construction: rather than blacklisting the localised
// "no tasks" message, it requires a well-formed row whose PID column matches.
// A German Windows prints "INFORMATION:" where an English one prints "INFO:",
// and a substring blacklist would parse that sentence as a process name.
func parseTasklistRow(text string, pid int) (name string, present bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "\"") {
			continue
		}
		fields := splitCSVLine(line)
		// Real rows are: name, pid, session, session#, memory.
		if len(fields) < 5 {
			continue
		}
		if fields[1] != strconv.Itoa(pid) {
			continue
		}
		return fields[0], true
	}
	return "", false
}

// splitCSVLine splits a simple quoted CSV row. tasklist never emits embedded
// quotes, so a full CSV parser would be more machinery than the format needs.
func splitCSVLine(line string) []string {
	parts := strings.Split(line, "\",\"")
	for i := range parts {
		parts[i] = strings.Trim(parts[i], "\"")
	}
	return parts
}

// windowsCreationTime returns the process start time, or the zero time when it
// cannot be determined.
func windowsCreationTime(pid int) time.Time {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("(Get-CimInstance Win32_Process -Filter \"ProcessId=%d\").CreationDate.ToString('o')", pid)).Output()
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}
	}
	return t
}

func queryProcessUnix(pid int) (procInfo, bool, error) {
	// Two -o flags, never "-o comm=,lstart=". Linux procps treats everything
	// after the first "=" as literal header text -- comma included -- so the
	// combined form prints only the comm column, under a header of ",lstart=".
	// Parsing that output made fields[0] the header fragment, verifyRecord
	// reported identityMismatch against our own child, and `tunnel down`
	// refused to stop a tunnel it had started. Separate flags request two
	// columns with two (empty) headers on every ps lineage.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=", "-o", "lstart=").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return procInfo{}, false, nil // no such process
		}
		return procInfo{}, false, fmt.Errorf("ps 查询失败: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return procInfo{}, false, nil
	}
	fields := strings.Fields(line)
	info := procInfo{Name: fields[0]}
	if len(fields) > 1 {
		// ps lstart format: "Mon Jan  2 15:04:05 2006"
		if t, perr := time.ParseInLocation("Mon Jan _2 15:04:05 2006",
			strings.Join(fields[1:], " "), time.Local); perr == nil {
			info.Created = t
		}
	}
	return info, true, nil
}
