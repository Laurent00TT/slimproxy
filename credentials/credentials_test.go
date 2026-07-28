package credentials

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// write creates a credential file. expiresIn of zero means no expiry recorded.
func write(t *testing.T, dir, name, provider, email string, expiresIn time.Duration, disabled bool) {
	t.Helper()
	raw := map[string]any{"type": provider, "email": email, "disabled": disabled}
	if expiresIn != 0 {
		raw["expired"] = now.Add(expiresIn).Format(time.RFC3339)
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStatusClassification(t *testing.T) {
	cases := []struct {
		name string
		c    Credential
		want Status
	}{
		{"valid", Credential{Expires: now.Add(4 * time.Hour)}, StatusOK},
		{"expiring soon", Credential{Expires: now.Add(10 * time.Minute)}, StatusExpiring},
		{"expired", Credential{Expires: now.Add(-time.Minute)}, StatusExpired},
		{"disabled beats expiry", Credential{Expires: now.Add(4 * time.Hour), Disabled: true}, StatusDisabled},
		{"broken beats everything", Credential{Expires: now.Add(4 * time.Hour), Err: errors.New("x")}, StatusBroken},
		// No expiry recorded: reporting "usable" would assert something the
		// file never said.
		{"no expiry is unknown", Credential{}, StatusUnknown},
	}
	for _, c := range cases {
		if got := c.c.Status(now); got != c.want {
			t.Errorf("%s: Status = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestUsableExcludesExpiredAndDisabled(t *testing.T) {
	usable := []Credential{
		{Expires: now.Add(4 * time.Hour)},
		{Expires: now.Add(10 * time.Minute)}, // expiring is still usable
	}
	for _, c := range usable {
		if !c.Usable(now) {
			t.Errorf("%v should be usable", c.Status(now))
		}
	}
	unusable := []Credential{
		{Expires: now.Add(-time.Minute)},
		{Expires: now.Add(4 * time.Hour), Disabled: true},
		{Err: errors.New("broken")},
		{}, // unknown expiry
	}
	for _, c := range unusable {
		if c.Usable(now) {
			t.Errorf("%v should not count as usable", c.Status(now))
		}
	}
}

// TestListSurfacesBrokenFiles: a file the proxy will fail to load is exactly
// what an operator needs to see, so it is listed with Err rather than skipped.
func TestListSurfacesBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "good.json", "claude", "a@example.com", 4*time.Hour, false)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Non-JSON files are not credentials and must not be reported at all.
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(creds) != 2 {
		t.Fatalf("got %d credentials, want 2 (the broken one included)", len(creds))
	}
	var sawBroken bool
	for _, c := range creds {
		if c.Name == "broken.json" {
			sawBroken = true
			if c.Err == nil {
				t.Error("an unparseable file was reported without an error")
			}
			if c.Status(now) != StatusBroken {
				t.Errorf("broken file classified as %v", c.Status(now))
			}
		}
		if c.Name == "README.txt" {
			t.Error("a non-JSON file was listed as a credential")
		}
	}
	if !sawBroken {
		t.Error("the broken file was silently skipped")
	}
}

func TestListOnMissingDirectory(t *testing.T) {
	if _, err := List(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing directory reported success")
	}
}

func TestFindAcceptsSeveralIdentifiers(t *testing.T) {
	creds := []Credential{
		{Name: "claude-a@example.com.json", Account: "a@example.com"},
		{Name: "codex-b@example.com.json", Account: "b@example.com"},
	}
	for _, id := range []string{
		"claude-a@example.com.json",
		"claude-a@example.com",
		"a@example.com",
		"A@EXAMPLE.COM", // case-insensitive
	} {
		got, err := Find(creds, id)
		if err != nil {
			t.Errorf("Find(%q): %v", id, err)
			continue
		}
		if got.Account != "a@example.com" {
			t.Errorf("Find(%q) resolved to %q", id, got.Account)
		}
	}

	if _, err := Find(creds, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id gave %v, want ErrNotFound", err)
	}
	if _, err := Find(creds, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty id gave %v, want ErrNotFound", err)
	}
}

// TestFindRejectsAmbiguity: this lookup feeds a deletion, and guessing there
// removes the wrong file.
func TestFindRejectsAmbiguity(t *testing.T) {
	creds := []Credential{
		{Name: "shared.json", Account: "dup@example.com"},
		{Name: "other.json", Account: "dup@example.com"},
	}
	_, err := Find(creds, "dup@example.com")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous id gave %v, want ErrAmbiguous", err)
	}
	// The message must name the candidates, or the operator cannot disambiguate.
	if !strings.Contains(err.Error(), "shared.json") || !strings.Contains(err.Error(), "other.json") {
		t.Errorf("error does not list the candidates: %v", err)
	}
}

// TestRemoveRefusesLastUsable guards the state where the proxy starts, accepts
// requests, and fails every one of them upstream -- broken while looking
// healthy from outside.
func TestRemoveRefusesLastUsable(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "only.json", "claude", "a@example.com", 4*time.Hour, false)

	_, err := Remove(dir, "a@example.com", false, now)
	if !errors.Is(err, ErrLastUsable) {
		t.Fatalf("Remove gave %v, want ErrLastUsable", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "only.json")); statErr != nil {
		t.Error("the file was deleted despite the refusal")
	}
}

func TestRemoveForceOverridesTheGuard(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "only.json", "claude", "a@example.com", 4*time.Hour, false)

	if _, err := Remove(dir, "a@example.com", true, now); err != nil {
		t.Fatalf("forced Remove: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "only.json")); statErr == nil {
		t.Error("-force did not delete the file")
	}
}

// TestRemoveAllowsWhenAnotherUsableRemains: the guard is about the last usable
// credential, not about deletion in general.
func TestRemoveAllowsWhenAnotherUsableRemains(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.json", "claude", "a@example.com", 4*time.Hour, false)
	write(t, dir, "b.json", "claude", "b@example.com", 4*time.Hour, false)

	if _, err := Remove(dir, "a@example.com", false, now); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a.json")); statErr == nil {
		t.Error("the file was not deleted")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "b.json")); statErr != nil {
		t.Error("the wrong file was deleted")
	}
}

