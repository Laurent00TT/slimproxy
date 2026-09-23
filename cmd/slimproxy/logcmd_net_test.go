package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Laurent00TT/slimproxy/journal"
)

// TestNetCell: the two net legs, rendered for the log table's 网络 column.
//
// i18n.Set is only ever called from main(), so a test binary that never links
// it stays on the package's zero-value language, Zh -- see i18n.Lang's doc
// comment. That makes the Chinese rendering the deterministic one to assert
// under `go test`, matching how cmd/slimproxy/logstate_test.go already reads.
func TestNetCell(t *testing.T) {
	e := journal.Event{Kind: journal.KindRequest, UploadMs: 900, WriteBlockMs: 150}
	if got := netCell(e); got != "传900ms┊写150ms" {
		t.Fatalf("netCell = %q, want %q", got, "传900ms┊写150ms")
	}
	if got := netCell(journal.Event{Kind: journal.KindRequest, UploadMs: 1200}); got != "传1.2s" {
		t.Fatalf("upload-only netCell = %q, want %q", got, "传1.2s")
	}
	if got := netCell(journal.Event{Kind: journal.KindRequest}); got != "—" {
		t.Fatalf("empty netCell = %q, want —", got)
	}
}

// TestNetCellCarriesBodySize: in_kb rides on the upload leg, so the row reads
// as a rate. Rows without in_kb are pinned unchanged by TestNetCell above; a
// size whose leg was sub-ms keeps the leg's dash instead of vanishing.
func TestNetCellCarriesBodySize(t *testing.T) {
	for _, tc := range []struct {
		e    journal.Event
		want string
	}{
		{journal.Event{Kind: journal.KindRequest, UploadMs: 1400, InKB: 2048, WriteBlockMs: 150}, "传1.4s·2.0MB┊写150ms"},
		{journal.Event{Kind: journal.KindRequest, UploadMs: 900, InKB: 512}, "传900ms·512KB"},
		{journal.Event{Kind: journal.KindRequest, InKB: 5}, "传—·5KB"},
	} {
		if got := netCell(tc.e); got != tc.want {
			t.Errorf("netCell(up=%d in=%d wb=%d) = %q, want %q", tc.e.UploadMs, tc.e.InKB, tc.e.WriteBlockMs, got, tc.want)
		}
	}
}

// TestLogStatsUploadThroughput: end to end, from JSONL on disk to the -stats
// line. The figure covers only rows with both in_kb and an upload leg of at
// least MinRatedUploadMs, and says how many of all requests that was -- a bare
// rate over a subset would read as describing every request.
func TestLogStatsUploadThroughput(t *testing.T) {
	run := func(t *testing.T, events ...journal.Event) string {
		t.Helper()
		logDir := t.TempDir()
		jw, err := journal.Open(filepath.Join(logDir, "events"), 7)
		if err != nil {
			t.Fatalf("journal.Open: %v", err)
		}
		at := time.Now().Add(-time.Minute)
		for _, e := range events {
			e.At = at
			jw.Append(e)
		}
		if err := jw.Close(); err != nil {
			t.Fatalf("close journal: %v", err)
		}

		cfgPath := writeTestConfig(t)
		cfg, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		cfg = append(cfg, []byte("log-dir: \""+filepath.ToSlash(logDir)+"\"\n")...)
		if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
			t.Fatal(err)
		}

		cx, out, _ := newTestContext()
		if err := dispatch(cx, []string{"log", "-config", cfgPath, "-stats"}); err != nil {
			t.Fatalf("log -stats: %v", err)
		}
		return out.String()
	}

	ok := true
	req := func(inKB, upMs int64) journal.Event {
		return journal.Event{Kind: journal.KindRequest, Route: "claude → claude", OK: &ok, LatencyMs: 9000, InKB: inKB, UploadMs: upMs}
	}

	t.Run("rated subset", func(t *testing.T) {
		out := run(t,
			req(2048, 1000), // 计入
			req(1024, 4000), // 计入
			req(0, 5000),    // 旧行：无 in_kb
			req(4096, 0),    // 无上传腿
			req(2048, journal.MinRatedUploadMs-1),
			req(0, 0),
		)
		// 3072KB ÷ 5s = 614.4KB/s；覆盖 2/6。
		want := "上传吞吐 614KB/s（2/6 个请求"
		if !strings.Contains(out, want) {
			t.Fatalf("-stats 缺 %q：\n%s", want, out)
		}
	})

	t.Run("nothing rated", func(t *testing.T) {
		out := run(t, req(0, 5000), req(4096, 0), req(0, 0))
		if !strings.Contains(out, "3 个请求") {
			t.Fatalf("-stats 没读到事件：\n%s", out)
		}
		if strings.Contains(out, "上传吞吐") {
			t.Fatalf("没有同时带 in_kb 与够长上传腿的行，却打印了吞吐：\n%s", out)
		}
	})
}
