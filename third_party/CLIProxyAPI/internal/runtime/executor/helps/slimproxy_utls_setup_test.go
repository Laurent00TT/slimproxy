package helps

// slimproxy patch regression tests (see SLIMPROXY_PATCHES.md 第 15-16 条).
//
// What is being pinned: the utls round tripper bounds each connection-setup
// attempt, retries a failed setup a fixed number of times, stops the moment
// the request's own context ends, and never retries once the request has been
// handed to a live connection; and, once a connection is up, that nothing of
// setup's bound is left on it, that a dead one is noticed, and that an idle
// one is closed. The failures are staged by a fake HTTP CONNECT
// proxy on loopback -- the same shapes Clash produced in the 2026-09-21..23
// window: a CONNECT answered 200 and then closed (the ~5.0s EOF inside the
// handshake), a tunnel that swallows the ClientHello (the ~60s hang), a
// CONNECT never answered. The proxy is reached through the real
// proxyutil.BuildDialer, so the ctx-aware CONNECT dial is under test as well.
// The upstream is a local TLS+HTTP/2 httptest server, trusted through the
// round tripper's rootCAs seam; nothing leaves the machine.
//
// These tests shrink package-level timings, so none of them may run in
// parallel.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// setSetupTimings shrinks the setup budget for one test and restores it after.
func setSetupTimings(t *testing.T, attempts int, attemptTimeout time.Duration, backoff []time.Duration) {
	t.Helper()
	oldAttempts, oldTimeout, oldBackoff := connectAttempts, connectAttemptTimeout, connectRetryBackoff
	connectAttempts, connectAttemptTimeout, connectRetryBackoff = attempts, attemptTimeout, backoff
	t.Cleanup(func() {
		connectAttempts, connectAttemptTimeout, connectRetryBackoff = oldAttempts, oldTimeout, oldBackoff
	})
}

// setH2Timings shrinks the HTTP/2 health-check and idle timings for one test
// and restores them after.
func setH2Timings(t *testing.T, readIdle, ping, idleConn time.Duration) {
	t.Helper()
	oldIdle, oldPing, oldConn := h2ReadIdleTimeout, h2PingTimeout, h2IdleConnTimeout
	h2ReadIdleTimeout, h2PingTimeout, h2IdleConnTimeout = readIdle, ping, idleConn
	t.Cleanup(func() {
		h2ReadIdleTimeout, h2PingTimeout, h2IdleConnTimeout = oldIdle, oldPing, oldConn
	})
}

// setupLogHook records the round tripper's log lines and lets a test act on a
// warning the moment it is logged.
type setupLogHook struct {
	mu      sync.Mutex
	lines   []setupLogLine
	onWarn  func(msg string)
	onWarnM sync.Mutex
}

type setupLogLine struct {
	level log.Level
	msg   string
}

func (h *setupLogHook) Levels() []log.Level { return log.AllLevels }

func (h *setupLogHook) Fire(e *log.Entry) error {
	if !strings.HasPrefix(e.Message, "utls: ") {
		return nil
	}
	h.mu.Lock()
	h.lines = append(h.lines, setupLogLine{level: e.Level, msg: e.Message})
	h.mu.Unlock()
	h.onWarnM.Lock()
	onWarn := h.onWarn
	h.onWarnM.Unlock()
	if onWarn != nil && e.Level == log.WarnLevel {
		onWarn(e.Message)
	}
	return nil
}

func (h *setupLogHook) matching(level log.Level, substr string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, l := range h.lines {
		if l.level == level && strings.Contains(l.msg, substr) {
			out = append(out, l.msg)
		}
	}
	return out
}

func (h *setupLogHook) all() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	for _, l := range h.lines {
		b.WriteString(l.level.String() + ": " + l.msg + "\n")
	}
	return b.String()
}

// captureSetupLogs installs a fresh hook on the standard logger, silences
// its output, and puts both back afterwards.
func captureSetupLogs(t *testing.T) *setupLogHook {
	t.Helper()
	hook := &setupLogHook{}
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldOut := logger.Out
	logger.AddHook(hook)
	logger.SetOutput(io.Discard)
	t.Cleanup(func() {
		logger.ReplaceHooks(oldHooks)
		logger.SetOutput(oldOut)
	})
	return hook
}

