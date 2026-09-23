package proxy

// Tests for the desperation preamble against a real net/http server.
//
// Everything in earlyflush_test.go runs on httptest.ResponseRecorder, which
// has no request body state machine: a response can start while the body is
// still arriving and nothing happens to the body. net/http does not work that
// way. Unless the handler has enabled full duplex, the first response write
// is treated as "the handler is done reading": with under 256KB unread the
// server closes the body -- draining the rest to nowhere, with the write held
// until the drain completes -- and the next Read fails with
// ErrBodyReadAfterClose; with more unread it forces Connection: close. The
// first shape is how the 20 requests whose desperation preamble paired with a
// 502 on 2026-09-23 died, each after 80-124s of uploading, each making the
// client resend the whole body. Only a real server shows it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/Laurent00TT/slimproxy/journal"
)

// streamingBody builds a stream:true /v1/messages body of exactly size bytes,
// with the stream field first so the desperation-time prefix proves streaming.
func streamingBody(t *testing.T, size int) []byte {
	t.Helper()
	head := `{"stream":true,"model":"claude-opus-5","messages":[{"role":"user","content":"`
	tail := `"}]}`
	pad := size - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("size %d too small for the JSON frame", size)
	}
	return []byte(head + strings.Repeat("a", pad) + tail)
}

// pausedUpload is what the client saw of one request whose body stalled
// mid-way.
type pausedUpload struct {
	resp *http.Response
	// midUpload is true when the response headers arrived while the rest of
	// the body was still held back -- the preamble went out during the
	// upload, which is the whole point of desperation.
	midUpload bool
	body      string
}

// uploadWithPause sends body, stalling after the first head bytes until the
// response headers arrive or wait passes, then sends the rest and reads the
// response to EOF.
//
// The bound on the stall is what keeps a regression red instead of hung: with
// net/http's default, the preamble's write waits for the body's lock, which
// the sniff holds while parked in Read, so nothing goes out until more body
// arrives -- and with under 256KB left it then drains the rest before the
// headers leave.
func uploadWithPause(t *testing.T, url string, header http.Header, body []byte, head int, wait time.Duration) pausedUpload {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range header {
		req.Header[k] = v
	}

	release := make(chan struct{})
	go func() {
		_, err := pw.Write(body[:head])
		if err == nil {
			<-release
			_, err = pw.Write(body[head:])
		}
		_ = pw.CloseWithError(err)
	}()

	// Proxy nil: the hermetic suite must not route a loopback request through
	// whatever the environment names.
	tr := &http.Transport{Proxy: nil}
	t.Cleanup(tr.CloseIdleConnections)
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Transport: tr}).Do(req)
		got <- result{resp, err}
	}()

	var r result
	mid := false
	select {
	case r = <-got:
		mid = true
	case <-time.After(wait):
	}
	close(release)
	if !mid {
		r = <-got
	}
	if r.err != nil {
		t.Fatalf("request failed: %v", r.err)
	}
	defer r.resp.Body.Close()
	b, err := io.ReadAll(r.resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return pausedUpload{resp: r.resp, midUpload: mid, body: string(b)}
}

// shrinkEarlyFlushTimers makes desperation fire in test time and the
// heartbeat write while the upload is stalled, so the preamble and its
// keep-alives are all concurrent with an unfinished body.
func shrinkEarlyFlushTimers(t *testing.T) {
	t.Helper()
	oldD, oldHB := earlyFlushDesperation, earlyFlushHeartbeatEvery
	earlyFlushDesperation = 100 * time.Millisecond
	earlyFlushHeartbeatEvery = 20 * time.Millisecond
	t.Cleanup(func() { earlyFlushDesperation, earlyFlushHeartbeatEvery = oldD, oldHB })
}