// TestGuardCountsRecoverableNotUsable pins the distinction that made the guard
// fail: an expired credential still carries a refresh token the proxy renews on
// its own, so it is worth keeping even though it cannot serve a request right
// now. Only a broken or disabled one is genuinely worthless.
func TestGuardCountsRecoverableNotUsable(t *testing.T) {
	// An expired sibling saves the deletion: the pool is not left empty.
	dir := t.TempDir()
	write(t, dir, "good.json", "claude", "a@example.com", 4*time.Hour, false)
	write(t, dir, "expired.json", "claude", "b@example.com", -time.Hour, false)
	if _, err := Remove(dir, "a@example.com", false, now); err != nil {
		t.Errorf("an expired-but-refreshable sibling should have allowed this: %v", err)
	}

	// A disabled sibling does not: the proxy was told not to use it.
	dir = t.TempDir()
	write(t, dir, "good.json", "claude", "a@example.com", 4*time.Hour, false)
	write(t, dir, "off.json", "claude", "c@example.com", 4*time.Hour, true)
	if _, err := Remove(dir, "a@example.com", false, now); !errors.Is(err, ErrLastUsable) {
		t.Errorf("a disabled sibling must not count as a survivor, got %v", err)
	}

	// Nor does a broken one: the proxy cannot load it at all.
	dir = t.TempDir()
	write(t, dir, "good.json", "claude", "a@example.com", 4*time.Hour, false)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(dir, "a@example.com", false, now); !errors.Is(err, ErrLastUsable) {
		t.Errorf("a broken sibling must not count as a survivor, got %v", err)
	}
}

// TestGuardProtectsExpiredLastCredential is the case the old rule got wrong.
// A pool of one expired credential counted as zero usable, so the guard saw
// nothing to protect and the deletion went through -- leaving a proxy that
// accepts requests and fails all of them.
func TestGuardProtectsExpiredLastCredential(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "expired.json", "claude", "b@example.com", -time.Hour, false)

	if _, err := Remove(dir, "b@example.com", false, now); !errors.Is(err, ErrLastUsable) {
		t.Errorf("deleting the last (refreshable) credential was allowed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "expired.json")); statErr != nil {
		t.Error("the file was deleted despite the refusal")
	}
}

// TestGuardProtectsCredentialWithNoRecordedExpiry: a file that never said when
// it expires is a reason for caution, not a licence to delete. Measured before
// the fix: this deletion succeeded silently, exit code 0, pool emptied.
func TestGuardProtectsCredentialWithNoRecordedExpiry(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "nostamp.json", "claude", "a@example.com", 0, false)

	if _, err := Remove(dir, "a@example.com", false, now); !errors.Is(err, ErrLastUsable) {
		t.Errorf("deleting the last credential (unknown expiry) was allowed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "nostamp.json")); statErr != nil {
		t.Error("the file was deleted despite the refusal")
	}
}

// TestRemoveBrokenLastCredentialIsAllowed: a file the proxy cannot load is not
// protecting anything, and removing it is how an operator cleans up.
func TestRemoveBrokenLastCredentialIsAllowed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(dir, "broken.json", false, now); err != nil {
		t.Errorf("removing an unloadable credential was refused: %v", err)
	}
}

