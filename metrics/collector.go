// Package metrics accumulates completed-request telemetry for the CLI to show.
//
// It was extracted from the deleted web dashboard. The collection logic is
// unchanged; what differs is the consumer. The dashboard read this over SSE from
// another process, so the API handed back four loose values and capped the
// recent list at what one HTML panel could show. The CLI runs in the same
// process, so a snapshot is a struct and the cap is the caller's business.
package metrics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Window is how far back route aggregation looks.
const Window = time.Hour

// DefaultRecentCap is how many completed requests Snapshot returns by default.
const DefaultRecentCap = 20

// Sample is one completed request.
//
// Note what this cannot be: an in-flight request. usage.Record is published when
// a request finishes, so there is no way from this interface to observe a stream
// that is still open. Anything built on this shows recent requests, never live
// ones.
type Sample struct {
	At time.Time
	// Account identifies which upstream account served the request.
	//
	// NOT the inbound protocol dialect, despite what this field was called for
	// most of this project's life. usage.Record.Source resolves to
	// Auth.AccountInfo(), which is an email for OAuth credentials -- and the
	// API KEY ITSELF for key-based ones. It is redacted on the way in for that
	// reason; see redactAccount.
	//
	// The inbound dialect is genuinely not available from a usage record. The
	// fidelity probe in proxy/fidelity.go observes it from the request path
	// instead.
	Account string
	Target  string // upstream provider
	Model   string
	TTFT    time.Duration
	Tokens  int64
	Failed  bool
	// Status is the upstream HTTP status when the request failed, zero when it
	// succeeded or when the failure carried no code.
	//
	// Kept because 401 and 429 call for opposite responses -- replace the
	// credential, or wait -- and a display that shows both as "failed" hides
	// the one distinction this proxy exists to manage.
	Status int

	// Cause says why it failed, for the failures Status cannot describe.
	//
	// A transport failure never receives an HTTP status, so Status is zero for
	// exactly the cases that are hardest to diagnose -- which is how 108 of 110
	// recorded failures came to carry no reason at all. Derived from
	// Record.Fail.Body and deliberately not the text itself; see Cause.
	Cause Cause

	// Latency is the whole request, where TTFT is only its first token. Both
	// matter and they fail differently: a slow first token is the upstream
	// thinking, a slow total with a fast first token is a long generation.
	Latency time.Duration

	// Auth identifies the credential that served this request.
	//
	// Without it a pool of several credentials produces failures that cannot be
	// attributed -- "some requests 401" is not actionable, "this credential
	// 401s" is.
	Auth string

	// Executor is the concrete upstream implementation that ran, which need not
	// equal Target: aliases and compatibility layers mean the provider a client
	// asked for and the executor that answered can differ.
	Executor string

	// CacheRead and CacheCreation are the prompt-cache halves of the input.
	//
	// The pair that answers whether caching is working at all. Claude Code
	// leans on it heavily -- system prompt, file contents, conversation
	// history -- and a proxy that perturbs the request in any way turns every
	// turn into a full-price miss. The symptom is invisible in the reply and
	// shows up only as quota burning several times faster than it should, so
	// the numbers have to be recorded rather than inferred later.
	CacheRead     int64
	CacheCreation int64

	// Quota5h is the fraction of the rolling five-hour subscription window
	// consumed, in [0,1]. Negative means the upstream did not report it.
	//
	// The number that actually decides anything on a subscription. Requests per
	// minute says how busy the last minute was; this says whether there is
	// anything left to be busy with.
	Quota5h float64
}

// HasQuota reports whether the upstream told us the window utilisation.
//
// A separate predicate rather than testing for zero: 0.0 is a real reading --
// a freshly reset window -- and displaying "unknown" for it would be as wrong
// as displaying 0% for a window nobody reported.
func (s Sample) HasQuota() bool { return s.Quota5h >= 0 }

