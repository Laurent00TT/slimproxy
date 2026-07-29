package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// refusalHooks restores information that the protocol translation throws away.
//
// When Anthropic blocks a request on policy it answers HTTP 200 with
// stop_reason "refusal" and a stop_details object carrying a category and a
// human-readable explanation. Translating that into the OpenAI schema produces
// an empty content string and finish_reason "stop" -- byte-identical to the
// model simply choosing to say nothing. The caller cannot tell a policy
// decision from an ordinary empty completion, and nothing is written to the log
// either, so neither can the operator.
//
// This is the right layer to fix it: the information exists before the
// translator runs and is gone after, so both halves of the problem are visible
// from here.
//
//   - NormalizeResponseBefore sees the upstream body while stop_details is
//     still present. It logs, and records the finding for this request.
//   - NormalizeResponseAfter sees the translated body and rewrites
//     finish_reason to "content_filter", which is what OpenAI clients already
//     understand to mean "blocked", rather than inventing a field nobody reads.
//
// Content is left empty on purpose. Writing the explanation into the assistant
// message would make a policy notice indistinguishable from model output, which
// trades one ambiguity for a worse one.
//
// Registration is the awkward part, and not where you would expect: see
// installRefusalHooks for why these have to be reinstalled per request rather
// than once at startup.
type refusalHooks struct {
	// pending carries a finding from the Before hook to the After hook for the
	// same request. Keyed by the context, which the registry passes unchanged
	// through both calls of one translation.
	//
	// A sync.Map rather than a field because translations run concurrently, and
	// a plain field would let one request's refusal rewrite another's response.
	pending sync.Map

	// live approximates len(pending), which sync.Map does not offer.
	//
	// Approximate is enough: it only gates the backstop in track, and the cost
	// of drifting is a cleanup that happens slightly early or slightly late.
	live atomic.Int64
}

// NormalizeRequest is required by the interface; requests need no adjustment.
func (h *refusalHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	return body
}

// TranslateRequest declines to perform the translation, and reports that
// nobody else did either.
//
// The registry consults this only when it has no built-in transformer for the
// pair, so reaching here means the request body is about to be forwarded to an
// upstream that speaks a different dialect -- unchanged, and until now
// silently. See untranslated.go.
func (h *refusalHooks) TranslateRequest(_ context.Context, from, to sdktranslator.Format, _ string, body []byte, _ bool) ([]byte, bool) {
	noteUntranslated(from, to, i18n.T("请求", "request "))
	return body, false
}

// TranslateResponse declines as well, under the same conditions and with the
// same consequence in the other direction.
func (h *refusalHooks) TranslateResponse(_ context.Context, from, to sdktranslator.Format, _ string, _, _, body []byte, _ bool) ([]byte, bool) {
	noteUntranslated(from, to, i18n.T("响应", "response "))
	return body, false
}

// NormalizeResponseBefore inspects the upstream body while it still carries the
// refusal, and leaves it untouched.
func (h *refusalHooks) NormalizeResponseBefore(ctx context.Context, _, _ sdktranslator.Format, model string, _, _, body []byte, stream bool) []byte {
	key := refusalKey(ctx)

	d, ok := findRefusal(body)
	if !ok {
		// A clean upstream body clears any earlier finding for this context.
		//
		// Not merely tidiness. CLIProxyAPI's retry loop reuses one context
		// across attempts (conductor's `for attempt := 0; ; attempt++` calls
		// ExecuteStream with the same ctx), so a first attempt that was refused
		// leaves a finding behind that the *successful* retry would then be
		// marked with -- handing the caller a complete answer labelled
		// content_filter. Clearing on a clean body scopes the finding to the
		// attempt that produced it.
		//
		// Through forget, not a bare Delete: this path removes map entries too,
		// and a Delete that skips the live counter makes the count drift up by
		// one per cleared finding, forever. Weeks of normal traffic would walk
		// it across maxPendingRefusals and trigger the emergency clear -- and
		// its "please report this" log line -- with nothing actually wrong.
		h.forget(key)
		return body
	}

	// Streaming delivers the refusal in one chunk among many, so this hook fires
	// repeatedly for a single response. Report once.
	if _, loaded := h.pending.LoadOrStore(key, d); !loaded {
		d.reportTranslation(model, stream)
		h.track(key)
	}
	return body
}

