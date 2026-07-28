package proxy

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/momo/slimproxy/journal"
)

// Inbound request fidelity probing.
//
// The journal's request events come from usage records, which describe what the
// upstream did. They cannot describe what the client asked for -- and the gap
// between those two is exactly where a proxy goes wrong without anyone
// noticing. A rewritten system array, a dropped cache_control, a truncated
// history: every one of them produces a perfectly ordinary-looking successful
// response.
//
// What this can and cannot see is worth stating plainly, because the limit
// shapes the design. It observes the request as it ARRIVES. The rewriting that
// matters happens later, inside CLIProxyAPI's executor, and is not visible from
// here at all. So this does not try to catch the rewrite directly; it records
// what the client intended, and the usage record records what the cache
// actually did. The two together answer the question: a request that arrived
// with cache_control and came back with zero cache reads is the alarm.

// fidelitySampleInterval is how often a request is inspected.
//
// Sampled rather than measured on every request because inspection means
// reading the whole body into memory and parsing it, and Claude Code's requests
// carry the entire conversation. The properties being watched -- which client,
// how the system prompt is shaped, whether caching is asked for -- are
// configuration-like: they do not vary request to request, so a sample a minute
// answers the question at a fraction of the cost.
const fidelitySampleInterval = time.Minute

// maxFidelityBody bounds what will be parsed.
//
// Above this only the size is recorded. A long conversation can reach many
// megabytes, and doubling that in memory to count JSON fields is a poor trade
// for a diagnostic.
const maxFidelityBody = 2 << 20 // 2 MiB

// FidelityProbeMiddleware records the shape of inbound requests, occasionally.
//
// Scope: the Anthropic dialect only. The gate below admits /v1/messages and
// nothing else, and describeRequest parses the Anthropic request shape (system
// block arrays, content blocks, cache_control placement) -- pointing it at an
// OpenAI or Gemini body would produce zero counts that read as facts. Callers
// speaking other dialects therefore generate no fidelity events at all: an
// empty fidelity stream under OpenAI-format traffic means "not observed",
// never "nothing to report". Widening the scope means writing a per-dialect
// describeRequest first, not just relaxing the gate.
func FidelityProbeMiddleware(j *journal.Writer) gin.HandlerFunc {
	var last atomic.Int64

	return func(c *gin.Context) {
		if !isMessagesPath(c.Request.URL.Path) || c.Request.Body == nil {
			c.Next()
			return
		}

		now := time.Now()
		prev := last.Load()
		if now.UnixNano()-prev < int64(fidelitySampleInterval) || !last.CompareAndSwap(prev, now.UnixNano()) {
			c.Next()
			return
		}

		// The body has to be put back: this middleware runs before the handler
		// that actually needs it, and a consumed reader would turn a diagnostic
		// into an outage.
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxFidelityBody+1))
		if err != nil {
			c.Next()
			return
		}
		c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), c.Request.Body))

		ev := describeRequest(body, c.GetHeader("User-Agent"), c.Request.URL.Path, requestSource(c.Request))
		ev.At = now
		j.Append(ev)

		c.Next()
	}
}

func isMessagesPath(p string) bool {
	return strings.HasSuffix(p, "/v1/messages") || strings.HasSuffix(p, "/messages")
}

// describeRequest extracts the shape of a request without keeping its content.
//
// Counts and flags only. The body carries the operator's actual conversation,
// so nothing from it is recorded verbatim -- the journal is meant to be kept
// for a week and read casually, which rules out anything a prompt could end up
// inside.
func describeRequest(body []byte, ua, path string, src journal.Source) journal.Event {
	ev := journal.Event{
		Kind:     journal.KindFidelity,
		Src:      src,
		UAClient: clientTag(ua),
		BodyKB:   int64(len(body)+512) / 1024,
	}

	if len(body) > maxFidelityBody {
		// Too large to parse; the size alone is still worth knowing, and
		// pretending the other fields were measured would be worse than
		// omitting them.
		ev.Detail = "方言 " + dialectOf(path) + " · 请求体过大，未解析结构"
		return ev
	}
	if !gjson.ValidBytes(body) {
		ev.Detail = "请求体不是合法 JSON"
		return ev
	}

	// The inbound dialect, which the usage record cannot supply: it is decided
	// by which endpoint the client called, and only a middleware sees that.
	ev.Detail = "方言 " + dialectOf(path)
	ev.Model = gjson.GetBytes(body, "model").String()
	ev.Messages = int(gjson.GetBytes(body, "messages.#").Int())

	system := gjson.GetBytes(body, "system")
	switch {
	case system.IsArray():
		system.ForEach(func(_, block gjson.Result) bool {
			ev.SystemBlocks++
			if block.Get("cache_control").Exists() {
				ev.CacheControls++
			}
			return true
		})
	case system.Exists():
		// A bare string system prompt cannot carry cache_control at all.
		ev.SystemBlocks = 1
	}

	// cache_control also appears on message content and on the last tool, which
	// is where Claude Code puts most of them.
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("cache_control").Exists() {
				ev.CacheControls++
			}
			return true
		})
		return true
	})
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		ev.Tools++
		if t.Get("cache_control").Exists() {
			ev.CacheControls++
		}
		return true
	})

	ev.Thinking = gjson.GetBytes(body, "thinking.type").String() == "enabled"
	ev.Stream = gjson.GetBytes(body, "stream").Bool()
	return ev
}

// clientTag classifies the client by User-Agent.
//
// The distinction that matters is whether the upstream emulation will rewrite
// this request. It rewrites everything whose User-Agent does not begin with
// "claude-cli" -- including an empty one, which is what it sees if the header
// fails to reach the executor. So "claude-cli" and "empty" are recorded
// separately: they take the same branch downstream but mean opposite things
// here, and only one of them is a bug.
func clientTag(ua string) string {
	switch {
	case strings.HasPrefix(ua, "claude-cli"):
		return "claude-cli"
	case strings.TrimSpace(ua) == "":
		return "(empty)"
	default:
		if i := strings.IndexAny(ua, "/ "); i > 0 {
			return ua[:i]
		}
		return ua
	}
}

// dialectOf names the inbound protocol from the endpoint that was called.
//
// This is the "openai → claude" half that the usage record does not carry, and
// the only place it is observable. Which endpoint the client chose IS the
// dialect: /v1/messages is Anthropic's, /v1/chat/completions is OpenAI's.
//
// Today only the "claude" branch is reachable: FidelityProbeMiddleware gates
// on /v1/messages before this runs. The other branches are kept so the label
// stays correct if the gate is ever widened -- but see the middleware comment
// for what widening actually requires.
func dialectOf(path string) string {
	switch {
	case strings.Contains(path, "/messages"):
		return "claude"
	case strings.Contains(path, "/chat/completions"), strings.Contains(path, "/responses"):
		return "openai"
	case strings.Contains(path, "generateContent"), strings.Contains(path, "/v1beta"):
		return "gemini"
	default:
		return "?"
	}
}
