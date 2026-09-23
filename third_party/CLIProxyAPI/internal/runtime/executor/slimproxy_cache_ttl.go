package executor

// slimproxy patch (see SLIMPROXY_PATCHES.md 第 10-12 条).
//
// Why this exists: Anthropic's prompt cache lives five minutes by default,
// and the clock runs from the START of the request that last touched the
// prefix -- time spent generating the answer counts against it. A Claude Code
// turn that runs past four minutes or so (agentic loops routinely do) comes
// back for the next turn to find the whole prefix evicted, and the entire
// conversation is written again at full price. Measured on 2026-09-16: 18
// full rewrites of a ~300k-token prefix in one morning, 15 of them right
// after a gap longer than five minutes. The one-hour lifetime exists for
// exactly this, and the client does not ask for it on every breakpoint.
// This is the one place that sees the request whole and knows which client
// sent it, so it is where the lifetime gets pinned.
//
// Why every breakpoint rather than only the ones missing a ttl: Anthropic
// requires longer lifetimes to precede shorter ones in evaluation order
// (tools -> system -> messages), and normalizeCacheControlTTL enforces that
// by stripping any 1h that follows a 5m. A mixed result would be flattened
// back to 5m without a trace; a uniform one has no ordering to violate. The
// same constraint fixes the call site: before normalizeCacheControlTTL, never
// after.
//
// Why only Claude Code: the justification is that client's turn length, and
// the premium is real -- a 1h write bills at 2x the base input rate against
// 1.25x for 5m. A client that never returns within five minutes anyway would
// pay it for nothing.

import (
	"context"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// applyClaudeCodeCacheTTL rewrites the lifetime of every cache_control
// breakpoint in a Claude Code request to cfg.ClaudeCode.CacheTTL.
//
// The payload comes back untouched -- the same bytes, not a copy -- when the
// knob is unset, when the client is not Claude Code, or when nothing needed
// changing, so the byte identity the surrounding pipeline's tests pin (see
// TestNormalizeCacheControlTTL_PreservesOriginalBytesWhenNoChange) survives.
// Blocks without cache_control never get one: WHICH prefixes are cached stays
// the client's decision; only for how long is decided here.
func applyClaudeCodeCacheTTL(ctx context.Context, cfg *config.Config, payload []byte) []byte {
	if cfg == nil {
		return payload
	}
	ttl := strings.TrimSpace(cfg.ClaudeCode.CacheTTL)
	if ttl == "" {
		return payload
	}
	if !isClaudeCodeUserAgent(getClientUserAgent(ctx)) {
		return payload
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	original := payload
	modified := false
	pin := func(path string, block gjson.Result) {
		cc := block.Get("cache_control")
		if !cc.IsObject() {
			return
		}
		if cc.Get("ttl").String() == ttl {
			return
		}
		updated, err := sjson.SetBytes(payload, path+".cache_control.ttl", ttl)
		if err != nil {
			return
		}
		payload = updated
		modified = true
	}

	if tools := gjson.GetBytes(payload, "tools"); tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			pin(fmt.Sprintf("tools.%d", int(idx.Int())), item)
			return true
		})
	}
	// A bare string system prompt cannot carry cache_control; only the array
	// form has blocks to visit.
	if system := gjson.GetBytes(payload, "system"); system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			pin(fmt.Sprintf("system.%d", int(idx.Int())), item)
			return true
		})
	}
	if messages := gjson.GetBytes(payload, "messages"); messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				pin(fmt.Sprintf("messages.%d.content.%d", int(msgIdx.Int()), int(itemIdx.Int())), item)
				return true
			})
			return true
		})
	}

	if !modified {
		return original
	}
	return payload
}

// isClaudeCodeUserAgent is the same test helps.ShouldCloak applies in "auto"
// mode -- the definition of "Claude Code client" the rest of this executor
// already lives by.
func isClaudeCodeUserAgent(ua string) bool {
	return strings.HasPrefix(ua, "claude-cli")
}
