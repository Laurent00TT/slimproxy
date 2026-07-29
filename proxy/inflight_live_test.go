package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// TestInFlightIsActuallyWiredIn drives a real request through a real service.
//
// The gap it closes: every other test here builds its own gin engine and
// registers the middleware by hand, which proves the middleware works and
// proves nothing about whether Build installs it. Those two failures look
// identical from the outside -- an empty panel -- and the second one is
// invisible to a test that constructs its own router.
//
// Asserted on Started rather than Snapshot: the request finishes long before
// this can look, so the live map is empty either way. A monotonic count is the
// only thing that separates "arrived and completed" from "never arrived".
//
// The request is expected to FAIL. There are no credentials here, so it comes
// back 401 or similar -- which is exactly right, because the middleware sits
// outside the handler chain and must record a request regardless of how it
// ends. A version that only counted successes would go quiet during the
// upstream outage it is most needed for.
func TestInFlightIsActuallyWiredIn(t *testing.T) {
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })

	port := freeTestPort(t)
	rt, err := Build(Config{
		Host:        "127.0.0.1",
		Port:        port,
		APIKeys:     []string{"k"},
		AuthDir:     filepath.Join(t.TempDir(), "auths"),
		LogDir:      t.TempDir(),
		JournalDays: JournalDisabled,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rt.Service.Run(ctx) }()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitForListener(t, addr)

	before := rt.Stats.InFlight().Started()

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求没有到达代理: %v", err)
	}
	_ = resp.Body.Close()

	if got := rt.Stats.InFlight().Started(); got <= before {
		t.Errorf("一个真实请求穿过了代理（HTTP %d），in-flight 计数却没动（%d → %d）："+
			"InFlightMiddleware 没有被 Build 装上，面板会永远显示不出进行中的请求",
			resp.StatusCode, before, got)
	}

	// And it must not still be held: the request is over.
	if n := len(rt.Stats.InFlight().Snapshot()); n != 0 {
		t.Errorf("请求已结束却仍有 %d 个挂在追踪器里", n)
	}
}

// waitForListener blocks until the service accepts connections, so the test
// does not race the server's startup.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("代理在 10 秒内没有开始监听 %s", addr)
}
