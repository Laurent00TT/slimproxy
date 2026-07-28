package proxy

import "testing"

// realRefusalChunk is the exact bytes Anthropic returned when a request was
// blocked on policy. Kept verbatim: a hand-written approximation would drift
// from the shape actually being parsed, which is the only shape that matters.
const realRefusalChunk = `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"This request triggered restrictions on violative cyber content and was blocked under Anthropic's Usage Policy."}},"usage":{"input_tokens":25,"output_tokens":1}}

`

func TestFindRefusalOnRealUpstreamChunk(t *testing.T) {
	d, ok := findRefusal([]byte(realRefusalChunk))
	if !ok {
		t.Fatal("failed to detect a refusal in the exact chunk that motivated this code")
	}
	if d.Category != "cyber" {
		t.Errorf("category = %q, want %q", d.Category, "cyber")
	}
	if d.Explanation == "" {
		t.Error("explanation must be carried through; it is the whole point of reporting")
	}
}

// TestFindRefusalIgnoresOrdinaryTraffic guards the false-positive direction. A
// logger that cried refusal on normal completions would be noise, and noise is
// how a real warning gets ignored.
func TestFindRefusalIgnoresOrdinaryTraffic(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
	}{
		{"content delta", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`},
		{"normal end turn", `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`},
		{"max tokens", `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`},
		{"ping", `event: ping` + "\n" + `data: {"type":"ping"}`},
		{"done sentinel", `data: [DONE]`},
		{"empty", ``},
		{"malformed json", `data: {"delta":{"stop_reason":"refusal"`},
		// The word appearing in user content must not trigger it -- only the
		// stop_reason field is authoritative.
		{"refusal as text", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"the word refusal appears here"}}`},
	}
	for _, c := range cases {
		if _, ok := findRefusal([]byte(c.chunk)); ok {
			t.Errorf("%s: reported a refusal on ordinary traffic: %s", c.name, c.chunk)
		}
	}
}

// TestFindRefusalTolerantOfFraming: chunks do not arrive with one event per
// call. Several may be batched, and the `data: ` prefix is not guaranteed.
func TestFindRefusalTolerantOfFraming(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
	}{
		{
			"no data prefix",
			`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"category":"cyber","explanation":"blocked"}}}`,
		},
		{
			"batched after a content delta",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"category":"cyber","explanation":"blocked"}}}`,
		},
		{
			"top-level shape, non-streaming",
			`{"stop_reason":"refusal","stop_details":{"category":"other","explanation":"blocked"}}`,
		},
	}
	for _, c := range cases {
		d, ok := findRefusal([]byte(c.chunk))
		if !ok {
			t.Errorf("%s: missed the refusal", c.name)
			continue
		}
		if d.Explanation != "blocked" {
			t.Errorf("%s: explanation = %q, want %q", c.name, d.Explanation, "blocked")
		}
	}
}

// TestFindRefusalMissingDetails: the fields are not contractually guaranteed.
// Detection must still succeed so the event is reported at all.
func TestFindRefusalMissingDetails(t *testing.T) {
	d, ok := findRefusal([]byte(`data: {"delta":{"stop_reason":"refusal"}}`))
	if !ok {
		t.Fatal("a refusal without stop_details is still a refusal")
	}
	if d.Category != "" {
		t.Errorf("category = %q, want empty", d.Category)
	}
	// report() is what substitutes a placeholder; findRefusal reports facts.
}
