package proxy

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// dumpCanary is planted in a request body so the assertion can be about the
// caller's content rather than about file names. A dump that lands under
// another name, or in the upstream's <auth-dir>/logs fallback, still fails.
const dumpCanary = "SLIMPROXY-GATE-CANARY-8f3a1d"

// freeTestPort returns a port nothing is listening on.
//
// Racy by construction -- the port can be taken between Close and the proxy
// binding it. Accepted because the alternative is a fixed port, which turns a
// developer's running slimproxy into a test failure.
func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot find a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// runProxy builds and starts a proxy, returning its base URL. It blocks until
// the port answers, so callers never race the listener.
func runProxy(t *testing.T, c Config) string {
	t.Helper()

	// Build reconfigures global logrus; put it back for later tests.
	//
	// These cases run with log-to-file off deliberately. Build keeps the log
	// file handle open for the life of the process (documented at its call to
	// setupLogging), which on Windows makes the TempDir holding it undeletable
	// -- and request dumps land under LogDir either way, so file logging buys
	// this test nothing but a cleanup failure.
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })

	rt, err := Build(c, t.TempDir())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rt.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("代理没有在 10 秒内停止")
		}
	})

	base := "http://" + c.Addr()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp", c.Addr(), 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			return base
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("代理没有在 %s 上开始监听", c.Addr())
	return base
}

// postWithCanary sends a request carrying dumpCanary and returns the status.
func postWithCanary(t *testing.T, base, apiKey string) int {
	t.Helper()
	body := fmt.Sprintf(`{"model":"claude-3-5-haiku-20241022","max_tokens":16,`+
		`"messages":[{"role":"user","content":%q}]}`, dumpCanary)

	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// findCanary walks root and returns the first file whose contents leak the
// canary, or "" when none does.
func findCanary(t *testing.T, root string) string {
	t.Helper()
	var hit string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || hit != "" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr == nil && strings.Contains(string(b), dumpCanary) {
			hit = path
		}
		return nil
	})
	return hit
}

// TestRequestLogOffMeansNothingOnDisk pins the fix for an inverted switch.
//
// `request-log: false` did not mean "do not write". The upstream middleware
// turns a disabled logger into logOnErrorOnly, and the response writer then
// logs with force=true on any 4xx or 5xx -- so an unauthenticated request was
// enough to put the caller's whole prompt on disk, verbatim, while -check
// reported 关闭. Two days of upstream trouble wrote 6.8MB of other people's
// conversations that way.
//
// The wording is asserted in the same test on purpose: what -check says and
// what reaches the disk are one claim, and they should fail together.
func TestRequestLogOffMeansNothingOnDisk(t *testing.T) {
	logDir := t.TempDir()
	c := Config{
		Host:       "127.0.0.1",
		Port:       freeTestPort(t),
		APIKeys:    []string{"the-right-key"},
		AuthDir:    filepath.Join(t.TempDir(), "auths"),
		LogToFile:  false,
		LogDir:     logDir,
		RequestLog: false,
	}

	if got := c.RequestLogTarget(); got != "关闭" {
		t.Fatalf("RequestLogTarget() = %q，want 关闭", got)
	}

	base := runProxy(t, c)

	// A wrong key is the cheapest way to reach the error path, and it is also
	// the case that matters most: anyone on the tunnel can send it.
	if status := postWithCanary(t, base, "the-wrong-key"); status != http.StatusUnauthorized {
		t.Fatalf("错误的 key 得到 %d，期望 401（测试没有走到错误路径，等于没测）", status)
	}

	if hit := findCanary(t, logDir); hit != "" {
		t.Errorf("request-log 关闭，但请求正文被写进了 %s\n"+
			"这正是 gatedRequestLogger 要堵的那条 force 路径", hit)
	}
	if hit := findCanary(t, filepath.Dir(c.AuthDir)); hit != "" {
		t.Errorf("请求正文落进了凭据目录树 %s（上游的 <auth-dir>/logs 回退路径）", hit)
	}

	// An empty requests/ would tell an operator that request logs exist. With
	// the gate on, nothing can write there, so nothing should create it.
	dir, err := c.resolvedRequestLogDir()
	if err != nil {
		t.Fatalf("resolvedRequestLogDir: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("request-log 关闭却建出了 %s：空目录会让人以为有日志", dir)
	}
}

// TestRequestLogDirTightenedWhenAlreadyPresent covers the directories builds
// before the gate left behind.
//
// Those hold real dumps, were created by upstream rather than by us, and were
// never hardened -- MkdirAll does not change an existing directory, and with
// request logging off the old code never looked at the path at all. Nothing
// else in the program visits it, so if Build skips it the exposure is
// permanent.
func TestRequestLogDirTightenedWhenAlreadyPresent(t *testing.T) {
	logDir := t.TempDir()
	dir := filepath.Join(logDir, "requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(dir, "error-v1-messages-old.log")
	if err := os.WriteFile(leftover, []byte(dumpCanary), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })
	c := Config{
		Host:       "127.0.0.1",
		Port:       freeTestPort(t),
		APIKeys:    []string{"k"},
		AuthDir:    filepath.Join(t.TempDir(), "auths"),
		LogDir:     logDir,
		RequestLog: false,
	}
	if _, err := Build(c, t.TempDir()); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("Build 把已存在的转储目录弄丢了: %v", err)
	}
	assertRestricted(t, dir)
}

// TestRequestLogOnStillWrites is the other half. A gate that also silences the
// switch when it is deliberately turned on would have traded one broken
// promise for another.
func TestRequestLogOnStillWrites(t *testing.T) {
	logDir := t.TempDir()
	c := Config{
		Host:       "127.0.0.1",
		Port:       freeTestPort(t),
		APIKeys:    []string{"the-right-key"},
		AuthDir:    filepath.Join(t.TempDir(), "auths"),
		LogToFile:  false,
		LogDir:     logDir,
		RequestLog: true,
	}

	base := runProxy(t, c)

	// With no credentials loaded this cannot reach an upstream; any outcome is
	// fine, the assertion is about what was recorded.
	_ = postWithCanary(t, base, "the-right-key")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if findCanary(t, logDir) != "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("request-log 开启，但请求正文没有落盘：gate 关得太狠，把明确要求的记录也堵了")
}
