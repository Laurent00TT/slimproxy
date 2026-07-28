// Package journal records what happened, durably, so it can be asked about
// later.
//
// It exists because everything this proxy knows about itself is currently
// either prose or ephemeral. The application log is human sentences, the
// request log is a verbatim transcript too heavy and too secret to keep, and
// metrics.Collector is a one-hour window that dies with the process. None of
// them answers "why was that request slow yesterday afternoon".
//
// The shape is one JSON object per line, one file per day. That is deliberately
// unambitious: it needs no database, survives a kill -9 with at most the last
// buffered line lost, and can be read by jq when this package's own query
// command does not have the filter someone wants.
//
// # Two sources, because one cannot see everything
//
// A completed request produces a usage.Record, which carries tokens, timings
// and the upstream's quota headers. But a request rejected at the router --
// a scan for /api/hello, a call with no key -- never reaches an executor and
// never produces a record at all. Those are visible only to an HTTP middleware.
// Recording only the first would report a perfectly healthy error rate on a
// proxy being probed continuously from the internet; recording only the second
// would lose every number worth having.
//
// # What is deliberately absent
//
// No request or response bodies, and no headers. proxy/requestlog.go already
// writes those when asked, with the warnings that belong to them. A journal
// meant to be kept for a week and read casually must not be a place where a
// credential can turn up.
package journal

import (
	"strings"
	"time"
)

// Kind classifies an event. Everything shares one timeline, because the value
// of this file is being able to see a failed request next to the tunnel
// reconnect that explains it.
type Kind string

const (
	// KindRequest is one completed upstream request.
	KindRequest Kind = "req"
	// KindReject is inbound traffic that never reached an upstream: rejected
	// for want of a key, or aimed at a path this proxy does not serve.
	KindReject Kind = "rej"
	// KindProxy is this process starting or stopping.
	KindProxy Kind = "proxy"
	// KindTunnel is a tunnel transition.
	KindTunnel Kind = "tunnel"
	// KindCred is a credential appearing, expiring, or being removed.
	KindCred Kind = "cred"
	// KindDoctor is a diagnostic verdict.
	KindDoctor Kind = "doctor"
	// KindFidelity is the observed shape of an inbound request: which client,
	// how the prompt is structured, whether caching was asked for.
	//
	// Separate from KindRequest because it describes the opposite end. A
	// request event says what the upstream did; this says what the client
	// asked for, and the interesting failures live in the gap between them.
	KindFidelity Kind = "fidelity"
)

