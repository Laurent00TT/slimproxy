// proxy/netmeter_wiring_test.go
package proxy

// The hop this file exists to pin, end to end and against the real fork code:
//
//	gin request ctx (NetTimings installed by NetMeterMiddleware)
//	  → BaseAPIHandler.GetContextWithCancel(handler, c, context.Background())
//	  → usage.Manager.Publish on the resulting executor ctx (async dispatch)
//	  → metrics.Collector.HandleUsage reads NetTimingsFrom(ctx)
//
// Every dialect handler in the fork passes context.Background() as the parent
// (claude/code_handlers.go, gemini/gemini_handlers.go, openai/*.go — the lone
// exception is the responses websocket, which passes its own parent), so if
// GetContextWithCancel does not make the executor ctx inherit the request
// ctx's value chain, the meter's numbers exist in the request ctx and die
// there: every real request publishes with Upload/WriteBlock absent. That is
// exactly what happened before the SLIMPROXY_PATCHES.md reparenting patch —
// this test was written first and observed RED against the unpatched fork.
//
// Unit tests cannot catch a regression here: netmeter_test.go proves the gin
// half, TestHandleUsageMergesNetTimings proves the collector half, and both
// stay green while the fork hop between them is severed.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	handlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"

	"github.com/Laurent00TT/slimproxy/metrics"
)

func TestNetTimingsSurviveForkContextHop(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A request whose ctx carries NetTimings, the way NetMeterMiddleware
	// leaves it. The values are distinctive so a zero can only mean "lost".
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
	// One clock read: MarkBodyDone stores now.Sub(started), so deriving both
	// stamps from the same instant is what makes the exact-equality assertion
	// below deterministic. Two time.Now() calls would add the inter-call delta
	// (~0 on Windows' coarse clock, tens of ns on Linux/macOS) and fail there.
	t0 := time.Now()
	nt := metrics.NewNetTimings(t0)
	nt.MarkBodyDone(t0.Add(300 * time.Millisecond))
	nt.AddWriteBlock(40 * time.Millisecond)
	c.Request = req.WithContext(metrics.WithNetTimings(req.Context(), nt))

	// The fork's real function, called the way the dialect handlers call it:
	// parent is context.Background(). The handler argument is only stored as
	// a ctx value, so nil is faithful enough here.
	h := &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}
	cliCtx, cliCancel := h.GetContextWithCancel(nil, c, context.Background())
	defer cliCancel()

	// The real async usage manager — an own instance rather than the default
	// one, so the test does not leave a plugin registered globally. Dispatch
	// mechanics (queue, goroutine, safeInvoke) are the same code path
	// PublishRecord drives.
	coll := metrics.NewCollector()
	observed := make(chan metrics.Sample, 1)
	coll.Observe(func(s metrics.Sample) { observed <- s })
	m := cliproxyusage.NewManager(0)
	m.Register(coll)
	m.Start(context.Background())
	defer m.Stop()

	m.Publish(cliCtx, cliproxyusage.Record{Model: "m", Latency: time.Second})

	select {
	case s := <-observed:
		if s.Upload != 300*time.Millisecond || s.WriteBlock != 40*time.Millisecond {
			t.Fatalf("执行 ctx 丢掉了请求 ctx 的值链：Upload=%v WriteBlock=%v（want 300ms/40ms）——"+
				"fork 的 GetContextWithCancel 重挂补丁（SLIMPROXY_PATCHES.md）可能被升级冲掉了",
				s.Upload, s.WriteBlock)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("usage manager 5s 内没有派发记录")
	}
}
