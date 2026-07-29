package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestJournalStillSeesTheRealStatus.
//
// The envelope defers WriteHeader so a replacement body can carry its own
// Content-Length, and AccessJournalMiddleware reads c.Writer.Status() after
// c.Next() to decide what to file. Those two facts touch: a wrapper that
// reported 200 while holding a 500 would leave the journal recording success
// for every failure it redacted -- trading a leak for a blind spot, which is
// the same shape of bug as the two-day outage nobody was told about.
func TestJournalStillSeesTheRealStatus(t *testing.T) {
	// Observed the way AccessJournalMiddleware observes it -- c.Writer.Status()
	// after c.Next() -- rather than through a RejectFolder, whose one-minute
	// folding window would make this a test of the folder's timer instead of the
	// wrapper's honesty.
	var filed []int
	r := gin.New()
	// Same order as Build: envelope outermost, journal's vantage point inside it.
	r.Use(ErrorEnvelopeMiddleware())
	r.Use(func(c *gin.Context) {
		c.Next()
		filed = append(filed, c.Writer.Status())
	})
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Data(http.StatusInternalServerError, "application/json",
			[]byte(`{"type":"error","error":{"type":"api_error","message":"dial tcp 198.18.0.42:443: connectex: nope"}}`))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("客户端看到的状态码 = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "198.18.0.42") {
		t.Errorf("接线之后脱敏失效: %s", rec.Body.String())
	}
	if len(filed) == 0 {
		t.Fatal("内层中间件没有跑到：包装 Writer 破坏了中间件链")
	}
	if filed[0] != http.StatusInternalServerError {
		t.Errorf("内层看到的状态码 = %d, want 500：包装后状态码对 journal 不可见，"+
			"失败会被记成成功", filed[0])
	}
}

// TestWrittenStaysFalseWhileHeld.
//
// Upstream guards its header writes with `if !c.Writer.Written()` in both
// WriteErrorResponse implementations. While a body is held those guards must
// still pass, or the response would go out without its Content-Type. Asserted
// directly because the consequence -- a client refusing to parse the body -- is
// invisible in a status-code check.
func TestWrittenStaysFalseWhileHeld(t *testing.T) {
	var seenDuring bool

	r := gin.New()
	r.Use(ErrorEnvelopeMiddleware())
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Status(http.StatusBadGateway)
		_, _ = c.Writer.Write([]byte(`{"err":"held"}`))
		// Upstream checks this after writing, before setting headers.
		seenDuring = c.Writer.Written()
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if seenDuring {
		t.Error("缓冲期间 Written() 返回 true：上游的 header 守卫会跳过 Content-Type 设置")
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q，缺失会让客户端拒绝解析", got)
	}
}
