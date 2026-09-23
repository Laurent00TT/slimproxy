// slimproxy patch: Claude models served ahead of the remote catalog
// (SLIMPROXY_PATCHES.md 第 13 条).
//
// Routing is gated on the catalog: a model id no auth has registered is a
// 502 "unknown provider for model" before any executor runs. The catalog is
// the embedded models.json only until the first successful remote fetch,
// which replaces it wholesale -- so an id added to the embedded file is gone
// seconds after every start, and a model Anthropic has released but
// router-for-me/models has not yet listed cannot be reached at all.
// claude-opus-5-5 was exactly that on 2026-09-23: thirteen Claude Code
// requests, thirteen local 502s, nothing sent upstream.
//
// The extras are merged when the catalog is READ, never written into the
// store. detectChangedProviders therefore keeps comparing remote against
// remote, so an unchanged refresh stays "no changes" and does not
// re-register every auth (which clears cooldown state) every three hours.
// The merge only fills gaps: once the catalog lists an id, its own entry is
// served. An extra still matters after the remote has caught up, because a
// start whose remote fetch fails serves the embedded models.json; it is dead
// weight once the vendored file lists the id too.
package registry

import "strings"

// withSlimproxyClaudeExtras appends every extra whose id the catalog lacks.
func withSlimproxyClaudeExtras(models []*ModelInfo) []*ModelInfo {
	present := make(map[string]struct{}, len(models))
	for _, m := range models {
		if m != nil {
			present[strings.ToLower(strings.TrimSpace(m.ID))] = struct{}{}
		}
	}
	for _, extra := range slimproxyClaudeExtras() {
		if _, ok := present[strings.ToLower(extra.ID)]; ok {
			continue
		}
		models = append(models, extra)
	}
	return models
}

// lookupSlimproxyClaudeExtra is LookupStaticModelInfo's fallback after every
// catalog section has missed, which keeps the remote entry winning there too.
func lookupSlimproxyClaudeExtra(modelID string) *ModelInfo {
	for _, extra := range slimproxyClaudeExtras() {
		if extra.ID == modelID {
			return extra
		}
	}
	return nil
}

// slimproxyClaudeExtras returns fresh copies on every call, so callers may
// mutate what they get.
//
// The thinking block is the load-bearing part. Opus 5.5 always thinks:
// thinking.type "enabled" with budget_tokens is a 400 at every effort level.
// A levels-only block (no min/max) makes the thinking layer treat the model
// as level-only, so a budget from any client is converted to adaptive plus
// output_config.effort instead of being sent as budget_tokens. Adding min/max
// -- the claude-fable-5 shape -- would reintroduce budget_tokens upstream.
// All five levels are listed because native Claude requests are validated
// against this list and Claude Code sends xhigh by default. zero_allowed is
// left false because the model cannot be told not to think; note it does not
// stop an explicit thinking.type "disabled" from being forwarded (that path
// ignores it for Claude targets, internal/thinking/validate.go).
func slimproxyClaudeExtras() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:                  "claude-opus-5-5",
			Object:              "model",
			Created:             1790121600, // 2026-09-23
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Opus 5.5",
			Description:         "Successor to Claude Opus 5 for long-running agentic coding and knowledge work",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking: &ThinkingSupport{
				DynamicAllowed: true,
				Levels:         []string{"low", "medium", "high", "xhigh", "max"},
			},
		},
	}
}
