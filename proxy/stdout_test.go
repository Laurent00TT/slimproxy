package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTakeStdoutCatchesBareFmtPrintf.
//
// The dashboard cannot coexist with anything writing to stdout, and CLIProxyAPI
// writes two lines with a bare fmt.Printf that no logger configuration can
// reach: the listener announcement, and one per watcher reload -- which fires
// every time the credential auto-refresh rewrites a token, roughly every 15
// minutes. Each lands inside a rendered frame and stays there.
//
// So this asserts the redirection at the level it has to work at: the process's
// os.Stdout, not a logger.
//
// Not parallel, and must not be made parallel: it swaps a process-global.
func TestTakeStdoutCatchesBareFmtPrintf(t *testing.T) {
	dir := t.TempDir()

	real, restore, err := TakeStdout(dir)
	if err != nil {
		t.Fatalf("TakeStdout: %v", err)
	}
	if real == nil {
		t.Fatal("未返回原始 stdout，面板将无处绘制")
	}
	if real == os.Stdout {
		t.Fatal("os.Stdout 未被替换，裸 fmt.Printf 仍会打进面板")
	}

	const marker = "API server started successfully on: 127.0.0.1:8317"
	fmt.Printf("%s\n", marker)
	fmt.Println("server clients and configuration updated: 1 clients")

	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if os.Stdout != real {
		t.Error("restore 之后 os.Stdout 未恢复")
	}

	body, err := os.ReadFile(filepath.Join(dir, StrayStdoutName))
	if err != nil {
		t.Fatalf("读取 %s: %v", StrayStdoutName, err)
	}
	if !strings.Contains(string(body), marker) {
		t.Errorf("裸 fmt.Printf 的输出未被捕获，文件内容:\n%s", body)
	}
	if !strings.Contains(string(body), "server clients") {
		t.Errorf("watcher 重载行未被捕获，文件内容:\n%s", body)
	}
}

// TestTakeStdoutRefusesWithoutADestination pins that stdout is never taken away
// with nowhere to put it -- that would be silent discard, which is worse than
// the mess it prevents.
func TestTakeStdoutRefusesWithoutADestination(t *testing.T) {
	before := os.Stdout
	if _, _, err := TakeStdout(""); err == nil {
		t.Error("没有日志目录时应拒绝接管 stdout，否则输出会被静默丢弃")
	}
	if os.Stdout != before {
		os.Stdout = before
		t.Error("失败时不应留下被替换的 os.Stdout")
	}
}

// TestWithoutStdoutTurnsOnFileLogging pins the other half: taking stdout away
// from logrus without giving it a file would discard every log line.
func TestWithoutStdoutTurnsOnFileLogging(t *testing.T) {
	c := Config{Port: 8317, APIKeys: []string{"k"}, LogDir: t.TempDir()}
	if c.LogToFile {
		t.Fatal("前提错误：本测试要求初始为 false")
	}

	dir, closer, err := setupLogging(&c, withoutStdout())
	if err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	defer func() { _ = closer.Close() }()

	if !c.LogToFile {
		t.Error("withoutStdout 应自动开启文件日志，否则日志无处可去")
	}
	if dir == "" {
		t.Error("应返回日志目录，供调用方告知操作者")
	}
}
