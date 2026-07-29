package proxy

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Who is allowed to tell this proxy where a request came from.
//
// gin ships trusting every proxy on the internet: NewEngine sets
// trustedProxies to 0.0.0.0/0 and ::/0 with ForwardedByClientIP on, so
// ClientIP() returns whatever the caller put in X-Forwarded-For. Neither this
// module nor CLIProxyAPI narrowed that, which made the client IP in the access
// log a value chosen by the client. Sending
//
//	X-Forwarded-For: 203.0.113.99
//
// to this port was enough to have 203.0.113.99 recorded as the origin -- and
// since Cloudflare appends to that header rather than replacing it, a request
// through the tunnel could name any address it liked, including 127.0.0.1 to
// pass as the operator's own traffic.
//
// Loopback is the only trustworthy relay here, and it genuinely is one:
// cloudflared runs on this machine and connects back to 127.0.0.1, so every
// tunnelled request arrives from loopback carrying the real address in a
// header. Trusting exactly that, and nothing else, is what makes the recorded
// address mean something again.
//
// What this deliberately does not do is set gin's TrustedPlatform. It looks
// like the right tool -- TrustedPlatform = "CF-Connecting-IP" is the documented
// Cloudflare recipe -- but ClientIP() returns that header's value before it
// checks anything at all (gin context.go:804-809, no isTrustedProxy call on
// that path). On a proxy that also serves direct local clients, any caller
// could then set CF-Connecting-IP and be believed. The header is still read,
// but through RemoteIPHeaders, which is gated on the connection actually coming
// from a trusted relay.
//
// Residual exposure, stated rather than papered over: a process on this machine
// can still forge the header, because loopback has to be trusted for the tunnel
// to work at all. That is not a meaningful escalation -- anything running here
// can read slimproxy.yaml and take the API keys directly -- but it does mean
// "local" in the journal is a statement about this machine, not a guarantee
// about which program on it.
var trustedRelays = []string{"127.0.0.1/32", "::1/128"}

// remoteIPHeaders is consulted in order, and only for connections from a
// trusted relay.
//
// CF-Connecting-IP first because Cloudflare sets it authoritatively -- it
// replaces any inbound copy, where X-Forwarded-For is appended to and therefore
// carries whatever the caller prepended. Both are checked because the proxy is
// useful behind other front-ends too, and X-Forwarded-For is what those speak.
var remoteIPHeaders = []string{"CF-Connecting-IP", "X-Forwarded-For"}

// configureTrustedProxies narrows what the engine will believe about origins.
//
// Returns an error rather than logging one so the caller decides how loud to
// be; a silent failure here restores trust-everyone without any outward sign,
// which is the failure mode this whole file exists to remove.
func configureTrustedProxies(engine *gin.Engine) error {
	engine.RemoteIPHeaders = remoteIPHeaders
	return engine.SetTrustedProxies(trustedRelays)
}

// engineConfigurator returns the hook handed to the SDK.
//
// The SDK's option takes no error, so a failure is reported here and reported
// as what it costs: not a broken proxy, but a log and a journal whose origin
// column is back to being caller-supplied.
func engineConfigurator() func(*gin.Engine) {
	return func(engine *gin.Engine) {
		if err := configureTrustedProxies(engine); err != nil {
			log.Errorf("slimproxy: 无法收窄可信代理范围，访问日志中的客户端 IP "+
				"将保持可被请求方伪造的状态（gin 默认信任 0.0.0.0/0）: %v", err)
		}
	}
}
