package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestNoTUIAcceptedInLegacyForm: `slimproxy -no-tui` is the form the guide
// gives service managers, and the flag-only dispatcher rejected it as
// undefined while forwarding bare -port happily -- a process started that way
// exited 2 before reading its config, which under a hidden window or a
// scheduler looks like nothing at all.
//
// Observed through the first thing serve does that fails fast without a
// terminal: binding the port. A busy port turns serve into an immediate,
// recognisable error; a usage error in its place is the regression. What
// this cannot observe is the flag reaching serve's own parser -- that only
// changes behaviour on a terminal, which a test does not have -- so it pins
// acceptance, and the forwarding line beside it is covered by reading.
func TestNoTUIAcceptedInLegacyForm(t *testing.T) {
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })
	path := writeTestConfig(t)
	state := t.TempDir()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cx, _, _ := newTestContext()
	err = dispatch(cx, []string{"-no-tui", "-config", path, "-state", state, "-port", fmt.Sprint(port)})
	if err == nil {
		t.Fatal("serve on a busy port returned nil")
	}
	if errors.Is(err, errUsage) {
		t.Fatalf("bare -no-tui was rejected as a usage error; the flag-only form does not accept serve's option: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(port)) {
		t.Fatalf("expected the bind failure on port %d, got: %v", port, err)
	}
}

// TestNoTUIReportedAsUnusedOutsideServe: under a selector the flag does
// nothing, and this package's rule is that a flag which quietly fails to
// apply is worse than one that is rejected -- so it is named on stderr, the
// way -port is under -init.
func TestNoTUIReportedAsUnusedOutsideServe(t *testing.T) {
	path := writeTestConfig(t)
	state := t.TempDir()

	cx, _, errb := newTestContext()
	if err := dispatch(cx, []string{"-no-tui", "-config", path, "-state", state, "-check"}); err != nil {
		t.Fatalf("-check with -no-tui: %v", err)
	}
	if !strings.Contains(errb.String(), "-no-tui") {
		t.Errorf("-no-tui under -check was not reported as ignored; stderr:\n%s", errb.String())
	}
}