// Route names what served this request.
//
// Deliberately no longer "client → provider": that read as a protocol
// translation pair and was never one, because the left-hand side is the
// account. Naming it accurately costs a column heading and stops the panel
// asserting something it cannot know.
func (s Sample) Route() string {
	switch {
	case s.Target == "":
		return s.Model
	case s.Account == "":
		return s.Target
	default:
		return s.Account + " @ " + s.Target
	}
}

// RouteAgg is per-route aggregation over the window.
type RouteAgg struct {
	Route   string
	Count   int
	AvgTTFT time.Duration
	Pct     int
}

// Snapshot is everything derived from the samples at one instant.
type Snapshot struct {
	RPM    int           // requests in the trailing minute
	TTFT   time.Duration // mean time to first token over the window
	Recent []Sample      // newest first
	Active []RouteAgg    // busiest first
	Uptime time.Duration
	Total  int // samples currently held
	// Pending is what is in flight right now, oldest first.
	//
	// Independent of Recent: a request is in exactly one of the two, and it
	// moves from this list to that one when the upstream finishes answering.
	Pending []Pending
}

// Collector accumulates completed requests.
//
// Safe for concurrent use: HandleUsage runs on the proxy's request goroutines
// while the CLI's render loop reads.
type Collector struct {
	mu      sync.Mutex
	samples []Sample // append-only within the window, pruned on write
	started time.Time

	// RecentCap bounds Snapshot.Recent. Zero means DefaultRecentCap.
	RecentCap int

	// observe receives every sample, for consumers that outlive the window --
	// the on-disk journal. Called while the lock is held so an observer cannot
	// see a sample the collector has not recorded.
	//
	// Must not block: it runs on the usage manager's dispatch goroutine, and a
	// slow observer backs up that queue for every other plugin.
	observe func(Sample)

	// inflight is the other half of the picture, and the half a usage record
	// cannot supply: a record exists only once the request is over. Kept here
	// rather than alongside so the dashboard reads one source and cannot show a
	// snapshot whose two halves came from different instants.
	//
	// Never guarded by c.mu -- it has its own. See Snapshot.
	inflight *InFlight
}

// InFlight is the tracker request middleware reports arrivals and departures
// to. Never nil.
func (c *Collector) InFlight() *InFlight { return c.inflight }

// Observe registers a callback for every completed request.
//
// One observer, not a list: there is exactly one journal, and a slice would
// invite a second consumer to be added without anyone weighing what it costs
// to run on this goroutine.
func (c *Collector) Observe(fn func(Sample)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe = fn
}

// NewCollector returns a collector whose uptime starts now.
func NewCollector() *Collector {
	return &Collector{started: time.Now(), inflight: NewInFlight()}
}

// HandleUsage implements cliproxyusage.Plugin.
//
// It must not block: the usage manager dispatches to plugins on its own
// goroutine, but a slow plugin still backs up that queue.
func (c *Collector) HandleUsage(_ context.Context, r cliproxyusage.Record) {
	sample := SampleFrom(r)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.samples = append(c.samples, sample)
	c.prune(time.Now())

	// Handed on before the lock is released so an observer cannot see a sample
	// the collector has not recorded, and vice versa.
	if c.observe != nil {
		c.observe(sample)
	}
}

// SampleFrom projects a usage record onto what this proxy reports.
//
// Exported and separate because more than one consumer needs it -- the
// in-memory window here, and the on-disk journal -- and a second extraction
// would be a second place for the field mapping to drift. The record carries
// considerably more than this keeps; what is dropped is dropped on purpose,
// most of it being upstream bookkeeping with no operator-facing meaning.
func SampleFrom(r cliproxyusage.Record) Sample {
	at := r.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	return Sample{
		At:            at,
		Account:       redactAccount(r.Source),
		Target:        r.Provider,
		Model:         r.Model,
		TTFT:          r.TTFT,
		Latency:       r.Latency,
		Tokens:        r.Detail.TotalTokens,
		CacheRead:     r.Detail.CacheReadTokens,
		CacheCreation: r.Detail.CacheCreationTokens,
		Failed:        r.Failed,
		Status:        r.Fail.StatusCode,
		Cause:         causeOf(r),
		Auth:          r.AuthID,
		Executor:      r.ExecutorType,
		Quota5h:       quotaFromHeaders(r.ResponseHeaders),
	}
}

