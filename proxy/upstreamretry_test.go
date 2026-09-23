package proxy

// Characterization tests pinning the upstream conductor's retry behavior on
// CLIProxyAPI v7.2.103 under this project's production settings
// (request-retry: 3, max-retry-interval: 30s, max-retry-credentials: 0,
// transient-error-cooldown-seconds: 0 -> upstream legacy default of 60s).
//
// What they pin, measured empirically (2026-07-30):
//
//   - A 529 from the upstream is attempted exactly ONCE. The conductor's
//     retry loop only re-runs a request while a credential sits in a cooldown
//     window with a recovery deadline, and 529 falls into MarkResult's default
//     branch which sets no deadline at all.
//   - Dial-class failures (no HTTP status) likewise set no deadline: one
//     attempt.
//   - 502-class transient failures DO get a cooldown deadline, but the legacy
//     default (60s) exceeds max-retry-interval (30s), so the loop declines to
//     wait: one attempt.
//
// In other words: with a single credential, `request-retry: 3` buys zero
// additional attempts for every failure class observed in production. The
// multi-attempt readings in earlier incident notes were an artifact of
// comparing the journal's executor-side duration against gin's whole-request
// latency -- successful requests show the same gap (tunnel upload overhead).
//
// If an upstream version bump makes any of these counts move, that is a real
// behavior change worth a deliberate decision, not a silent one -- these
// assertions are the tripwire.
//
// "Attempt" here is one executor run, i.e. one write of the request. A failed
// connection setup is retried below this layer, inside the engine's utls
// round tripper (third_party/CLIProxyAPI/SLIMPROXY_PATCHES.md 第 15 条), where
// no byte of the request has been written yet; it does not move these counts.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// upstreamStatusErr mirrors the visible method set of the upstream internal
// statusErr type (Error / StatusCode / RetryAfter), which is how the Claude
// executor reports non-2xx upstream responses.
type upstreamStatusErr struct {
	code int
	msg  string
}

func (e upstreamStatusErr) Error() string              { return e.msg }
func (e upstreamStatusErr) StatusCode() int            { return e.code }
func (e upstreamStatusErr) RetryAfter() *time.Duration { return nil }

func overloadedErr() upstreamStatusErr {
	return upstreamStatusErr{code: 529, msg: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
}

// countingExecutor fails every attempt with a fixed error and counts attempts.
// When chunkErr is set, ExecuteStream instead returns a well-formed stream
// whose first chunk carries the error -- the bootstrap-failure shape the real
// Claude executor produces when the upstream rejects the request mid-handshake.
type countingExecutor struct {
	calls    int
	err      error
	chunkErr error
}

func (c *countingExecutor) Identifier() string { return "claude" }

func (c *countingExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	c.calls++
	return cliproxyexecutor.Response{}, c.err
}

func (c *countingExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	c.calls++
	if c.chunkErr != nil {
		ch := make(chan cliproxyexecutor.StreamChunk, 1)
		ch <- cliproxyexecutor.StreamChunk{Err: c.chunkErr}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: ch}, nil
	}
	return nil, c.err
}

func (c *countingExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	c.calls++
	return cliproxyexecutor.Response{}, c.err
}

func (c *countingExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}

