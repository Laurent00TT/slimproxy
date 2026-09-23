package proxy

// Tests for the early SSE preamble (earlyflush.go).
//
// The contract under test has two halves that must both hold:
//
//   - Requests the handler answers inside the threshold are byte-for-byte
//     untouched -- real status, real body, no SSE preamble. The wrap must be
//     invisible on every fast path, because fast paths are almost all of them.
//   - Requests that outlive the threshold get the preamble, heartbeats while
//     silent, passthrough once the stream starts, and an in-stream error
//     event when the handler tries to fail over HTTP after the 200 is gone
//     out.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// earlyFlushTestDelay is short enough to keep the suite fast and long enough
// that "the handler answered first" is unambiguous on a loaded CI box.
const earlyFlushTestDelay = 40 * time.Millisecond

// serveEarlyFlush runs one request through the middleware and a handler.
func serveEarlyFlush(t *testing.T, delay time.Duration, notes earlyFlushNotes, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.Use(EarlyFlushMiddleware(delay, notes))
	r.POST("/v1/messages", handler)
	r.POST("/v1/other", handler)
	rec := httptest.NewRecorder()
	path := "/v1/messages"
	if strings.Contains(body, `"path":"other"`) {
		path = "/v1/other"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.ServeHTTP(rec, req)
	return rec
}

// TestEarlyFlushLeavesFastOutcomesAlone: an error written inside the
// threshold keeps its HTTP status and JSON body. This is the semantic the
// threshold exists to protect -- fast failures (the ~1-3s 403s measured
// 2026-08-04) must keep retry-relevant status codes.
func TestEarlyFlushLeavesFastOutcomesAlone(t *testing.T) {
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, `{"stream":true}`, func(c *gin.Context) {
		c.Data(http.StatusForbidden, "application/json", []byte(`{"type":"error","error":{"type":"permission_error","message":"permission denied"}}`))
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 untouched", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q; the preamble leaked onto a fast failure", ct)
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Errorf("body %q contains SSE framing on the fast path", rec.Body.String())
	}
}

// TestEarlyFlushIgnoresNonStreaming: a non-streaming request may be slow --
// its response is one JSON document and MUST NOT be preceded by SSE headers.
func TestEarlyFlushIgnoresNonStreaming(t *testing.T) {
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, `{"stream":false}`, func(c *gin.Context) {
		time.Sleep(3 * earlyFlushTestDelay)
		c.Data(http.StatusOK, "application/json", []byte(`{"id":"msg_1"}`))
	})
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q; preamble fired for a non-streaming request", ct)
	}
	if got := rec.Body.String(); got != `{"id":"msg_1"}` {
		t.Errorf("body = %q, want the JSON untouched", got)
	}
}

// TestEarlyFlushIgnoresOtherPaths: only /v1/messages is in scope.
func TestEarlyFlushIgnoresOtherPaths(t *testing.T) {
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, `{"stream":true,"path":"other"}`, func(c *gin.Context) {
		time.Sleep(3 * earlyFlushTestDelay)
		c.Data(http.StatusOK, "application/json", []byte(`{}`))
	})
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q; preamble fired outside /v1/messages", ct)
	}
}

// TestEarlyFlushPreambleHeartbeatThenStream: the slow-success path. The
// preamble commits 200 + SSE headers, keep-alive comments cover the silence,
// and the handler's own frames pass through untouched afterwards.
func TestEarlyFlushPreambleHeartbeatThenStream(t *testing.T) {
	old := earlyFlushHeartbeatEvery
	earlyFlushHeartbeatEvery = 15 * time.Millisecond
	defer func() { earlyFlushHeartbeatEvery = old }()

	var waited atomic.Int64
	notes := earlyFlushNotes{flushed: func(w time.Duration, _ streamVerdict) { waited.Store(int64(w)) }}
	chunk := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	rec := serveEarlyFlush(t, earlyFlushTestDelay, notes, `{"stream":true}`, func(c *gin.Context) {
		time.Sleep(4 * earlyFlushTestDelay)
		c.Writer.WriteString(chunk)
		c.Writer.Flush()
	})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream from the preamble", ct)
	}
	body := rec.Body.String()
	hb := strings.Index(body, ": keep-alive\n\n")
	data := strings.Index(body, chunk)
	if hb < 0 {
		t.Errorf("body %q carries no keep-alive comment; the silence was uncovered", body)
	}
	if data < 0 {
		t.Fatalf("body %q lost the handler's own frame", body)
	}
	if hb >= 0 && hb > data {
		t.Errorf("keep-alive at %d after the handler's frame at %d; heartbeat outlived the stream", hb, data)
	}
	if !rec.Flushed {
		t.Error("nothing was flushed; the preamble never reached the wire")
	}
	if got := time.Duration(waited.Load()); got < earlyFlushTestDelay {
		t.Errorf("flushed callback reported %s, want >= the %s threshold", got, earlyFlushTestDelay)
	}
}

