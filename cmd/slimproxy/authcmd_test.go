package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// authFixture writes a config and credential directory, returning the config path.
func authFixture(t *testing.T, creds map[string]time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, ttl := range creds {
		body, _ := json.Marshal(map[string]any{
			"type": "claude", "email": name + "@example.com",
			"expired": time.Now().Add(ttl).Format(time.RFC3339),
		})
		if err := os.WriteFile(filepath.Join(authDir, name+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "slimproxy.yaml")
	body := "host: \"127.0.0.1\"\nport: 8317\napi-keys: [\"k-real-not-placeholder-4471\"]\nauth-dir: \"" +
		filepath.ToSlash(authDir) + "\"\nmodels: []\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestAuthOptionOrderIsFlexible: the flag package stops at the first non-flag
// argument, so without explicit handling `auth rm NAME -config X` would treat
// the option as a second positional argument and reject a spelling every other
// CLI accepts.
func TestAuthOptionOrderIsFlexible(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"a": 4 * time.Hour, "b": 4 * time.Hour})

	for _, args := range [][]string{
		{"auth", "rm", "a", "-config", cfg, "-y"},
		{"auth", "rm", "-config", cfg, "-y", "a"},
	} {
		// Re-create the fixture per iteration: the first removal succeeds.
		cfg = authFixture(t, map[string]time.Duration{"a": 4 * time.Hour, "b": 4 * time.Hour})
		args[argIndexOf(args, cfg)] = cfg

		cx, out, _ := newTestContext()
		if err := dispatch(cx, args); err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if !strings.Contains(out.String(), "已删除") {
			t.Errorf("%v: removal not reported: %s", args, out.String())
		}
	}
}

// argIndexOf finds where the config path sits so the fixture can be swapped in.
func argIndexOf(args []string, _ string) int {
	for i, a := range args {
		if strings.HasSuffix(a, "slimproxy.yaml") {
			return i
		}
	}
	return len(args) - 1
}

// TestAuthRemoveRefusesLastUsable: the guard has to hold at the command layer,
// not only in the credentials package.
func TestAuthRemoveRefusesLastUsable(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"only": 4 * time.Hour})
	authDir := filepath.Join(filepath.Dir(cfg), "auths")

	cx, _, _ := newTestContext()
	err := dispatch(cx, []string{"auth", "rm", "only", "-config", cfg, "-y"})
	if err == nil {
		t.Fatal("removing the last usable credential was allowed")
	}
	if !strings.Contains(err.Error(), "最后一个可用凭据") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(authDir, "only.json")); statErr != nil {
		t.Error("the file was deleted despite the refusal")
	}
}

func TestAuthRemoveForceOverrides(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"only": 4 * time.Hour})
	authDir := filepath.Join(filepath.Dir(cfg), "auths")

	cx, _, _ := newTestContext()
	if err := dispatch(cx, []string{"auth", "rm", "only", "-config", cfg, "-y", "-force"}); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(authDir, "only.json")); statErr == nil {
		t.Error("-force did not delete the file")
	}
}

// TestAuthListReportsAPoolThatCannotServe.
//
// The state worth an alarm is the one where the proxy starts, accepts requests
// and fails every one of them -- healthy from outside, broken in fact.
//
// The fixture is a credential the proxy cannot load at all. It used to be an
// expired one, which is a different state: an expired access token comes with a
// refresh token and the proxy renews it on its own, so that pool works. Gating
// the alarm on Usable raised it for a deployment that was about to be fine,
// while `auth rm` in the same file has always used Recoverable for this exact
// sentence.
func TestAuthListReportsAPoolThatCannotServe(t *testing.T) {
	cfg := brokenAuthFixture(t)

	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"auth", "list", "-config", cfg}); err != nil {
		t.Fatalf("auth list: %v", err)
	}
	if !strings.Contains(out.String(), "全部失败") {
		t.Errorf("一个无法加载任何凭据的池未被告警: %s", out.String())
	}
}

// TestAuthListDoesNotAlarmOnARefreshablePool is the other side: an expired but
// refreshable pool must not be described as one where every request fails.
func TestAuthListDoesNotAlarmOnARefreshablePool(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"stale": -time.Hour})

	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"auth", "list", "-config", cfg}); err != nil {
		t.Fatalf("auth list: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "全部失败") {
		t.Errorf("过期但可刷新的凭据被报成了每个请求都会失败: %s", got)
	}
	if !strings.Contains(got, "待自动刷新") {
		t.Errorf("应说明这些凭据会被自动刷新: %s", got)
	}
}

// brokenAuthFixture writes a credential file the proxy cannot parse, which is
// one of exactly two states credentials.Recoverable treats as worthless.
func brokenAuthFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "slimproxy.yaml")
	body := "host: \"127.0.0.1\"\nport: 8317\napi-keys: [\"k-real-not-placeholder-4471\"]\nauth-dir: \"" +
		filepath.ToSlash(authDir) + "\"\nmodels: []\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAuthUnknownSubcommandIsUsageError(t *testing.T) {
	cx, _, _ := newTestContext()
	err := dispatch(cx, []string{"auth", "frobnicate"})
	if err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error does not name the offending verb: %v", err)
	}
}

