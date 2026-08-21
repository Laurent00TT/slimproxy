// slimproxy patch: guard tests for the GetContextWithCancel reparenting patch
// (see SLIMPROXY_PATCHES.md). Every dialect handler passes context.Background()
// as the executor parent; the patch makes that case inherit the request ctx's
// value chain so request-scoped values (slimproxy's net timings) survive into
// async usage publication. These tests pin the three properties an upstream
// rebase could silently lose: value inheritance, unchanged cancellation
// propagation, and an explicit parent staying untouched. The root module's
// forkcheck package runs them from `go test ./...`.
package handlers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type reparentProbeKey struct{}

func newReparentTestContext(t *testing.T, reqCtx context.Context) (*gin.Context, *BaseAPIHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	c.Request = req.WithContext(reqCtx)
	return c, &BaseAPIHandler{Cfg: &config.SDKConfig{}}
}

// TestContextReparentInheritsRequestValues: with a Background parent (what the
// dialect handlers pass), a value on the request ctx must be readable from the
// executor ctx. This is the assertion that fails if the patch is reverted.
func TestContextReparentInheritsRequestValues(t *testing.T) {
	reqCtx := context.WithValue(context.Background(), reparentProbeKey{}, "alive")
	c, h := newReparentTestContext(t, reqCtx)
	newCtx, cancel := h.GetContextWithCancel(nil, c, context.Background())
	defer cancel()
	if got, _ := newCtx.Value(reparentProbeKey{}).(string); got != "alive" {
		t.Fatalf("executor ctx lost the request ctx value chain (got %q) -- "+
			"the reparenting patch in GetContextWithCancel is gone", got)
	}
}

// TestContextReparentKeepsCancelPropagation: request-ctx cancellation must
// still cancel the executor ctx. Pre-patch a bridge goroutine provided this;
// post-patch direct parentage does. Either implementation passes -- the
// behavior is the contract.
func TestContextReparentKeepsCancelPropagation(t *testing.T) {
	reqCtx, cancelReq := context.WithCancel(context.Background())
	c, h := newReparentTestContext(t, reqCtx)
	newCtx, cancel := h.GetContextWithCancel(nil, c, context.Background())
	defer cancel()
	cancelReq()
	select {
	case <-newCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("request ctx cancellation no longer reaches the executor ctx")
	}
}

// TestContextReparentRespectsExplicitParent: a caller that passes its own
// parent (the responses websocket does) must keep it -- reparenting applies
// only to the no-real-parent case.
func TestContextReparentRespectsExplicitParent(t *testing.T) {
	reqCtx := context.WithValue(context.Background(), reparentProbeKey{}, "request")
	c, h := newReparentTestContext(t, reqCtx)
	parent := context.WithValue(context.Background(), reparentProbeKey{}, "explicit")
	newCtx, cancel := h.GetContextWithCancel(nil, c, parent)
	defer cancel()
	if got, _ := newCtx.Value(reparentProbeKey{}).(string); got != "explicit" {
		t.Fatalf("explicit parent was overridden by the request ctx (got %q)", got)
	}
}
