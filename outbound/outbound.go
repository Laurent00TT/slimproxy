// Package outbound routes upstream connections through a proxy that may or
// may not be there.
//
// It exists because this deployment alternates between two VPNs with opposite
// requirements. Clash Verge is a system proxy: nothing is captured
// transparently, and a Go process that dials the provider directly is refused
// by source IP (403), so traffic must go through the local proxy port. vpn07
// is a TUN: everything is captured transparently, the proxy port has no
// listener, and a config still pointing at it fails every request in
// milliseconds. The same proxy-url is mandatory under one VPN and fatal under
// the other, and the switch happens outside this process with no signal.
//
// The engine's HTTP transport resolves its proxy once at construction, so no
// amount of config reloading can follow the switch (measured: editing the
// effective config changed nothing until restart). Instead of teaching the
// engine to re-resolve -- it lives in a dependency -- the proxy address it is
// given never changes: a loopback CONNECT relay owned by this package. The
// relay decides per connection whether to chain through the real proxy or
// dial direct, based on the one observable fact that distinguishes the two
// environments: whether the proxy port has a listener.
//
// Deciding on "is the port listening" rather than "does the request succeed"
// is deliberate. Under Verge a direct dial succeeds at the TCP and TLS layers
// and is only refused by the provider's edge -- discovering the wrong path by
// trying it costs a real request (and its tokens). A listen probe costs a
// sub-millisecond loopback dial and is observable before any request is
// risked. The probe's timeout is sized for loopback, which is why the config
// layer refuses a non-loopback proxy-url with fallback on: against a distant
// proxy the probe would read RTT as absence and silently divert everything
// direct.
//
// A listener is not necessarily an HTTP proxy -- the port could be held by a
// SOCKS-only config or another process entirely. The chain handshake runs
// under its own deadline, and on a protocol failure the relay falls back to
// direct and holds that verdict for a backoff window, reporting the actual
// handshake error rather than pretending the port was closed.
//
// The relay binds 127.0.0.1 only. Like the upstream proxy port it fronts, it
// is reachable by any local process and relays anywhere; that is the same
// exposure the machine already accepts by running the VPN's own local proxy.
package outbound

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options tunes the relay. The zero value is usable: every field falls back
// to the default named beside it.
type Options struct {
	// ProbeTTL is how long one probe's verdict is trusted. Its cost is the
	// window after a VPN switch during which connections still take the old
	// path: those either fail fast (chaining to a dead port) or burn one
	// request against the provider's region block (dialing direct under
	// Verge). Requests through this proxy arrive seconds apart, so a couple
	// of seconds bounds the damage to at most one or two requests.
	ProbeTTL time.Duration
	// ProbeTimeout bounds the probe dial. The target is loopback, where
	// "listening" answers in microseconds; anything slower is "down".
	ProbeTimeout time.Duration
	// DialTimeout bounds the outbound dial (direct or to the proxy).
	DialTimeout time.Duration
	// HandshakeTimeout bounds the chained CONNECT exchange -- the write and
	// the response read -- so a port that accepts but never speaks HTTP
	// cannot pin a goroutine and its sockets forever. The probe cannot see
	// this case: accepting is exactly what such a port does.
	HandshakeTimeout time.Duration
	// HandshakeBackoff is how long a handshake-class failure keeps the
	// verdict at direct without re-probing. Distinct from ProbeTTL because
	// the failure is different in kind: a dead port heals the moment the VPN
	// starts, but a listening port that is not an HTTP proxy is a
	// misconfiguration that re-testing every two seconds would turn into a
	// journal-flooding flip loop. Thirty seconds keeps the record honest --
	// still complaining, at a readable rate -- without hiding the problem.
	HandshakeBackoff time.Duration
	// OnFlip is told each time the chosen path changes, including the first
	// probe -- the initial choice is as worth recording as any later switch.
	// reason is empty when via is true, and the concrete failure (dial
	// error, handshake error) when it is false: the journal line built from
	// it must be able to say what actually happened, not guess "not
	// listening" for failures that were something else.
	// Called synchronously under the relay's probe lock so notifications
	// cannot arrive out of order; it must not call back into the relay.
	OnFlip func(via bool, reason string)
}

func (o Options) withDefaults() Options {
	if o.ProbeTTL <= 0 {
		o.ProbeTTL = 2 * time.Second
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 250 * time.Millisecond
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 15 * time.Second
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 5 * time.Second
	}
	if o.HandshakeBackoff <= 0 {
		o.HandshakeBackoff = 30 * time.Second
	}
	return o
}

// connectHeaderTimeout bounds how long a client may take to send its CONNECT
// once connected. The engine sends it immediately after dialing; a client
// that has sent nothing in five seconds is not the engine.
const connectHeaderTimeout = 5 * time.Second

// Listening answers whether the proxy named by rawURL has a listener right
// now, with one short dial. For one-shot callers -- the login flow -- that
// need the serving path's fallback decision without running a relay.
func Listening(rawURL string, timeout time.Duration) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	c, err := net.DialTimeout("tcp", hostport(u), timeout)
	if c != nil {
		_ = c.Close()
	}
	return err == nil
}

