// Package credentials inspects and manages the upstream OAuth credentials the
// proxy serves requests with.
//
// It sits between the CLI and CLIProxyAPI's sdk/auth: listing and removal are
// implemented here against the credential files directly, while login delegates
// to the SDK's per-provider authenticators, which own the OAuth flows.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Credential is one credential file, as far as this package needs to read it.
//
// Deliberately a partial view: the tokens themselves are never loaded into a
// field, so nothing here can accidentally print one.
type Credential struct {
	// File is the absolute path of the credential file.
	File string
	// Name is the file's base name, and the identifier CLI commands accept.
	Name string
	// Provider is the credential type, e.g. "claude".
	Provider string
	// Account is the human-readable owner, usually an email address.
	Account string
	// Expires is when the access token stops working. Zero when the file did
	// not say.
	Expires time.Time
	// LastRefresh is when it was last renewed. Zero when unknown.
	LastRefresh time.Time
	// Disabled reports the file's own disabled flag.
	Disabled bool
	// Err is set when the file exists but could not be understood. Such a
	// credential is listed rather than skipped: a file the proxy will fail to
	// load is exactly what an operator needs to see.
	Err error
}

// Status classifies a credential for display.
type Status int

const (
	// StatusOK: usable as far as this can tell.
	StatusOK Status = iota
	// StatusExpiring: valid but close enough to expiry to be worth noting.
	StatusExpiring
	// StatusExpired: the access token's expiry has passed. Not necessarily
	// broken -- a refresh token may still renew it -- but it cannot serve a
	// request until it does.
	StatusExpired
	// StatusDisabled: the file marks itself disabled.
	StatusDisabled
	// StatusBroken: the file could not be parsed.
	StatusBroken
	// StatusUnknown: no expiry was recorded, so validity cannot be judged.
	StatusUnknown
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "可用"
	case StatusExpiring:
		return "即将过期"
	case StatusExpired:
		return "已过期"
	case StatusDisabled:
		return "已禁用"
	case StatusBroken:
		return "损坏"
	default:
		return "未知"
	}
}

// expiringWindow is how far ahead of expiry a credential is called expiring.
//
// Chosen to be longer than the proxy's own refresh interval, so a credential
// this reports as expiring is one that automatic refresh has already had a
// chance to renew and did not.
const expiringWindow = 30 * time.Minute

// Status classifies the credential at the given instant.
//
// Takes the clock as an argument rather than calling time.Now, so the
// boundaries are testable without sleeping.
func (c Credential) Status(now time.Time) Status {
	switch {
	case c.Err != nil:
		return StatusBroken
	case c.Disabled:
		return StatusDisabled
	case c.Expires.IsZero():
		// No expiry recorded. Reporting "usable" would assert something the
		// file never said.
		return StatusUnknown
	case now.After(c.Expires):
		return StatusExpired
	case now.Add(expiringWindow).After(c.Expires):
		return StatusExpiring
	default:
		return StatusOK
	}
}

// Usable reports whether the proxy could currently serve a request with this.
//
// Answers a display question: what does the pool look like right now. Do not
// use it to decide whether a credential is worth keeping -- see Recoverable.
func (c Credential) Usable(now time.Time) bool {
	switch c.Status(now) {
	case StatusOK, StatusExpiring:
		return true
	default:
		return false
	}
}

// Recoverable reports whether the proxy would load this credential and could
// end up serving requests with it.
//
// Deliberately weaker than Usable, and deliberately a separate predicate. An
// expired access token is not usable this instant, but the file carries a
// refresh token and the proxy renews it on its own -- so it is very much worth
// keeping. A credential with no recorded expiry is in the same position: the
// file says nothing, which is a reason to be careful with it rather than a
// licence to delete it.
//
// Only two states are genuinely worthless: one the proxy refuses to load
// (broken), and one it was told not to use (disabled).
//
// Reusing Usable here is what let `auth rm` delete the last credential in a
// pool: a pool of one expired-but-refreshable credential counted as zero
// usable, so the guard saw nothing to protect.
func (c Credential) Recoverable(now time.Time) bool {
	switch c.Status(now) {
	case StatusBroken, StatusDisabled:
		return false
	default:
		return true
	}
}

// rawCredential is the on-disk shape, limited to what is read.
type rawCredential struct {
	Type        string `json:"type"`
	Email       string `json:"email"`
	Expired     string `json:"expired"`
	LastRefresh string `json:"last_refresh"`
	Disabled    bool   `json:"disabled"`
}