func TestAuthRejectsExtraPositionalArguments(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"a": 4 * time.Hour})
	cx, _, _ := newTestContext()
	err := dispatch(cx, []string{"auth", "rm", "a", "b", "-config", cfg})
	if err == nil {
		t.Fatal("two identifiers accepted for a single removal")
	}
	if !strings.Contains(err.Error(), "无法识别") {
		t.Errorf("error is not about the extra argument: %v", err)
	}
}

// TestAuthRemoveRequiresConfirmation covers the guard that had no test at all:
// every existing removal test passed -y, so deleting the entire confirmation
// block would have left the suite green.
func TestAuthRemoveRequiresConfirmation(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"a": 4 * time.Hour, "b": 4 * time.Hour})
	authDir := filepath.Join(filepath.Dir(cfg), "auths")

	// Without -y and without a real "yes", nothing may be deleted. Which of the
	// two refusals happens depends on what stdin is -- a pipe is rejected
	// outright, a character device is prompted and then declines on EOF -- and
	// the invariant is what matters, not which branch a test binary lands in.
	cx, out, _ := newTestContext()
	err := dispatch(cx, []string{"auth", "rm", "a", "-config", cfg})
	if _, statErr := os.Stat(filepath.Join(authDir, "a.json")); statErr != nil {
		t.Fatal("the file was deleted without confirmation")
	}
	if err == nil && !strings.Contains(out.String(), "已取消") {
		t.Errorf("neither refused nor reported a cancellation: %s", out.String())
	}
	if err != nil && !strings.Contains(err.Error(), "-y") {
		t.Errorf("error does not tell scripts what to do: %v", err)
	}
}

// TestAuthRemoveForceStillRequiresConfirmation pins that -force and -y are
// orthogonal. -force answers "will this empty the pool"; -y answers "do you
// agree". Merging them would make the rm -f muscle memory delete without asking.
func TestAuthRemoveForceStillRequiresConfirmation(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"only": 4 * time.Hour})
	authDir := filepath.Join(filepath.Dir(cfg), "auths")

	cx, out, _ := newTestContext()
	err := dispatch(cx, []string{"auth", "rm", "only", "-config", cfg, "-force"})
	// The point is that -force does not imply -y: it answers "will this empty
	// the pool", not "do you agree".
	if _, statErr := os.Stat(filepath.Join(authDir, "only.json")); statErr != nil {
		t.Fatal("-force deleted the file without any confirmation")
	}
	if err == nil && !strings.Contains(out.String(), "已取消") {
		t.Errorf("-force proceeded without confirmation: %s", out.String())
	}
}

// TestAuthRemoveReportsRemainingPool: "已删除 X" alone leaves the operator to
// discover an empty pool from a failing request.
func TestAuthRemoveReportsRemainingPool(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"a": 4 * time.Hour, "b": 4 * time.Hour})

	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"auth", "rm", "a", "-config", cfg, "-y"}); err != nil {
		t.Fatalf("removal: %v", err)
	}
	if !strings.Contains(out.String(), "剩余 1 个凭据") {
		t.Errorf("the remaining pool was not reported: %s", out.String())
	}
}

// TestAuthAddValidatesProviderBeforeSideEffects: a typo used to print
// "正在为 nonesuch 启动授权流程" and create the credential directory before
// failing -- reporting an action it had not taken.
func TestAuthAddValidatesProviderBeforeSideEffects(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths-not-created-yet")
	cfg := filepath.Join(dir, "slimproxy.yaml")
	body := "host: \"127.0.0.1\"\nport: 8317\napi-keys: [\"k-real-not-placeholder-4471\"]\nauth-dir: \"" +
		filepath.ToSlash(authDir) + "\"\nmodels: []\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cx, out, _ := newTestContext()
	err := dispatch(cx, []string{"auth", "add", "nonesuch", "-config", cfg})
	if err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if strings.Contains(out.String(), "启动授权流程") {
		t.Error("announced a flow it never started")
	}
	if _, statErr := os.Stat(authDir); statErr == nil {
		t.Error("created the credential directory for an invalid provider")
	}
}

// TestAuthRemoveBrokenCredential: cleaning up a file the proxy cannot load is
// one of the operations an operator most needs, and it must not be blocked by
// the last-credential guard.
func TestAuthRemoveBrokenCredential(t *testing.T) {
	cfg := authFixture(t, nil)
	authDir := filepath.Join(filepath.Dir(cfg), "auths")
	if err := os.WriteFile(filepath.Join(authDir, "broken.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"auth", "rm", "broken.json", "-config", cfg, "-y"}); err != nil {
		t.Fatalf("removing a broken credential was refused: %v", err)
	}
	if !strings.Contains(out.String(), "已删除") {
		t.Errorf("removal not reported: %s", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(authDir, "broken.json")); statErr == nil {
		t.Error("the broken file was not deleted")
	}
}

// TestAuthOptionOrderInterleaved covers the spelling the first implementation
// rejected: an option, then the positional argument, then more options.
func TestAuthOptionOrderInterleaved(t *testing.T) {
	cfg := authFixture(t, map[string]time.Duration{"a": 4 * time.Hour, "b": 4 * time.Hour})
	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"auth", "rm", "-y", "a", "-config", cfg}); err != nil {
		t.Fatalf("interleaved options rejected: %v", err)
	}
	if !strings.Contains(out.String(), "已删除") {
		t.Errorf("removal not reported: %s", out.String())
	}
}
