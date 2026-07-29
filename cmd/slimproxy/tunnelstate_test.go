package main

import (
	"errors"
	"testing"

	"github.com/Laurent00TT/slimproxy/tunnel"
)

// TestTunnelStateReportsWhatHappened pins the fix for an event that recorded
// the intent instead of the outcome.
//
// Both journal call sites hardcoded State to the operation being attempted, so
// a failed "up" was written as State:"up" with Detail:"失败: 隧道已在运行".
// Two of the three tunnel events in the first three days of running were that
// contradiction -- and the timeline is read precisely when someone is trying to
// work out whether the tunnel was up during an outage.
func TestTunnelStateReportsWhatHappened(t *testing.T) {
	boom := errors.New("隧道已在运行")

	cases := []struct {
		name string
		st   tunnel.Status
		err  error
		want string
	}{
		// The real case from the journal: Up refused because a tunnel was
		// already running. The operation failed; the tunnel is up. Reporting
		// "failed" here would be as wrong as reporting "up" unconditionally.
		{"up refused because already running", tunnel.Status{State: tunnel.Running}, boom, "up"},
		{"up succeeded", tunnel.Status{State: tunnel.Running}, nil, "up"},
		{"down succeeded", tunnel.Status{State: tunnel.Configured}, nil, "down"},
		{"down on an unconfigured host", tunnel.Status{State: tunnel.NotConfigured}, nil, "down"},
		// Status unreadable and the operation failed: the only case where
		// there is nothing to report but the failure itself.
		{"state unknown after a failure", tunnel.Status{State: tunnel.StateUnknown}, boom, "failed"},
		{"state unknown without a failure", tunnel.Status{State: tunnel.StateUnknown}, nil, "unknown"},
	}

	for _, c := range cases {
		if got := tunnelState(c.st, c.err); got != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}
}
