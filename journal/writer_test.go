package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/momo/slimproxy/metrics"
)

func readDay(t *testing.T, dir, day string) []Event {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, day+".jsonl"))
	if err != nil {
		t.Fatalf("读取 %s: %v", day, err)
	}
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("解析 %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestWriterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 7)
	if err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 7, 26, 14, 32, 11, 0, time.Local)
	w.Append(FromSample(metrics.Sample{
		At: at, Account: "openai@acct", Target: "claude", Model: "claude-opus-5",
		TTFT: 812 * time.Millisecond, Latency: 3140 * time.Millisecond,
		Tokens: 1892, Auth: "claude-you@x.com", Executor: "claude", Quota5h: 0.62,
	}))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readDay(t, dir, "2026-07-26")
	if len(events) != 1 {
		t.Fatalf("写出 %d 条，应为 1", len(events))
	}
	e := events[0]
	// "account @ provider", not "client → provider": the left-hand side is the
	// account that served the request. usage.Record carries no inbound dialect,
	// and calling it one was wrong for most of this project's life.
	if e.Route != "openai@acct @ claude" || e.Model != "claude-opus-5" {
		t.Errorf("路由/模型不对: %+v", e)
	}
	if e.OK == nil || !*e.OK {
		t.Error("成功的请求应记录 ok=true")
	}
	if e.TTFTMs != 812 || e.LatencyMs != 3140 {
		t.Errorf("时延不对: ttft=%d ms=%d", e.TTFTMs, e.LatencyMs)
	}
	if e.Quota5h == nil || *e.Quota5h != 0.62 {
		t.Errorf("配额水位未记录: %+v", e.Quota5h)
	}
	if e.Auth != "claude-you@x.com" {
		t.Errorf("未记录是哪个凭据服务的: %q", e.Auth)
	}
}

// TestZeroQuotaIsDistinctFromAbsent.
//
// A freshly reset five-hour window really is at zero, and a provider that never
// sends the header reports nothing. Collapsing them would make "0% used" and
// "unknown" the same line in a file kept precisely to answer how much was left.
func TestZeroQuotaIsDistinctFromAbsent(t *testing.T) {
	fresh := FromSample(metrics.Sample{At: time.Now(), Quota5h: 0})
	if fresh.Quota5h == nil {
		t.Error("0.0 是真实读数，不应被当作缺失")
	}
	absent := FromSample(metrics.Sample{At: time.Now(), Quota5h: -1})
	if absent.Quota5h != nil {
		t.Error("上游未报告配额时不应写出一个数字")
	}
}

// TestSuccessCarriesNoStatus.
//
// The usage record never reports the status of a success, so writing 200 would
// record an observation that was never made -- the same reason the panel shows
// "ok" instead of a number.
func TestSuccessCarriesNoStatus(t *testing.T) {
	e := FromSample(metrics.Sample{At: time.Now(), Failed: false, Status: 0})
	if e.Status != 0 {
		t.Errorf("成功的请求不应带状态码: %d", e.Status)
	}
	f := FromSample(metrics.Sample{At: time.Now(), Failed: true, Status: 429})
	if f.Status != 429 {
		t.Errorf("失败应保留真实状态码，实际 %d", f.Status)
	}
}

// TestAppendNeverBlocks.
//
// Append runs on the usage manager's dispatch goroutine. Blocking there backs
// up every other plugin behind a disk write, so a full queue must drop and
// count rather than wait.
func TestAppendNeverBlocks(t *testing.T) {
	w := &Writer{events: make(chan Event, 2), done: make(chan struct{})}
	// No goroutine draining: the queue fills immediately.
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize+100; i++ {
			w.Append(Event{Kind: KindRequest})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Append 阻塞了：会把 usage 分发 goroutine 拖住")
	}
	if w.Stats().Dropped == 0 {
		t.Error("队列满时应记录丢弃数，静默丢事件的日志比不写更糟")
	}
}

// TestRetentionDeletesByDayNotMtime.
//
// A file's mtime moves when it is written, so today's file looks newest and a
// restored backup looks new regardless of content. The name is the day the
// events belong to, which is the thing being retained.
func TestRetentionDeletesByDayNotMtime(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	old := now.AddDate(0, 0, -30).Format("2006-01-02")
	recent := now.AddDate(0, 0, -1).Format("2006-01-02")
	for _, day := range []string{old, recent} {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Something else's file must survive: this deletes things.
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &Writer{dir: dir, retention: 7 * 24 * time.Hour}
	w.prune(now)

	if _, err := os.Stat(filepath.Join(dir, old+".jsonl")); !os.IsNotExist(err) {
		t.Error("超过保留期的文件未被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, recent+".jsonl")); err != nil {
		t.Error("保留期内的文件被误删")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("不是本包写的文件被删除了")
	}
}

// TestEventsRotateByDay: a request just after midnight belongs to the new day's
// file, not to whichever file happened to be open.
func TestEventsRotateByDay(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 26, 23, 59, 59, 0, time.Local)
	w.Append(Event{At: base, Kind: KindRequest})
	w.Append(Event{At: base.Add(2 * time.Second), Kind: KindRequest})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	days, err := Days(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("应写出两天的文件，实际 %v", days)
	}
}
