package main

import (
	"strings"
	"testing"

	"github.com/Laurent00TT/slimproxy/journal"
)

// TestStateDetailShowsStreamIdentity: the stall guard stamps its health
// events with the stream they are about, and the whole point is answering
// "which model, whose credential, how far in" WITHOUT opening the raw JSONL.
// Fields that reach the file but not the screen would be stamped for nobody
// -- review found exactly that gap between the journal write and this
// renderer.
func TestStateDetailShowsStreamIdentity(t *testing.T) {
	got := stateDetail(journal.Event{
		Kind:      journal.KindHealth,
		State:     "notail",
		Model:     "claude-opus-5",
		Auth:      "abc123.json",
		LatencyMs: 195_000,
		Detail:    "detail text",
	})
	for _, want := range []string{"notail", "opus-5", "abc123", "3m15s", "detail text"} {
		if !strings.Contains(got, want) {
			t.Errorf("stateDetail 输出缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "0 分钟") || strings.Contains(got, "0m ") {
		t.Errorf("流龄被按分钟粒度截没了：\n%s", got)
	}

	// Events without stream identity -- proxy start, tunnel transitions --
	// must render exactly as before: no dashes, no empty separators.
	plain := stateDetail(journal.Event{Kind: journal.KindProxy, State: "start", Detail: "cfg"})
	if plain != "start · cfg" {
		t.Errorf("无流身份的事件行变了：%q, want %q", plain, "start · cfg")
	}
}
