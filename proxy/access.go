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
// Cf-Connecting-Ip decides it, not RemoteAddr, and that is not a shortcut:
// cloudflared connects back to 127.0.0.1, so as far as RemoteAddr is concerned
// every request from the public internet is a local one. Without reading the
// header, "local" and "tunnel" would be indistinguishable and the split would
// be worthless.
//
// The header is trivially forgeable by anything that can already reach the
// port. That is acceptable here and nowhere else: this classifies a log line.
// Nothing in this program makes an authorisation decision from it, and nothing
// should -- inbound authorisation is api-keys, checked upstream of this.
func requestSource(r *http.Request) journal.Source {
	if r == nil {
		return journal.SourceRemote
	}
	if strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")) != "" {
		return journal.SourceTunnel
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
		return journal.SourceLocal
	}
	return journal.SourceRemote
}
