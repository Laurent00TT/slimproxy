package proxy

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Laurent00TT/slimproxy/journal"
)

// AccessJournalMiddleware records inbound requests that were refused.
//
// The other half of the journal. A completed upstream request publishes a
// usage.Record carrying tokens, timings and quota headers; a request rejected
// at the router -- a scan for /api/hello, a call with no key -- never reaches
// an executor and produces nothing at all. This deployment is already being
// probed from the internet, so a journal built only on usage records would
// report a spotless error rate while the public hostname absorbs a steady
// stream of 404s.
//
// Only non-2xx responses are recorded. A successful request either produced a
// usage record already, or was a health check nobody needs a week of history
// for -- and logging every success here would duplicate the one and bury the
// other.
func AccessJournalMiddleware(folder *journal.RejectFolder) gin.HandlerFunc {
	return func(c *gin.Context) {
		at := time.Now()
		c.Next()

		status := c.Writer.Status()
		if status >= 200 && status < 300 {
			return
		}
		folder.Add(c.Request.Method, c.Request.URL.Path, status, requestSource(c.Request), at)
	}
}

// requestSource classifies where a request entered from.
//
// RemoteAddr is consulted first and the header only afterwards. The order is
// the substance: cloudflared runs on this machine and connects back to
// 127.0.0.1, so a tunnelled request is indistinguishable from a local one by
// address alone and Cf-Connecting-Ip is the only thing that separates them --
// but a connection that did not come from loopback did not come through the
// tunnel, whatever headers it carries. Reading the header first, as this did,
// meant a direct caller could hand over one header and be filed as tunnel
// traffic; on a proxy listening on 0.0.0.0 that was open to the entire network.
//
// What remains forgeable is a process on this machine claiming to be tunnel
// traffic, which loopback having to be trusted makes unavoidable -- see the
// commentary in trustedproxy.go, which narrows the same trust for the client
// IP that upstream's access log records.
//
// Still only a log line either way. Nothing in this program makes an
// authorisation decision from it, and nothing should -- inbound authorisation
// is api-keys, checked upstream of this.
func requestSource(r *http.Request) journal.Source {
	if r == nil {
		return journal.SourceRemote
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return journal.SourceRemote
	}
	if strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")) != "" {
		return journal.SourceTunnel
	}
	return journal.SourceLocal
}