// List reads every credential in dir.
//
// A file that cannot be parsed is returned with Err set rather than omitted.
// Silently skipping it would hide the one file most likely to explain why the
// proxy is failing every request.
func List(dir string) ([]Credential, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("凭据目录 %s 不存在", dir)
		}
		return nil, fmt.Errorf("无法读取凭据目录 %s: %w", dir, err)
	}

	var out []Credential
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		out = append(out, read(filepath.Join(dir, e.Name())))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func read(path string) Credential {
	c := Credential{File: path, Name: filepath.Base(path)}
	body, err := os.ReadFile(path)
	if err != nil {
		c.Err = fmt.Errorf("无法读取: %w", err)
		return c
	}
	var raw rawCredential
	if err := json.Unmarshal(body, &raw); err != nil {
		c.Err = fmt.Errorf("不是合法 JSON: %w", err)
		return c
	}
	c.Provider = raw.Type
	c.Account = strings.TrimSpace(raw.Email)
	c.Disabled = raw.Disabled
	c.Expires = parseTime(raw.Expired)
	c.LastRefresh = parseTime(raw.LastRefresh)
	return c
}

// parseTime accepts the layouts these files have been observed to use, and
// returns the zero time rather than guessing when none match.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ErrNotFound is returned when no credential matches an identifier.
var ErrNotFound = errors.New("没有匹配的凭据")

// ErrAmbiguous is returned when an identifier matches more than one.
var ErrAmbiguous = errors.New("标识匹配到多个凭据")

// Find resolves an identifier to exactly one credential.
//
// Accepts the file name, the name without .json, or the account address. An
// ambiguous match is an error rather than a pick: this is the lookup a deletion
// runs on, and guessing there removes the wrong file.
func Find(creds []Credential, id string) (Credential, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Credential{}, ErrNotFound
	}

	var matches []Credential
	for _, c := range creds {
		if strings.EqualFold(c.Name, id) ||
			strings.EqualFold(stripJSONExt(c.Name), id) ||
			(c.Account != "" && strings.EqualFold(c.Account, id)) {
			matches = append(matches, c)
		}
	}

	switch len(matches) {
	case 0:
		return Credential{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return Credential{}, fmt.Errorf("%w: %q 匹配 %s", ErrAmbiguous, id, strings.Join(names, ", "))
	}
}

// stripJSONExt removes a .json suffix regardless of its case.
//
// List accepts the extension case-insensitively, so a file named "Claude.JSON"
// is a credential; matching its short name case-sensitively would let an
// operator see it in the listing and then be told it does not exist.
func stripJSONExt(name string) string {
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".json") {
		return name[:len(name)-len(ext)]
	}
	return name
}

// ErrLastUsable is returned when removing a credential would leave the proxy
// with none that can serve a request.
var ErrLastUsable = errors.New("这是最后一个可用凭据")

// Remove deletes a credential by identifier.
//
// A thin wrapper over RemoveResolved for callers that have not already listed
// the pool. Anything that shows the operator what it is about to delete should
// use RemoveResolved instead, so the file described and the file deleted are
// the same one.
func Remove(dir, id string, force bool, now time.Time) (Credential, error) {
	creds, err := List(dir)
	if err != nil {
		return Credential{}, err
	}
	target, err := Find(creds, id)
	if err != nil {
		return Credential{}, err
	}
	return target, RemoveResolved(creds, target, force, now)
}

// RemoveResolved deletes a credential the caller has already resolved.
//
// force skips the last-credential guard. That guard exists because removing the
// last one leaves a proxy that starts, accepts requests, and fails every one of
// them upstream -- broken while looking healthy from outside.
//
// Both the gate and the count use Recoverable rather than Usable. The old rule
// used Usable for both, and a pool holding a single expired-but-refreshable
// credential counted as zero usable -- so deleting the last one skipped the
// guard entirely and emptied the pool with exit code 0.
//
// The gate remains, though: removing something the proxy cannot load anyway
// (broken, disabled) takes nothing away, so refusing it would only stop an
// operator from tidying up.
func RemoveResolved(creds []Credential, target Credential, force bool, now time.Time) error {
	if target.File == "" {
		return ErrNotFound
	}
	if !force && target.Recoverable(now) {
		remaining := 0
		for _, c := range creds {
			if c.File != target.File && c.Recoverable(now) {
				remaining++
			}
		}
		if remaining == 0 {
			return fmt.Errorf("%w：删除后代理将接受请求但每一个都会失败", ErrLastUsable)
		}
	}
	if err := os.Remove(target.File); err != nil {
		return fmt.Errorf("删除 %s 失败: %w", target.File, err)
	}
	return nil
}
