package outbound

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startOrigin is a TCP server speaking a trivial ping/pong so a test can
// prove bytes flowed end to end through whichever path the relay chose.
func startOrigin(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				if string(buf) == "ping" {
					_, _ = c.Write([]byte("pong"))
				}
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// fakeProxy is a minimal CONNECT proxy that counts the CONNECTs it serves,
// so a test can tell chained traffic from direct traffic.
type fakeProxy struct {
	ln       net.Listener
	connects atomic.Int32
}

func startFakeProxy(t *testing.T, addr string) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProxy{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *fakeProxy) handle(c net.Conn) {
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		// A probe dial connects and closes without sending anything; that is
		// not a CONNECT and must not count as one.
		_ = c.Close()
		return
	}
	if req.Method != http.MethodConnect {
		_ = c.Close()
		return
	}
	p.connects.Add(1)
	up, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		_ = c.Close()
		return
	}
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(up, br) }()
	go func() { defer wg.Done(); _, _ = io.Copy(c, up) }()
	wg.Wait()
	_ = c.Close()
	_ = up.Close()
}

// freePort reserves and releases a loopback port, returning an address that
// is (momentarily) guaranteed unlistened.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// connectThrough performs a CONNECT handshake with the relay and returns the
// tunnel plus the status line the relay answered.
func connectThrough(t *testing.T, relay *Relay, target string) (net.Conn, string) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(relay.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	return c, resp.Status
}

func pingPong(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("got %q, want pong", buf)
	}
}

// flipLog records OnFlip calls for assertions.
type flipLog struct {
	mu      sync.Mutex
	flips   []bool
	reasons []string
}

func (f *flipLog) add(via bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flips = append(f.flips, via)
	f.reasons = append(f.reasons, reason)
}

func (f *flipLog) get() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.flips...)
}

func (f *flipLog) lastReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reasons) == 0 {
		return ""
	}
	return f.reasons[len(f.reasons)-1]
}

func TestDirectWhenProxyDown(t *testing.T) {
	origin := startOrigin(t)
	var flips flipLog
	relay, err := Start("http://"+freePort(t), Options{OnFlip: flips.add})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	c, status := connectThrough(t, relay, origin.Addr().String())
	defer c.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status %q, want 200", status)
	}
	pingPong(t, c)

	if got := flips.get(); len(got) != 1 || got[0] != false {
		t.Errorf("flips = %v, want the initial direct verdict reported once", got)
	}
}

func TestChainsWhenProxyUp(t *testing.T) {
	origin := startOrigin(t)
	proxy := startFakeProxy(t, "127.0.0.1:0")
	var flips flipLog
	relay, err := Start("http://"+proxy.ln.Addr().String(), Options{OnFlip: flips.add})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	c, status := connectThrough(t, relay, origin.Addr().String())
	defer c.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status %q, want 200", status)
	}
	pingPong(t, c)

	if n := proxy.connects.Load(); n != 1 {
		t.Errorf("proxy served %d CONNECTs, want 1 (traffic must have chained)", n)
	}
	if got := flips.get(); len(got) != 1 || got[0] != true {
		t.Errorf("flips = %v, want the initial via-proxy verdict reported once", got)
	}
}

// TestFlipWhenProxyAppears is the VPN switch itself: the relay starts with
// the port dead (vpn07 world), the proxy comes up (Verge world), and traffic
// follows without the relay being told anything.
func TestFlipWhenProxyAppears(t *testing.T) {
	origin := startOrigin(t)
	addr := freePort(t)
	var flips flipLog
	relay, err := Start("http://"+addr, Options{ProbeTTL: 10 * time.Millisecond, OnFlip: flips.add})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	c1, _ := connectThrough(t, relay, origin.Addr().String())
	pingPong(t, c1)
	_ = c1.Close()

	proxy := startFakeProxy(t, addr)
	time.Sleep(20 * time.Millisecond) // let the verdict expire

	c2, _ := connectThrough(t, relay, origin.Addr().String())
	pingPong(t, c2)
	_ = c2.Close()

	if n := proxy.connects.Load(); n != 1 {
		t.Errorf("proxy served %d CONNECTs, want 1 after it came up", n)
	}
	if got := flips.get(); len(got) != 2 || got[0] != false || got[1] != true {
		t.Errorf("flips = %v, want [direct, via-proxy]", got)
	}
}

