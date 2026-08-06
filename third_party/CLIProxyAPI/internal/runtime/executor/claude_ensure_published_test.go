package executor

// slimproxy patch regression test (see SLIMPROXY_PATCHES.md).
//
// What is being pinned: the claude stream executor publishes a usage record
// for EVERY stream, however the stream ends. Upstream v7.2.103 published only
// on a scanned top-level-usage line (the tail message_delta) or a scanner
// error; a stream that ended cleanly before the tail published nothing, and
// the request vanished from the books while the access log recorded a 200.
// The patch adds the same reporter.EnsurePublished backstop the
// openai_compat executor has always had.
//
// Run this FIRST after rebasing the patches onto a new upstream version: it
// fails loudly if the publish points moved or the backstop got lost.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// recordSink collects every record the default usage manager dispatches.
//
// The default manager is process-global and plugins cannot be unregistered,
// so one sink serves every test in this file and tests tell their records
// apart by model name -- each test uses a model string nobody else does.
type recordSink struct {
	mu      sync.Mutex
	records []usage.Record
}

func (s *recordSink) HandleUsage(_ context.Context, r usage.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

func (s *recordSink) byModel(model string) []usage.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []usage.Record
	for _, r := range s.records {
		if r.Model == model {
			out = append(out, r)
		}
	}
	return out
}

var (
	sinkOnce sync.Once
	sink     = &recordSink{}
)

func startUsageSink(t *testing.T) *recordSink {
	t.Helper()
	sinkOnce.Do(func() {
		usage.RegisterPlugin(sink)
		usage.StartDefault(context.Background())
	})
	return sink
}

// sseUpstream serves one canned SSE body for POST /v1/messages and closes.
func sseUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
}

func runClaudeStream(t *testing.T, upstream *httptest.Server, model string) {
	t.Helper()
	e := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "ensure-published-probe",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": upstream.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model)),
	}
	opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("claude")}

	result, err := e.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-result.Chunks:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not close within 5s")
		}
	}
}

func awaitRecords(t *testing.T, s *recordSink, model string, want int) []usage.Record {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		got := s.byModel(model)
		if len(got) >= want {
			// One more beat, so a hypothetical duplicate publish has time to
			// arrive and fail the exact-count assertion below.
			time.Sleep(50 * time.Millisecond)
			return s.byModel(model)
		}
		select {
		case <-deadline:
			t.Fatalf("saw %d usage records for %s within 5s, want %d -- the EnsurePublished backstop patch is gone or broken", len(got), model, want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestClaudeStreamNoTailStillPublishes is the patch's reason to exist: a
// stream ending cleanly BEFORE the tail message_delta must still publish
// exactly one record -- zero tokens, not failed -- instead of nothing.
func TestClaudeStreamNoTailStillPublishes(t *testing.T) {
	const model = "claude-probe-notail"
	s := startUsageSink(t)
	upstream := sseUpstream(t, ""+
		"event: message_start\n"+
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\""+model+"\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n"+
		"\n"+
		"event: content_block_delta\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n"+
		"\n")
	defer upstream.Close()

	runClaudeStream(t, upstream, model)
	got := awaitRecords(t, s, model, 1)

	if len(got) != 1 {
		t.Fatalf("got %d usage records, want exactly 1 (the once must not double-publish)", len(got))
	}
	if got[0].Failed {
		t.Errorf("backstop record is marked failed; a silent clean ending is not a known failure")
	}
	if got[0].Detail.TotalTokens != 0 {
		t.Errorf("backstop record carries %d tokens; nothing was scanned, so it must be zero, not invented", got[0].Detail.TotalTokens)
	}
}

// TestClaudeStreamWithTailPublishesOnce: the ordinary path must be untouched
// by the patch -- one record, real token counts, no duplicate from the
// backstop (Publish and EnsurePublished share the reporter's once).
func TestClaudeStreamWithTailPublishesOnce(t *testing.T) {
	const model = "claude-probe-tail"
	s := startUsageSink(t)
	upstream := sseUpstream(t, ""+
		"event: message_start\n"+
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"model\":\""+model+"\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n"+
		"\n"+
		"event: message_delta\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":42}}\n"+
		"\n"+
		"event: message_stop\n"+
		"data: {\"type\":\"message_stop\"}\n"+
		"\n")
	defer upstream.Close()

	runClaudeStream(t, upstream, model)
	got := awaitRecords(t, s, model, 1)

	if len(got) != 1 {
		t.Fatalf("got %d usage records, want exactly 1 -- more than one means the backstop double-published", len(got))
	}
	if got[0].Detail.OutputTokens != 42 {
		t.Errorf("published OutputTokens = %d, want 42 from the scanned tail", got[0].Detail.OutputTokens)
	}
}
