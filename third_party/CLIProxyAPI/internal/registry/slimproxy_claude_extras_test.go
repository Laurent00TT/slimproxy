// slimproxy patch: guard tests for the pinned Claude models (see
// SLIMPROXY_PATCHES.md 第 13 条). An upstream rebase that drops either call
// site compiles cleanly and fails only here -- and in production as a local
// 502 for a model Anthropic serves. The root module's forkcheck package runs
// these from `go test ./...`.
package registry

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

const pinnedOpus55 = "claude-opus-5-5"

// pinCatalog loads data as the catalog for the test and restores the
// previous one afterwards.
func pinCatalog(t *testing.T, data []byte) {
	t.Helper()
	previous := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = previous
		modelsCatalogStore.mu.Unlock()
	})
	if err := loadModelsFromBytes(data, "test"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
}

// embeddedCatalogWithClaude returns the embedded catalog with extra appended
// to its claude section -- a remote that has caught up.
func embeddedCatalogWithClaude(t *testing.T, extra map[string]any) []byte {
	t.Helper()
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(embeddedModelsJSON, &catalog); err != nil {
		t.Fatalf("parse embedded catalog: %v", err)
	}
	var claude []map[string]any
	if err := json.Unmarshal(catalog["claude"], &claude); err != nil {
		t.Fatalf("parse claude section: %v", err)
	}
	claude = append(claude, extra)
	var err error
	if catalog["claude"], err = json.Marshal(claude); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func claudeEntries(id string) []*ModelInfo {
	var out []*ModelInfo
	for _, m := range GetClaudeModels() {
		if m.ID == id {
			out = append(out, m)
		}
	}
	return out
}

func rawCatalogHasClaude(id string) bool {
	for _, m := range getModels().Claude {
		if m != nil && m.ID == id {
			return true
		}
	}
	return false
}

// TestClaudeExtrasFillCatalogGap: a catalog without the model still routes
// it -- registration (GetClaudeModels), the static lookup the thinking layer
// falls back on, and the per-channel listing all see exactly one entry.
func TestClaudeExtrasFillCatalogGap(t *testing.T) {
	pinCatalog(t, embeddedModelsJSON)
	if rawCatalogHasClaude(pinnedOpus55) {
		t.Skipf("the embedded catalog now lists %s itself; the extra is dead weight -- see SLIMPROXY_PATCHES.md 第 14 条的撤销条件", pinnedOpus55)
	}

	if got := claudeEntries(pinnedOpus55); len(got) != 1 {
		t.Fatalf("GetClaudeModels has %d %s entries, want 1", len(got), pinnedOpus55)
	}
	if LookupStaticModelInfo(pinnedOpus55) == nil {
		t.Fatalf("LookupStaticModelInfo(%s) = nil: the thinking layer would treat it as user-defined and send budget_tokens", pinnedOpus55)
	}
	found := false
	for _, m := range GetStaticModelDefinitionsByChannel("claude") {
		found = found || m.ID == pinnedOpus55
	}
	if !found {
		t.Fatalf("claude channel definitions lack %s", pinnedOpus55)
	}

	// Callers own what they get: mutating it must not leak into the next read.
	claudeEntries(pinnedOpus55)[0].Thinking.Levels[0] = "mutated"
	if got := claudeEntries(pinnedOpus55)[0].Thinking.Levels[0]; got != "low" {
		t.Fatalf("mutation leaked into the pinned entry: levels[0] = %q", got)
	}
}

// TestClaudeExtrasSurviveRemoteRefresh: the refresh replaces the catalog
// wholesale -- the reason an edit to the embedded models.json does not last.
// The extra must still be served afterwards, must not have been written into
// the store, and an unchanged refresh must stay "no changes" (a spurious
// claude change would re-register every auth every three hours).
func TestClaudeExtrasSurviveRemoteRefresh(t *testing.T) {
	pinCatalog(t, embeddedModelsJSON)
	srv, served := serveCatalog(t, embeddedModelsJSON)
	pinModelsURLs(t, srv.URL)
	SetModelsHTTPClient(nil)

	var notified [][]string
	SetModelRefreshCallback(func(changed []string) { notified = append(notified, changed) })
	t.Cleanup(func() { SetModelRefreshCallback(nil) })

	before := getModels()
	tryRefreshModels(context.Background(), "extras guard refresh")
	if served.Load() == 0 || getModels() == before {
		t.Fatalf("the refresh did not replace the catalog (served=%d): the guard proves nothing", served.Load())
	}

	if got := claudeEntries(pinnedOpus55); len(got) != 1 {
		t.Fatalf("after a refresh from a remote without %s, GetClaudeModels has %d entries, want 1", pinnedOpus55, len(got))
	}
	if rawCatalogHasClaude(pinnedOpus55) {
		t.Fatalf("%s was written into the store; change detection now compares the overlay against the remote", pinnedOpus55)
	}
	if len(notified) != 0 {
		t.Fatalf("an unchanged refresh reported changes %v", notified)
	}
}

// TestClaudeExtrasYieldToRemoteEntry: once the remote catalog lists the id,
// its entry is the one served, once, in both lookups.
func TestClaudeExtrasYieldToRemoteEntry(t *testing.T) {
	pinCatalog(t, embeddedCatalogWithClaude(t, map[string]any{
		"id":           pinnedOpus55,
		"object":       "model",
		"owned_by":     "anthropic",
		"type":         "claude",
		"display_name": "remote entry",
	}))

	got := claudeEntries(pinnedOpus55)
	if len(got) != 1 || got[0].DisplayName != "remote entry" {
		t.Fatalf("GetClaudeModels served %d entries (first: %+v), want the remote's alone", len(got), got)
	}
	if m := LookupStaticModelInfo(pinnedOpus55); m == nil || m.DisplayName != "remote entry" {
		t.Fatalf("LookupStaticModelInfo = %+v, want the remote entry", m)
	}
}

// TestClaudeExtraOpus55IsLevelOnly pins the shape the thinking layer needs.
// min/max would make the model budget-capable and put budget_tokens on the
// wire, which Opus 5.5 rejects; a missing level turns Claude Code's own
// effort into a local 400.
func TestClaudeExtraOpus55IsLevelOnly(t *testing.T) {
	m := lookupSlimproxyClaudeExtra(pinnedOpus55)
	if m == nil {
		t.Fatalf("no pinned %s", pinnedOpus55)
	}
	if m.Type != "claude" || m.ContextLength != 1000000 || m.MaxCompletionTokens != 128000 {
		t.Fatalf("type/context/max tokens = %q/%d/%d, want claude/1000000/128000", m.Type, m.ContextLength, m.MaxCompletionTokens)
	}
	th := m.Thinking
	if th == nil {
		t.Fatal("no thinking block: every client's effort would be stripped")
	}
	if th.Min != 0 || th.Max != 0 {
		t.Fatalf("thinking min/max = %d/%d: budget-capable, would send budget_tokens", th.Min, th.Max)
	}
	if th.ZeroAllowed {
		t.Fatal("zero_allowed set on a model that cannot stop thinking")
	}
	if want := []string{"low", "medium", "high", "xhigh", "max"}; !slices.Equal(th.Levels, want) {
		t.Fatalf("levels = %v, want %v", th.Levels, want)
	}
}