// TestFallsBackWhenProxyDiesMidVerdict: the probe said listening, the proxy
// died before the dial. The connection must still succeed -- direct -- and
// the verdict must be corrected without waiting out the TTL.
func TestFallsBackWhenProxyDiesMidVerdict(t *testing.T) {
	origin := startOrigin(t)
	addr := freePort(t)
	proxy := startFakeProxy(t, addr)
	var flips flipLog
	relay, err := Start("http://"+addr, Options{ProbeTTL: time.Hour, OnFlip: flips.add})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	c1, _ := connectThrough(t, relay, origin.Addr().String())
	pingPong(t, c1)
	_ = c1.Close()

	_ = proxy.ln.Close()

	// TTL is an hour: the stale "listening" verdict is still cached, so this
	// exercises the chain-dial failure path, not a fresh probe.
	c2, status := connectThrough(t, relay, origin.Addr().String())
	defer c2.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status %q, want 200 via direct fallback", status)
	}
	pingPong(t, c2)

	if got := flips.get(); len(got) != 2 || got[1] != false {
		t.Errorf("flips = %v, want the death recorded as a flip to direct", got)
	}
}

// TestProbeVerdictIsCached: within the TTL the relay must not re-dial the
// target for every connection.
func TestProbeVerdictIsCached(t *testing.T) {
	origin := startOrigin(t)
	proxy := startFakeProxy(t, "127.0.0.1:0")
	relay, err := Start("http://"+proxy.ln.Addr().String(), Options{ProbeTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	for i := 0; i < 3; i++ {
		c, _ := connectThrough(t, relay, origin.Addr().String())
		pingPong(t, c)
		_ = c.Close()
	}

	// Accepts = 1 probe + 3 CONNECTs. More means the cache is not caching;
	// the probe itself is invisible in connects (it sends no request).
	if n := proxy.connects.Load(); n != 3 {
		t.Errorf("proxy served %d CONNECTs, want 3", n)
	}
}

func TestNonConnectRefused(t *testing.T) {
	relay, err := Start("http://"+freePort(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	c, err := net.Dial("tcp", strings.TrimPrefix(relay.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "501") {
		t.Errorf("status line %q, want 501", line)
	}
}

func TestStartRejectsNonHTTPScheme(t *testing.T) {
	for _, raw := range []string{"socks5://127.0.0.1:1080", "https://127.0.0.1:7897"} {
		if _, err := Start(raw, Options{}); err == nil {
			t.Errorf("Start(%q) accepted a scheme the relay cannot chain through", raw)
		}
	}
}

// silentListener accepts and never speaks: the shape of a port held by a
// SOCKS-only config or an unrelated service. The probe cannot tell it from a
// healthy proxy -- accepting is exactly what it does.
type silentListener struct {
	ln      net.Listener
	mu      sync.Mutex
	conns   []net.Conn // held so the runtime finalizer cannot close them
	accepts atomic.Int32
}

func startSilentListener(t *testing.T, addr string) *silentListener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s := &silentListener{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepts.Add(1)
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
	})
	return s
}

// TestSilentListenerFallsBackAndBacksOff: a target that accepts but never
// answers must not hang the request -- the handshake deadline expires, the
// connection falls back to direct, the reported reason names the handshake
// rather than claiming "not listening", and the backoff keeps later
// connections off the broken port without re-probing it every TTL.
func TestSilentListenerFallsBackAndBacksOff(t *testing.T) {
	origin := startOrigin(t)
	silent := startSilentListener(t, "127.0.0.1:0")
	var flips flipLog
	relay, err := Start("http://"+silent.ln.Addr().String(), Options{
		ProbeTTL:         10 * time.Millisecond,
		HandshakeTimeout: 200 * time.Millisecond,
		HandshakeBackoff: time.Hour,
		OnFlip:           flips.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	start := time.Now()
	c1, status := connectThrough(t, relay, origin.Addr().String())
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status %q, want 200 via direct fallback", status)
	}
	pingPong(t, c1)
	_ = c1.Close()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("fallback took %v; the handshake deadline is not bounding the silent listener", elapsed)
	}
	if reason := flips.lastReason(); !strings.Contains(reason, "handshake") {
		t.Errorf("reason %q does not name the handshake; a journal built from it would misdirect the reader", reason)
	}

	// Within the backoff window the broken port must be left alone entirely:
	// no probe, no chain attempt.
	before := silent.accepts.Load()
	time.Sleep(20 * time.Millisecond) // well past ProbeTTL, well inside backoff
	c2, _ := connectThrough(t, relay, origin.Addr().String())
	pingPong(t, c2)
	_ = c2.Close()
	if after := silent.accepts.Load(); after != before {
		t.Errorf("broken port was contacted again during backoff (%d -> %d accepts)", before, after)
	}
}

// TestNoFlipAfterClose: handlers outlive Close by design, so a verdict
// change after Close must not fire OnFlip -- in production the callback
// writes into a journal whose channel the owner has already closed, and a
// select-send on a closed channel panics past its default case.
func TestNoFlipAfterClose(t *testing.T) {
	proxyAddr := freePort(t)
	proxy := startFakeProxy(t, proxyAddr)
	var flips flipLog
	relay, err := Start("http://"+proxyAddr, Options{ProbeTTL: time.Hour, OnFlip: flips.add})
	if err != nil {
		t.Fatal(err)
	}

	origin := startOrigin(t)
	c1, _ := connectThrough(t, relay, origin.Addr().String())
	pingPong(t, c1)
	_ = c1.Close()

	before := len(flips.get())
	_ = relay.Close()
	_ = proxy.ln.Close()
	// A surviving handler noticing the dead proxy after Close must stay
	// silent. markDown is what such a handler would call.
	relay.markDown("dial tcp: refused")
	if after := len(flips.get()); after != before {
		t.Errorf("OnFlip fired after Close (%d -> %d flips); in production this is a panic into a closed journal", before, after)
	}
}

// startOriginOn is startOrigin pinned to a specific address, for tests that
// need the origin to appear at an address the relay has already failed to
// dial.
func startOriginOn(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				if string(buf) == "ping" {
					_, _ = c.Write([]byte("pong"))
				}
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// TestDirectDialRetriesTransientFailure: a dial that fails once and would
// succeed a beat later must succeed through the relay's single retry.
// Measured 2026-08-06 under the TUN VPN: 67 scattered dial failures against
// ~1100 successes in an afternoon -- transient churn, exactly one beat wide.
func TestDirectDialRetriesTransientFailure(t *testing.T) {
	oldDelay := directDialRetryDelay
	directDialRetryDelay = 200 * time.Millisecond
	defer func() { directDialRetryDelay = oldDelay }()

	// A real free port with nothing listening yet: the first dial is refused.
	target := freePort(t)
	var flips flipLog
	relay, err := Start("http://"+freePort(t), Options{OnFlip: flips.add})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	// The origin appears during the retry pause.
	go func() {
		time.Sleep(80 * time.Millisecond)
		startOriginOn(t, target)
	}()

	c, status := connectThrough(t, relay, target)
	defer c.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status %q, want 200——一次重试本该救回这个瞬时失败", status)
	}
	pingPong(t, c)
}

// TestDirectDialStillFailsWhenTargetStaysDead: the retry must not change the
// outcome for a genuinely dead target -- one extra beat, then the honest 502.
func TestDirectDialStillFailsWhenTargetStaysDead(t *testing.T) {
	oldDelay := directDialRetryDelay
	directDialRetryDelay = 50 * time.Millisecond
	defer func() { directDialRetryDelay = oldDelay }()

	target := freePort(t) // nothing ever listens here
	relay, err := Start("http://"+freePort(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	_, status := connectThrough(t, relay, target)
	if !strings.Contains(status, "502") {
		t.Fatalf("CONNECT status %q, want 502 for a dead target", status)
	}
}
