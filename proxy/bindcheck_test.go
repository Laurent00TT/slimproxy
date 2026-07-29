package proxy

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestEnsureCanBindNamesTheLikelyCause.
//
// Starting must not proceed when the port is already held. The failure it
// prevents is specific: CLIProxyAPI reports a bind error only after printing
// "API server started successfully" and loading every credential, so in panel
// mode the sequence renders as a dashboard that appears and vanishes a second
// later -- a crash, as far as the operator can tell, with the actual error
// scrolling past after the alternate screen has already gone.
func TestEnsureCanBindNamesTheLikelyCause(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法建立测试监听: %v", err)
	}
	defer func() { _ = ln.Close() }()

	err = ensureCanBind(ln.Addr().String())
	if err == nil {
		t.Fatal("端口已被占用时应报错，否则面板会先占屏再消失")
	}
	if !strings.Contains(err.Error(), "slimproxy status") {
		t.Errorf("错误信息应告诉操作者怎么查: %v", err)
	}
	if !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("错误信息应点名是哪个地址: %v", err)
	}
}

// TestEnsureCanBindAllowsAFreePort pins that the check does not itself hold the
// port it just tested -- the proxy binds it moments later.
func TestEnsureCanBindAllowsAFreePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法建立测试监听: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if err := ensureCanBind(addr); err != nil {
		t.Fatalf("空闲端口应通过: %v", err)
	}
	// Bindable again immediately: the check must release what it probed.
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("探测之后端口仍被占用，说明检查没有释放它: %v", err)
	}
	_ = again.Close()
}

// TestBuildDoesNotMaterializeWhenThePortIsTaken is the reason the check sits in
// Build rather than in the callers.
//
// A second instance on a busy port is doomed, but it used to do damage on the
// way down: materialize rewrites the effective configuration, the upstream
// watcher reloads that file, and so the settings of the instance that could not
// start became the settings of the instance already serving. Panel mode and
// -no-tui differ in exactly one materialized field (log-to-file, forced on by
// the panel), which is enough to redirect a running proxy's logs.
//
// The port being taken is checked by observing that no effective config was
// written -- the artifact, not the code path.
func TestBuildDoesNotMaterializeWhenThePortIsTaken(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法建立测试监听: %v", err)
	}
	defer func() { _ = held.Close() }()

	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })

	stateDir := t.TempDir()
	c := Config{
		Host:    "127.0.0.1",
		Port:    held.Addr().(*net.TCPAddr).Port,
		APIKeys: []string{"k"},
		AuthDir: filepath.Join(t.TempDir(), "auths"),
		LogDir:  t.TempDir(),
	}

	if _, err := Build(c, stateDir); err == nil {
		t.Fatal("端口被占用时 Build 应当失败")
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			t.Errorf("绑不上端口却写出了 %s：正在服役的实例会热重载这个文件", e.Name())
		}
	}
}