// TestEarlyFlushDesperationKeepsUploadAlive: a desperation preamble must not
// cost the request its body, whichever side of net/http's 256KB line the
// unread remainder falls on.
//
// The chain is the slimproxy half of what Build installs -- envelope, meter,
// early flush, in that order -- over a real net/http server, so the full
// duplex switch has to unwrap through every slimproxy writer to reach it.
func TestEarlyFlushDesperationKeepsUploadAlive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size, head int
	}{
		// Under 256KB left: net/http's default drains the rest into
		// io.Discard and the handler never sees it -- the 2026-09-23 shape.
		{"remainder under 256KB", 192 << 10, 64 << 10},
		// Over 256KB left: net/http's default leaves the body alone but
		// forces Connection: close on the response.
		{"remainder over 256KB", 1 << 20, 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			shrinkEarlyFlushTimers(t)

			var flushed, translated, cut, duplexFailed atomic.Int32
			notes := earlyFlushNotes{
				flushed:               func(time.Duration, streamVerdict) { flushed.Add(1) },
				translated:            func(int) { translated.Add(1) },
				uploadCut:             func(int64, int64, error) { cut.Add(1) },
				fullDuplexUnavailable: func(error) { duplexFailed.Add(1) },
			}
			seen := make(chan []byte, 1)
			frame := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"

			r := gin.New()
			r.Use(ErrorEnvelopeMiddleware())
			r.Use(NetMeterMiddleware())
			r.Use(EarlyFlushMiddleware(time.Minute, notes))
			r.POST("/v1/messages", func(c *gin.Context) {
				b, err := io.ReadAll(c.Request.Body)
				if err != nil {
					t.Errorf("handler body read: %v", err)
				}
				seen <- b
				_, _ = c.Writer.WriteString(frame)
				c.Writer.Flush()
			})
			srv := httptest.NewServer(r)
			defer srv.Close()

			body := streamingBody(t, tc.size)
			got := uploadWithPause(t, srv.URL+"/v1/messages", nil, body, tc.head, 3*time.Second)

			if !got.midUpload {
				t.Error("预发头没有在上传途中送达：它被 net/http 的 body 收尾挡在了剩余 body 之后" +
					"（desperation 存在的意义就是在上传途中先把头发出去）")
			}
			if got.resp.StatusCode != http.StatusOK || !strings.Contains(got.resp.Header.Get("Content-Type"), "text/event-stream") {
				t.Errorf("status %d, Content-Type %q; want the 200 SSE preamble", got.resp.StatusCode, got.resp.Header.Get("Content-Type"))
			}
			select {
			case b := <-seen:
				if !bytes.Equal(b, body) {
					t.Errorf("handler 收到 %d 字节，want 完整的 %d 字节——剩余 body 被丢弃了", len(b), len(body))
				}
			default:
				t.Error("handler 根本没有运行：上传在预发头之后被掐断，请求以 502 告终，客户端只能整份重发")
			}
			if strings.Contains(got.body, "event: error") {
				t.Errorf("流里出现了 error 事件（上传被掐断的症状）：%q", tail(got.body))
			}
			if !strings.Contains(got.body, frame) {
				t.Errorf("流里没有 handler 自己的帧：%q", tail(got.body))
			}
			if got.resp.Close || strings.EqualFold(got.resp.Header.Get("Connection"), "close") {
				t.Error("响应带了 Connection: close——net/http 在剩余 body 超过 256KB 时强加的收尾，全双工下不该出现")
			}
			if n := flushed.Load(); n != 1 {
				t.Errorf("flushed fired %d times, want 1 (the desperation preamble)", n)
			}
			if n := translated.Load() + cut.Load(); n != 0 {
				t.Errorf("translated/uploadCut fired %d times for a request that succeeded", n)
			}
			if n := duplexFailed.Load(); n != 0 {
				t.Errorf("fullDuplexUnavailable fired %d times over a real net/http writer", n)
			}
		})
	}
}

// tail keeps failure messages readable when a response carries heartbeats.
func tail(s string) string {
	if len(s) > 300 {
		return "…" + s[len(s)-300:]
	}
	return s
}

// duplexProbe stands in for net/http's writer: it records whether a
// ResponseController's EnableFullDuplex reached it.
type duplexProbe struct {
	gin.ResponseWriter
	enabled bool
}

