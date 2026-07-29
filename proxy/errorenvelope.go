package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// The error envelope: what a caller is told when a request fails.
//
// Upstream builds a failure response by putting the Go error's text straight
// into the message field -- sdk/api/handlers/handlers_errors.go does it in
// WriteErrorResponse, and ClaudeCodeAPIHandler does it again in its own
// override via toClaudeError. A dial failure therefore reached the caller as
//
//	{"type":"error","error":{"type":"api_error","message":
//	 "Post \"https://api.anthropic.com/v1/messages\": dial tcp 198.18.0.42:443:
//	  connectex: A connection attempt failed because the connected party did
//	  not properly respond after a period of time..."}}
//
// which hands over two things worth having: 198.18.0.0/15 is the RFC 2544
// range, so its presence says this machine routes through fake-ip proxy
// software, and "connectex" is a Windows-only syscall name, so it says which
// OS. Neither is the caller's business, and on a proxy reachable from the
// public internet neither should be a stranger's either.
//
// Fixing it at the source is not available: every one of those methods takes
// *interfaces.ErrorMessage, an internal/ type this module cannot name, so none
// of them can be wrapped or replaced. The response writer is the one place
// that sees every dialect's output, so that is where this sits.
//
// The rule is deny-by-default. A failure body is replaced unless it can be
// recognised as the upstream's own, because the leak in the first place came
// from a path nobody had enumerated, and an allowlist fails closed when the
// next unenumerated path appears.

// maxEnvelopeBody bounds what is held in memory before a decision.
//
// Failure bodies are small -- the largest upstream error observed is under 2 KB.
// Anything past this is not a failure envelope in any shape this code knows, so
// it is passed through rather than buffered: refusing to answer would be a worse
// failure than the leak this guards.
const maxEnvelopeBody = 64 << 10

// upstreamErrorTypes are the error.type values Anthropic itself emits.
//
// Membership is necessary but not sufficient for passthrough -- "api_error" is
// also what the SDK stamps on a locally generated failure, which is exactly the
// collision that makes structure alone useless as a signal. safeMessage settles
// those cases.
var upstreamErrorTypes = map[string]bool{
	"invalid_request_error": true,
	"authentication_error":  true,
	"permission_error":      true,
	"not_found_error":       true,
	"request_too_large":     true,
	"rate_limit_error":      true,
	"api_error":             true,
	"overloaded_error":      true,
	"billing_error":         true,
}

// ipLiteral matches a dotted quad, with or without a port.
//
// Deliberately not anchored to valid ranges: 999.1.2.3 is not a real address
// but its presence in a failure message still means something local leaked.
var ipLiteral = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// hostPort matches "host:port" forms that carry an internal endpoint even when
// the host is a name rather than an address.
var hostPort = regexp.MustCompile(`\b[a-zA-Z0-9.\-]+:\d{2,5}\b`)

// ErrorEnvelopeMiddleware replaces failure bodies that were built from a local
// error with a fixed message, and leaves the upstream's own failures alone.
//
// Placed as a middleware rather than a router hook because it has to wrap the
// writer before any handler touches it.
func ErrorEnvelopeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		w := &envelopeWriter{ResponseWriter: c.Writer}
		c.Writer = w
		c.Next()
		w.finish()
	}
}

// envelopeWriter defers a failure body until the handler is done with it.
//
// Success and streaming responses are never buffered: the decision is made once,
// at WriteHeader, and everything after it is a straight pass-through. A proxy
// that buffered SSE would turn a live token stream into a single delivery at the
// end, so getting this wrong is not a subtle regression.
type envelopeWriter struct {
	gin.ResponseWriter

	status    int
	buffering bool
	decided   bool
	buf       bytes.Buffer
	// overflowed records that the body outgrew the bound and was released to
	// the client, so finish must not write it a second time.
	overflowed bool
}

// decide is called once, as late as possible but before any byte is written.
func (w *envelopeWriter) decide(code int) {
	if w.decided {
		return
	}
	w.decided = true
	w.status = code

	// Only failures are candidates. A 2xx or 3xx body is never rewritten.
	if code < http.StatusBadRequest {
		return
	}
	// Streaming failures are past saving: the caller already holds a 200 and a
	// half-written event stream, and the error arrives as an SSE frame rather
	// than through this path. The leak this file exists for is not here --
	// bootstrap failures are written as JSON before the stream opens (see
	// code_handlers.go:260, where WriteErrorResponse runs on a dial failure
	// even for stream:true requests).
	if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		return
	}
	w.buffering = true
}

func (w *envelopeWriter) WriteHeader(code int) {
	w.decide(code)
	if w.buffering {
		// Held back: the status is written in finish, because a replacement
		// body needs its own Content-Length.
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *envelopeWriter) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader first means 200.
	w.decide(http.StatusOK)
	if !w.buffering {
		return w.ResponseWriter.Write(b)
	}
	if w.buf.Len()+len(b) > maxEnvelopeBody {
		// Too large to be a failure envelope. Release what was held and let the
		// rest stream through untouched.
		w.buffering = false
		w.overflowed = true
		w.ResponseWriter.WriteHeader(w.status)
		if w.buf.Len() > 0 {
			_, _ = w.ResponseWriter.Write(w.buf.Bytes())
			w.buf.Reset()
		}
		return w.ResponseWriter.Write(b)
	}
	return w.buf.Write(b)
}

