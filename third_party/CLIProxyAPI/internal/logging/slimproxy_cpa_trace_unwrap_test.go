// slimproxy patch: guard test for the CPA trace writer's Unwrap (see
// SLIMPROXY_PATCHES.md 第 15/16 条). A rebase that drops the patch file
// compiles cleanly and fails only here -- and in production only as uploads
// killed mid-way behind an early SSE preamble. The root module's forkcheck
// package runs this from `go test ./...`.
package logging

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestCPATraceWriterPassesFullDuplexThrough enables full duplex from inside
// the middleware chain, the way an embedder middleware registered after
// CPATraceIDMiddleware does, over a real net/http server: the call has to
// unwrap through this writer and gin's to reach net/http's own.
func TestCPATraceWriterPassesFullDuplexThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(CPATraceIDMiddleware())
	errs := make(chan error, 1)
	engine.POST("/v1/messages", func(c *gin.Context) {
		if _, ok := c.Writer.(*cpaTraceResponseWriter); !ok {
			t.Errorf("c.Writer is %T, want the CPA trace writer on the path under test", c.Writer)
		}
		errs <- http.NewResponseController(c.Writer).EnableFullDuplex()
		c.Status(http.StatusNoContent)
	})
	srv := httptest.NewServer(engine)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if err := <-errs; err != nil {
		t.Fatalf("EnableFullDuplex through the CPA trace writer: %v -- its Unwrap is gone, "+
			"and slimproxy's early SSE preamble will again cut uploads short", err)
	}
}