// slowReader delivers its payload in two halves with a pause between them,
// modelling a large conversation crawling through the tunnel.
type slowReader struct {
	first, rest io.Reader
	pause       time.Duration
	paused      bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	n, err := s.first.Read(p)
	if n > 0 || err != io.EOF {
		return n, err
	}
	if !s.paused {
		s.paused = true
		time.Sleep(s.pause)
	}
	return s.rest.Read(p)
}

// TestEarlyFlushClockStartsAtArrival: the threshold is measured from the
// request's start, not from the moment its body finished arriving. Measured
// 2026-08-05: a 1.8MB body spent 85s of Cloudflare's ~100s window in upload;
// anchoring the delay at body-complete pushed the preamble to arrival+115s
// and the edge answered with the exact 524 this middleware exists to
// prevent. A request whose upload already consumed the threshold must flush
// as soon as the sniff can rule it streaming, not a full threshold later.
func TestEarlyFlushClockStartsAtArrival(t *testing.T) {
	var waited atomic.Int64
	var flushedAt atomic.Int64
	start := time.Now()
	notes := earlyFlushNotes{flushed: func(w time.Duration, _ streamVerdict) {
		waited.Store(int64(w))
		flushedAt.Store(int64(time.Since(start)))
	}}

	body := `{"stream":true}`
	upload := 3 * earlyFlushTestDelay // the upload alone overshoots the threshold

	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, notes))
	r.POST("/v1/messages", func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay) // slow handler; the flush must not wait for it
		c.Writer.WriteString("event: message_start\ndata: {}\n\n")
		c.Writer.Flush()
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &slowReader{
		first: strings.NewReader(body[:4]),
		rest:  strings.NewReader(body[4:]),
		pause: upload,
	})
	// httptest.NewRequest cannot know the length of a custom reader, and the
	// middleware skips bodies with no declared length.
	req.ContentLength = int64(len(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := time.Duration(waited.Load()); got == 0 {
		t.Fatal("the preamble never fired for a request whose upload alone crossed the threshold")
	} else if got < upload {
		t.Errorf("flushed callback reported %s silence, want >= the %s the upload took (the clock must start at arrival)", got, upload)
	}
	// The regression being pinned: with the clock anchored at body-complete,
	// the flush lands at upload+threshold. Anchored at arrival it must land
	// essentially at upload-complete. Half a threshold of slack absorbs
	// scheduler noise while still failing the old behaviour clearly.
	if got := time.Duration(flushedAt.Load()); got >= upload+earlyFlushTestDelay/2 {
		t.Errorf("preamble left at %s after arrival, want ~%s (upload end); the threshold was re-added on top of the upload", got, upload)
	}
}

// TestEarlyFlushDesperationBeatsSlowUpload: when the body is still uploading
// at the desperation deadline, the preamble must go out DURING the upload --
// waiting for the sniff means waiting past the edge's guillotine. Measured
// 2026-08-05: two uploads outlived the whole ~100s window and both flushes
// landed on connections the edge had already severed.
func TestEarlyFlushDesperationBeatsSlowUpload(t *testing.T) {
	oldD := earlyFlushDesperation
	earlyFlushDesperation = 60 * time.Millisecond
	defer func() { earlyFlushDesperation = oldD }()

	var flushedAt atomic.Int64
	start := time.Now()
	notes := earlyFlushNotes{flushed: func(time.Duration, streamVerdict) { flushedAt.Store(int64(time.Since(start))) }}

	body := `{"stream":true}`
	upload := 4 * earlyFlushDesperation // the upload alone far outlives the desperation deadline

	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, notes))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Writer.WriteString("event: message_start\ndata: {}\n\n")
		c.Writer.Flush()
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &slowReader{
		first: strings.NewReader(body[:4]),
		rest:  strings.NewReader(body[4:]),
		pause: upload,
	})
	req.ContentLength = int64(len(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := time.Duration(flushedAt.Load())
	if got == 0 {
		t.Fatal("the preamble never fired")
	}
	// The whole point: the flush must land while the body is still arriving,
	// not after it. Half an upload of slack absorbs scheduler noise while
	// still failing a body-complete-anchored flush clearly.
	if got >= upload {
		t.Errorf("preamble left at %s, after the %s upload finished; desperation must fire during the upload", got, upload)
	}
	if !strings.Contains(rec.Body.String(), "event: message_start") {
		t.Errorf("body %q lost the handler's frame after a desperation flush", rec.Body.String())
	}
}

// TestEarlyFlushDesperationHoldsForProvenNonStreaming: when the prefix has
// already said stream:false at the desperation deadline, the preamble must be
// WITHHELD and the JSON response delivered untouched. SSE headers here would
// not degrade the response but destroy it -- the client's non-streaming call
// parses JSON and reports "empty or malformed response (HTTP 200)", the
// unretryable error measured on 2026-08-06 against Claude Code's
// non-streaming fallback (9 large stream:false bodies in one afternoon).
func TestEarlyFlushDesperationHoldsForProvenNonStreaming(t *testing.T) {
	oldD := earlyFlushDesperation
	earlyFlushDesperation = 60 * time.Millisecond
	defer func() { earlyFlushDesperation = oldD }()

	var held atomic.Int64
	var flushes atomic.Int64
	notes := earlyFlushNotes{
		flushed:         func(time.Duration, streamVerdict) { flushes.Add(1) },
		desperationHeld: func(time.Duration) { held.Add(1) },
	}

	// The prefix that arrives instantly carries the complete stream:false;
	// the rest of the body crawls in far past the desperation deadline.
	body := `{"stream":false,"messages":[{"role":"user","content":"hello"}]}`
	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, notes))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(`{"id":"msg_1"}`))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &slowReader{
		first: strings.NewReader(body[:len(`{"stream":false,`)]),
		rest:  strings.NewReader(body[len(`{"stream":false,`):]),
		pause: 4 * earlyFlushDesperation,
	})
	req.ContentLength = int64(len(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON——预发头会把非流式响应毁成客户端的 malformed 200", ct)
	}
	if got := rec.Body.String(); got != `{"id":"msg_1"}` {
		t.Errorf("body = %q，非流式响应必须原样送达", got)
	}
	if held.Load() != 1 {
		t.Errorf("desperationHeld 上报了 %d 次，want 1——按兵不动也必须留痕", held.Load())
	}
	if flushes.Load() != 0 {
		t.Errorf("flushed 上报了 %d 次，want 0", flushes.Load())
	}
}