// connectProxy is a loopback HTTP CONNECT proxy whose n-th connection (from
// 1) is handled by handle -- so a test decides, per attempt, what "Clash"
// does with it.
type connectProxy struct {
	ln       net.Listener
	accepted atomic.Int32
	handle   func(p *connectProxy, n int, c net.Conn, br *bufio.Reader)

	mu    sync.Mutex
	conns []net.Conn
}

func newConnectProxy(t *testing.T, handle func(p *connectProxy, n int, c net.Conn, br *bufio.Reader)) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &connectProxy{ln: ln, handle: handle}
	go func() {
		for {
			c, errAccept := ln.Accept()
			if errAccept != nil {
				return
			}
			p.track(c)
			n := int(p.accepted.Add(1))
			go p.handle(p, n, c, bufio.NewReader(c))
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			_ = c.Close()
		}
	})
	return p
}

func (p *connectProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
}

func (p *connectProxy) url() string { return "http://" + p.ln.Addr().String() }

// readConnect consumes the CONNECT request; false means the client went away.
func readConnect(br *bufio.Reader) bool {
	req, err := http.ReadRequest(br)
	return err == nil && req.Method == http.MethodConnect
}

func answerConnect(c net.Conn) bool {
	_, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	return err == nil
}

// silentBeforeConnect never answers the CONNECT: setup stalls in the dial phase.
func silentBeforeConnect(_ *connectProxy, _ int, c net.Conn, br *bufio.Reader) {
	readConnect(br)
	_, _ = io.Copy(io.Discard, br)
}

// silentAfterConnect answers 200 and swallows the ClientHello: setup stalls
// in the handshake, the ~60s shape.
func silentAfterConnect(_ *connectProxy, _ int, c net.Conn, br *bufio.Reader) {
	if readConnect(br) && answerConnect(c) {
		_, _ = io.Copy(io.Discard, br)
	}
}

// closeAfterConnect answers 200 and hangs up, as Clash does when its dial to
// a dead node fails: the handshake reads EOF, the ~5.0s shape. It sends a FIN
// and drains rather than closing outright, because Clash has consumed the
// ClientHello by then -- and closing a socket with unread input makes Windows
// send an RST, which the client reads as "connection aborted", not EOF.
func closeAfterConnect(_ *connectProxy, _ int, c net.Conn, br *bufio.Reader) {
	defer func() { _ = c.Close() }()
	if !readConnect(br) || !answerConnect(c) {
		return
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		_, _ = io.Copy(io.Discard, br)
	}
}

// tunnel answers 200 and relays to target. While frozen is set, bytes in
// either direction are dropped and nothing is closed -- a path that died
// without a FIN or RST reaching either end. clientGone, if set, is called
// when the client ends its side first (FIN or RST): a read cut because the
// proxy itself closed the connection, after the upstream side ended, does
// not count.
func (p *connectProxy) tunnel(c net.Conn, br *bufio.Reader, target string, frozen *atomic.Bool, clientGone func()) {
	if !readConnect(br) {
		return
	}
	up, err := net.Dial("tcp", target)
	if err != nil {
		_ = c.Close()
		return
	}
	p.track(up)
	if !answerConnect(c) {
		return
	}
	relay := func(dst net.Conn, src io.Reader, srcGone func()) {
		buf := make([]byte, 32<<10)
		for {
			n, errRead := src.Read(buf)
			if n > 0 && (frozen == nil || !frozen.Load()) {
				if _, errWrite := dst.Write(buf[:n]); errWrite != nil {
					return
				}
			}
			if errRead != nil {
				if srcGone != nil && !errors.Is(errRead, net.ErrClosed) {
					srcGone()
				}
				if frozen == nil || !frozen.Load() {
					_ = dst.Close()
				}
				return
			}
		}
	}
	go relay(up, br, clientGone)
	relay(c, up, nil)
}

func tunnelTo(target string, frozen *atomic.Bool) func(p *connectProxy, n int, c net.Conn, br *bufio.Reader) {
	return func(p *connectProxy, _ int, c net.Conn, br *bufio.Reader) {
		p.tunnel(c, br, target, frozen, nil)
	}
}

