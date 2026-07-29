package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Laurent00TT/slimproxy/journal"
)

// clientIPSeenBy runs one request through a configured engine and reports what
// ClientIP() concluded -- the same call upstream's access log makes.
func clientIPSeenBy(t *testing.T, remoteAddr string, headers map[string]string) string {
	t.Helper()

	r := gin.New()
	if err := configureTrustedProxies(r); err != nil {
		t.Fatalf("configureTrustedProxies: %v", err)
	}

	var seen string
	r.GET("/x", func(c *gin.Context) { seen = c.ClientIP() })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
	return seen
}

// TestForgedForwardedForIsIgnored is the regression.
//
// gin trusts 0.0.0.0/0 out of the box, so before this the assertion below
// returned the forged value and every IP in the access log was caller-supplied.
// Removing configureTrustedProxies from clientIPSeenBy makes this fail, which
// is how it was confirmed to test something.
func TestForgedForwardedForIsIgnored(t *testing.T) {
	const real = "198.51.100.7"
	const forged = "203.0.113.99"

	got := clientIPSeenBy(t, real+":51000", map[string]string{
		"X-Forwarded-For": forged,
	})
	if got == forged {
		t.Errorf("ClientIP = %q，请求方自己声明的地址被采信了", got)
	}
	if got != real {
		t.Errorf("ClientIP = %q, want %q（实际连接地址）", got, real)
	}
}

// TestForgedLoopbackClaimIsIgnored.
//
// The nastiest shape of the forgery: claiming to be the operator's own traffic.
// Anything filtering or trusting by "is this local" would have been reading a
// value the internet chose.
func TestForgedLoopbackClaimIsIgnored(t *testing.T) {
	for _, hdr := range []string{"X-Forwarded-For", "CF-Connecting-IP", "X-Real-IP"} {
		t.Run(hdr, func(t *testing.T) {
			got := clientIPSeenBy(t, "198.51.100.7:51000", map[string]string{
				hdr: "127.0.0.1",
			})
			if got == "127.0.0.1" {
				t.Errorf("外部请求通过 %s 伪装成了本机流量", hdr)
			}
		})
	}
}

// TestTunnelledRequestKeepsRealClientIP is the other half: the narrowing must
// not cost the address that makes tunnel logs worth keeping.
//
// cloudflared connects from loopback, so without trusting loopback every
// request from the internet would be recorded as 127.0.0.1.
func TestTunnelledRequestKeepsRealClientIP(t *testing.T) {
	const real = "203.0.113.42"

	got := clientIPSeenBy(t, "127.0.0.1:52000", map[string]string{
		"CF-Connecting-IP": real,
	})
	if got != real {
		t.Errorf("ClientIP = %q, want %q：隧道流量丢失了真实来源", got, real)
	}
}

// TestTunnelledForwardedForTakesTheAppendedAddress.
//
// Cloudflare appends to X-Forwarded-For instead of replacing it, so the header
// arrives as "<whatever the caller sent>, <address Cloudflare observed>".
// Taking the leftmost value -- the intuitive reading of "the original client"
// -- would take the forged one; gin walks it right to left, which is why the
// caller's prefix cannot win.
func TestTunnelledForwardedForTakesTheAppendedAddress(t *testing.T) {
	const observed = "203.0.113.42"

	got := clientIPSeenBy(t, "127.0.0.1:52000", map[string]string{
		"X-Forwarded-For": "10.0.0.1, 127.0.0.1, " + observed,
	})
	if got != observed {
		t.Errorf("ClientIP = %q, want %q：XFF 前缀里的伪造值被采信了", got, observed)
	}
}

// TestTrustedRelayListIsAcceptedByGin guards the CIDR strings themselves.
//
// SetTrustedProxies is the only thing that validates them, and its failure path
// leaves the engine trusting everyone -- silently, which is precisely the state
// this file removes. A typo here would be invisible without this.
func TestTrustedRelayListIsAcceptedByGin(t *testing.T) {
	if err := configureTrustedProxies(gin.New()); err != nil {
		t.Fatalf("可信代理列表被 gin 拒绝，引擎会退回信任全网: %v", err)
	}
}

// TestRequestSourceRequiresLoopbackBeforeBelievingTheHeader.
//
// The classification used to read Cf-Connecting-Ip first, so a direct caller
// could be filed as tunnel traffic by sending one header. On a proxy listening
// on 0.0.0.0 that was available to the whole network, and it corrupts the one
// split the journal keeps: loopback traffic is the operator's own, tunnel
// traffic is the internet's, and the baseline for each is different.
func TestRequestSourceRequiresLoopbackBeforeBelievingTheHeader(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		cfHeader   string
		want       journal.Source
	}{
		{"直连且伪造 CF 头", "198.51.100.7:51000", "203.0.113.99", journal.SourceRemote},
		{"直连无头", "198.51.100.7:51000", "", journal.SourceRemote},
		{"cloudflared 转发", "127.0.0.1:52000", "203.0.113.42", journal.SourceTunnel},
		{"本机直连", "127.0.0.1:52000", "", journal.SourceLocal},
		{"IPv6 回环 + 隧道", "[::1]:52000", "203.0.113.42", journal.SourceTunnel},
		{"IPv6 回环", "[::1]:52000", "", journal.SourceLocal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.cfHeader != "" {
				req.Header.Set("Cf-Connecting-Ip", tc.cfHeader)
			}
			if got := requestSource(req); got != tc.want {
				t.Errorf("requestSource = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequestSourceHandlesNil pins the defensive branch, which classifies as
// remote -- the conservative end, since an unknown origin should not be filed
// among the operator's own traffic.
func TestRequestSourceHandlesNil(t *testing.T) {
	if got := requestSource(nil); got != journal.SourceRemote {
		t.Errorf("requestSource(nil) = %q, want %q", got, journal.SourceRemote)
	}
}