// TestEarlyFlushDesperationBetsOnUnknownPrefix: when the stream field has not
// arrived by the desperation deadline, the preamble still goes out -- the
// residual bet -- and the flushed note says verdictUnknown, which is how the
// journal sizes the residue. A non-streaming request whose stream field trails
// the body will still be hurt here; the note is what proves how often.
func TestEarlyFlushDesperationBetsOnUnknownPrefix(t *testing.T) {
	oldD := earlyFlushDesperation
	earlyFlushDesperation = 60 * time.Millisecond
	defer func() { earlyFlushDesperation = oldD }()

	var sawVerdict atomic.Int64
	sawVerdict.Store(-1)
	notes := earlyFlushNotes{
		flushed: func(_ time.Duration, v streamVerdict) { sawVerdict.Store(int64(v)) },
	}

	// stream trails the messages array: the instant prefix has no verdict.
	body := `{"messages":[{"role":"user","content":"hello"}],"stream":true}`
	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, notes))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Writer.WriteString("event: message_start\ndata: {}\n\n")
		c.Writer.Flush()
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &slowReader{
		first: strings.NewReader(body[:20]),
		rest:  strings.NewReader(body[20:]),
		pause: 4 * earlyFlushDesperation,
	})
	req.ContentLength = int64(len(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q; 前缀未见 stream 字段时 desperation 仍要按流式赌", ct)
	}
	if got := streamVerdict(sawVerdict.Load()); got != verdictUnknown {
		t.Errorf("flushed 报的判定 = %v, want verdictUnknown——journal 靠它统计残余赌注", got)
	}
}