// newH2Upstream starts a TLS+HTTP/2 server standing in for the upstream and
// returns its address and a pool that trusts it. Its certificate names
// example.com, the host every test request is addressed to; the proxy
// ignores the CONNECT target, so no name is ever resolved.
func newH2Upstream(t *testing.T, h http.HandlerFunc) (string, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return srv.Listener.Addr().String(), roots
}

// newTestRoundTripper builds the round tripper exactly as production does
// for an http:// proxy-url, then trusts the test upstream.
func newTestRoundTripper(t *testing.T, p *connectProxy, roots *x509.CertPool) *utlsRoundTripper {
	t.Helper()
	rt := newUtlsRoundTripper(p.url())
	if _, ok := rt.dialer.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	}); !ok {
		t.Fatalf("dialer for an http:// proxy-url is %T, not a ContextDialer: the dial phase would be unbounded", rt.dialer)
	}
	rt.rootCAs = roots
	// Registered after the upstream and proxy, so it runs before them: an
	// open client connection would keep httptest's Close waiting.
	t.Cleanup(func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		for _, c := range rt.connections {
			_ = c.Close()
		}
	})
	return rt
}

func newTestRequest(t *testing.T, ctx context.Context, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// returnWithin runs f and fails the test if it has not returned within limit.
// Every call site's context ends well before limit, so f still blocked means
// some step ignores that context -- the very bug under test. Without this the
// bug shows up as a hang until go test's ten-minute timeout, which is not a
// guard anyone waits for. A stuck f is released by the proxy's cleanup.
func returnWithin[T any](t *testing.T, limit time.Duration, f func() (T, error)) (T, error) {
	t.Helper()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := f()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(limit):
		t.Fatalf("still blocked %v in, after its context had ended: part of the path ignores the context", limit)
		var zero T
		return zero, nil
	}
}

func roundTripWithin(t *testing.T, rt *utlsRoundTripper, req *http.Request, limit time.Duration) (*http.Response, error) {
	t.Helper()
	return returnWithin(t, limit, func() (*http.Response, error) { return rt.RoundTrip(req) })
}

// Each setup attempt is cut at connectAttemptTimeout and the request gives up
// after connectAttempts of them -- in both places a setup was seen to stall:
// the proxy never answering CONNECT (dial phase: proxyutil's CONNECT dialer
// must honour the attempt's context) and a tunnel that swallows the
// ClientHello (handshake phase). Without the per-attempt bound the single
// attempt runs until the 5s watchdog; with a dial that ignores its context
// (upstream dialled with context.Background()) not even that ends it, and
// returnWithin gives up instead.
func TestUtlsSetupGivesUpAfterAttemptBudget(t *testing.T) {
	cases := []struct {
		name   string
		phase  string
		handle func(p *connectProxy, n int, c net.Conn, br *bufio.Reader)
	}{
		{"CONNECT never answered", setupPhaseDial, silentBeforeConnect},
		{"handshake never answered", setupPhaseHandshake, silentAfterConnect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const attemptTimeout = 200 * time.Millisecond
			backoff := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
			setSetupTimings(t, 3, attemptTimeout, backoff)
			logs := captureSetupLogs(t)
			p := newConnectProxy(t, tc.handle)
			rt := newTestRoundTripper(t, p, nil)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			resp, err := roundTripWithin(t, rt, newTestRequest(t, ctx, []byte("{}")), 8*time.Second)
			elapsed := time.Since(start)
			if resp != nil {
				_ = resp.Body.Close()
			}

			if err == nil {
				t.Fatal("setup against a silent peer succeeded")
			}
			if ctx.Err() != nil {
				t.Fatalf("the 5s watchdog ended the request (err %v): setup attempts are not bounded", err)
			}
			if got := p.accepted.Load(); got != 3 {
				t.Fatalf("proxy saw %d connections, want 3 (one per attempt)", got)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v; want it to carry context.DeadlineExceeded so the journal files a timeout", err)
			}
			if !strings.Contains(err.Error(), "after 3 attempts") {
				t.Errorf("err = %v; want it to say the attempts ran out", err)
			}
			if minimum := 3 * attemptTimeout; elapsed < minimum {
				t.Errorf("gave up after %v, before three attempts of %v could have run", elapsed, attemptTimeout)
			}
			if maximum := 3*attemptTimeout + 30*time.Millisecond + 2*time.Second; elapsed > maximum {
				t.Errorf("gave up after %v, well past three attempts of %v", elapsed, attemptTimeout)
			}
			if warns := logs.matching(log.WarnLevel, "failed in "+tc.phase+" phase"); len(warns) != 3 {
				t.Errorf("%d warnings name the %s phase, want 3; log:\n%s", len(warns), tc.phase, logs.all())
			}
		})
	}
}

