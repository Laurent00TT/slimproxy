package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A configured restriction must not turn into an unrestricted running proxy.
// Test Build, not just Validate: moving validation behind materialization could
// replace another instance's effective configuration before returning the error.
func TestBuildRefusesUnenforcedModelList(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Host: "127.0.0.1", Port: 8317, APIKeys: []string{"test-model-guard-key"},
		AuthDir: filepath.Join(dir, "auths"), Models: []string{"gpt-5.5"},
		LogDir: filepath.Join(dir, "logs"),
	}
	path := filepath.Join(dir, EffectiveConfigName(cfg.Port))
	const sentinel = "existing instance configuration\n"
	if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := Build(cfg, dir)
	if rt != nil {
		t.Cleanup(func() { rt.closeRelay(); rt.closeJournal() })
	}
	if err == nil || !strings.Contains(err.Error(), "model allowlist is not enforced") {
		t.Fatalf("Build must refuse an unenforced model list, got runtime=%v err=%v", rt != nil, err)
	}
	if rt != nil {
		t.Fatal("invalid model restriction produced a runtime")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != sentinel {
		t.Fatalf("refused config changed the existing effective config: %q, %v", body, err)
	}
	if _, err := os.Stat(cfg.AuthDir); !os.IsNotExist(err) {
		t.Fatalf("refused config touched the credential directory: %v", err)
	}
}

func TestModelListEmptyAcceptedAndBlankEntryRefused(t *testing.T) {
	for _, models := range [][]string{nil, {}} {
		cfg := Config{Port: 8317, APIKeys: []string{"test-model-guard-key"}, Models: models}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("empty model list must remain valid: %v", err)
		}
	}
	cfg := Config{Port: 8317, APIKeys: []string{"test-model-guard-key"}, Models: []string{""}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "models") {
		t.Fatalf("blank entry must not silently disable the requested restriction: %v", err)
	}
}
