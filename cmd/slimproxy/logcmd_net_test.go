package main

import (
	"testing"

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
