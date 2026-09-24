package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestSlimproxyCodexStreamUsageOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tail   string
		cancel bool
		failed bool
		output int64
	}{
		{name: "cancel while waiting upstream", cancel: true, failed: true},
		{name: "completed without usage", tail: `{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[]}}`},
		{name: "completed with usage", tail: `{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`, output: 3},
		{name: "EOF without terminal", failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startUsageSink(t)
			model := fmt.Sprintf("slimproxy-codex-usage-%d", time.Now().UnixNano())
			upstreamCanceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n")
				w.(http.Flusher).Flush()
				if tc.cancel {
					<-r.Context().Done()
					close(upstreamCanceled)
					return
				}
				if tc.tail != "" {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.tail)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := NewCodexExecutor(&config.Config{})
			result, err := e.ExecuteStream(ctx, &cliproxyauth.Auth{
				ID: "codex-usage-test", Provider: "codex",
				Attributes: map[string]string{"api_key": "test", "base_url": server.URL},
			}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"` + model + `","input":"test"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case chunk := <-result.Chunks:
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no initial event")
			}
			if tc.cancel {
				cancel()
				select {
				case <-upstreamCanceled:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream request was not canceled")
				}
			}
			deadline := time.After(3 * time.Second)
			for {
				select {
				case _, ok := <-result.Chunks:
					if !ok {
						goto closed
					}
				case <-deadline:
					t.Fatal("stream goroutine did not exit")
				}
			}
		closed:
			records := awaitRecords(t, s, model, 1)
			if len(records) != 1 {
				t.Fatalf("usage records = %d, want exactly one", len(records))
			}
			r := records[0]
			if r.Failed != tc.failed || r.Detail.OutputTokens != tc.output {
				t.Fatalf("failed=%v output=%d, want failed=%v output=%d", r.Failed, r.Detail.OutputTokens, tc.failed, tc.output)
			}
			if tc.cancel && !strings.Contains(r.Fail.Body, "context canceled") {
				t.Fatalf("cancellation classified as %q", r.Fail.Body)
			}
		})
	}
}