func TestRecoverableClassification(t *testing.T) {
	cases := []struct {
		name string
		c    Credential
		want bool
	}{
		{"valid", Credential{Expires: now.Add(4 * time.Hour)}, true},
		{"expired but refreshable", Credential{Expires: now.Add(-time.Hour)}, true},
		{"no recorded expiry", Credential{}, true},
		{"disabled", Credential{Expires: now.Add(4 * time.Hour), Disabled: true}, false},
		{"broken", Credential{Err: errors.New("x")}, false},
	}
	for _, c := range cases {
		if got := c.c.Recoverable(now); got != c.want {
			t.Errorf("%s: Recoverable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRemoveUnknownIdentifier(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.json", "claude", "a@example.com", 4*time.Hour, false)
	if _, err := Remove(dir, "nobody", false, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remove gave %v, want ErrNotFound", err)
	}
}

func TestParseTimeLayouts(t *testing.T) {
	// The layout these files were observed to use.
	if got := parseTime("2026-07-26T19:45:33+08:00"); got.IsZero() {
		t.Error("the observed RFC3339 layout was not parsed")
	}
	// Anything unrecognised yields the zero time, which Status reports as
	// unknown rather than guessing a validity.
	for _, s := range []string{"", "  ", "not a time", "26/07/2026"} {
		if got := parseTime(s); !got.IsZero() {
			t.Errorf("parseTime(%q) = %v, want zero", s, got)
		}
	}
}

func TestProviderRegistryIsConsistent(t *testing.T) {
	names := ProviderNames()
	if len(names) == 0 {
		t.Fatal("no providers registered")
	}
	for _, n := range names {
		if !KnownProvider(n) {
			t.Errorf("%q is listed but not recognised", n)
		}
		if !KnownProvider(strings.ToUpper(n)) {
			t.Errorf("%q not matched case-insensitively", n)
		}
	}
	if KnownProvider("nonesuch") {
		t.Error("an unknown provider was accepted")
	}
}

func TestLoginRejectsUnknownProviderBeforeDoingAnything(t *testing.T) {
	_, err := Login(nil, LoginRequest{Provider: "nonesuch", AuthDir: t.TempDir(),
		Prompt: func(string) (string, error) { return "", nil }})
	if err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	// The message must list what is available.
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error does not list the available providers: %v", err)
	}
}

// TestLoginRequiresAPrompt: an authenticator that needs input and has no way to
// ask would block forever, which looks like a hang rather than a mistake.
//
// The assertion names the cause rather than just demanding an error: without a
// prompt the call fails anyway, further in and for a different reason, so
// "returned an error" would pass even with the guard removed.
func TestLoginRequiresAPrompt(t *testing.T) {
	_, err := Login(nil, LoginRequest{Provider: "claude", AuthDir: t.TempDir()})
	if err == nil {
		t.Fatal("Login proceeded without an interaction callback")
	}
	if !strings.Contains(err.Error(), "交互回调") {
		t.Errorf("failed for a different reason than the missing prompt: %v", err)
	}
}

func TestLoginRequiresAuthDir(t *testing.T) {
	_, err := Login(nil, LoginRequest{Provider: "claude",
		Prompt: func(string) (string, error) { return "", nil }})
	if err == nil {
		t.Fatal("Login proceeded without a credential directory")
	}
	if !strings.Contains(err.Error(), "凭据目录") {
		t.Errorf("failed for a different reason than the missing directory: %v", err)
	}
}

// TestLoginCarriesProxyURL: the serving path routes through proxy-url, so a
// login that dropped it would reach the provider directly -- working in tests,
// timing out on exactly the machines that configured a proxy because they need
// one, and never hinting that the setting was ignored.
func TestLoginCarriesProxyURL(t *testing.T) {
	cfg := loginConfig(LoginRequest{
		Provider: "claude",
		AuthDir:  "/tmp/auths",
		ProxyURL: "http://127.0.0.1:7890",
	})
	if cfg.ProxyURL != "http://127.0.0.1:7890" {
		t.Errorf("ProxyURL = %q, want it carried through", cfg.ProxyURL)
	}
	if cfg.AuthDir != "/tmp/auths" {
		t.Errorf("AuthDir = %q, want it carried through", cfg.AuthDir)
	}

	// No proxy configured must stay empty rather than pick up a default.
	if got := loginConfig(LoginRequest{AuthDir: "/x"}).ProxyURL; got != "" {
		t.Errorf("ProxyURL = %q with none configured, want empty", got)
	}
}
