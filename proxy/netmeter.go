// proxy/netmeter.go
package proxy

// Measuring the two network legs the journal could not see.
//
// A slow request has three suspects -- the client/tunnel delivering the body,
// the upstream thinking, the return path draining -- and until now only the
// middle one was on record (usage Latency/TTFT). Attribution meant manually
// reconciling the access log's total against the journal's ms. This meter
// closes that gap from inside the process: body EOF minus arrival is the
// upload leg, and cumulative time spent inside client writes is the only form
// of return-path pressure that stays measurable while the stream is pipelined
// ("upstream done -> client done" is ~0 by construction and would be a field
// that never fires). The body wrapper also counts the bytes the upload leg
// carried (in_kb): a duration alone cannot say whether 5s of upload was a
// slow link or a big conversation.
//
// Registration order is load-bearing: this must run BEFORE both middlewares
// that drain the network body and replay it from memory -- FidelityProbe
// (io.ReadAll on sampled /v1/messages bodies) and EarlyFlush (bodySniffer
// replaces the body with a bytes.Reader). A meter behind either would clock
// the in-memory replay at ~0. EarlyFlush also must stay the last registration
// (its writer closest to the handler), so this one cannot be last. See the
// registration site in proxy.go.
//
// Known undercount (wb_ms): ErrorEnvelope registers before this meter, so its
// writer sits below the meter, closest to the network. The failure bodies it
// buffers are written to the client in its own finish(), after the metered
// chain has unwound -- those writes (a few hundred bytes) never pass the
// meter and wb_ms misses them. Negligible, and consistent with the spec's
// rule: undercount, never overcount.

import (
	"io"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// NetMeterMiddleware stamps every request with a NetTimings and installs the
// two probes. Unconditional and cheap (two small wrappers, one ctx value):
// only requests that end in a usage record ever surface the numbers, so
// non-proxy paths carry a meter nobody reads.
func NetMeterMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		nt := metrics.NewNetTimings(time.Now())
		c.Request.Body = &meterBody{inner: c.Request.Body, nt: nt}
		c.Writer = &meterWriter{ResponseWriter: c.Writer, nt: nt}
		c.Request = c.Request.WithContext(metrics.WithNetTimings(c.Request.Context(), nt))
		c.Next()
	}
}

// meterBody marks the timings when the network body reaches EOF, and counts
// every byte it hands up on the way. A body the handler never fully reads
// (abort, reject) simply leaves Upload at 0 -- absent, not wrong -- while
// BodyBytes keeps what was read before it was dropped.
//
// The registration order above matters to the count as well as the clock. At
// the network side of both drainers every delivered byte passes here exactly
// once -- including bytes a drainer consumed and then gave up on
// (FidelityProbe's read-error path hands the half-drained body on) -- while
// FidelityProbe's and EarlyFlush's in-memory replays sit above this wrapper,
// where they cannot be counted a second time.
type meterBody struct {
	inner io.ReadCloser
	nt    *metrics.NetTimings
}

func (b *meterBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	// Counted before the error check: io.Reader may deliver the final bytes
	// together with EOF (or any error), and dropping them would short the
	// body by exactly its last chunk.
	b.nt.AddBodyBytes(n)
	if err == io.EOF {
		b.nt.MarkBodyDone(time.Now())
	}
	return n, err
}

func (b *meterBody) Close() error { return b.inner.Close() }

// meterWriter accumulates the time client writes spend blocked. Embedding
// gin.ResponseWriter forwards everything not measured (Status, Written,
// Hijack, ...), the same shape earlyFlushWriter relies on.
type meterWriter struct {
	gin.ResponseWriter
	nt *metrics.NetTimings
}

func (w *meterWriter) Write(b []byte) (int, error) {
	t := time.Now()
	n, err := w.ResponseWriter.Write(b)
	w.nt.AddWriteBlock(time.Since(t))
	return n, err
}

func (w *meterWriter) WriteString(s string) (int, error) {
	t := time.Now()
	n, err := w.ResponseWriter.WriteString(s)
	w.nt.AddWriteBlock(time.Since(t))
	return n, err
}

func (w *meterWriter) Flush() {
	t := time.Now()
	w.ResponseWriter.Flush()
	w.nt.AddWriteBlock(time.Since(t))
}