func (p *duplexProbe) EnableFullDuplex() error {
	p.enabled = true
	return nil
}

// TestSlimproxyWritersUnwrap: every slimproxy writer that can sit between
// the early-flush middleware and net/http must pass a ResponseController
// through. One that does not makes EnableFullDuplex fail with ErrNotSupported
// for every request, and desperation preambles go back to killing uploads.
// Checked per wrapper, so the failure names the one that lost its Unwrap.
func TestSlimproxyWritersUnwrap(t *testing.T) {
	base, _ := gin.CreateTestContext(httptest.NewRecorder())
	for _, tc := range []struct {
		name string
		wrap func(gin.ResponseWriter) http.ResponseWriter
	}{
		{"envelopeWriter", func(w gin.ResponseWriter) http.ResponseWriter { return &envelopeWriter{ResponseWriter: w} }},
		{"meterWriter", func(w gin.ResponseWriter) http.ResponseWriter { return &meterWriter{ResponseWriter: w} }},
		{"earlyFlushWriter", func(w gin.ResponseWriter) http.ResponseWriter { return &earlyFlushWriter{ResponseWriter: w} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &duplexProbe{ResponseWriter: base.Writer}
			if err := http.NewResponseController(tc.wrap(probe)).EnableFullDuplex(); err != nil {
				t.Fatalf("%s 挡住了 ResponseController：%v——它缺 Unwrap() http.ResponseWriter", tc.name, err)
			}
			if !probe.enabled {
				t.Fatalf("%s 没有把 EnableFullDuplex 传到底下的 writer", tc.name)
			}
		})
	}
}

// TestEnableFullDuplexNamesTheBlockingWriter: when the switch fails, the
// error says which writer stopped the unwrap chain -- ErrNotSupported alone
// gives an operator nothing to act on.
func TestEnableFullDuplexNamesTheBlockingWriter(t *testing.T) {
	// A recorder has neither EnableFullDuplex nor Unwrap.
	err := enableFullDuplex(&meterWriter{ResponseWriter: &envelopeWriter{ResponseWriter: opaqueWriter{}}})
	if err == nil {
		t.Fatal("enableFullDuplex succeeded through a writer that cannot unwrap")
	}
	if !strings.Contains(err.Error(), "opaqueWriter") {
		t.Errorf("error %q does not name the writer the chain stopped at", err)
	}
}

// opaqueWriter is a gin.ResponseWriter with no Unwrap and no full duplex --
// the shape a future wrapper takes when it forgets Unwrap.
type opaqueWriter struct{ gin.ResponseWriter }

// TestFullDuplexFailureIsSaidOnce: a failed switch is reported where the
// operator reads -- the log, and the journal `slimproxy log` reads back --
// naming the cause, and exactly once in each -- the cause is structural, so
// it fails on every request, and a line per request would bury everything
// else.
func TestFullDuplexFailureIsSaidOnce(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	dir := t.TempDir()
	jw, err := journal.Open(dir, 1)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	rt := &Runtime{Journal: jw}
	cause := fmt.Errorf("feature not supported (the writer chain stops at *proxy.opaqueWriter)")
	rt.noteFullDuplexUnavailable(cause)
	rt.noteFullDuplexUnavailable(cause)
	// Close flushes; Read before it would race the writer goroutine.
	if err := jw.Close(); err != nil {
		t.Fatalf("journal close: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "opaqueWriter") {
		t.Fatalf("warning does not name the blocking writer: %q", out)
	}
	if n := strings.Count(out, "opaqueWriter"); n != 1 {
		t.Errorf("warning logged %d times for two failures, want once per process", n)
	}

	events := earlyFlushJournal(t, dir)
	if len(events) != 1 {
		t.Fatalf("journal 里有 %d 条 earlyflush 事件，want 恰好 1 条（两次失败、每进程只记一次）：%v", len(events), eventDetails(events))
	}
	if !strings.Contains(events[0].Detail, "opaqueWriter") {
		t.Errorf("journal 事件没有点名挡住链条的 writer：%q", events[0].Detail)
	}
}

// earlyFlushJournal reads back the KindHealth/earlyflush events a closed
// journal holds.
func earlyFlushJournal(t *testing.T, dir string) []journal.Event {
	t.Helper()
	res, err := journal.Read(dir, journal.Query{})
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	var out []journal.Event
	for _, e := range res.Events {
		if e.Kind == journal.KindHealth && e.State == "earlyflush" {
			out = append(out, e)
		}
	}
	return out
}

// eventDetails keeps a failure message to the part a reader can act on.
func eventDetails(events []journal.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Detail
	}
	return out
}

