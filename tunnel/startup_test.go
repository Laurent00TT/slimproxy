package tunnel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSuccessMarkerFromAPreviousRunIsNotEvidence.
//
// The log is opened O_APPEND and never truncated, so a successful launch leaves
// "Registered tunnel connection" in it permanently. Scanning the whole file
// made the next launch match that line on its first poll and report success
// before cloudflared had done anything at all.
//
// That defeated the check entirely: given a bad tunnel ID cloudflared does not
// exit, it retries forever, and catching exactly that is why the marker check
// replaced a liveness test. The repository's own cloudflared.log carried six
// markers against three launches.
func TestSuccessMarkerFromAPreviousRunIsNotEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloudflared.log")
	previous := "2026-01-01 INF Starting tunnel\n" +
		"2026-01-01 INF Registered tunnel connection connIndex=0\n"
	if err := os.WriteFile(path, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	offset := int64(len(previous))

	// Premise: the marker really is in the file.
	if !logContains(path, connectedMarker, 0) {
		t.Fatal("前提不成立：文件里没有标记，测试无意义")
	}

	if logContains(path, connectedMarker, offset) {
		t.Error("上一次运行留下的成功标记被当成了本次的启动证据")
	}

	// Now this run actually connects.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("2026-01-02 INF Registered tunnel connection connIndex=0\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if !logContains(path, connectedMarker, offset) {
		t.Error("本次运行写入的标记没有被识别")
	}
}

// TestOpenLogReportsWhereThisRunBegins pins the other half: the offset must be
// the existing size, or the guard above has nothing to work with.
func TestOpenLogReportsWhereThisRunBegins(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{StateDir: dir}

	path, f, start, err := m.openLog()
	if err != nil {
		t.Fatalf("openLog: %v", err)
	}
	if start != 0 {
		t.Errorf("新文件的起点应为 0，实际 %d", start)
	}
	if _, err := f.WriteString("first run output\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	_, f2, start2, err := m.openLog()
	if err != nil {
		t.Fatalf("openLog 第二次: %v", err)
	}
	defer func() { _ = f2.Close() }()

	if start2 != info.Size() {
		t.Errorf("第二次的起点 = %d，应为已有文件大小 %d", start2, info.Size())
	}
	if start2 == 0 {
		t.Error("起点为 0 意味着会重新扫描上一次的输出")
	}
}

// TestLogContainsHandlesAMissingFile: the log may not exist yet on the first
// poll, and that is not an error worth failing a launch over.
func TestLogContainsHandlesAMissingFile(t *testing.T) {
	if logContains(filepath.Join(t.TempDir(), "absent.log"), connectedMarker, 0) {
		t.Error("不存在的日志不应报告命中")
	}
}