// TestEarlyFlushDesperationResidueSurvives drives the residual path the
// arming cannot close: desperation fires on verdictUnknown (stream field not
// in the prefix), the full body then rules non-streaming, tryDisarm returns
// false because the preamble is committed, and the wrapper must stay
// installed to deliver the JSON in-stream without crash, hang, or detaching
// with committed headers. Review found this path lost its only test when the
// old blind-desperation test was replaced; a regression here would ship a
// broken teardown for exactly the requests the verdictUnknown journal note
// exists to count.
func TestEarlyFlushDesperationResidueSurvives(t *testing.T) {
	oldD := earlyFlushDesperation
	earlyFlushDesperation = 60 * time.Millisecond
	defer func() { earlyFlushDesperation = oldD }()

	var sawVerdict atomic.Int64
	sawVerdict.Store(-1)
	notes := earlyFlushNotes{
		flushed: func(_ time.Duration, v streamVerdict) { sawVerdict.Store(int64(v)) },
	}

	// stream:false trails the messages array, so the desperation-time prefix
	// has no verdict and the preamble goes out on the odds.
	body := `{"messages":[{"role":"user","content":"hello"}],"stream":false}`
	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, notes))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(`{"id":"msg_1"}`))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &slowReader{
		first: strings.NewReader(body[:20]),
		rest:  strings.NewReader(body[20:]),
		pause: 4 * earlyFlushDesperation,
	})
	req.ContentLength = int64(len(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the committed 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q; the committed preamble cannot be unsent", ct)
	}
	if !strings.Contains(rec.Body.String(), `{"id":"msg_1"}`) {
		t.Errorf("body %q lost the JSON response; the wrapper must deliver it in-stream", rec.Body.String())
	}
	if got := streamVerdict(sawVerdict.Load()); got != verdictUnknown {
		t.Errorf("flushed verdict = %v, want verdictUnknown", got)
	}
}

// failingUpload delivers its first bytes, stalls, then fails the read -- a
// body that stops arriving after desperation has already committed the
// preamble.
type failingUpload struct {
	first io.Reader
	pause time.Duration
	err   error
}

func (f *failingUpload) Read(p []byte) (int, error) {
	if n, err := f.first.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	time.Sleep(f.pause)
	return 0, f.err
}

