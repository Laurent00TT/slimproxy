// slimproxy patch: the utls round tripper's connection setup is bounded per
// attempt and retried (SLIMPROXY_PATCHES.md 第 15 条).
//
// The path is slimproxy -> proxy-url (Clash) -> proxy node -> api.anthropic.com,
// and every request builds a fresh client, so every request pays a setup.
// Upstream dialled with context.Background() and ran the uTLS handshake with
// no deadline. Clash answers CONNECT 200 at once and only then dials the node
// with its own 5s timeout, so a dead node surfaces as an EOF inside the
// handshake ~5.0s in; separately, handshakes hung ~60s until something
// outside cut them. 2026-09-21..23: 82 failures at ~5.0s and 47 at ~60s, all
// before any response byte, each forcing Claude Code to resend a 0.5-2MB
// body -- and with a single credential nothing above this layer retries
// (slimproxy's proxy/upstreamretry_test.go pins one attempt).
package helps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// Package variables rather than constants so the tests can shrink them.
var (
	// connectAttemptTimeout bounds one setup attempt: dial (the proxy's
	// CONNECT included), uTLS handshake and HTTP/2 preface. It must cover
	// Clash's own 5s node dial plus a handshake -- a healthy setup takes
	// ~0.6s -- or an attempt through a slow but live node would be cut off
	// before it could have succeeded, turning a slow request into a failed one.
	connectAttemptTimeout = 15 * time.Second
	// connectAttempts is the total number of setup attempts per request,
	// the first included.
	connectAttempts = 3
	// connectRetryBackoff[i] is the pause before attempt i+2; the last entry
	// repeats should connectAttempts outgrow the list.
	connectRetryBackoff = []time.Duration{1 * time.Second, 3 * time.Second}
	// After h2ReadIdleTimeout without a frame the client PINGs, and closes
	// the connection if no ack arrives within h2PingTimeout. Without them a
	// connection that dies silently mid-response (a node gone without a
	// FIN or RST reaching us through the proxy) hangs the stream until
	// something far above gives up. Anthropic streams carry SSE pings, so a
	// PING after 30s of silence costs nothing on a live connection. It is
	// sent whether or not a stream is open, though, which is what
	// h2IdleConnTimeout is for.
	h2ReadIdleTimeout = 30 * time.Second
	h2PingTimeout     = 15 * time.Second
	// h2IdleConnTimeout closes a connection once it has carried no stream
	// for that long. Every request builds its own round tripper, so the
	// connection a request leaves behind is never reused and nothing here
	// ever closes it: it used to be reaped by an idle timer further along
	// (Clash's, the node's, the peer's). The health check above PINGs idle
	// connections too, and each PING and ack is traffic that resets every
	// such timer counting it, so without this bound every request would
	// leave a connection PINGing through Clash and the node for good. The
	// timer runs only while no stream is open, so it never cuts a response
	// however long it runs. 90s, as net/http's DefaultTransport.
	h2IdleConnTimeout = 90 * time.Second
)

// The setup phases named in the per-attempt log line -- the only record of
// where a failing setup actually stalls, since slimproxy's journal keeps no
// more than a cause enum.
const (
	setupPhaseDial      = "dial"
	setupPhaseHandshake = "handshake"
	setupPhaseH2        = "h2"
)

// getOrCreateConnection returns the cached connection for host, or sets one
// up. At most one request per host sets up at a time; the others wait.
//
// A waiter leaves as soon as its own request is over: the setup it is
// waiting on can run connectAttempts full attempts, and a caller that has
// gone gains nothing by waiting that out. The creator releases the waiters
// on every exit -- failure and panic included -- or they would wait forever.
func (t *utlsRoundTripper) getOrCreateConnection(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()
	for {
		if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
			t.mu.Unlock()
			return h2Conn, nil
		}
		done, busy := t.pending[host]
		if !busy {
			break
		}
		t.mu.Unlock()
		select {
		case <-done:
			// The creator finished, not necessarily well: its own request may
			// have ended, or its attempts run out. Look again; with nothing to
			// reuse, this request makes its own attempts rather than
			// inheriting a verdict reached under another request's context.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		t.mu.Lock()
	}
	done := make(chan struct{})
	t.pending[host] = done
	t.mu.Unlock()

	var h2Conn *http2.ClientConn
	var err error
	defer func() {
		t.mu.Lock()
		delete(t.pending, host)
		if err == nil && h2Conn != nil {
			t.connections[host] = h2Conn
		}
		t.mu.Unlock()
		close(done)
	}()
	h2Conn, err = t.createConnection(ctx, host, addr)
	return h2Conn, err
}

