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
	"sync/atomic"
	"time"

	tls "github.com/refraction-networking/utls"
	log "github.com/sirupsen/logrus"
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
	//
	// The ack wait is 30s, not 15s, because a lost PING is not a dead path
	// here. It rides Clash's TCP to the node, and a brief stall puts it into
	// Windows retransmission (RTO from 300ms, doubling: resends at ~0.5, 1.5,
	// 3.5, 7.5, 15.5s), so at 15s any stall outlasting ~7.5s after a PING
	// killed a request that would have recovered -- a failure this layer never
	// caused before the patch, and one Claude Code answers by resending the
	// whole body. 30s lets the ~15.5s resend land, and puts a live stream's
	// cutoff (30s silence + wait) at ~60s, past the node's own retransmission
	// of a ~25s stall; a dead connection is still found before the 90s stall
	// guard and the edge's 100s cutoff. Each such cut of a live request is
	// logged (lostPingReporter), which is how to tell whether 30s is enough.
	h2ReadIdleTimeout = 30 * time.Second
	h2PingTimeout     = 30 * time.Second
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

	// Each connection gets its own Transport, so the CountError hook below is
	// per connection: it learns which one it is through live, set once the
	// connection exists.
	var live atomic.Pointer[http2.ClientConn]
	tr := &http2.Transport{
		ReadIdleTimeout: h2ReadIdleTimeout,
		PingTimeout:     h2PingTimeout,
		IdleConnTimeout: h2IdleConnTimeout,
		CountError:      lostPingReporter(LogWithRequestID(ctx), addr, h2ReadIdleTimeout, h2PingTimeout, &live),
	}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		stopGuard()
		_ = conn.Close()
		return nil, setupPhaseH2, attemptError(attemptCtx, err)
	}
	live.Store(h2Conn)
	if !stopGuard() {
		// The attempt ended while the preface was going out: the guard has
		// closed, or is closing, the connection under h2Conn.
		_ = h2Conn.Close()
		return nil, setupPhaseH2, attemptCtx.Err()
	}
	return h2Conn, "", nil
}

// lostPingType is what the HTTP/2 client passes to CountError each time a
// health-check PING fails, just before it closes the connection: net/http's
// internal client (which x/net's Transport wraps from go1.27 on, handing it
// CountError through HTTP2Config) and x/net's own pre-1.27 client both say
// "conn_close_lost_ping".
const lostPingType = "conn_close_lost_ping"

// lostPingReporter returns a connection's CountError hook, which logs the
// health check killing that connection while it still carried a request.
// That is the one way the check can hurt live traffic -- a stall the path
// would have ridden out, taken for a dead connection -- and the request it
// ends leaves no other record of why: the error says only "http2: client
// connection lost", and slimproxy's journal keeps no more than a cause enum.
// These lines count how often the check cuts a request, which is what says
// whether h2PingTimeout is long enough.
//
// A failed PING is not always the check cutting a request, so two reports are
// skipped:
//   - The connection is already closed. The check did not close it, and the
//     report is not about the check. net/http's client readLoop does not stop
//     its ReadIdleTimeout timer when it exits, so a connection closed for any
//     other reason -- the leftover every request leaves, reaped at
//     h2IdleConnTimeout -- reports a lost PING a ReadIdleTimeout later. And a
//     PING still awaiting its ack when the peer or the proxy hangs up fails at
//     that moment, while the request the hang-up ended can still count as an
//     open stream.
//   - No stream is open. The check did close the connection, but it carried
//     no request, so nothing was hurt.
//
// Logging either would bury the real kills: the first alone is one false
// warning per request.
//
// The logger is the one of the request that set the connection up. Every
// request builds its own round tripper, so that is the request the connection
// carries.
func lostPingReporter(logger *log.Entry, addr string, readIdle, pingTimeout time.Duration, live *atomic.Pointer[http2.ClientConn]) func(errType string) {
	return func(errType string) {
		if errType != lostPingType {
			return
		}
		h2Conn := live.Load()
		if h2Conn == nil {
			// Not handed out yet, so no request on it.
			return
		}
		// Safe to call here: the client reports a lost PING from the health
		// check's own goroutine, holding none of the connection's locks.
		st := h2Conn.State()
		if st.Closed || st.StreamsActive == 0 {
			return
		}
		logger.Warnf("utls: health check closed the connection to %s with %d active stream(s): the PING it sent after %s without a frame got no ack within %s",
			addr, st.StreamsActive, readIdle, pingTimeout)
	}
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