// The first attempt meets the ~5.0s shape -- CONNECT 200, then the proxy
// hangs up inside the handshake -- and the second reaches the upstream: the
// request succeeds on a second dial, the upstream sees it exactly once, and
// both the failure (with its phase) and the recovery are logged.
func TestUtlsSetupRetriesAfterHandshakeEOF(t *testing.T) {
	setSetupTimings(t, 3, 2*time.Second, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond})
	logs := captureSetupLogs(t)
	var hits atomic.Int32
	upstream, roots := newH2Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	})
	p := newConnectProxy(t, func(p *connectProxy, n int, c net.Conn, br *bufio.Reader) {
		if n == 1 {
			closeAfterConnect(p, n, c, br)
			return
		}
		p.tunnel(c, br, upstream, nil, nil)
	})
	rt := newTestRoundTripper(t, p, roots)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := roundTripWithin(t, rt, newTestRequest(t, ctx, []byte(`{"model":"x"}`)), 15*time.Second)
	if err != nil {
		t.Fatalf("request failed although the second attempt had a working path: %v\nlog:\n%s", err, logs.all())
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("got %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}
	if got := p.accepted.Load(); got != 2 {
		t.Errorf("proxy saw %d connections, want exactly 2", got)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream saw the request %d times, want 1", got)
	}
	if warns := logs.matching(log.WarnLevel, "failed in handshake phase on attempt 1/3"); len(warns) != 1 {
		t.Errorf("want one warning for the failed first handshake; log:\n%s", logs.all())
	} else if !strings.Contains(warns[0], "EOF") {
		t.Errorf("warning %q does not carry the handshake's EOF", warns[0])
	}
	if infos := logs.matching(log.InfoLevel, "set up on attempt 2/3"); len(infos) != 1 {
		t.Errorf("want one line recording the recovery on attempt 2; log:\n%s", logs.all())
	}
}

// When every attempt meets the EOF shape, the error keeps the chain (io.EOF)
// and the words it adds match nothing slimproxy's journal classifies on.
// metrics.CauseFromText reads this text: "eof" files as connect, which is
// what this failure was before the retry existed. A wrapper saying "utls"
// would contain "tls" and move every such failure to the tls cause. The
// words below are the classifier's own table, copied: the fork cannot import
// slimproxy.
func TestUtlsSetupExhaustedKeepsJournalCause(t *testing.T) {
	setSetupTimings(t, 3, 2*time.Second, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond})
	captureSetupLogs(t)
	p := newConnectProxy(t, closeAfterConnect)
	rt := newTestRoundTripper(t, p, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := roundTripWithin(t, rt, newTestRequest(t, ctx, []byte("{}")), 15*time.Second)
	if err == nil {
		t.Fatal("setup through a proxy that always hangs up succeeded")
	}
	if got := p.accepted.Load(); got != 3 {
		t.Fatalf("proxy saw %d connections, want 3", got)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v; want the handshake's io.EOF kept in the chain", err)
	}
	inner := errors.Unwrap(err)
	if inner == nil {
		t.Fatalf("err = %v wraps nothing", err)
	}
	added := strings.ToLower(strings.TrimSuffix(err.Error(), inner.Error()))
	for _, word := range []string{
		"no such host", "lookup ", "dns",
		"tls", "certificate", "x509",
		"context canceled", "context cancelled", "client disconnected",
		"timeout", "timed out", "deadline exceeded",
		"refused", "connectex", "dial ", "connect:", "unreachable",
		"reset by peer", "broken pipe", "eof", "no route to host",
	} {
		if strings.Contains(added, word) {
			t.Errorf("wrapper %q contains %q, which the journal's cause classifier matches", added, word)
		}
	}
}

// A request whose context ends during the pause between attempts returns at
// once with that context's error, and no further attempt is dialled.
func TestUtlsSetupStopsWhenRequestEndsDuringBackoff(t *testing.T) {
	const backoff = 5 * time.Second
	setSetupTimings(t, 3, 2*time.Second, []time.Duration{backoff, backoff})
	logs := captureSetupLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The warning for attempt 1 is logged just before the backoff starts;
	// cancelling from it puts the cancellation inside the pause.
	logs.onWarnM.Lock()
	logs.onWarn = func(msg string) {
		if strings.Contains(msg, "attempt 1/3") {
			time.AfterFunc(50*time.Millisecond, cancel)
		}
	}
	logs.onWarnM.Unlock()
	p := newConnectProxy(t, closeAfterConnect)
	rt := newTestRoundTripper(t, p, nil)

	start := time.Now()
	_, err := roundTripWithin(t, rt, newTestRequest(t, ctx, []byte("{}")), 15*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned %v after the start, i.e. sat out the %v backoff after the request ended", elapsed, backoff)
	}
	// Give a stray attempt, if any were started, time to reach the proxy.
	time.Sleep(200 * time.Millisecond)
	if got := p.accepted.Load(); got != 1 {
		t.Errorf("proxy saw %d connections, want 1: nothing may be dialled after the request ended", got)
	}
}

// An error after setup -- the request is on the wire and has reached the
// upstream -- goes back to the caller after one attempt. Retrying it could
// run and bill the request twice; the boundary is the whole point of doing
// the retry below RoundTrip rather than around it.
//
// Each case fails the first request only once the upstream has read all of
// it, and lets any later request through: a retry shows up as a second
// upstream hit and a success, rather than hiding behind a repeat of the same
// failure. The cases are the shapes such a failure takes: the upstream
// resetting the stream, and the path dying under the request -- hung up, or
// gone silent so that only the PING health check notices. The last two are
// what a node dying mid-request looks like, surface as connection errors
// rather than stream errors, and are the likeliest to be "helpfully" retried
// by error type.
func TestUtlsRequestErrorAfterSetupIsNotRetried(t *testing.T) {
	cases := []struct {
		name string
		// upstreamResets makes the upstream fail the first request itself;
		// otherwise onArrival acts on the proxy's first connection.
		upstreamResets bool
		onArrival      func(c net.Conn, frozen *atomic.Bool)
	}{
		{name: "upstream resets the stream", upstreamResets: true},
		{name: "connection closed after the request arrived", onArrival: func(c net.Conn, _ *atomic.Bool) {
			_ = c.Close()
		}},
		{name: "connection silent after the request arrived", onArrival: func(_ net.Conn, frozen *atomic.Bool) {
			frozen.Store(true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSetupTimings(t, 3, 2*time.Second, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond})
			// The silent path is noticed by the health check, ~400ms in.
			setH2Timings(t, 200*time.Millisecond, 200*time.Millisecond, h2IdleConnTimeout)
			captureSetupLogs(t)
			var hits atomic.Int32
			arrived := make(chan struct{})
			upstream, roots := newH2Upstream(t, func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if n > 1 {
					_, _ = io.WriteString(w, "ok")
					return
				}
				close(arrived)
				if tc.upstreamResets {
					// RST_STREAM, after the whole request arrived.
					panic(http.ErrAbortHandler)
				}
				<-r.Context().Done()
			})
			testDone := t.Context()
			p := newConnectProxy(t, func(p *connectProxy, n int, c net.Conn, br *bufio.Reader) {
				if n > 1 || tc.onArrival == nil {
					p.tunnel(c, br, upstream, nil, nil)
					return
				}
				var frozen atomic.Bool
				go func() {
					select {
					case <-arrived:
						tc.onArrival(c, &frozen)
					case <-testDone.Done():
					}
				}()
				p.tunnel(c, br, upstream, &frozen, nil)
			})
			rt := newTestRoundTripper(t, p, roots)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := roundTripWithin(t, rt, newTestRequest(t, ctx, bytes.Repeat([]byte("x"), 64<<10)), 15*time.Second)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Error("the failed request came back as a response: it was sent again")
			} else if ctx.Err() != nil {
				t.Fatalf("the 10s watchdog ended the request (err %v): the failure was never noticed", err)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("upstream saw the request %d times, want exactly 1", got)
			}
			if got := p.accepted.Load(); got != 1 {
				t.Errorf("proxy saw %d connections, want 1: an error after setup must not start another", got)
			}
		})
	}
}