// TestEarlyFlushReportsFailedFullDuplex: when the switch cannot reach
// net/http, the middleware says so on every in-scope request, naming the
// writer the chain stopped at, and still serves the request.
//
// httptest.ResponseRecorder is exactly such a writer: gin's writer unwraps
// to it, and it has neither EnableFullDuplex nor Unwrap. In production the
// same report is the only signal that some writer between the middleware and
// net/http has lost its Unwrap -- desperation preambles would otherwise go
// back to killing uploads without a word.
//
// Per request here; once per process is the Runtime's policy, applied where
// the log and journal are written. Deduplicating in the middleware as well
// would put one policy in two places.
func TestEarlyFlushReportsFailedFullDuplex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var failures []error
	notes := earlyFlushNotes{
		// Called on the request goroutine, before the timer is armed, so a
		// plain slice is safe.
		fullDuplexUnavailable: func(err error) { failures = append(failures, err) },
	}
	var seen [][]byte
	r := gin.New()
	r.Use(EarlyFlushMiddleware(time.Minute, notes))
	handler := func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("handler body read: %v", err)
		}
		seen = append(seen, b)
		c.String(http.StatusOK, "ok")
	}
	r.POST("/v1/messages", handler)
	r.POST("/v1/messages/count_tokens", handler)

	body := `{"stream":true,"messages":[]}`
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Errorf("request %d: got %d %q; a failed switch must not fail the request", i, rec.Code, rec.Body.String())
		}
	}
	if len(failures) != 2 {
		t.Fatalf("fullDuplexUnavailable fired %d times for two requests over a recorder, want 2："+
			"开关失败时中间件没有报告，等于默默假设它成功了", len(failures))
	}
	for i, err := range failures {
		if !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("failure %d = %v, want http.ErrNotSupported underneath", i, err)
		}
		if !strings.Contains(err.Error(), "*httptest.ResponseRecorder") {
			t.Errorf("failure %d = %q, does not name the writer the chain stopped at", i, err)
		}
	}
	for i, b := range seen {
		if string(b) != body {
			t.Errorf("handler %d saw %q, want the whole body", i, b)
		}
	}

	// Out of scope: the switch is not attempted, so nothing is reported.
	// Requests the preamble can never touch keep net/http's default.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
	if len(failures) != 2 {
		t.Errorf("fullDuplexUnavailable fired for an out-of-scope request (%d reports)："+
			"全双工只该在 desperation 可能介入的请求上开启", len(failures))
	}
}