// causeOf classifies a record's failure, and says nothing about a success.
//
// Reads Fail.Body, which upstream fills with the Go error's verbatim text
// (internal/runtime/executor/helps/usage_helpers.go, failFromErrors). That text
// is the only description a transport failure ever produces, and it is also
// full of local addresses -- so it is classified here and does not travel any
// further.
func causeOf(r cliproxyusage.Record) Cause {
	if !r.Failed {
		return CauseNone
	}
	return CauseFrom(r.Fail.StatusCode, r.Fail.Body)
}

// prune drops samples older than the window. Called under mu.
//
// It filters rather than trimming a prefix, because samples do NOT arrive in
// chronological order: Record.RequestedAt is when a request STARTED, but the
// record is published when it FINISHES. A stream that opened at T+0 and closed
// at T+60 is appended after a short request that opened at T+30 and closed at
// T+31. Any prefix-trim would then stop at the first recent entry and prune
// nothing.
func (c *Collector) prune(now time.Time) {
	cut := now.Add(-Window)
	kept := c.samples[:0]
	for _, sm := range c.samples {
		if !sm.At.Before(cut) {
			kept = append(kept, sm)
		}
	}
	c.samples = kept
}

// Snapshot returns the derived numbers for one render.
func (c *Collector) Snapshot(now time.Time) Snapshot {
	// Read before c.mu is taken. InFlight has a lock of its own, and nesting
	// the two here would establish an ordering that a later caller reaching the
	// other way could deadlock against. Nothing needs them consistent with each
	// other: a request that completes in the gap is simply reported as finished
	// one frame earlier than it would have been.
	pending := c.inflight.Snapshot()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)

	// Pending goes in at construction, not at the end. There is an early return
	// two lines down for the no-samples case, and that case -- a proxy that has
	// started a request but not yet finished one -- is exactly when something in
	// flight is the only thing there is to report.
	snap := Snapshot{Uptime: now.Sub(c.started), Total: len(c.samples), Pending: pending}
	if len(c.samples) == 0 {
		return snap
	}

	// req/min over the trailing minute, not the whole window: the headline
	// number should react within a minute of traffic stopping.
	minuteAgo := now.Add(-time.Minute)
	var ttftSum time.Duration
	var ttftN int
	byRoute := map[string]*RouteAgg{}

	for _, sm := range c.samples {
		if sm.At.After(minuteAgo) {
			snap.RPM++
		}
		if sm.TTFT > 0 {
			ttftSum += sm.TTFT
			ttftN++
		}
		key := sm.Route()
		agg := byRoute[key]
		if agg == nil {
			agg = &RouteAgg{Route: key}
			byRoute[key] = agg
		}
		agg.Count++
		agg.AvgTTFT += sm.TTFT
	}

	if ttftN > 0 {
		snap.TTFT = ttftSum / time.Duration(ttftN)
	}

	// Recent: newest first, capped. Sorted explicitly rather than read off the
	// tail, for the same out-of-order reason prune filters (see prune).
	cap := c.RecentCap
	if cap <= 0 {
		cap = DefaultRecentCap
	}
	byTime := make([]Sample, len(c.samples))
	copy(byTime, c.samples)
	sort.Slice(byTime, func(i, j int) bool { return byTime[i].At.After(byTime[j].At) })
	if len(byTime) > cap {
		byTime = byTime[:cap]
	}
	snap.Recent = byTime

	total := len(c.samples)
	for _, agg := range byRoute {
		if agg.Count > 0 {
			agg.AvgTTFT /= time.Duration(agg.Count)
		}
		agg.Pct = int(float64(agg.Count) / float64(total) * 100)
		snap.Active = append(snap.Active, *agg)
	}
	sort.Slice(snap.Active, func(i, j int) bool {
		if snap.Active[i].Count != snap.Active[j].Count {
			return snap.Active[i].Count > snap.Active[j].Count
		}
		return snap.Active[i].Route < snap.Active[j].Route
	})
	return snap
}

