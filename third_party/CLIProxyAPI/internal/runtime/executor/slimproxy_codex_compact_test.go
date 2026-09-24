package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestSlimproxyCodexCompactFailureCooldown(t *testing.T) {
	for _, tc := range []struct {
		name          string
		alt           string
		status        int
		wantAvailable bool
	}{
		{"compact endpoint missing", "responses/compact", http.StatusNotFound, true},
		{"inference model missing", "", http.StatusNotFound, false},
		{"compact unauthorized", "responses/compact", http.StatusUnauthorized, false},
		{"compact forbidden", "responses/compact", http.StatusForbidden, false},
		{"compact quota exhausted", "responses/compact", http.StatusTooManyRequests, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					wantPath := "/responses"
					if tc.alt != "" {
						wantPath = "/" + tc.alt
					}
					if r.URL.Path != wantPath {
						t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, `{"error":{"message":"upstream capability failure"}}`)
					return
				}
				if r.URL.Path != "/responses" {
					t.Errorf("followup path = %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer server.Close()
			const model = "slimproxy-compact-cooldown-test-model"
			const authID = "slimproxy-compact-cooldown-test-auth"
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(NewCodexExecutor(&config.Config{}))
			_, err := manager.Register(context.Background(), &cliproxyauth.Auth{
				ID: authID, Provider: "codex", Status: cliproxyauth.StatusActive,
				Attributes: map[string]string{"api_key": "test", "base_url": server.URL},
			})
			if err != nil {
				t.Fatal(err)
			}
			modelRegistry := registry.GetGlobalRegistry()
			modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"` + model + `","input":"test"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: tc.alt}
			_, err = manager.Execute(context.Background(), []string{"codex"}, req, opts)
			if got := statusCodeFromTestError(t, err); got != tc.status {
				t.Fatalf("first error status = %d, want %d; error = %v", got, tc.status, err)
			}
			opts.Alt = ""
			opts.Stream = true
			stream, followupErr := manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
			if tc.wantAvailable {
				if followupErr != nil {
					t.Fatalf("compact failure disabled inference: %v", followupErr)
				}
				chunks := 0
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					chunks++
				}
				if chunks == 0 || calls.Load() != 2 {
					t.Fatalf("followup did not complete: chunks=%d upstream_calls=%d", chunks, calls.Load())
				}
			} else {
				if followupErr == nil {
					t.Fatal("credential/model failure failed to block inference")
				}
				if calls.Load() != 1 {
					t.Fatalf("cooldown bypassed: upstream_calls=%d", calls.Load())
				}
			}
		})
	}
}
