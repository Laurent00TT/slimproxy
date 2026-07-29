package main

import (
	"testing"

	"github.com/Laurent00TT/slimproxy/journal"
	"github.com/Laurent00TT/slimproxy/metrics"
)

// TestStatusColumnNamesTheFailure.
//
// The column showed a bare "err" whenever there was no HTTP status -- which is
// every transport failure, and was 108 of the 110 failures in the audited
// window. A week of "err" is the state this whole change exists to leave: the
// file claims to record why requests fail and, for the largest category, did
// not.
func TestStatusColumnNamesTheFailure(t *testing.T) {
	failed := false

	cases := []struct {
		name  string
		event journal.Event
		want  string
	}{
		{
			"传输失败显示原因",
			journal.Event{OK: &failed, Cause: metrics.CauseConnect},
			"connect",
		},
		{
			"解析失败",
			journal.Event{OK: &failed, Cause: metrics.CauseDNS},
			"dns",
		},
		{
			// A status is more specific than "upstream", so it wins the column.
			"有状态码时优先显示状态码",
			journal.Event{OK: &failed, Status: 429, Cause: metrics.CauseUpstream},
			"429",
		},
		{
			// Nothing gained by printing "other" -- it says no more than "err"
			// and reads as though it were a diagnosis.
			"未知原因仍显示 err",
			journal.Event{OK: &failed, Cause: metrics.CauseOther},
			"err",
		},
		{
			"没有原因字段的旧事件",
			journal.Event{OK: &failed},
			"err",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestStatus(tc.event); got != tc.want {
				t.Errorf("requestStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSuccessStillReadsOk guards the branch the change did not touch, since a
// regression there would mislabel every successful request in the table.
func TestSuccessStillReadsOk(t *testing.T) {
	ok := true
	if got := requestStatus(journal.Event{OK: &ok}); got != "ok" {
		t.Errorf("requestStatus = %q, want \"ok\"", got)
	}
}