// NormalizeResponseAfter rewrites the translated body when the request was
// refused.
func (h *refusalHooks) NormalizeResponseAfter(ctx context.Context, _, to sdktranslator.Format, _ string, _, _, body []byte, stream bool) []byte {
	key := refusalKey(ctx)
	v, ok := h.pending.Load(key)
	if !ok {
		return body
	}

	d, _ := v.(refusalDetails)
	out, changed := markContentFiltered(body, to, stream)

	switch {
	case !stream:
		// Non-streaming translation calls this once, so the finding is spent.
		h.forget(key)
	case changed:
		// Streaming rewrites finish_reason, which only exists on the terminal
		// frame -- so a successful rewrite means this response is over.
		//
		// Without this the streaming path never deleted anything at all: the
		// only Delete was under `if !stream`, and the streaming branch is the
		// one that fires. Each refused stream leaked an entry plus the whole
		// context chain it keys on, forever, in a process designed to run for
		// weeks.
		h.forget(key)
	}

	if !changed && dialectCarriesRefusal(to) {
		// Nothing to warn about: the client speaks the dialect the refusal
		// arrived in, so it reaches the caller intact with its own
		// stop_reason and stop_details. Warning here claimed a loss that did
		// not happen -- and on a project whose whole pitch is not reporting
		// unknowns as fine, reporting fine as broken points just as wrongly.
		return out
	}
	if !changed {
		// Say so rather than leave the impression the rewrite happened. The
		// caller still gets an empty completion; at least the log explains it.
		log.Warnf("slimproxy: upstream refusal (category=%s) could not be marked in the %s response; "+
			"the caller sees an empty completion with an unchanged finish_reason", orUnspecified(d.Category), to)
	}
	return out
}

// dialectCarriesRefusal reports whether a client speaking this format receives
// the upstream refusal in a form it already understands.
//
// Anthropic's own refusal shape is stop_reason:"refusal" with stop_details, so
// a Claude client gets the full finding whether or not anything is rewritten.
// Every other dialect has no equivalent, which is what the rewrite exists for.
func dialectCarriesRefusal(to sdktranslator.Format) bool {
	return strings.EqualFold(string(to), "claude")
}

// maxPendingRefusals bounds the in-flight findings map.
//
// The deletions above cover every path that reaches a conclusion, but a stream
// that is abandoned mid-response -- client disconnect, upstream reset, a
// cancelled context -- produces neither a clean body nor a terminal frame, so
// its entry has nothing to remove it. That is rare and small, and it is also
// unbounded, which in a process meant to run for weeks is the wrong shape of
// rare.
//
// Generous enough that no realistic concurrency reaches it, so crossing the
// line is itself information.
const maxPendingRefusals = 1024

// track counts a newly stored finding and clears the map if it has grown past
// anything explainable.
//
// Dropping findings degrades gracefully: the worst outcome is a refusal that
// reaches the caller unmarked, which is the behaviour that existed before this
// file. Growing without limit degrades into memory exhaustion. And it says so
// either way -- a silent cap would hide exactly the bug it is capping.
func (h *refusalHooks) track(key any) {
	if n := h.live.Add(1); n <= maxPendingRefusals {
		return
	}
	count := 0
	h.pending.Range(func(k, _ any) bool {
		h.pending.Delete(k)
		count++
		return true
	})
	h.live.Store(0)
	log.Warnf(i18n.T(
		"slimproxy: 待处理的上游拒绝记录超过 %d 条，已清空 %d 条。这说明有响应既没有正常结束也没有被清理——请报告此问题",
		"slimproxy: pending upstream refusal records exceeded %d; %d cleared. Some response neither finished normally nor was cleaned up -- please report this"), maxPendingRefusals, count)
}

// forget removes a finding that has been acted on.
func (h *refusalHooks) forget(key any) {
	if _, loaded := h.pending.LoadAndDelete(key); loaded {
		h.live.Add(-1)
	}
}

// refusalKey derives the map key linking one translation's two hook calls.
//
// The context is the only value the registry passes unchanged through both, and
// contexts are comparable in practice. "In practice" is not good enough for a
// map key -- an incomparable one panics -- so this narrows it to a pointer.
func refusalKey(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	// A context value is an interface; comparing interfaces holding
	// incomparable dynamic types panics. Every context implementation in the
	// standard library is a pointer or a comparable struct, so this is safe
	// today, and the recover in installRefusalHooks covers the rest.
	return ctx
}

func orUnspecified(s string) string {
	if s == "" {
		return "unspecified"
	}
	return s
}

// reportTranslation logs a refusal seen during translation.
//
// It states only what this hook knows: that the upstream refused. What the
// caller ultimately receives depends on the client dialect, which the Before
// hook does not see -- a Claude client gets the native refusal intact, an
// OpenAI chat client gets finish_reason "content_filter", everyone else gets
// an empty completion and a warning from the After hook. The earlier text
// asserted the content_filter outcome unconditionally, which sent operators
// of every other dialect looking for a rewrite that never happened.
func (d refusalDetails) reportTranslation(model string, stream bool) {
	mode := "non-streaming"
	if stream {
		mode = "streaming"
	}
	log.Warnf("upstream refused this request on policy grounds (model=%s, category=%s, %s): %s",
		model, orUnspecified(d.Category), mode, d.Explanation)
}