// TestBuildWiresEveryEarlyFlushNote: every reporter the middleware takes is
// connected to the Runtime, and Build hands the middleware that bundle.
//
// A nil note is how a test opts out, so the middleware cannot tell a missing
// one from an unwanted one. Unwired uploadCut means an interrupted upload is
// recorded nowhere; unwired fullDuplexUnavailable means a broken unwrap chain
// is never mentioned. Neither shows up in any request-level test.
func TestBuildWiresEveryEarlyFlushNote(t *testing.T) {
	// fullDuplexUnavailable also warns; this test reads the journal instead.
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	dir := t.TempDir()
	jw, err := journal.Open(dir, 1)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	rt := &Runtime{Journal: jw}
	notes := rt.earlyFlushNotes()

	// Every field, by reflection, so a note added later and not wired here
	// fails too.
	v := reflect.ValueOf(notes)
	reporters := 0
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Func {
			continue
		}
		reporters++
		if f.IsNil() {
			t.Errorf("earlyFlushNotes.%s 没有接到 Runtime 上：这类事件会被静默丢弃", v.Type().Field(i).Name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// And each one reaches the journal: a non-nil note that records nothing
	// is the same silence.
	cutErr := errors.New("body read failed mid-upload")
	notes.flushed(time.Second, verdictStreaming)
	notes.desperationHeld(time.Second)
	notes.translated(http.StatusBadGateway)
	notes.timedOut(time.Second)
	notes.uploadCut(123, 4567, cutErr)
	notes.fullDuplexUnavailable(errors.New("feature not supported (the writer chain stops at *proxy.opaqueWriter)"))
	if err := jw.Close(); err != nil {
		t.Fatalf("journal close: %v", err)
	}
	events := earlyFlushJournal(t, dir)
	if len(events) != reporters {
		t.Errorf("journal 里有 %d 条 earlyflush 事件，want %d（每个 note 一条）：%v", len(events), reporters, eventDetails(events))
	}
	var cut, duplex bool
	for _, e := range events {
		cut = cut || (strings.Contains(e.Detail, cutErr.Error()) && strings.Contains(e.Detail, "4567"))
		duplex = duplex || strings.Contains(e.Detail, "opaqueWriter")
	}
	if !cut {
		t.Error("uploadCut 没有留下带字节数和读错误的记录")
	}
	if !duplex {
		t.Error("fullDuplexUnavailable 没有留下点名 writer 的记录")
	}

	// Build must pass this bundle, not a literal of its own: the checks
	// above see only what earlyFlushNotes returns.
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	if !strings.Contains(string(src), "EarlyFlushMiddleware(c.streamEarlyFlush(), rt.earlyFlushNotes())") {
		t.Error("Build 不再把 rt.earlyFlushNotes() 交给 EarlyFlushMiddleware——上面的逐字段检查就看不到它实际装配的 notes 了" +
			"（注册方式改了的话请同步改本守卫）")
	}
}

// TestEarlyFlushFullDuplexThroughBuiltChain drives the desperation path
// through the service Build assembles.
//
// The chain the full duplex switch has to unwrap through is longer than the
// slimproxy writers: the engine's own CPA trace writer sits outside all of
// them (SLIMPROXY_PATCHES.md 第 15 条), and any middleware added later that
// wraps the writer joins it. A hand-built engine cannot see either; this test
// fails the moment anything in the real chain stops unwrapping, because the
// switch then fails and the preamble is held behind net/http's drain.
func TestEarlyFlushFullDuplexThroughBuiltChain(t *testing.T) {
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })
	shrinkEarlyFlushTimers(t)

	port := freeTestPort(t)
	rt, err := Build(Config{
		Host:        "127.0.0.1",
		Port:        port,
		APIKeys:     []string{"k"},
		AuthDir:     filepath.Join(t.TempDir(), "auths"),
		LogDir:      t.TempDir(),
		JournalDays: JournalDisabled,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rt.Service.Run(ctx) }()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitForListener(t, addr)

	h := http.Header{}
	h.Set("Authorization", "Bearer k")
	h.Set("Content-Type", "application/json")
	// Under 256KB left at the stall: the shape a failed switch turns into a
	// held preamble rather than a merely closed connection.
	got := uploadWithPause(t, "http://"+addr+"/v1/messages", h, streamingBody(t, 192<<10), 64<<10, 3*time.Second)

	if !got.midUpload {
		t.Error("经 Build 装配的链路上，预发头没有在上传途中送达：链上某个 writer 不再 Unwrap，" +
			"EnableFullDuplex 失败，预发头被 net/http 的 body 收尾挡住（2026-09-23 那 20 个请求的死法）")
	}
	if got.resp.StatusCode != http.StatusOK || !strings.Contains(got.resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("status %d, Content-Type %q; want the 200 SSE preamble", got.resp.StatusCode, got.resp.Header.Get("Content-Type"))
	}
}