// Event is one line of the journal.
//
// Field names are short because there is one of these per request and they are
// read by machines far more often than by people. Every optional field is
// omitempty so an event carries only what was actually established -- an absent
// field means "not known", which is a distinction this project spends a lot of
// effort preserving elsewhere and would be a shame to lose here.
type Event struct {
	At   time.Time `json:"ts"`
	Kind Kind      `json:"kind"`

	// --- request fields ---

	// Route is "client→provider", the translation pair exercised.
	Route string `json:"route,omitempty"`
	Model string `json:"model,omitempty"`
	// OK reports success. Present on every request event, including successes,
	// because a filter for failures should not have to infer them from the
	// absence of something.
	OK *bool `json:"ok,omitempty"`
	// Status is the upstream HTTP status when a request failed. Absent on
	// success: the usage record never reports the status of one, and inventing
	// 200 would be inventing an observation.
	Status int `json:"status,omitempty"`
	// TTFTMs is time to first token; LatencyMs is the whole request. Both,
	// because they fail differently.
	TTFTMs    int64 `json:"ttft_ms,omitempty"`
	LatencyMs int64 `json:"ms,omitempty"`
	Tokens    int64 `json:"tok,omitempty"`
	// CacheRead and CacheCreation record prompt-cache activity.
	//
	// Kept per request rather than aggregated because the question is usually
	// "which requests stopped hitting, and what changed around then" -- and
	// that needs the individual readings next to the state events on the same
	// timeline.
	CacheRead     int64 `json:"cache_r,omitempty"`
	CacheCreation int64 `json:"cache_w,omitempty"`
	// Auth is which credential served it, so a failure can be attributed to one
	// rather than to "the pool".
	Auth string `json:"auth,omitempty"`
	// Exec is the executor that ran, which need not equal the provider asked
	// for.
	Exec string `json:"exec,omitempty"`
	// Quota5h is the fraction of the rolling five-hour window consumed. A
	// pointer so that 0.0 -- a freshly reset window -- is distinguishable from
	// a provider that reports nothing.
	Quota5h *float64 `json:"quota5h,omitempty"`

	// --- reject fields ---

	// Path is the request path, without query. Queries are dropped rather than
	// redacted: they are attacker-controlled on a public endpoint and have no
	// diagnostic value here.
	Path   string `json:"path,omitempty"`
	Method string `json:"method,omitempty"`
	// Count is how many identical rejects this line stands for. Scanners repeat;
	// one line per probe would drown the file that is supposed to explain it.
	Count int `json:"n,omitempty"`
	// Src says where the traffic entered from -- see Source.
	Src Source `json:"src,omitempty"`

	// --- fidelity fields (inbound request shape) ---

	// UAClient is the client class taken from the User-Agent: "claude-cli",
	// "(empty)", or the leading token of whatever else it was.
	//
	// Decisive rather than decorative: the upstream emulation rewrites the
	// system prompt of everything that is not claude-cli, and an empty header
	// takes the same branch. Recording the two apart is what would let anyone
	// notice the header failing to arrive.
	UAClient string `json:"ua,omitempty"`
	// SystemBlocks and CacheControls count the request's cacheable structure as
	// the client sent it.
	//
	// A request that arrives with cache_control markers and comes back with
	// zero cache reads is the alarm this pair exists to raise -- neither number
	// says anything useful alone.
	SystemBlocks  int   `json:"sys_blocks,omitempty"`
	CacheControls int   `json:"cc,omitempty"`
	Messages      int   `json:"msgs,omitempty"`
	Tools         int   `json:"tools,omitempty"`
	Thinking      bool  `json:"thinking,omitempty"`
	Stream        bool  `json:"stream,omitempty"`
	BodyKB        int64 `json:"body_kb,omitempty"`

	// --- state fields ---

	// State is the transition, for state events: "start", "up", "down".
	State string `json:"state,omitempty"`
	// Level is a diagnostic severity, for doctor events.
	Level string `json:"level,omitempty"`
	// Detail is free text for a human. Never carries credential material: the
	// producers are all in this repository and the reader is a log file kept
	// for a week.
	Detail string `json:"detail,omitempty"`
}

// Source is where inbound traffic arrived from.
//
// Worth recording because the two have completely different baselines. Loopback
// traffic is the operator's own; tunnel traffic includes whatever the internet
// sends at a public hostname, and this deployment is already being scanned. An
// error rate that mixes them describes neither.
type Source string

const (
	// SourceLocal is a connection from this machine.
	SourceLocal Source = "local"
	// SourceTunnel is a connection that arrived through Cloudflare.
	SourceTunnel Source = "tunnel"
	// SourceRemote is a direct connection from another host.
	SourceRemote Source = "remote"
)

// Failed reports whether a request event represents a failure.
func (e Event) Failed() bool { return e.Kind == KindRequest && e.OK != nil && !*e.OK }

// Noise reports whether an event is inbound traffic this deployment did not ask
// for.
//
// Public hostnames get scanned. Counting those probes as errors makes the error
// rate a measure of the internet's curiosity rather than of this proxy's
// health, so they are separable -- not discarded, because "how much is being
// thrown at me" is a real question, just a different one.
func (e Event) Noise() bool { return e.Kind == KindReject && e.Src != SourceLocal }

// boolPtr is a helper for the OK field, whose absence is meaningful.
func boolPtr(b bool) *bool { return &b }

// f64Ptr is the same for Quota5h.
func f64Ptr(f float64) *float64 { return &f }

// normalisePath strips the query and bounds the length.
//
// Both parts matter on a public endpoint: a query string is attacker-controlled
// and has no diagnostic value here, and an unbounded path would let anyone
// choose how many bytes of this file they get to write.
func normalisePath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	const max = 120
	if len(p) > max {
		return p[:max] + "…"
	}
	return p
}
