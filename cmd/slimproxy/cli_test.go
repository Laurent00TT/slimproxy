package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestContext returns a context whose output is captured.
func newTestContext() (*cliContext, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &cliContext{
		configPath: defaultConfigPath,
		stateDir:   ".",
		stdout:     &out,
		stderr:     &errb,
	}, &out, &errb
}

// writeTestConfig writes a minimal valid config and returns its path.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "slimproxy.yaml")
	body := `host: "127.0.0.1"
port: 8317
api-keys:
  - "test-key-not-a-placeholder-9f3a"
allow-unauthenticated: false
auth-dir: "` + filepath.ToSlash(filepath.Join(dir, "auths")) + `"
proxy-url: ""
request-retry: 3
max-retry-interval: 30
max-retry-credentials: 0
debug: false
request-log: false
log-to-file: false
models: []
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestBothCallingFormsAgree is the point of keeping the old flags: an existing
// script must get byte-identical behaviour from `-check` and `check`. If these
// ever diverge it means a second implementation crept in.
func TestBothCallingFormsAgree(t *testing.T) {
	path := writeTestConfig(t)
	state := t.TempDir()

	cxOld, outOld, _ := newTestContext()
	if err := dispatch(cxOld, []string{"-config", path, "-state", state, "-check"}); err != nil {
		t.Fatalf("legacy form failed: %v", err)
	}

	cxNew, outNew, _ := newTestContext()
	if err := dispatch(cxNew, []string{"check", "-config", path, "-state", state}); err != nil {
		t.Fatalf("subcommand form failed: %v", err)
	}

	if outOld.String() != outNew.String() {
		t.Errorf("the two calling forms produced different output.\nlegacy:\n%s\nsubcommand:\n%s",
			outOld.String(), outNew.String())
	}
	if !strings.Contains(outNew.String(), "配置 OK") {
		t.Errorf("check did not report success: %s", outNew.String())
	}
}

// TestLegacyFlagOrderIndependence pins that the selector may appear before or
// after its options, which the original interface allowed.
func TestLegacyFlagOrderIndependence(t *testing.T) {
	path := writeTestConfig(t)
	state := t.TempDir()

	cxA, outA, _ := newTestContext()
	if err := dispatch(cxA, []string{"-check", "-config", path, "-state", state}); err != nil {
		t.Fatalf("selector first: %v", err)
	}
	cxB, outB, _ := newTestContext()
	if err := dispatch(cxB, []string{"-config", path, "-state", state, "-check"}); err != nil {
		t.Fatalf("selector last: %v", err)
	}
	if outA.String() != outB.String() {
		t.Error("flag order changed the result")
	}
}

// TestPortOverrideReachesBothForms: -port is the one option that mutates the
// loaded config rather than just locating it, so it is the most likely to be
// dropped when the argument list is rebuilt for a subcommand.
func TestPortOverrideReachesBothForms(t *testing.T) {
	path := writeTestConfig(t)
	state := t.TempDir()

	for _, args := range [][]string{
		{"-config", path, "-state", state, "-port", "9999", "-check"},
		{"check", "-config", path, "-state", state, "-port", "9999"},
	} {
		cx, out, _ := newTestContext()
		if err := dispatch(cx, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), ":9999") {
			t.Errorf("%v: port override was lost, output was:\n%s", args, out.String())
		}
	}
}

func TestSubcommandRoutes(t *testing.T) {
	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"routes"}); err != nil {
		t.Fatalf("routes: %v", err)
	}
	if !strings.Contains(out.String(), "客户端协议") {
		t.Errorf("routes produced no table: %s", out.String())
	}

	cxLegacy, outLegacy, _ := newTestContext()
	if err := dispatch(cxLegacy, []string{"-routes"}); err != nil {
		t.Fatalf("-routes: %v", err)
	}
	if outLegacy.String() != out.String() {
		t.Error("routes and -routes disagree")
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	cx, _, _ := newTestContext()
	err := dispatch(cx, []string{"tunnl"})
	if err == nil {
		t.Fatal("unknown command was accepted")
	}
	// Classify, don't pattern-match: main decides the exit code from this.
	if !errors.Is(err, errUsage) {
		t.Errorf("not classified as a usage error, so it would exit 1 instead of 2: %v", err)
	}
	if !strings.Contains(err.Error(), "tunnl") {
		t.Errorf("error does not name the offending command: %v", err)
	}
	// Assert on a summary rather than a command name: names also appear in
	// commandList's static prose ("旧式写法（-check / -init / -routes）"), so
	// asserting on a name passes even if the registry loop is deleted entirely.
	if !strings.Contains(err.Error(), lookup("routes").summary) {
		t.Errorf("error does not list available commands: %v", err)
	}
}

func TestHelpListsEveryRegisteredCommand(t *testing.T) {
	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"help"}); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, c := range commands {
		// Summary, not name -- see TestUnknownCommandIsUsageError.
		if !strings.Contains(out.String(), c.summary) {
			t.Errorf("help omits command %q", c.name)
		}
	}
}

// TestLeftoverArgumentsAreRejected covers the worst failure this CLI can have:
// flag.Parse stops at the first non-flag argument and leaves the rest in Args().
// Unchecked, `slimproxy -config prod.yaml check` sets no selector, falls through
// to serve, and BINDS A PORT with production credentials on a command the user
// meant as a dry run.
func TestLeftoverArgumentsAreRejected(t *testing.T) {
	path := writeTestConfig(t)
	cases := [][]string{
		{"-config", path, "check"},       // subcommand after options
		{"-config", path, "init"},        // same, for a destructive command
		{"-check", "garbage"},            // stray word
		{"check", "--", "-config", path}, // everything after --
		{"routes", "extra"},
	}
	for _, args := range cases {
		cx, out, _ := newTestContext()
		err := dispatch(cx, args)
		if err == nil {
			t.Errorf("%v: accepted, output was:\n%s", args, out.String())
			continue
		}
		if !errors.Is(err, errUsage) {
			t.Errorf("%v: not a usage error: %v", args, err)
		}
		if strings.Contains(out.String(), "正在监听") {
			t.Errorf("%v: STARTED SERVING instead of reporting a usage error", args)
		}
	}
}

// TestConflictingSelectorsAreRejected: picking the first of two selectors runs
// `-check -init` as init, which writes a config file and exits 0 when the user
// asked to validate one.
func TestConflictingSelectorsAreRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "should-not-exist.yaml")
	for _, args := range [][]string{
		{"-check", "-init", "-config", cfg},
		{"-check", "-routes"},
		{"-init", "-routes", "-config", cfg},
	} {
		cx, _, _ := newTestContext()
		err := dispatch(cx, args)
		if err == nil {
			t.Errorf("%v: accepted two selectors", args)
		} else if !errors.Is(err, errUsage) {
			t.Errorf("%v: not a usage error: %v", args, err)
		}
		if _, statErr := os.Stat(cfg); statErr == nil {
			t.Errorf("%v: wrote a config file while reporting a conflict", args)
			_ = os.Remove(cfg)
		}
	}
}

// TestBadFlagIsUsageErrorInBothForms pins the exit code. The original binary
// used flag.ExitOnError, which exits 2 on a bad flag; a form that exits 1
// instead is an observable regression for any script checking the status.
func TestBadFlagIsUsageErrorInBothForms(t *testing.T) {
	for _, args := range [][]string{
		{"-bogus"},
		{"check", "-bogus"},
		{"-port", "not-a-number", "-check"},
	} {
		cx, _, _ := newTestContext()
		err := dispatch(cx, args)
		if err == nil {
			t.Errorf("%v: bad flag accepted", args)
			continue
		}
		if !errors.Is(err, errUsage) {
			t.Errorf("%v: would exit 1, want usage error (exit 2): %v", args, err)
		}
	}
}

// TestHelpIsPrintedOnce guards against the flag package and this package both
// writing usage: the package prints from inside Parse before returning ErrHelp.
func TestHelpIsPrintedOnce(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"check", "-h"}} {
		cx, out, errb := newTestContext()
		if err := dispatch(cx, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		combined := out.String() + errb.String()
		if n := strings.Count(combined, "用法:"); n > 1 {
			t.Errorf("%v: usage printed %d times", args, n)
		}
		if out.Len() == 0 {
			t.Errorf("%v: -h wrote nothing to stdout", args)
		}
	}
}

func TestHelpForOneCommand(t *testing.T) {
	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"help", "check"}); err != nil {
		t.Fatalf("help check: %v", err)
	}
	if !strings.Contains(out.String(), "slimproxy check") {
		t.Errorf("help check did not print its usage line: %s", out.String())
	}
}

// TestInitBothForms exercises the one command whose options do not overlap with
// the common set, so its argument rebuild is the easiest to get wrong.
func TestInitBothForms(t *testing.T) {
	for _, form := range []string{"legacy", "subcommand"} {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "gen.yaml")
		auth := filepath.Join(dir, "custom-auth-x1")

		var args []string
		if form == "legacy" {
			args = []string{"-init", "-config", cfg, "-init-auth-dir", auth}
		} else {
			args = []string{"init", "-config", cfg, "-init-auth-dir", auth}
		}

		cx, _, _ := newTestContext()
		if err := dispatch(cx, args); err != nil {
			t.Fatalf("%s init: %v", form, err)
		}
		body, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatalf("%s: config not written: %v", form, err)
		}
		if !strings.Contains(string(body), "api-keys:") {
			t.Errorf("%s: generated config has no api-keys", form)
		}
		// A distinctive directory name, not "auths": the old assertion used the
		// basename, which equalled the flag's default, so it passed even when
		// the value was dropped entirely. Matching on the path as written is
		// unreliable here because YAML escapes Windows backslashes.
		if !strings.Contains(string(body), "custom-auth-x1") {
			t.Errorf("%s: -init-auth-dir was ignored; config says:\n%s", form, body)
		}
	}
}

// TestInitRefusesOverwriteWithoutForce guards a destructive path: the config
// holds generated keys, and silently replacing it would invalidate every client
// already configured against them.
func TestInitRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gen.yaml")
	auth := filepath.Join(dir, "auths")

	cx, _, _ := newTestContext()
	if err := dispatch(cx, []string{"init", "-config", cfg, "-init-auth-dir", auth}); err != nil {
		t.Fatalf("first init: %v", err)
	}
	first, _ := os.ReadFile(cfg)

	cx2, _, _ := newTestContext()
	if err := dispatch(cx2, []string{"init", "-config", cfg, "-init-auth-dir", auth}); err == nil {
		t.Error("second init overwrote an existing config without -force")
	}
	after, _ := os.ReadFile(cfg)
	if string(first) != string(after) {
		t.Error("the refused init still modified the file")
	}

	cx3, _, _ := newTestContext()
	if err := dispatch(cx3, []string{"init", "-config", cfg, "-init-auth-dir", auth, "-force"}); err != nil {
		t.Fatalf("forced init: %v", err)
	}
	forced, _ := os.ReadFile(cfg)
	if string(forced) == string(first) {
		t.Error("-force did not regenerate the config (keys should differ)")
	}
}