func (w *envelopeWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Flush must not reach the real writer while a body is held, or the status line
// would go out ahead of the decision. Nothing streams on the failure path, so
// there is no latency cost to swallowing it here.
func (w *envelopeWriter) Flush() {
	if w.buffering {
		return
	}
	w.ResponseWriter.Flush()
}

// Written reports the real writer's state, which stays false while buffering --
// that is what upstream's `if !c.Writer.Written()` guards rely on to set headers.
func (w *envelopeWriter) Written() bool {
	if w.buffering {
		return false
	}
	return w.ResponseWriter.Written()
}

func (w *envelopeWriter) Status() int {
	if w.decided {
		return w.status
	}
	return w.ResponseWriter.Status()
}

// finish writes whatever the decision turned out to be.
func (w *envelopeWriter) finish() {
	if !w.buffering || w.overflowed {
		return
	}
	w.buffering = false

	body := w.buf.Bytes()
	out := body
	if !isUpstreamFailure(body) {
		out = replacementEnvelope(w.status, body)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(out)))
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(out)
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// claudeError is the failure shape both Anthropic and the SDK produce.
type claudeError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// isUpstreamFailure reports whether a body can be shown to the caller unchanged.
//
// Every condition has to hold. The type enumeration alone would pass a locally
// built "api_error" straight through, and the message check alone would pass any
// shape at all -- it is the conjunction that makes this an allowlist rather than
// two weak filters.
func isUpstreamFailure(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	var ce claudeError
	if err := json.Unmarshal(trimmed, &ce); err != nil {
		return false
	}
	if ce.Type != "error" || !upstreamErrorTypes[ce.Error.Type] {
		return false
	}
	return safeMessage(ce.Error.Message)
}

// safeMessage reports whether a message could have come from the upstream.
//
// Constraints on shape, not a list of forbidden words. Anthropic's failure text
// is a short English sentence; a Go transport error is long and carries an
// endpoint. Checking the shape means an error format nobody has seen yet is
// still caught, which a keyword list would not manage.
func safeMessage(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" || len(msg) > 200 {
		return false
	}
	if ipLiteral.MatchString(msg) || hostPort.MatchString(msg) {
		return false
	}
	// Path separators mean a local filename is in there.
	if strings.ContainsAny(msg, `\`) || strings.Contains(msg, "://") {
		return false
	}
	// Anything outside printable ASCII is not upstream failure text.
	for _, r := range msg {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// replacementEnvelope builds the body the caller receives instead.
//
// The original text is read to classify the failure and then dropped. A caller
// needs to know whether retrying is worth anything -- DNS failure and quota
// exhaustion call for opposite responses -- and that is the whole of what these
// strings carry. None of them names an address, a port, a path, or an OS.
func replacementEnvelope(status int, original []byte) []byte {
	msg := classifyFailure(status, string(original))
	body := claudeError{Type: "error"}
	body.Error.Type = errorTypeFor(status)
	body.Error.Message = msg
	out, err := json.Marshal(body)
	if err != nil {
		// Unreachable for this struct, but a failure here must still be a valid
		// envelope rather than an empty body.
		return []byte(`{"type":"error","error":{"type":"api_error","message":"upstream request failed"}}`)
	}
	return out
}

func errorTypeFor(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// classifyFailure turns the discarded original into one of a fixed set of lines.
//
// Matching is on the Go standard library's own wording, which is stable across
// releases and identical on every platform for these cases -- the platform-
// specific part ("connectex" versus "connect:") is exactly what is being
// dropped, so it is matched loosely and never echoed.
func classifyFailure(status int, original string) string {
	s := strings.ToLower(original)
	switch {
	case strings.Contains(s, "no such host"), strings.Contains(s, "dns"):
		return "upstream unreachable: dns resolution failed"
	case strings.Contains(s, "tls"), strings.Contains(s, "certificate"),
		strings.Contains(s, "x509"):
		return "upstream unreachable: tls handshake failed"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline exceeded"),
		strings.Contains(s, "timed out"):
		return "upstream timeout"
	case strings.Contains(s, "refused"), strings.Contains(s, "connectex"),
		strings.Contains(s, "dial "), strings.Contains(s, "connect:"),
		strings.Contains(s, "network is unreachable"), strings.Contains(s, "reset by peer"):
		return "upstream unreachable: connection failed"
	}
	// No recognised transport wording. Still replaced -- deny-by-default is the
	// point -- but the caller is told only what the status already says.
	switch status {
	case http.StatusUnauthorized:
		return "authentication failed"
	case http.StatusForbidden:
		return "permission denied"
	case http.StatusNotFound:
		return "not found"
	case http.StatusTooManyRequests:
		return "rate limited"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "upstream unavailable"
	default:
		return "upstream request failed"
	}
}