// TestEarlyFlushUploadCutIsNotAnUpstreamFailure: a body that stops arriving
// after the preamble ends the stream with an error event, as before, but is
// recorded as the upload failure it is. It used to be filed through
// translated -- "upstream failed after the preamble (HTTP 502)" -- which is
// how the 20 uploads killed on 2026-09-23 read as upstream faults although no
// upstream was ever contacted.
func TestEarlyFlushUploadCutIsNotAnUpstreamFailure(t *testing.T) {
	oldD := earlyFlushDesperation
	earlyFlushDesperation = 60 * time.Millisecond
	defer func() { earlyFlushDesperation = oldD }()

	var translated atomic.Int32
	type cutReport struct {
		received, declared int64
		err                error
	}
	cuts := make(chan cutReport, 2)
	notes := earlyFlushNotes{
		translated: func(int) { translated.Add(1) },
		uploadCut: func(received, declared int64, err error) {
			cuts <- cutReport{received, declared, err}
		},
	}

	prefix := `{"stream":true,"messages":[`
	declared := int64(len(prefix) + 4096)
	readErr := errors.New("body read failed mid-upload")
	var ran atomic.Bool

	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, notes))
	r.POST("/v1/messages", func(c *gin.Context) { ran.Store(true) })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &failingUpload{
		first: strings.NewReader(prefix),
		pause: 4 * earlyFlushDesperation,
		err:   readErr,
	})
	req.ContentLength = declared
	r.ServeHTTP(rec, req)

	if ran.Load() {
		t.Error("handler ran on a body that never finished arriving")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q; desperation should have committed the preamble first", ct)
	}
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("body %q: the stream must still end with an error event", rec.Body.String())
	}
	if n := translated.Load(); n != 0 {
		t.Errorf("translated fired %d times：上传中断被记成了「预发头之后上游失败」，而上游从未被联系", n)
	}
	select {
	case got := <-cuts:
		if got.received != int64(len(prefix)) || got.declared != declared {
			t.Errorf("uploadCut reported %d/%d bytes, want %d/%d", got.received, got.declared, len(prefix), declared)
		}
		if !errors.Is(got.err, readErr) {
			t.Errorf("uploadCut err = %v, want the body read's own error", got.err)
		}
	default:
		t.Fatal("uploadCut never fired：上传中断没有留下自己的记录")
	}
}

// TestSniffStreamField pins the three-state prefix predicate.
func TestSniffStreamField(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   streamVerdict
	}{
		{"完整 stream:true", `{"stream":true,"messages":[`, verdictStreaming},
		{"完整 stream:false", `{"stream":false,"messages":[`, verdictNonStreaming},
		{"stream:1 非 false 即流式（上游谓词）", `{"stream":1,"messages":[`, verdictStreaming},
		{"键还没到", `{"messages":[{"role":"user","content":"hi"}`, verdictUnknown},
		{"空前缀", ``, verdictUnknown},
		{"字符串内容里的 stream 文本不算", `{"messages":[{"role":"user","content":"set \"stream\":false please"}],"model":"m"`, verdictUnknown},
		{"嵌套对象里的 stream 不算", `{"metadata":{"stream":false},"messages":[`, verdictUnknown},
	}
	for _, tc := range cases {
		if got := sniffStreamField([]byte(tc.prefix)); got != tc.want {
			t.Errorf("%s: sniffStreamField(%q) = %v, want %v", tc.name, tc.prefix, got, tc.want)
		}
	}
}

