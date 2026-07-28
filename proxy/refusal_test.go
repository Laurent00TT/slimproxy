package proxy

import (
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// findRefusal itself is covered in requestlog_test.go. What follows tests the
// other half: turning a detected refusal into something the caller can act on.

// The translated response a caller actually receives for a refusal -- empty
// content, finish_reason "stop", indistinguishable from the model choosing to
// say nothing. Captured verbatim from a request log.
const translatedRefusal = `{"id":"msg_011CdQ284j4pNHKuPtHJ8DAb","object":"chat.completion","created":1785039295,"model":"claude-opus-5","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":1927,"completion_tokens":1,"total_tokens":1928}}`

func TestMarkContentFilteredRewritesFinishReason(t *testing.T) {
	out, changed := markContentFiltered([]byte(translatedRefusal), "openai", false)
	if !changed {
		t.Fatal("finish_reason was not rewritten")
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"content_filter"`) {
		t.Errorf("want finish_reason content_filter, got: %s", s)
	}
	if strings.Contains(s, `"finish_reason":"stop"`) {
		t.Error("the original finish_reason survived the rewrite")
	}
	// The empty content is deliberate: writing the policy explanation into the
	// assistant message would make a notice indistinguishable from model
	// output, trading one ambiguity for a worse one.
	if !strings.Contains(s, `"content":""`) {
		t.Errorf("content should stay empty, got: %s", s)
	}
}

// TestMarkContentFilteredPreservesNumbers is the reason decodeJSONObject sets
// UseNumber. Decoding into interface{} makes every number a float64, and
// re-encoding renders large integers in scientific notation -- corrupting
// timestamps and token counts while "fixing" the response.
func TestMarkContentFilteredPreservesNumbers(t *testing.T) {
	out, changed := markContentFiltered([]byte(translatedRefusal), "openai", false)
	if !changed {
		t.Fatal("expected a rewrite")
	}
	s := string(out)
	for _, want := range []string{
		`"created":1785039295`,
		`"prompt_tokens":1927`,
		`"completion_tokens":1`,
		`"total_tokens":1928`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("number was reformatted, expected %s in: %s", want, s)
		}
	}
	if strings.Contains(s, "e+") || strings.Contains(s, "E+") {
		t.Errorf("a number was rendered in scientific notation: %s", s)
	}
}

func TestMarkContentFilteredLeavesOtherProtocolsAlone(t *testing.T) {
	// Claude's own schema already carries stop_reason "refusal" plus its
	// details, so that caller loses nothing and rewriting would corrupt it.
	// Gemini spells the field differently and is not guessed at.
	for _, format := range []string{"claude", "gemini", "codex", "antigravity", "interactions"} {
		out, changed := markContentFiltered([]byte(translatedRefusal), sdktranslator.Format(format), false)
		if changed {
			t.Errorf("%s: response was rewritten but should have been left alone", format)
		}
		if string(out) != translatedRefusal {
			t.Errorf("%s: body was modified", format)
		}
	}
}

// TestMarkContentFilteredOnlyReplacesStop pins that an identified termination
// reason is never overwritten. If the response ended for length or tool_calls,
// that is what actually terminated it, and reporting a refusal there would
// destroy accurate information.
func TestMarkContentFilteredOnlyReplacesStop(t *testing.T) {
	for _, reason := range []string{"length", "tool_calls", "content_filter"} {
		body := strings.Replace(translatedRefusal, `"finish_reason":"stop"`, `"finish_reason":"`+reason+`"`, 1)
		out, changed := markContentFiltered([]byte(body), "openai", false)
		if changed {
			t.Errorf("finish_reason %q was overwritten", reason)
		}
		if string(out) != body {
			t.Errorf("finish_reason %q: body was modified", reason)
		}
	}
}

func TestMarkContentFilteredStreamFrame(t *testing.T) {
	frame := `data: {"id":"x","object":"chat.completion.chunk","created":1785039295,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	out, changed := markContentFiltered([]byte(frame), "openai", true)
	if !changed {
		t.Fatal("streaming frame was not rewritten")
	}
	s := string(out)
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("SSE framing was lost: %s", s)
	}
	if !strings.Contains(s, `"finish_reason":"content_filter"`) {
		t.Errorf("want content_filter, got: %s", s)
	}
	if !strings.Contains(s, `"created":1785039295`) {
		t.Errorf("number reformatted in stream frame: %s", s)
	}
}

// A frame carrying several events must have every terminating choice marked,
// and the non-JSON event lines around them left intact.
func TestMarkContentFilteredMultiEventFrame(t *testing.T) {
	frame := "event: chunk\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n" +
		"event: chunk\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n"
	out, changed := markContentFiltered([]byte(frame), "openai", true)
	if !changed {
		t.Fatal("multi-event frame was not rewritten")
	}
	s := string(out)
	if strings.Count(s, "event: chunk") != 2 {
		t.Errorf("event lines were lost: %s", s)
	}
	if !strings.Contains(s, `"finish_reason":"content_filter"`) {
		t.Errorf("terminating choice not marked: %s", s)
	}
	if !strings.Contains(s, `"content":"hi"`) {
		t.Errorf("content delta was damaged: %s", s)
	}
}

func TestMarkContentFilteredHandlesGarbage(t *testing.T) {
	// A body that is not JSON must be returned untouched rather than dropped.
	for _, body := range []string{"", "not json", "{", `{"choices":"not-an-array"}`, `{}`} {
		out, changed := markContentFiltered([]byte(body), "openai", false)
		if changed {
			t.Errorf("%q: reported a change it could not have made", body)
		}
		if string(out) != body {
			t.Errorf("%q: body was altered to %q", body, out)
		}
	}
}

// TestCleanBodyDecrementsLiveCounter.
//
// Was: the clean-body path in NormalizeResponseBefore removed the pending
// entry with a bare Delete, skipping the live counter that forget maintains.
// Every refusal cleared by a retry drifted the count up by one, permanently;
// weeks of normal traffic would walk it across maxPendingRefusals and fire the
// emergency clear -- whose log line asks the operator to report a bug that
// does not exist.
func TestCleanBodyDecrementsLiveCounter(t *testing.T) {
	h := &refusalHooks{}
	ctx := context.Background()
	refusal := []byte(`{"stop_reason":"refusal","stop_details":{"category":"x","explanation":"y"}}`)
	clean := []byte(`{"stop_reason":"end_turn"}`)

	h.NormalizeResponseBefore(ctx, "", "", "m", nil, nil, refusal, false)
	if n := h.live.Load(); n != 1 {
		t.Fatalf("live = %d after storing a finding, want 1", n)
	}
	h.NormalizeResponseBefore(ctx, "", "", "m", nil, nil, clean, false)
	if n := h.live.Load(); n != 0 {
		t.Errorf("live = %d after a clean body cleared the finding, want 0", n)
	}
}

// TestMarkContentFilteredLeavesResponsesDialectAlone.
//
// Was: "openai-response" sat in markContentFiltered's dialect switch, but the
// Responses schema has no choices[].finish_reason, so setFinishReason failed
// on every response while the switch claimed the dialect was covered. Honest
// behaviour is to decline up front and let the After hook's warning fire --
// the same outcome, minus the code asserting a capability it does not have.
func TestMarkContentFilteredLeavesResponsesDialectAlone(t *testing.T) {
	body := `{"object":"response","status":"completed","output":[]}`
	out, changed := markContentFiltered([]byte(body), "openai-response", false)
	if changed {
		t.Fatal("openai-response body reported as rewritten; no rewrite semantics exist for it")
	}
	if string(out) != body {
		t.Errorf("body was altered without a rewrite: %s", out)
	}
}
