package metrics

import "strings"

// Cause is why a request failed, in the coarsest terms that still change what
// an operator would do about it.
//
// The journal's stated purpose is to answer why a request failed, and for the
// largest category of failure it could not: 108 of 110 `ok:false` lines in the
// audited window carried neither a status code nor a reason, because
// usage.Record.Fail.Body -- which holds the Go error's full text -- was never
// read. Status alone cannot cover those: a transport failure never gets an HTTP
// status, so the field it would live in is zero exactly when the question is
// hardest.
//
// An enumeration rather than the text it is derived from. Fail.Body is the Go
// error verbatim, which carries the dial target and a platform syscall name --
// the same material proxy/errorenvelope.go keeps out of responses. The journal
// separately promises to hold no request or response bodies, and a failure
// message is a response body by any honest reading. So the text is read,
// classified, and dropped.
type Cause string

const (
	// CauseNone is a request that did not fail.
	CauseNone Cause = ""
	// CauseDNS is a name that would not resolve.
	CauseDNS Cause = "dns"
	// CauseConnect is a TCP connection that could not be established or was
	// broken before a response: refused, unreachable, reset, EOF.
	CauseConnect Cause = "connect"
	// CauseTLS is a handshake or certificate failure.
	CauseTLS Cause = "tls"
	// CauseTimeout is a deadline passing with no answer.
	CauseTimeout Cause = "timeout"
	// CauseCanceled is the client going away before the answer arrived.
	//
	// Separated from CauseTimeout because it is not a fault: the caller pressed
	// Ctrl-C. Folding the two together makes a busy interactive session look
	// like an outage, which is the opposite of what the health tracker needs.
	CauseCanceled Cause = "canceled"
	// CauseUpstream is a failure the upstream answered with an HTTP status.
	// The status itself is recorded separately; this only says the failure got
	// that far.
	CauseUpstream Cause = "upstream"
	// CauseOther is a genuine failure whose text matched nothing known.
	//
	// Kept distinct from CauseNone on purpose: "failed for a reason not
	// recognised here" is a different fact from "did not fail", and collapsing
	// them would hide the arrival of a new failure mode -- which is how the
	// original gap went unnoticed.
	CauseOther Cause = "other"
)

// TransportCauses are the causes derived from an error text rather than from a
// status code -- the ones that exist because a status could not describe them.
//
// Exported so the caller-facing wording in proxy/errorenvelope.go can be
// checked against this list instead of maintaining a parallel one. Two tables
// would drift, and the drift would surface as a caller being told "tls" for a
// request the journal recorded as "connect", with nothing to say which was
// right. Adding a cause here without giving it wording there fails a test.
func TransportCauses() []Cause {
	return []Cause{CauseDNS, CauseConnect, CauseTLS, CauseTimeout, CauseCanceled}
}

// Display renders the cause in Chinese, for a panel or a CLI table.
//
// Deliberately not String(). Cause is written to the journal and read back from
// it, so its canonical rendering has to be the wire value -- and a String()
// returning Chinese would quietly make every %v, %s and %q disagree with what
// is on disk, including in the failure messages of the tests meant to catch
// exactly that. Callers wanting the wire value use string(c) or any verb;
// callers wanting prose ask for it.
func (c Cause) Display() string {
	switch c {
	case CauseDNS:
		return "域名解析失败"
	case CauseConnect:
		return "连接失败"
	case CauseTLS:
		return "TLS 握手失败"
	case CauseTimeout:
		return "超时"
	case CauseCanceled:
		return "客户端取消"
	case CauseUpstream:
		return "上游报错"
	case CauseOther:
		return "其他"
	default:
		return "正常"
	}
}

// CauseFrom classifies a failure from its status code and error text.
//
// Called only for requests that actually failed; a zero status with empty text
// is read as "failed, reason not stated" rather than as success.
//
// Matching is on the Go standard library's own wording, which is stable across
// releases. The platform-specific spellings ("connectex" on Windows, "connect:"
// elsewhere) are both matched here so the classification does not silently
// change meaning when the same deployment moves between hosts.
func CauseFrom(status int, errText string) Cause {
	// An HTTP status means the upstream answered. Which status it was is
	// already recorded; repeating it here would add nothing.
	if status > 0 {
		return CauseUpstream
	}
	return CauseFromText(errText)
}

// CauseFromText classifies from the error text alone, ignoring any status.
//
// Split out because a status code does not always mean the upstream answered.
// A response built locally after a dial failure is stamped 500 by the SDK
// before it reaches anything that could know better, so the caller-facing
// classifier in proxy/errorenvelope.go has to reach past the status to the text
// -- while a usage record's status is trustworthy and CauseFrom should short
// -circuit on it. Same table, two entry points, one place to change it.
func CauseFromText(errText string) Cause {
	s := strings.ToLower(strings.TrimSpace(errText))
	if s == "" {
		return CauseOther
	}

	switch {
	case containsAny(s, "no such host", "lookup ", "dns"):
		return CauseDNS
	// Before timeout: "net/http: TLS handshake timeout" is both, and the TLS
	// half is the more specific and more actionable of the two.
	case containsAny(s, "tls", "certificate", "x509"):
		return CauseTLS
	// Before timeout as well: context.Canceled's text contains neither, but
	// keeping the pair adjacent makes the precedence between them explicit
	// rather than incidental.
	case containsAny(s, "context canceled", "context cancelled", "client disconnected"):
		return CauseCanceled
	case containsAny(s, "timeout", "timed out", "deadline exceeded"):
		return CauseTimeout
	case containsAny(s, "refused", "connectex", "dial ", "connect:",
		"unreachable", "reset by peer", "broken pipe", "eof", "no route to host"):
		return CauseConnect
	default:
		return CauseOther
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
