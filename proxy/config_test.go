package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cliproxyconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestMaterializedConfigRoundTrips is the regression test for the reload path.
//
// slimproxy hands CLIProxyAPI a materialized config file and the file watcher
// re-reads THAT PATH on every change. If LoadConfig rejects what materialize
// wrote, hot reload dies inside the watcher and logs to logrus only -- the
// process keeps serving the config it booted with and nothing surfaces.
//
// So this asserts the one property the whole arrangement depends on: what we
// write, CLIProxyAPI can read.
func TestMaterializedConfigRoundTrips(t *testing.T) {
	dir := t.TempDir()
	c := Config{
		Host:         "127.0.0.1",
		Port:         8317,
		APIKeys:      []string{"k"},
		AuthDir:      filepath.Join(dir, "auths"),
		RequestRetry: 3,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	path, err := materialize(c.build(), dir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	}

	got, err := cliproxyconfig.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig rejected our own materialized config: %v\n"+
			"hot reload is dead: internal/watcher/config_reload.go bails on this error "+
			"and only logs it, so config changes silently never apply", err)
	}

	// The values we own must survive the trip.
	if got.Port != 8317 {
		t.Errorf("port: want 8317, got %d", got.Port)
	}
	if len(got.APIKeys) != 1 {
		t.Errorf("api-keys: want 1, got %d", len(got.APIKeys))
	}
	if got.RemoteManagement.SecretKey != "" {
		t.Error("management secret survived materialization; hardening lost")
	}
	if !got.RemoteManagement.DisableControlPanel {
		t.Error("control panel re-enabled by the round trip")
	}
	if got.Plugins.Enabled {
		t.Error("plugin host re-enabled by the round trip")
	}
	if got.Pprof.Enable {
		t.Error("pprof re-enabled by the round trip")
	}
}

