package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestShutdownError pins that a planned stop exits zero.
//
// Before this, Service.Run's unconditional ctx.Err() propagated straight to
// os.Exit(1): every Ctrl-C, systemctl stop and pod termination looked like a
// crash to its supervisor. CLIProxyAPI's own binary swallows it; slimproxy did
// not, so the divergence hit 100% of deployments 100% of the time.
func TestShutdownError(t *testing.T) {
	if got := shutdownError(nil); got != nil {
		t.Errorf("nil should stay nil, got %v", got)
	}
	if got := shutdownError(context.Canceled); got != nil {
		t.Errorf("planned shutdown must exit 0, got %v", got)
	}
	// Wrapped, the way Run may return it.
	if got := shutdownError(fmt.Errorf("server stopped: %w", context.Canceled)); got != nil {
		t.Errorf("wrapped cancellation must exit 0, got %v", got)
	}
	// A missed shutdown deadline is a genuine failure and must survive.
	if got := shutdownError(context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("deadline exceeded must propagate, got %v", got)
	}
	boom := errors.New("bind: address already in use")
	if got := shutdownError(boom); !errors.Is(got, boom) {
		t.Errorf("real errors must propagate, got %v", got)
	}
}