func (c *countingExecutor) HttpRequest(_ context.Context, _ *coreauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newRetryProbeManager(t *testing.T, ex coreauth.ProviderExecutor) *coreauth.Manager {
	t.Helper()
	m := coreauth.NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)
	m.RegisterExecutor(ex)
	if _, err := m.Register(context.Background(), &coreauth.Auth{ID: "claude-retry-probe", Provider: "claude"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	// Selection filters credentials through the global model registry; an auth
	// with no registered models is never picked and the executor is never hit.
	cliproxy.GlobalModelRegistry().RegisterClient("claude-retry-probe", "claude", []*cliproxy.ModelInfo{
		{ID: "claude-opus-5", Object: "model", Type: "claude"},
	})
	t.Cleanup(func() { cliproxy.GlobalModelRegistry().UnregisterClient("claude-retry-probe") })
	return m
}

func probeRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{Model: "claude-opus-5"}
}

func assertSingleAttempt(t *testing.T, label string, calls int, err error) {
	t.Helper()
	if calls != 1 {
		t.Fatalf("%s: upstream attempted %d times, the pinned behavior is exactly 1", label, calls)
	}
	if err == nil {
		t.Fatalf("%s: expected the upstream failure to surface, got nil", label)
	}
}

func TestUpstreamRetry_529IsAttemptedOnce_Execute(t *testing.T) {
	ex := &countingExecutor{err: overloadedErr()}
	m := newRetryProbeManager(t, ex)
	_, err := m.Execute(context.Background(), []string{"claude"}, probeRequest(), cliproxyexecutor.Options{})
	assertSingleAttempt(t, "529 Execute", ex.calls, err)
	var se cliproxyexecutor.StatusError
	if !errors.As(err, &se) || se.StatusCode() != 529 {
		t.Fatalf("529 Execute: client-facing status lost, got %v", err)
	}
}

func TestUpstreamRetry_529IsAttemptedOnce_ExecuteStream(t *testing.T) {
	ex := &countingExecutor{err: overloadedErr()}
	m := newRetryProbeManager(t, ex)
	_, err := m.ExecuteStream(context.Background(), []string{"claude"}, probeRequest(), cliproxyexecutor.Options{Stream: true})
	assertSingleAttempt(t, "529 ExecuteStream", ex.calls, err)
	var se cliproxyexecutor.StatusError
	if !errors.As(err, &se) || se.StatusCode() != 529 {
		t.Fatalf("529 ExecuteStream: client-facing status lost, got %v", err)
	}
}

func TestUpstreamRetry_529IsAttemptedOnce_StreamBootstrapChunk(t *testing.T) {
	ex := &countingExecutor{chunkErr: overloadedErr()}
	m := newRetryProbeManager(t, ex)
	result, err := m.ExecuteStream(context.Background(), []string{"claude"}, probeRequest(), cliproxyexecutor.Options{Stream: true})
	if ex.calls != 1 {
		t.Fatalf("529 bootstrap chunk: upstream attempted %d times, the pinned behavior is exactly 1", ex.calls)
	}
	// The conductor converts a bootstrap failure into either an error or an
	// error-bearing stream; the 529 must be recoverable from whichever shape
	// comes back, because that is what reaches the client.
	if err != nil {
		var se cliproxyexecutor.StatusError
		if !errors.As(err, &se) || se.StatusCode() != 529 {
			t.Fatalf("529 bootstrap chunk: client-facing status lost, got %v", err)
		}
		return
	}
	if result == nil || result.Chunks == nil {
		t.Fatalf("529 bootstrap chunk: no error and no stream returned")
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			var se cliproxyexecutor.StatusError
			if !errors.As(chunk.Err, &se) || se.StatusCode() != 529 {
				t.Fatalf("529 bootstrap chunk: client-facing status lost in stream, got %v", chunk.Err)
			}
			return
		}
	}
	t.Fatalf("529 bootstrap chunk: stream closed without surfacing the failure")
}

func TestUpstreamRetry_DialFailureIsAttemptedOnce(t *testing.T) {
	ex := &countingExecutor{err: errors.New(`Post "https://api.anthropic.com/v1/messages?beta=true": dial tcp 198.18.0.28:443: connectex: connection attempt failed`)}
	m := newRetryProbeManager(t, ex)
	_, err := m.Execute(context.Background(), []string{"claude"}, probeRequest(), cliproxyexecutor.Options{})
	assertSingleAttempt(t, "dial Execute", ex.calls, err)
}

func TestUpstreamRetry_502IsAttemptedOnce(t *testing.T) {
	ex := &countingExecutor{err: upstreamStatusErr{code: 502, msg: "bad gateway"}}
	m := newRetryProbeManager(t, ex)
	_, err := m.Execute(context.Background(), []string{"claude"}, probeRequest(), cliproxyexecutor.Options{})
	assertSingleAttempt(t, "502 Execute", ex.calls, err)
}
