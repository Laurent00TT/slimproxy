package proxy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// syncBuffer is written by logrus and read by the test, so it needs its own
// lock -- logrus holds the logger mutex while writing, not this one.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// restoreGlobalLogger puts the process logger back, since these tests move it.
func restoreGlobalLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetLevel(log.InfoLevel)
		pinnedLogOutput.Store(nil)
	})
}

// fastPin shortens the loop so a test does not wait five seconds.
func fastPin(t *testing.T) {
	t.Helper()
	previous := logPinInterval
	logPinInterval = 20 * time.Millisecond
	t.Cleanup(func() { logPinInterval = previous })
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestLogOutputSurvivesTakeover is the regression.
//
// CLIProxyAPI reconfigures the global logger on a config reload
// (server_reload.go calls logging.ConfigureLogOutput, which ends in
// log.SetOutput). setupLogging runs once in Build and has no reload hook, so
// the takeover used to be permanent: every subsequent line went to
// logs/main.log and `slimproxy > run.log` collected nothing, with no message
// saying why.
func TestLogOutputSurvivesTakeover(t *testing.T) {
	restoreGlobalLogger(t)
	fastPin(t)

	ours := &syncBuffer{}
	setLogOutput(ours)

	// Exactly what upstream does to us.
	theirs := &syncBuffer{}
	log.SetOutput(theirs)

	// Confirm the takeover actually took, or the rest proves nothing.
	log.Info("during-takeover")
	if !strings.Contains(theirs.String(), "during-takeover") {
		t.Fatal("测试没有真的模拟到劫持，后续断言无意义")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go keepLogOutputPinned(ctx)

	restored := waitFor(t, 2*time.Second, func() bool {
		log.Info("after-pin")
		return strings.Contains(ours.String(), "after-pin")
	})
	if !restored {
		t.Error("日志被上游改道后没有恢复：`slimproxy > run.log` 会从此收不到任何东西")
	}
}

// TestPinLoopStopsWithContext.
//
// A goroutine that outlives Run would keep taking the logger mutex, and in a
// process that starts and stops the proxy repeatedly -- tests, an embedding
// host -- they would accumulate and fight over the destination.
func TestPinLoopStopsWithContext(t *testing.T) {
	restoreGlobalLogger(t)
	fastPin(t)

	ours := &syncBuffer{}
	setLogOutput(ours)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { keepLogOutputPinned(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消 context 后 pin 循环没有退出")
	}

	// And it really has let go: a later takeover is not undone.
	theirs := &syncBuffer{}
	log.SetOutput(theirs)
	time.Sleep(5 * logPinInterval)
	log.Info("after-stop")
	if strings.Contains(ours.String(), "after-stop") {
		t.Error("循环已退出却仍在改写日志目的地")
	}
}

// TestSetupLoggingRegistersItsDestination.
//
// The loop restores whatever setLogOutput last recorded, so a SetOutput that
// bypassed it would leave the loop reasserting a stale destination -- the log
// would work at first and then move, which is worse than the bug being fixed.
// This pins that setupLogging goes through the recording path for the file
// case; the stdout case is covered below.
func TestSetupLoggingRegistersItsDestination(t *testing.T) {
	restoreGlobalLogger(t)
	pinnedLogOutput.Store(nil)

	dir := t.TempDir()
	c := &Config{LogToFile: true, LogDir: dir}
	if _, closer, err := setupLogging(c); err != nil {
		t.Fatalf("setupLogging: %v", err)
	} else {
		defer func() { _ = closer.Close() }()
	}

	if pinnedLogOutput.Load() == nil {
		t.Fatal("setupLogging 没有登记日志目的地：pin 循环无从恢复")
	}

	// The recorded destination must be the real one, not a placeholder: writing
	// through it has to reach the log file.
	log.Info("registered-destination-probe")
	body, err := os.ReadFile(filepath.Join(dir, "slimproxy.log"))
	if err != nil {
		t.Fatalf("读取日志文件: %v", err)
	}
	if !strings.Contains(string(body), "registered-destination-probe") {
		t.Error("登记的目的地与实际写入的文件不一致")
	}
}

// TestStdoutModeIsAlsoPinned covers the branch with no log file at all, which
// is the default and the one where a silent redirect is hardest to notice --
// there is no file to check, only an empty terminal.
func TestStdoutModeIsAlsoPinned(t *testing.T) {
	restoreGlobalLogger(t)
	pinnedLogOutput.Store(nil)

	if _, _, err := setupLogging(&Config{LogToFile: false}); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	if pinnedLogOutput.Load() == nil {
		t.Error("stdout 模式没有登记目的地：被改道后无法恢复")
	}
}
