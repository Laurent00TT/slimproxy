package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// leakyDialError is what upstream puts in Record.Fail.Body verbatim.
const leakyDialError = `Post "https://api.anthropic.com/v1/messages": ` +
	`dial tcp 198.18.0.42:443: connectex: A connection attempt failed because ` +
	`the connected party did not properly respond after a period of time`

// TestFailureReachesDiskWithACauseAndWithoutTheText walks the whole path a real
// failure takes -- usage record, sample, event, JSONL on disk -- because every
// hop is a place the reason could be dropped again or the text could tag along.
//
// Both halves matter and they pull in opposite directions: the journal exists
// to say why a request failed, and it promises to hold no response bodies. The
// classification satisfies the first without breaking the second.
func TestFailureReachesDiskWithACauseAndWithoutTheText(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 7)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	sample := metrics.SampleFrom(cliproxyusage.Record{
		Provider:    "claude",
		Model:       "claude-sonnet-4",
		RequestedAt: time.Now(),
		Failed:      true,
		Fail:        cliproxyusage.Failure{Body: leakyDialError},
	})
	w.Append(FromSample(sample))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw := readTodaysFile(t, dir)

	// The reason has to be there, or nothing was gained.
	if !strings.Contains(raw, `"cause":"connect"`) {
		t.Errorf("落盘的事件里没有失败原因:\n%s", raw)
	}
	// And none of the error text may have come with it.
	for _, marker := range []string{"198.18.0.42", "connectex", ":443", "api.anthropic.com"} {
		if strings.Contains(raw, marker) {
			t.Errorf("原始错误文本的片段 %q 进了事件流:\n%s", marker, raw)
		}
	}
}

// TestSuccessWritesNoCause keeps `cause` usable as a failure filter: omitempty
// plus an empty value means a jq query for it selects exactly the failures.
func TestSuccessWritesNoCause(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 7)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w.Append(FromSample(metrics.SampleFrom(cliproxyusage.Record{
		Provider: "claude", RequestedAt: time.Now(), Failed: false,
	})))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if raw := readTodaysFile(t, dir); strings.Contains(raw, "cause") {
		t.Errorf("成功的请求也写了 cause 字段:\n%s", raw)
	}
}

// TestCauseSurvivesRoundTrip: the query command reads these back, so a field
// that serialises but does not deserialise would show every failure as blank
// again -- the same symptom, one layer further out.
func TestCauseSurvivesRoundTrip(t *testing.T) {
	original := FromSample(metrics.SampleFrom(cliproxyusage.Record{
		Provider:    "claude",
		RequestedAt: time.Now(),
		Failed:      true,
		Fail:        cliproxyusage.Failure{Body: `dial tcp: lookup foo: no such host`},
	}))

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Event
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Cause != metrics.CauseDNS {
		t.Errorf("往返之后 Cause = %q, want %q", back.Cause, metrics.CauseDNS)
	}
}

// TestUpstreamFailureKeepsBothStatusAndCause.
//
// They answer different questions -- "what did it say" and "did it get far
// enough to say anything" -- and a 429 next to "upstream" is what distinguishes
// a refusal that arrived from a connection that never landed.
func TestUpstreamFailureKeepsBothStatusAndCause(t *testing.T) {
	e := FromSample(metrics.SampleFrom(cliproxyusage.Record{
		Provider:    "claude",
		RequestedAt: time.Now(),
		Failed:      true,
		Fail:        cliproxyusage.Failure{StatusCode: 429, Body: `rate limit exceeded`},
	}))
	if e.Status != 429 {
		t.Errorf("Status = %d, want 429", e.Status)
	}
	if e.Cause != metrics.CauseUpstream {
		t.Errorf("Cause = %q, want %q", e.Cause, metrics.CauseUpstream)
	}
}

func readTodaysFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				t.Fatalf("ReadFile: %v", rerr)
			}
			return string(body)
		}
	}
	t.Fatal("事件目录里没有 .jsonl 文件")
	return ""
}