// Uptime since the collector was created, which is process start in practice.
func (c *Collector) Uptime(now time.Time) time.Duration { return now.Sub(c.started) }

// anthropicQuotaHeader reports how much of the rolling five-hour subscription
// window has been consumed, as a percentage.
//
// This is the number that decides anything on a subscription plan. Requests per
// minute describes how busy the last minute was; utilisation says whether there
// is anything left to be busy with, and it is the only warning before a 429.
const anthropicQuotaHeader = "Anthropic-Ratelimit-Unified-5h-Utilization"

// quotaFromHeaders extracts the window utilisation as a fraction in [0,1].
//
// Returns -1 when the upstream said nothing, which is distinct from 0: a fresh
// window really is at zero, and reporting "unknown" for it would be as wrong as
// reporting 0% for a provider that never sends the header at all.
//
// Only this one value is read out of the headers. Keeping the whole http.Header
// would put upstream authentication echoes and request ids into every sample,
// and from there into anything that persists them.
func quotaFromHeaders(h http.Header) float64 {
	if h == nil {
		return -1
	}
	raw := strings.TrimSpace(h.Get(anthropicQuotaHeader))
	if raw == "" {
		return -1
	}
	raw = strings.TrimSuffix(raw, "%")
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return -1
	}
	// Providers have been observed reporting both 0-1 and 0-100. Values above 1
	// are read as percentages; the ambiguity at exactly 1.0 resolves to "fully
	// consumed", which is the safe reading of the two.
	if v > 1 {
		v /= 100
	}
	if v < 0 {
		return -1
	}
	if v > 1 {
		v = 1
	}
	return v
}

// Cached reports whether any part of this request's input was served from the
// prompt cache.
func (s Sample) Cached() bool { return s.CacheRead > 0 }

// CacheRatio is the fraction of input tokens that came from cache, in [0,1].
//
// Returns -1 when there is nothing to divide, which is not the same as zero: a
// request with no cacheable prefix has no ratio, while a request that should
// have hit and did not has a ratio of zero. Collapsing them would hide exactly
// the case worth alarming on.
func (s Sample) CacheRatio() float64 {
	total := s.CacheRead + s.CacheCreation + s.Tokens
	if total <= 0 {
		return -1
	}
	return float64(s.CacheRead) / float64(total)
}

// redactAccount keeps an account identifier usable without keeping a secret.
//
// usage.Record.Source is an email for OAuth credentials and the API key itself
// for key-based ones -- and this value reaches the panel, the journal on disk,
// and anything else that reports on traffic. An email is an identifier; a key
// is a credential, and the two arrive through the same field with nothing to
// tell them apart.
//
// The test is the "@": every OAuth account here is an email address, and no API
// key is. Anything else is truncated to a recognisable stub, which is enough to
// tell two credentials apart in a listing and useless to anyone who reads the
// file.
func redactAccount(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "@") {
		return s
	}
	// A prefix alone is not enough: two keys from the same provider share it,
	// so "sk-a***" would collapse every Anthropic key into one identity and
	// lose the reason for recording it -- telling two credentials apart in a
	// listing. A short digest restores that without carrying any of the secret.
	const keep = 4
	sum := sha256.Sum256([]byte(s))
	digest := hex.EncodeToString(sum[:])[:4]
	if len(s) <= keep {
		return "***" + digest
	}
	return s[:keep] + "***" + digest
}
