package main

import (
	"os"
	"strings"
	"testing"
)

// Both command forms must reject the list before printing "config OK".
// This exercises strict YAML loading as well as the operator-facing check.
func TestCheckRefusesUnenforcedModelList(t *testing.T) {
	path := writeTestConfig(t)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "models: []", "models: [gpt-5.5]", 1))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"check", "-config", path},
		{"-check", "-config", path},
	} {
		cx, out, _ := newTestContext()
		err := dispatch(cx, args)
		if err == nil || !strings.Contains(err.Error(), "model allowlist is not enforced") {
			t.Fatalf("%v: expected unsupported model list error, got %v", args, err)
		}
		if out.Len() != 0 {
			t.Fatalf("%v: invalid config produced a success summary: %s", args, out.String())
		}
	}
}