func hostport(u *url.URL) string {
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return u.Host
}

// Relay is a loopback HTTP CONNECT proxy that forwards through a configured
// upstream proxy when its port is listening and dials direct when it is not.
type Relay struct {
	target string // host:port of the upstream proxy candidate
	auth   string // Proxy-Authorization value for the chained CONNECT, or ""
	opt    Options
	ln     net.Listener

	// closed gates OnFlip. Handlers deliberately outlive Close -- they carry
	// live streams -- and their verdict updates would otherwise fire the
	// callback into an owner that has already torn down what the callback
	// writes to (measured: a post-shutdown flip reached a closed journal
	// channel, and a select-send on a closed channel panics past its default
	// case, taking the whole process with it).
	closed atomic.Bool

	// mu guards the probe cache and is held across the probe dial itself.
	// Holding a lock over network I/O is normally a smell; here the dial is
	// loopback with a 250ms cap, and the alternative -- letting concurrent
	// connections race the first probe -- hands the losers a default verdict
	// that may burn a request against the provider's region block.
	mu          sync.Mutex
	probed      bool
	up          bool
	lastProbe   time.Time
	brokenUntil time.Time
}

// Start parses the upstream proxy URL, binds a loopback listener on an
// ephemeral port, and begins serving. The URL must be http://: the relay
// chains with a plain CONNECT, and silently accepting a socks5 or https
// proxy-url here would mean speaking the wrong protocol to it.
func Start(rawURL string, opt Options) (*Relay, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("outbound: proxy-url %q: %w", rawURL, err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("outbound: proxy-url scheme %q is not supported with fallback-direct; only http:// proxies can be probed and chained", u.Scheme)
	}
	host := hostport(u)

	var auth string
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pass))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("outbound: bind relay: %w", err)
	}

	r := &Relay{target: host, auth: auth, opt: opt.withDefaults(), ln: ln}
	go r.serve()
	return r, nil
}

// URL is the address the engine should be given as its proxy.
func (r *Relay) URL() string { return "http://" + r.ln.Addr().String() }

// Target is the upstream proxy this relay probes and chains through.
func (r *Relay) Target() string { return r.target }

// Close stops accepting new connections and silences OnFlip. Established
// tunnels are left to end on their own: they carry live streams, and the
// process is usually exiting anyway.
func (r *Relay) Close() error {
	r.closed.Store(true)
	return r.ln.Close()
}

func (r *Relay) serve() {
	// Accept errors other than "listener closed" are retried with backoff,
	// after net/http.Server: what surfaces here besides closure is the
	// resource-exhaustion class (EMFILE and kin), which is transient by
	// nature. Returning on it would leave the listener open but unserved --
	// every engine dial would then sit in the kernel backlog with no answer,
	// a permanent outage bought by a passing shortage, on a port every probe
	// would keep calling alive.
	var delay time.Duration
	for {
		c, err := r.ln.Accept()
		if err != nil {
			if r.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			time.Sleep(delay)
			continue
		}
		delay = 0
		go r.handle(c)
	}
}

// viaProxy answers "should this connection chain through the proxy", probing
// when the cached verdict has expired.
func (r *Relay) viaProxy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if now.Before(r.brokenUntil) {
		return false
	}
	if r.probed && now.Sub(r.lastProbe) < r.opt.ProbeTTL {
		return r.up
	}
	c, err := net.DialTimeout("tcp", r.target, r.opt.ProbeTimeout)
	if c != nil {
		_ = c.Close()
	}
	if err != nil {
		r.record(false, err.Error())
	} else {
		r.record(true, "")
	}
	return r.up
}

// markDown records a failed proxy dial without waiting for the TTL: the
// probe said "listening" but the connection said otherwise, and the
// connection is the better witness.
func (r *Relay) markDown(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record(false, reason)
}

// markBroken records a handshake-class failure: the port is listening but
// did not behave like an HTTP proxy. The verdict is held at direct for the
// backoff window -- re-probing on the normal TTL would read the still-open
// port as healthy again and flip back within seconds, forever.
func (r *Relay) markBroken(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.brokenUntil = time.Now().Add(r.opt.HandshakeBackoff)
	r.record(false, reason)
}

// record updates the cache and reports flips. Callers hold mu.
func (r *Relay) record(up bool, reason string) {
	flip := !r.probed || up != r.up
	r.probed, r.up, r.lastProbe = true, up, time.Now()
	if flip && !r.closed.Load() && r.opt.OnFlip != nil {
		r.opt.OnFlip(up, reason)
	}
}