// sharedRefusalHooks is installed repeatedly, so it must be a single instance:
// the pending map linking one translation's two hook calls would otherwise be
// discarded between them.
var sharedRefusalHooks = &refusalHooks{}

// installRefusalHooks points the default translator registry at our hooks.
//
// Called per request rather than once at startup, which needs explaining.
// CLIProxyAPI's Service.syncPluginRuntimeConfigForConfig ends with
// SetPluginHooks(s.pluginHost), and the plugin host is created unconditionally
// -- builder.Build does `if pluginHost == nil { pluginHost = pluginhost.New() }`
// regardless of plugins.enabled. So a registration made before Run is
// overwritten during startup, and again on every config reload. Measured, not
// assumed: hooks registered after Build were called zero times.
//
// Reinstalling per request is therefore the only placement that survives. The
// cost is one mutex-guarded pointer write against a request that spends seconds
// upstream. Overwriting the plugin host's hooks is safe here specifically
// because slimproxy pins plugins off (see hardeningNotes), so the hooks being
// displaced are a pass-through.
func installRefusalHooks() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("slimproxy: installing refusal hooks panicked: %v", r)
		}
	}()
	sdktranslator.SetPluginHooks(sharedRefusalHooks)
	return nil
}

// RefusalHookMiddleware reinstalls the refusal hooks ahead of each request.
//
// A middleware rather than a one-off because the registration does not survive
// startup or a config reload; see installRefusalHooks.
func RefusalHookMiddleware() gin.HandlerFunc {
	var reported sync.Once
	return func(c *gin.Context) {
		if err := installRefusalHooks(); err != nil {
			reported.Do(func() {
				log.Warnf("slimproxy: %v; upstream policy refusals will reach callers as empty completions with finish_reason \"stop\"", err)
			})
		}
		c.Next()
	}
}

// markContentFiltered sets finish_reason to "content_filter" on a translated
// response.
//
// Only the OpenAI chat schema is touched. Claude's own schema already carries
// stop_reason "refusal" and its stop_details intact, so a caller speaking that
// protocol loses nothing and rewriting would corrupt it. Gemini spells the
// field differently and is left alone rather than guessed at. The OpenAI
// Responses schema ("openai-response") is also left alone -- it has no
// choices[].finish_reason at all (refusals there are status "incomplete" plus
// incomplete_details), so listing it here made setFinishReason fail on every
// response while the code claimed the dialect was covered. Until someone
// implements the Responses semantics, those callers get the warning below,
// which is the truth.
func markContentFiltered(body []byte, to sdktranslator.Format, stream bool) ([]byte, bool) {
	switch string(to) {
	case "openai":
	default:
		return body, false
	}
	if stream {
		return markStreamFrame(body)
	}
	return markObject(body)
}

// markObject rewrites finish_reason in a single JSON object.
func markObject(body []byte) ([]byte, bool) {
	doc, ok := decodeJSONObject(body)
	if !ok {
		return body, false
	}
	if !setFinishReason(doc) {
		return body, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false
	}
	return out, true
}

// markStreamFrame rewrites finish_reason inside an SSE frame.
//
// A frame may hold several events and may or may not still carry the `data: `
// prefix depending on where in the pipeline it is observed, so both shapes are
// handled rather than assumed.
func markStreamFrame(body []byte) ([]byte, bool) {
	lines := bytes.Split(body, []byte("\n"))
	changed := false
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		prefix := []byte(nil)
		if rest, found := bytes.CutPrefix(trimmed, []byte("data:")); found {
			prefix = []byte("data: ")
			trimmed = bytes.TrimSpace(rest)
		}
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		doc, ok := decodeJSONObject(trimmed)
		if !ok || !setFinishReason(doc) {
			continue
		}
		out, err := json.Marshal(doc)
		if err != nil {
			continue
		}
		lines[i] = append(prefix, out...)
		changed = true
	}
	if !changed {
		return body, false
	}
	return bytes.Join(lines, []byte("\n")), true
}

// decodeJSONObject parses body preserving number formatting.
//
// UseNumber matters: decoding into interface{} turns every number into a
// float64, and re-encoding would render token counts and unix timestamps in
// scientific notation. That would corrupt the response while "fixing" it.
func decodeJSONObject(body []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	return doc, true
}

// setFinishReason replaces a "stop" finish_reason with "content_filter".
//
// Only "stop" is replaced. Any other value means the response ended for a
// reason the translator did identify -- length, tool_calls -- and overwriting
// it would destroy accurate information to report a refusal that, by then, is
// not what actually terminated the response.
func setFinishReason(doc map[string]any) bool {
	choices, ok := doc["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if reason, ok := choice["finish_reason"].(string); ok && reason == "stop" {
			choice["finish_reason"] = "content_filter"
			changed = true
		}
	}
	return changed
}