// TestEarlyFlushTranslatesLateError: after the preamble, an HTTP failure must
// arrive as one SSE error event carrying the upstream's own envelope, and
// Status() must keep reporting the intended status for the journal behind.
func TestEarlyFlushTranslatesLateError(t *testing.T) {
	var translated atomic.Int64
	var journalSaw atomic.Int64
	upstream := `{"type":"error","error":{"type":"permission_error","message":"permission denied"}}`

	r := gin.New()
	// Stands in for AccessJournalMiddleware: reads the status through the
	// wrapper after everything inside has finished.
	r.Use(func(c *gin.Context) {
		c.Next()
		journalSaw.Store(int64(c.Writer.Status()))
	})
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, earlyFlushNotes{
		translated: func(s int) { translated.Store(int64(s)) },
	}))
	r.POST("/v1/messages", func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		// The shape WriteErrorResponse produces: status, then the JSON body.
		c.Writer.WriteHeader(http.StatusForbidden)
		c.Writer.WriteString(upstream)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`)))

	if rec.Code != http.StatusOK {
		t.Errorf("wire status = %d; after the preamble it can only be 200", rec.Code)
	}
	want := "event: error\ndata: " + upstream + "\n\n"
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body = %q, want the error event %q", rec.Body.String(), want)
	}
	if got := translated.Load(); got != http.StatusForbidden {
		t.Errorf("translated callback got %d, want 403", got)
	}
	if got := journalSaw.Load(); got != http.StatusForbidden {
		t.Errorf("journal-side Status() = %d, want the intended 403, not the wire's 200", got)
	}
}

// TestEarlyFlushLateGarbageBecomesEnvelope: a late failure whose body is not
// a well-formed upstream envelope (or spans lines, which SSE data: framing
// cannot carry) is replaced, not forwarded.
func TestEarlyFlushLateGarbageBecomesEnvelope(t *testing.T) {
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, `{"stream":true}`, func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		c.Writer.WriteHeader(http.StatusBadGateway)
		c.Writer.WriteString("upstream exploded\nat C:\\some\\path\\deep.go:42")
	})
	body := rec.Body.String()
	i := strings.Index(body, "event: error\ndata: ")
	if i < 0 {
		t.Fatalf("body = %q, want an error event", body)
	}
	payload := body[i+len("event: error\ndata: "):]
	payload = strings.TrimSuffix(strings.TrimSpace(payload), "\n")
	if strings.Contains(payload, "\n") {
		t.Fatalf("payload %q spans lines; SSE data framing would smear it", payload)
	}
	var ce claudeError
	if err := json.Unmarshal([]byte(payload), &ce); err != nil {
		t.Fatalf("payload %q is not JSON: %v", payload, err)
	}
	if ce.Type != "error" || ce.Error.Type != "api_error" {
		t.Errorf("payload %q, want a replacement api_error envelope", payload)
	}
	if strings.Contains(payload, "deep.go") {
		t.Errorf("payload %q leaked the local path", payload)
	}
}

// TestEarlyFlushSilentHandlerGetsErrorFrame: a handler that returns having
// written nothing after the preamble must not leave the client holding a
// silent open stream -- the stream ends with an explicit error event.
func TestEarlyFlushSilentHandlerGetsErrorFrame(t *testing.T) {
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, `{"stream":true}`, func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
	})
	if !strings.Contains(rec.Body.String(), "event: error\ndata: ") {
		t.Errorf("body = %q, want a closing error event after a silent handler", rec.Body.String())
	}
}

// TestEarlyFlushRestoresBodyForHandler: the sniff must not eat the body.
func TestEarlyFlushRestoresBodyForHandler(t *testing.T) {
	sent := `{"stream":true,"model":"claude-x","messages":[]}`
	var got atomic.Value
	serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, sent, func(c *gin.Context) {
		raw, _ := c.GetRawData()
		got.Store(string(raw))
		c.Data(http.StatusOK, "application/json", []byte(`{}`))
	})
	if got.Load() != sent {
		t.Errorf("handler read %q, want the original body %q", got.Load(), sent)
	}
}

// TestEarlyFlushThroughErrorEnvelope: the production stack order. The
// envelope middleware wraps outside; a late failure must come out as the SSE
// frame, once, with no JSON replacement body appended by the envelope.
func TestEarlyFlushThroughErrorEnvelope(t *testing.T) {
	upstream := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	r := gin.New()
	r.Use(ErrorEnvelopeMiddleware())
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, earlyFlushNotes{}))
	r.POST("/v1/messages", func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		c.Writer.WriteHeader(http.StatusTooManyRequests)
		c.Writer.WriteString(upstream)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`)))

	if rec.Code != http.StatusOK {
		t.Errorf("wire status = %d, want the preamble's 200", rec.Code)
	}
	body := rec.Body.String()
	if n := strings.Count(body, upstream); n != 1 {
		t.Errorf("upstream envelope appears %d times in %q, want exactly once (inside the SSE frame)", n, body)
	}
	if !strings.Contains(body, "event: error\ndata: "+upstream) {
		t.Errorf("body = %q, want the SSE error frame", body)
	}
}

// TestEarlyFlushDisabledByNonPositiveDelay: negative resolves to 0 upstream
// of the middleware; 0 must mean "never wrap".
func TestEarlyFlushDisabledByNonPositiveDelay(t *testing.T) {
	rec := serveEarlyFlush(t, 0, earlyFlushNotes{}, `{"stream":true}`, func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		c.Data(http.StatusForbidden, "application/json", []byte(`{}`))
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: disabled means fully untouched", rec.Code)
	}
}