// Requests queued behind another request's setup follow their own context:
// one whose request ends leaves at once, while the setup it waited on is
// still stuck; one that outlives a creator whose request was cancelled is
// released and makes its own attempt.
func TestUtlsSetupWaitersFollowTheirOwnContext(t *testing.T) {
	setSetupTimings(t, 1, 10*time.Second, nil)
	captureSetupLogs(t)
	upstream, roots := newH2Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	creatorDialled := make(chan struct{})
	p := newConnectProxy(t, func(p *connectProxy, n int, c net.Conn, br *bufio.Reader) {
		if n == 1 {
			close(creatorDialled)
			silentBeforeConnect(p, n, c, br)
			return
		}
		p.tunnel(c, br, upstream, nil, nil)
	})
	rt := newTestRoundTripper(t, p, roots)

	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	defer cancelCreator()
	creatorReq := newTestRequest(t, creatorCtx, []byte("{}"))
	creatorDone := make(chan error, 1)
	go func() {
		resp, err := rt.RoundTrip(creatorReq)
		if resp != nil {
			_ = resp.Body.Close()
		}
		creatorDone <- err
	}()
	select {
	case <-creatorDialled:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the proxy")
	}

	// The leaver's request ends while the creator's setup is still stuck.
	leaverCtx, cancelLeaver := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelLeaver()
	start := time.Now()
	_, err := roundTripWithin(t, rt, newTestRequest(t, leaverCtx, []byte("{}")), 15*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err = %v, want its own context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waiter whose request ended stayed %v, waiting out another request's setup", elapsed)
	}
	if got := p.accepted.Load(); got != 1 {
		t.Fatalf("proxy saw %d connections; the waiter must wait, not dial", got)
	}

	// The stayer queues behind the stuck creator, whose request is then
	// cancelled: it must be released and set up its own connection.
	stayerCtx, cancelStayer := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelStayer()
	type result struct {
		body string
		err  error
	}
	stayerReq := newTestRequest(t, stayerCtx, []byte("{}"))
	stayerDone := make(chan result, 1)
	go func() {
		resp, err := rt.RoundTrip(stayerReq)
		if err != nil {
			stayerDone <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		stayerDone <- result{body: string(b)}
	}()
	time.Sleep(200 * time.Millisecond) // let the stayer reach the wait
	if got := p.accepted.Load(); got != 1 {
		t.Fatalf("proxy saw %d connections before the creator ended; the stayer must be waiting", got)
	}
	cancelCreator()

	select {
	case err := <-creatorDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("creator err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("creator did not return promptly after its request was cancelled")
	}
	select {
	case res := <-stayerDone:
		if res.err != nil || res.body != "ok" {
			t.Fatalf("stayer got %q, %v; want \"ok\" through its own setup", res.body, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stayer was never released after the creator's setup ended")
	}
	if got := p.accepted.Load(); got != 2 {
		t.Errorf("proxy saw %d connections, want 2 (creator's stuck one, stayer's own)", got)
	}
}

// A connection that dies silently mid-response -- no FIN, no RST, just no
// more bytes, as when a node vanishes behind the proxy -- is detected by the
// HTTP/2 PING health check and fails the body read, instead of hanging it.
func TestUtlsConnectionDetectsSilentDeathMidResponse(t *testing.T) {
	setH2Timings(t, 200*time.Millisecond, 200*time.Millisecond, h2IdleConnTimeout)
	captureSetupLogs(t)

	upstream, roots := newH2Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: ping\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	var frozen atomic.Bool
	p := newConnectProxy(t, tunnelTo(upstream, &frozen))
	rt := newTestRoundTripper(t, p, roots)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := roundTripWithin(t, rt, newTestRequest(t, ctx, []byte("{}")), 8*time.Second)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, 64)
	if _, err := resp.Body.Read(first); err != nil {
		t.Fatalf("first event: %v", err)
	}

	frozen.Store(true)
	start := time.Now()
	_, err = returnWithin(t, 8*time.Second, func() ([]byte, error) { return io.ReadAll(resp.Body) })
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("body ended cleanly although the path went dead")
	}
	if ctx.Err() != nil {
		t.Fatalf("the 5s watchdog ended the read (err %v): the dead connection went undetected", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("dead connection detected after %v; PING health check should take ~400ms", elapsed)
	}
}

// The per-attempt bound ends with setup: a response that runs well past
// connectAttemptTimeout -- as a long generation runs past the real 15s -- is
// read to the end. Whatever enforces the bound (the attempt's context, the
// guard that closes the raw connection, proxyutil's CONNECT watchdog, or a
// deadline set on the connection) must be gone once setup has succeeded, or
// every long response would be cut at 15s. The idle-connection timeout is
// shorter than the pause here too: it may only run while no stream is open.
func TestUtlsAttemptBoundDoesNotCapTheResponse(t *testing.T) {
	const attemptTimeout = 500 * time.Millisecond
	setSetupTimings(t, 1, attemptTimeout, nil)
	setH2Timings(t, h2ReadIdleTimeout, h2PingTimeout, attemptTimeout)
	captureSetupLogs(t)
	upstream, roots := newH2Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		time.Sleep(3 * attemptTimeout)
		_, _ = io.WriteString(w, "b")
	})
	p := newConnectProxy(t, tunnelTo(upstream, nil))
	rt := newTestRoundTripper(t, p, roots)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resp, err := roundTripWithin(t, rt, newTestRequest(t, ctx, []byte("{}")), 10*time.Second)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := returnWithin(t, 10*time.Second, func() ([]byte, error) { return io.ReadAll(resp.Body) })
	if err != nil || string(body) != "ab" {
		t.Fatalf("read %q, %v; want \"ab\" and no error: a bound meant for setup or for idle connections cut a live response", body, err)
	}
}

// The connection a request leaves behind is closed once it has carried no
// stream for h2IdleConnTimeout. Every request builds its own round tripper,
// so that connection is never reused, and nothing else here closes it. The
// health check PINGs it whether or not a stream is open, and every PING and
// ack resets the traffic-counting idle timers along the path (Clash's, the
// node's) that used to reap it -- so without this bound each request would
// leave a connection PINGing through the proxy for good. Here the health
// check runs every 100ms throughout, and the client must still hang up.
func TestUtlsIdleConnectionIsClosed(t *testing.T) {
	const idleConn = 500 * time.Millisecond
	setSetupTimings(t, 1, 2*time.Second, nil)
	setH2Timings(t, 100*time.Millisecond, 100*time.Millisecond, idleConn)
	captureSetupLogs(t)
	upstream, roots := newH2Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	})
	clientGone := make(chan struct{})
	var goneOnce sync.Once
	p := newConnectProxy(t, func(p *connectProxy, _ int, c net.Conn, br *bufio.Reader) {
		p.tunnel(c, br, upstream, nil, func() { goneOnce.Do(func() { close(clientGone) }) })
	})
	rt := newTestRoundTripper(t, p, roots)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := roundTripWithin(t, rt, newTestRequest(t, ctx, []byte("{}")), 8*time.Second)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("read %q, %v; want \"ok\"", body, err)
	}

	select {
	case <-clientGone:
	case <-time.After(idleConn + 3*time.Second):
		t.Fatalf("the client kept its idle connection open %v past the %v idle timeout: "+
			"left-behind connections are never closed", 3*time.Second, idleConn)
	}
	if got := p.accepted.Load(); got != 1 {
		t.Errorf("proxy saw %d connections, want 1", got)
	}
}
