package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// runInFlight sends one request through the middleware and reports what the
// tracker held while the handler was running, and what it held afterwards.
//
// Both halves matter and they fail differently: a middleware that never records
// leaves the panel as dead as it was before this existed, and one that never
// clears leaves a row counting up forever after the request is long gone.
func runInFlight(t *testing.T, method, path string, handler gin.HandlerFunc) (during, after int) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	f := metrics.NewInFlight()
	r := gin.New()
	r.Use(InFlightMiddleware(f))
	r.Handle(method, path, func(c *gin.Context) {
		during = len(f.Snapshot())
		if handler != nil {
			handler(c)
		}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.ServeHTTP(rec, req)
	return during, len(f.Snapshot())
}

func TestInFlightRecordsAndClears(t *testing.T) {
	during, after := runInFlight(t, http.MethodPost, "/v1/messages", nil)
	if during != 1 {
		t.Errorf("处理期间应有 1 个进行中，实际 %d——面板在请求跑着的时候仍是空的", during)
	}
	if after != 0 {
		t.Errorf("处理结束后应清空，实际 %d——那一行会永远数下去", after)
	}
}

// TestInFlightClearsAfterPanic is why End is deferred rather than called after
// c.Next().
//
// gin recovers panics in its own middleware chain, so a handler that blows up
// produces a 500 and the process carries on -- with, without the defer, a row
// on the dashboard for a request that has already failed, never to be removed.
func TestInFlightClearsAfterPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	f := metrics.NewInFlight()
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(InFlightMiddleware(f))
	r.POST("/v1/messages", func(*gin.Context) { panic("upstream exploded") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	r.ServeHTTP(rec, req)

	if n := len(f.Snapshot()); n != 0 {
		t.Errorf("handler panic 后仍留下 %d 个进行中——defer 没生效", n)
	}
}

// TestInFlightGate pins what the middleware admits.
//
// The asymmetry is deliberate and worth protecting: admitting one extra POST
// costs a row that really is a request in progress, while missing a dialect
// reproduces the original defect exactly -- a request running with nothing on
// screen to show for it. So the gate excludes rather than enumerates, and these
// cases pin both edges of that choice.
func TestInFlightGate(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"anthropic", http.MethodPost, "/v1/messages", 1},
		{"openai", http.MethodPost, "/v1/chat/completions", 1},
		{"openai responses", http.MethodPost, "/v1/responses", 1},
		{"gemini", http.MethodPost, "/v1beta/models/gemini-pro:generateContent", 1},
		// 未来新增的方言不需要改这里，这正是排除式 gate 的目的
		{"未知方言", http.MethodPost, "/v9/something/new", 1},

		{"健康检查", http.MethodGet, "/health", 0},
		{"模型列表", http.MethodGet, "/v1/models", 0},
		{"管理接口", http.MethodPost, "/v0/management/config", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			during, after := runInFlight(t, tc.method, tc.path, nil)
			if during != tc.want {
				t.Errorf("%s %s 处理期间进行中 = %d，应为 %d", tc.method, tc.path, during, tc.want)
			}
			if after != 0 {
				t.Errorf("结束后应清空，实际 %d", after)
			}
		})
	}
}