// TestEarlyFlushTimerAfterFinishIsInert pins the review's highest-severity
// find: time.AfterFunc does not wait for a dispatched callback, so flushNow
// can acquire the lock after finish has run. The first draft then committed
// a preamble to a writer gin had already recycled and started a heartbeat
// goroutine nothing could ever stop; the tombstone must make that sequence a
// no-op.
func TestEarlyFlushTimerAfterFinishIsInert(t *testing.T) {
	rec := httptest.NewRecorder()
	testCtx, _ := gin.CreateTestContext(rec)
	w := &earlyFlushWriter{ResponseWriter: testCtx.Writer, started: time.Now()}
	w.timer = time.AfterFunc(time.Hour, w.flushNow)
	defer w.timer.Stop()

	w.finish()
	w.flushNow() // the callback that lost the race, arriving late

	if w.preflushed {
		t.Error("flushNow preflushed a finished request")
	}
	if w.hbStop != nil {
		t.Error("flushNow started a heartbeat on a finished request; nothing will ever stop it")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("late callback wrote %q into a recycled writer", rec.Body.String())
	}
}

// TestEarlyFlushClientCancelProducesNoFakeError: the client hanging up during
// the wait makes the upstream return without writing. Filing that as an
// upstream 502 would pollute the failure taxonomy's canceled-vs-5xx split, so
// the cancel path must end quietly: no error frame, no translated report.
func TestEarlyFlushClientCancelProducesNoFakeError(t *testing.T) {
	var translated atomic.Int64
	reqCtx, hangUp := context.WithCancel(context.Background())
	defer hangUp()

	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, earlyFlushNotes{
		translated: func(s int) { translated.Store(int64(s)) },
	}))
	r.POST("/v1/messages", func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay) // preamble goes out
		hangUp()                            // client leaves
		// The upstream's ctx.Done branch: return with zero writes.
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`)).WithContext(reqCtx)
	r.ServeHTTP(rec, req)

	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("preamble never fired; the test exercised nothing")
	}
	if strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("body = %q fabricates an upstream error for a voluntary cancel", rec.Body.String())
	}
	if got := translated.Load(); got != 0 {
		t.Errorf("translated(%d) reported an upstream failure for a client cancel", got)
	}
}

// TestEarlyFlushEmptyStreamSuccess: the upstream's empty-close path sets SSE
// headers and flushes without one Write. That is a completed response the
// upstream chose to send -- rewriting it into a fabricated 502 error frame
// hands a live client a failure that never happened.
func TestEarlyFlushEmptyStreamSuccess(t *testing.T) {
	var translated atomic.Int64
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{
		translated: func(s int) { translated.Store(int64(s)) },
	}, `{"stream":true}`, func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		// setSSEHeaders + flusher.Flush(), verbatim from code_handlers.go.
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Writer.Flush()
	})
	if strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("body = %q rewrote an empty-stream success into an error", rec.Body.String())
	}
	if got := translated.Load(); got != 0 {
		t.Errorf("translated(%d) fired on a successful empty stream", got)
	}
}

// TestEarlyFlushMidStreamErrorFramePassesThrough: after real stream bytes, a
// failure is the handler's to deliver (WriteTerminalError: c.Status + its own
// SSE error frame). Capturing that frame for re-translation would rewrite
// overloaded_error into api_error and destroy the client's retry signal.
func TestEarlyFlushMidStreamErrorFramePassesThrough(t *testing.T) {
	var translated atomic.Int64
	frame := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{
		translated: func(s int) { translated.Store(int64(s)) },
	}, `{"stream":true}`, func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		c.Writer.WriteString("event: message_start\ndata: {}\n\n")
		// WriteTerminalError's shape: bookkeeping status, then the frame.
		c.Writer.WriteHeader(529)
		c.Writer.WriteString(frame)
	})
	if n := strings.Count(rec.Body.String(), frame); n != 1 {
		t.Errorf("terminal frame appears %d times in %q, want exactly once, verbatim", n, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "api_error") {
		t.Error("overloaded_error was re-translated into api_error; retry signal destroyed")
	}
	if got := translated.Load(); got != 0 {
		t.Errorf("translated(%d) fired for a handler-delivered mid-stream error", got)
	}
}

// TestEarlyFlushSniffMirrorsUpstreamPredicate: the upstream streams anything
// where the field exists and is not literal false -- "stream": 1 included.
// Sniffing with Bool() left that subset unprotected.
func TestEarlyFlushSniffMirrorsUpstreamPredicate(t *testing.T) {
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, `{"stream":1}`, func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		c.Writer.WriteString("event: message_start\ndata: {}\n\n")
	})
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Error(`"stream":1 did not engage the preamble; upstream would stream it and CF would 524 it`)
	}
}

// TestEarlyFlushSkipsChunkedAndHugeBodies: the sniff runs ahead of inbound
// auth, so it must not buffer what it will not bound.
func TestEarlyFlushSkipsChunkedAndHugeBodies(t *testing.T) {
	handler := func(c *gin.Context) {
		time.Sleep(2 * earlyFlushTestDelay)
		c.Data(http.StatusOK, "application/json", []byte(`{}`))
	}
	r := gin.New()
	r.Use(EarlyFlushMiddleware(earlyFlushTestDelay, earlyFlushNotes{}))
	r.POST("/v1/messages", handler)

	// No usable Content-Length (chunked-style reader).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", io.MultiReader(strings.NewReader(`{"stream":true}`)))
	r.ServeHTTP(rec, req)
	if strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Error("preamble fired for a request with no Content-Length; unbounded pre-auth buffering")
	}

	// Content-Length past the sniff bound.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	req.ContentLength = earlyFlushMaxSniff + 1
	r.ServeHTTP(rec, req)
	if strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Error("preamble fired for an oversized body; the sniff bound was not honoured")
	}
}

// TestEarlyFlushWatchdogEndsSilentStream: the heartbeat defeats every timeout
// between here and the caller, so the proxy needs its own bound. Past it the
// stream must end with an explicit in-stream timeout AND the handler's
// context must be cancelled so a wedged executor unwinds.
func TestEarlyFlushWatchdogEndsSilentStream(t *testing.T) {
	oldHB, oldSilence := earlyFlushHeartbeatEvery, earlyFlushMaxSilence
	earlyFlushHeartbeatEvery = 10 * time.Millisecond
	earlyFlushMaxSilence = 60 * time.Millisecond
	defer func() { earlyFlushHeartbeatEvery, earlyFlushMaxSilence = oldHB, oldSilence }()

	var timedOut atomic.Int64
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{
		timedOut: func(d time.Duration) { timedOut.Store(int64(d)) },
	}, `{"stream":true}`, func(c *gin.Context) {
		// A wedged executor: nothing to say, only unwound by cancellation.
		<-c.Request.Context().Done()
	})

	body := rec.Body.String()
	if n := strings.Count(body, "event: error"); n != 1 {
		t.Fatalf("body = %q carries %d error frames, want exactly one from the watchdog", body, n)
	}
	if timedOut.Load() == 0 {
		t.Error("watchdog fired but never reported itself; the timeout would be invisible in the journal")
	}
	// Reaching here at all proves the cancel unwound the blocked handler.
}

// TestEarlyFlushShadowHeadersReachFastResponse: the handler only ever sees
// the shadow header map (that is what keeps the timer goroutine and the
// handler's lock-free header writes off the same map), so the commit path
// must carry its headers onto the real response.
func TestEarlyFlushShadowHeadersReachFastResponse(t *testing.T) {
	rec := serveEarlyFlush(t, earlyFlushTestDelay, earlyFlushNotes{}, `{"stream":true}`, func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		c.Header("X-Custom", "carried")
		c.Writer.WriteHeader(http.StatusForbidden)
		c.Writer.WriteString(`{"type":"error","error":{"type":"permission_error","message":"permission denied"}}`)
	})
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want the handler's application/json committed from the shadow", got)
	}
	if got := rec.Header().Get("X-Custom"); got != "carried" {
		t.Errorf("X-Custom = %q, want %q", got, "carried")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}