// TestClaudeCodeCacheTTLReachesEngine pins the knob's whole delivery path:
// Config -> build() -> the fork's ClaudeCodeConfig -> the materialized file
// CLIProxyAPI re-reads on reload. The executor reads the value from the last
// of those (SLIMPROXY_PATCHES.md 第 10-12 条), so a knob that stops short of
// it is a knob that silently does nothing -- and a renamed yaml tag on either
// side would stop it exactly there.
func TestClaudeCodeCacheTTLReachesEngine(t *testing.T) {
	dir := t.TempDir()
	c := Config{
		Host:               "127.0.0.1",
		Port:               8317,
		APIKeys:            []string{"k"},
		AuthDir:            filepath.Join(dir, "auths"),
		ClaudeCodeCacheTTL: " 1h ",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := c.build().ClaudeCode.CacheTTL; got != "1h" {
		t.Fatalf("build() handed the engine claude-code.cache-ttl %q, want 1h (trimmed)", got)
	}

	path, err := materialize(c.build(), dir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	got, err := cliproxyconfig.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig rejected the materialized config: %v", err)
	}
	if got.ClaudeCode.CacheTTL != "1h" {
		t.Fatalf("claude-code.cache-ttl did not survive the round trip: %q", got.ClaudeCode.CacheTTL)
	}

	// Unset must stay unset: the executor treats "" as "send as written",
	// and a default sneaking in here would rewrite every client's breakpoints.
	c.ClaudeCodeCacheTTL = ""
	if got := c.build().ClaudeCode.CacheTTL; got != "" {
		t.Fatalf("an empty knob reached the engine as %q", got)
	}
}

// TestClaudeCodeCacheTTLValidated: the engine forwards the value verbatim and
// Anthropic would reject every Claude Code request at run time, so anything
// but the two lifetimes that exist has to die at startup.
func TestClaudeCodeCacheTTLValidated(t *testing.T) {
	base := Config{Port: 8317, APIKeys: []string{"k"}}
	for _, ok := range []string{"", "5m", "1h", " 1h"} {
		c := base
		c.ClaudeCodeCacheTTL = ok
		if err := c.Validate(); err != nil {
			t.Errorf("claude-code-cache-ttl %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"2h", "60m", "1H", "true"} {
		c := base
		c.ClaudeCodeCacheTTL = bad
		err := c.Validate()
		if err == nil {
			t.Errorf("claude-code-cache-ttl %q accepted; Anthropic would refuse it on every request", bad)
			continue
		}
		if !strings.Contains(err.Error(), "claude-code-cache-ttl") {
			t.Errorf("error for %q does not name the knob: %v", bad, err)
		}
	}
}

// TestManagementPasswordEnvIsRejected guards the gap between what slimproxy
// claims and what CLIProxyAPI does.
//
// internal/api/server.go:331-333 reads MANAGEMENT_PASSWORD from the
// environment; :400 ORs it into hasManagementSecret and registers the entire
// management route set, and :2019 re-asserts that on every reload. Pinning
// cfg.RemoteManagement.SecretKey to "" does not stop it. Since slimproxy's
// whole claim is that the management API is gone, an exported
// MANAGEMENT_PASSWORD has to be a startup error, not a silent override.
func TestManagementPasswordEnvIsRejected(t *testing.T) {
	base := Config{Port: 8317, APIKeys: []string{"k"}}

	t.Setenv("MANAGEMENT_PASSWORD", "")
	if err := base.Validate(); err != nil {
		t.Errorf("empty MANAGEMENT_PASSWORD must not fail validation: %v", err)
	}

	t.Setenv("MANAGEMENT_PASSWORD", "hunter2")
	err := base.Validate()
	if err == nil {
		t.Fatal("MANAGEMENT_PASSWORD is set: it re-enables the full management API " +
			"(internal/api/server.go:400-404) that this proxy claims to disable, so startup must refuse")
	}
	if !contains(err.Error(), "MANAGEMENT_PASSWORD") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (hay == needle || len(needle) == 0 ||
		func() bool {
			for i := 0; i+len(needle) <= len(hay); i++ {
				if hay[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}())
}

// TestPlaceholderAPIKeysRejected guards the highest-exposure gap the audit
// found: CLIProxyAPI refuses to serve a single request when it sees its own
// example keys, but that safe mode is an internal server option with no sdk/api
// wrapper, so an SDK embedder inherits none of it.
func TestPlaceholderAPIKeysRejected(t *testing.T) {
	for _, key := range []string{"change-me-to-something-random", "your-api-key-1", "changeme"} {
		c := Config{Port: 8317, APIKeys: []string{key}}
		err := c.Validate()
		if err == nil {
			t.Errorf("placeholder %q accepted: this would be a live relay guarded by a key published in a public repo", key)
			continue
		}
		if !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("%q: error should say placeholder, got %v", key, err)
		}
	}
	// A real key must still pass.
	if err := (&Config{Port: 8317, APIKeys: []string{"9f3a2b7c1d"}}).Validate(); err != nil {
		t.Errorf("real key rejected: %v", err)
	}
}

// TestAuthDirExpandsHome pins the fix for the most likely migration failure:
// ~/.cli-proxy-api is what CLIProxyAPI defaults to and writes logins into, and
// without expansion it became a literal directory named "~".
func TestAuthDirExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	c := Config{Port: 8317, APIKeys: []string{"k"}, AuthDir: "~/.cli-proxy-api"}
	got := c.build().AuthDir

	if strings.Contains(got, "~") {
		t.Errorf("auth-dir still contains a literal ~: %s", got)
	}
	if want := filepath.Join(home, ".cli-proxy-api"); got != want {
		t.Errorf("auth-dir\n got %s\nwant %s", got, want)
	}
}

// TestStorageBackendEnvRefused: those variables select a Postgres/git/object
// store in CLIProxyAPI's binary and are unreadable through its SDK, so leaving
// them set means slimproxy silently loads zero credentials from ./auths.
func TestStorageBackendEnvRefused(t *testing.T) {
	base := Config{Port: 8317, APIKeys: []string{"k"}}
	if err := base.Validate(); err != nil {
		t.Fatalf("baseline should pass: %v", err)
	}
	for _, name := range []string{"PGSTORE_DSN", "GITSTORE_GIT_URL", "OBJECTSTORE_BUCKET"} {
		t.Setenv(name, "something")
		err := base.Validate()
		if err == nil {
			t.Errorf("%s set but startup allowed: credentials would silently not load", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error should name the variable, got %v", name, err)
		}
		t.Setenv(name, "")
	}
}

// TestAntigravityCreditsDefaultOn: the ParseConfigBytes seed leaves this false,
// which disables the credits fallback once free-tier credentials are exhausted.
// config.example.yaml ships it true.
func TestAntigravityCreditsDefaultOn(t *testing.T) {
	c := Config{Port: 8317, APIKeys: []string{"k"}}
	if !c.build().QuotaExceeded.AntigravityCredits {
		t.Error("antigravity credits fallback is off; exhausted free-tier credentials will fail instead of falling back")
	}
}