// createConnection sets up a connection to addr: dial (through the proxy, if
// any), uTLS handshake, HTTP/2 preface. Each attempt is bounded by
// connectAttemptTimeout; a failed one is retried, up to connectAttempts in
// total, until the request's own context ends.
//
// Retrying is safe here and nowhere later. Until this returns, no byte of the
// request has been written -- the proxy has seen a CONNECT, the peer a
// ClientHello and an HTTP/2 preface, none of which the upstream acts on or
// bills. Once RoundTrip hands the request to the returned connection it may
// have reached the upstream, and a resend could run it twice, so failures
// from there on go back to the caller after one attempt, as they always did.
func (t *utlsRoundTripper) createConnection(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	logger := LogWithRequestID(ctx)
	var lastErr error
	for attempt := 1; attempt <= connectAttempts; attempt++ {
		if attempt > 1 {
			if errWait := waitSetupBackoff(ctx, attempt-2); errWait != nil {
				return nil, errWait
			}
		}
		start := time.Now()
		h2Conn, phase, err := t.connectOnce(ctx, host, addr)
		if err == nil {
			if attempt > 1 {
				logger.Infof("utls: connection to %s set up on attempt %d/%d (%s) after %d failed",
					addr, attempt, connectAttempts, time.Since(start).Round(time.Millisecond), attempt-1)
			}
			return h2Conn, nil
		}
		logger.Warnf("utls: connection setup to %s failed in %s phase on attempt %d/%d after %s: %v",
			addr, phase, attempt, connectAttempts, time.Since(start).Round(time.Millisecond), err)
		lastErr = err
		if ctx.Err() != nil {
			// The request itself is over -- client gone, or its own deadline
			// passed -- so another attempt could only be thrown away.
			return nil, err
		}
	}
	// slimproxy's journal classifies this text (metrics.CauseFromText): the
	// wrapper must not contain a word it matches ("tls", "dial ", "timeout",
	// "eof", ...), or a handshake EOF that files as "connect" today would
	// start filing under whatever the wrapper happened to say.
	return nil, fmt.Errorf("connection setup failed after %d attempts: %w", connectAttempts, lastErr)
}

// connectOnce makes one bounded setup attempt, returning the phase that
// failed alongside the error.
func (t *utlsRoundTripper) connectOnce(ctx context.Context, host, addr string) (*http2.ClientConn, string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, connectAttemptTimeout)
	defer cancel()

	conn, err := dialSetupContext(attemptCtx, t.dialer, "tcp", addr)
	if err != nil {
		return nil, setupPhaseDial, attemptError(attemptCtx, err)
	}
	// Close the raw connection if the attempt ends before setup completes.
	// HandshakeContext already interrupts itself; the guard is what bounds
	// the HTTP/2 preface write, which takes no context.
	stopGuard := context.AfterFunc(attemptCtx, func() { _ = conn.Close() })

	tlsConn := tls.UClient(conn, &tls.Config{ServerName: host, RootCAs: t.rootCAs}, tls.HelloChrome_Auto)
	if err := tlsConn.HandshakeContext(attemptCtx); err != nil {
		stopGuard()
		_ = conn.Close()
		return nil, setupPhaseHandshake, attemptError(attemptCtx, err)
	}

	tr := &http2.Transport{
		ReadIdleTimeout: h2ReadIdleTimeout,
		PingTimeout:     h2PingTimeout,
		IdleConnTimeout: h2IdleConnTimeout,
	}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		stopGuard()
		_ = conn.Close()
		return nil, setupPhaseH2, attemptError(attemptCtx, err)
	}
	if !stopGuard() {
		// The attempt ended while the preface was going out: the guard has
		// closed, or is closing, the connection under h2Conn.
		_ = h2Conn.Close()
		return nil, setupPhaseH2, attemptCtx.Err()
	}
	return h2Conn, "", nil
}

// dialSetupContext dials under ctx when the dialer supports it. Every dialer
// newUtlsRoundTripper can hold does: proxy.Direct, proxyutil's HTTP CONNECT
// dialer (whose CONNECT write and read are cut when ctx ends), and x/net's
// SOCKS5 dialer. A plain proxy.Dialer would leave the dial itself unbounded
// -- the handshake bound would still apply.
func dialSetupContext(ctx context.Context, d proxy.Dialer, network, addr string) (net.Conn, error) {
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return d.Dial(network, addr)
}

// attemptError names the attempt's end as the cause when it is what stopped
// the phase. A CONNECT read or a preface write cut by closing the connection
// otherwise surfaces as "use of closed network connection", which reads like
// a local bug and matches none of the journal's causes; with the context
// error in front it files as the timeout (or cancellation) it was.
func attemptError(attemptCtx context.Context, err error) error {
	ctxErr := attemptCtx.Err()
	if ctxErr == nil || errors.Is(err, ctxErr) {
		return err
	}
	return fmt.Errorf("%w: %w", ctxErr, err)
}

// waitSetupBackoff pauses before a retry, returning early with the request's
// error if it ends first.
func waitSetupBackoff(ctx context.Context, i int) error {
	if len(connectRetryBackoff) == 0 {
		return ctx.Err()
	}
	if i >= len(connectRetryBackoff) {
		i = len(connectRetryBackoff) - 1
	}
	timer := time.NewTimer(connectRetryBackoff[i])
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
