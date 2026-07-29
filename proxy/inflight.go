package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// InFlightMiddleware reports request arrivals and departures to the tracker the
// dashboard reads.
//
// The gap this closes: every other observer in this package fires when a
// request is over. The usage record does not exist until the upstream has
// answered, so a dashboard built on it shows nothing at all while a request is
// running -- and these requests run for ten to twenty seconds. An operator
// watching the panel during one sees a frame that has not changed and concludes
// the display is dead, which is the report that prompted this.
//
// Costs one map insert and one delete per request. Nothing is read from the
// body: learning the model would mean copying the whole conversation on every
// request, which is the price that made FidelityProbeMiddleware sample once a
// minute instead of measuring.
func InFlightMiddleware(f *metrics.InFlight) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isInFlightWorthy(c.Request) {
			c.Next()
			return
		}
		token := f.Begin(c.Request.URL.Path, time.Now())
		// Deferred, not called after c.Next(). A handler that panics still has
		// to release its row -- and a panic recovered further up the chain would
		// otherwise leave a request on screen that has already failed, forever.
		defer f.End(token)
		c.Next()
	}
}

// isInFlightWorthy decides what counts as a request worth showing.
//
// Written as an exclusion rather than a list of known LLM paths, which is the
// opposite of isMessagesPath in fidelity.go, and deliberately so. That probe
// parses an Anthropic body, so admitting a Gemini path would make it produce
// counts that are wrong; being narrow is what keeps it honest. This one only
// notes that something is running, so the two errors are not symmetrical:
// admitting one extra POST costs a row that is genuinely a request in progress,
// while missing a dialect reproduces the exact defect being fixed -- a request
// running with nothing on screen to show for it.
//
// POST is the discriminator. Every dialect this proxy translates posts its
// completion request; the health endpoint and the model listings are GETs, so
// neither can produce a row that sits at zero seconds and never moves.
func isInFlightWorthy(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost {
		return false
	}
	// The management API is a POST-heavy control plane that has nothing to do
	// with serving a model, and its calls are instant -- rows for them would
	// flicker in and out at exactly the rate that makes a display hard to read.
	return !strings.Contains(r.URL.Path, "/management/")
}