func (r *Relay) handle(c net.Conn) {
	// Deadline on the CONNECT read so a connection that never says what it
	// wants cannot hold the goroutine open; cleared once the tunnel is up,
	// where a stream may legitimately idle far longer.
	_ = c.SetReadDeadline(time.Now().Add(connectHeaderTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if req.Method != http.MethodConnect {
		// The engine only ever CONNECTs (every provider endpoint is https).
		// Answering anything else would make this a general open proxy, which
		// it has no reason to be.
		respond(c, "501 Not Implemented")
		_ = c.Close()
		return
	}
	hostport := req.Host
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		respond(c, "400 Bad Request")
		_ = c.Close()
		return
	}

	if r.viaProxy() {
		if r.chain(c, br, hostport) {
			return
		}
		// The proxy vanished or misbehaved between probe and use. Fall
		// through and dial direct rather than failing the request: if no VPN
		// is capturing traffic either, the provider will refuse it with an
		// honest status.
	}
	r.direct(c, br, hostport)
}

// directDialRetryDelay is the pause before the direct path's one retry.
// A var only so tests can shorten it.
var directDialRetryDelay = 250 * time.Millisecond

// direct dials the origin and splices. Under a TUN VPN the dial is captured
// transparently; with no VPN at all it reaches the provider bare and is
// refused there, which is the honest outcome -- there was no working path.
//
// A failed dial gets exactly one retry after a beat -- but only when the
// failure was FAST. Measured 2026-08-06 under the TUN VPN: 67 dial failures
// against ~1100 successes in an afternoon, arriving as scattered singles --
// transient churn in the VPN's capture path, exactly one beat wide, and a
// dial carries no request bytes so retrying it cannot duplicate anything.
// The timeout class is excluded on purpose: a dial that consumed the full
// DialTimeout is a blackholed route (SYNs silently dropped, the signature of
// a capture-path switch in progress), and retrying it doubles the caller's
// time-to-failure from ~15s to ~30s during exactly the windows this relay
// exists to survive. This is the only place the retry can live either way:
// the upstream engine attempts each dial once and hands the failure straight
// to the caller.
func (r *Relay) direct(c net.Conn, br *bufio.Reader, hostport string) {
	up, err := net.DialTimeout("tcp", hostport, r.opt.DialTimeout)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			respond(c, "502 upstream dial failed")
			_ = c.Close()
			return
		}
		time.Sleep(directDialRetryDelay)
		up, err = net.DialTimeout("tcp", hostport, r.opt.DialTimeout)
	}
	if err != nil {
		respond(c, "502 upstream dial failed")
		_ = c.Close()
		return
	}
	respond(c, "200 Connection Established")
	splice(c, br, up, up)
}

// chain dials the upstream proxy and issues the CONNECT there. Returns false
// only when the proxy could not be dialed or would not complete the
// handshake -- the caller falls back to direct -- and true whenever the
// proxy answered the CONNECT, whatever it answered: once the proxy has
// spoken HTTP, its verdict is the request's verdict, and retrying a refusal
// elsewhere would hide it.
func (r *Relay) chain(c net.Conn, br *bufio.Reader, hostport string) bool {
	up, err := net.DialTimeout("tcp", r.target, r.opt.DialTimeout)
	if err != nil {
		r.markDown("dial " + r.target + ": " + err.Error())
		return false
	}

	// The handshake runs under a deadline of its own. The probe can only see
	// that the port accepts; a port held by a SOCKS-only config or an
	// unrelated service accepts and then says nothing, and without this
	// deadline each such connection would pin a goroutine and two sockets
	// until the far side gave up -- which is never, for a listener that is
	// not talking.
	_ = up.SetDeadline(time.Now().Add(r.opt.HandshakeTimeout))

	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", hostport, hostport)
	if r.auth != "" {
		fmt.Fprintf(&b, "Proxy-Authorization: %s\r\n", r.auth)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(up, b.String()); err != nil {
		_ = up.Close()
		r.markBroken("CONNECT write to " + r.target + ": " + err.Error())
		return false
	}

	pbr := bufio.NewReader(up)
	resp, err := http.ReadResponse(pbr, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = up.Close()
		r.markBroken("CONNECT handshake with " + r.target + ": " + err.Error())
		return false
	}
	if resp.StatusCode != http.StatusOK {
		// Propagate the proxy's refusal verbatim rather than translating it:
		// the status is the diagnostic.
		respond(c, fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
		_ = resp.Body.Close()
		_ = up.Close()
		_ = c.Close()
		return true
	}
	_ = up.SetDeadline(time.Time{})
	respond(c, "200 Connection Established")
	// pbr may hold bytes the proxy sent after its 200; reading from up
	// directly would drop them.
	splice(c, br, up, pbr)
	return true
}

// respond writes a minimal HTTP/1.1 status. The reason phrase travels into
// the engine's error text, so it is chosen to say something.
func respond(c net.Conn, status string) {
	_, _ = io.WriteString(c, "HTTP/1.1 "+status+"\r\n\r\n")
}

// splice pumps both directions until each side's read ends, half-closing the
// peer's write side as each direction finishes so TLS close_notify sequences
// complete instead of being cut.
//
// The readers are passed separately from the conns because both sides sit
// behind bufio.Readers that may already hold bytes; reading from the conns
// would silently drop them.
func splice(c net.Conn, cr io.Reader, up net.Conn, upr io.Reader) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, cr)
		halfClose(up)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, upr)
		halfClose(c)
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = c.Close()
	_ = up.Close()
}

func halfClose(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.CloseWrite()
		return
	}
	_ = c.Close()
}
