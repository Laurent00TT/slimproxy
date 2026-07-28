// Package proxy runs a deliberately small reverse proxy on top of
// CLIProxyAPI's public SDK.
//
// What "slim" means here, precisely: this package narrows the CONFIGURATION
// and RUNTIME SURFACE, not the dependency tree. The provider executors that
// carry the upstream client emulation live in CLIProxyAPI's internal/ tree and
// Go's internal-package rule makes sdk/cliproxy.Builder the only way to reach
// them from another module. Building a Service therefore links gin, pion,
// redis, lumberjack and the rest regardless of what is switched off. What this
// package does control is what actually runs and what an operator can
// misconfigure: no management API, no control panel, no plugin host, no pprof,
// no usage queue.
//
// The emulation layer is used exactly as CLIProxyAPI ships it. Nothing here
// modifies, extends or hardens it.
package proxy

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	cliproxyconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// Config is the whole knob set this proxy exposes. Everything absent from it
// is pinned to a hardened value by build(); see hardeningNotes.
type Config struct {
	// Host is the bind address. Empty means all interfaces.
	Host string `yaml:"host"`
	// Port is the listen port. Required.
	Port int `yaml:"port"`

	// APIKeys are the keys inbound clients must present.
	//
	// This is required, and deliberately so. CLIProxyAPI's AuthMiddleware is
	// fail-open: with no access provider registered it calls c.Next() for every
	// request (internal/api/server.go:2142-2145). An empty key list therefore
	// does not mean "no auth configured", it means "no auth enforced" on a
	// process holding live subscription credentials. Set AllowUnauthenticated
	// to accept that explicitly.
	APIKeys []string `yaml:"api-keys"`

	// AllowUnauthenticated permits starting with no APIKeys. Only meaningful
	// when the listener is not reachable from outside the host.
	AllowUnauthenticated bool `yaml:"allow-unauthenticated"`

	// AuthDir holds OAuth credential files. Defaults to "auths".
	AuthDir string `yaml:"auth-dir"`

	// ProxyURL routes upstream traffic through an http/https/socks5 proxy.
	// Per-credential proxy-url in the credential file overrides this.
	ProxyURL string `yaml:"proxy-url"`

	// RequestRetry caps outer retry attempts across credentials.
	//
	// CLIProxyAPI ships no default for this: internal/config/config.go:779-793
	// leaves it zero, and the outer wait-and-retry loop is skipped entirely
	// when it is zero (conductor.go:4197-4199). Only config.example.yaml
	// suggests 3. Left at zero here you get single-pass credential iteration
	// with no cooldown-aware retry.
	RequestRetry int `yaml:"request-retry"`

	// MaxRetryInterval bounds, in seconds, how long the retry loop will wait
	// for a cooling credential before giving up. Zero disables waiting.
	MaxRetryInterval int `yaml:"max-retry-interval"`

	// MaxRetryCredentials caps how many credentials one request may try.
	// Zero means unlimited.
	MaxRetryCredentials int `yaml:"max-retry-credentials"`

	// Debug raises log verbosity.
	Debug bool `yaml:"debug"`

	// RequestLog writes full request/response bodies to disk.
	//
	// Redaction is header-and-query-name based only (internal/util/provider.go
	// :222-288); bodies are written verbatim. Any credential carried in a body
	// lands in the log unredacted. Leave this off unless the log directory is
	// treated as secret material.
	RequestLog bool `yaml:"request-log"`

	// LogToFile writes application logs to disk instead of stdout.
	LogToFile bool `yaml:"log-to-file"`

	// Models optionally restricts which model names this proxy will serve.
	//
	// Empty means serve everything the registry knows about for the loaded
	// credentials. See AllowModel.
	Models []string `yaml:"models"`

	// LogDir is where application logs go when LogToFile is on. Defaults to
	// "logs" relative to the working directory.
	//
	// Deliberately a slimproxy knob rather than deferring to CLIProxyAPI's
	// resolver, which silently prefers $WRITABLE_PATH -- an ambient variable
	// that would make -check report one directory while logs land in another.
	LogDir string `yaml:"log-dir"`

	// RequestLogDir is where request/response body logs go when RequestLog is
	// on. Defaults to <LogDir>/requests.
	RequestLogDir string `yaml:"request-log-dir"`

	// JournalDays is how long the structured event stream is kept, in days.
	// Zero means journal.DefaultRetentionDays.
	//
	// Separate from the application log's rotation because they answer
	// different questions and cost different amounts. The application log is
	// prose for reading when something is on fire; the journal is one JSON
	// object per request, kept so "why was that slow last Tuesday" has an
	// answer. Neither retention makes sense for the other.
	//
	// The journal holds no bodies and no headers -- see the journal package --
	// but it does name credentials and hostnames, which is why it is bounded
	// rather than kept forever.
	JournalDays int `yaml:"journal-days"`
}

// orAllInterfaces names the empty host for a message an operator reads.
func orAllInterfaces(host string) string {
	if strings.TrimSpace(host) == "" {
		return "(empty = all interfaces)"
	}
	return host
}

// isLoopbackHost reports whether the bind address is loopback-only.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ConfigRow is one line of the configuration summary.
type ConfigRow struct {
	K string
	V string
	// Warn marks a value worth drawing attention to -- a disabled subsystem, or
	// a setting whose current value has a security consequence.
	Warn bool
}

// ConfigRows renders the configuration summary. It reports posture, not
// secrets: api-keys shows a count, never a value.
func (c *Config) ConfigRows() []ConfigRow {
	yesNo := func(b bool) string {
		if b {
			return "开启"
		}
		return "关闭"
	}
	dash := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "—"
		}
		return s
	}
	models := "全部"
	if len(c.Models) > 0 {
		models = fmt.Sprintf("%d 项白名单", len(c.Models))
	}
	retry := fmt.Sprint(c.RequestRetry)
	if c.RequestRetry == 0 {
		retry = "0（重试循环关闭）"
	}
	creds := "不限"
	if c.MaxRetryCredentials > 0 {
		creds = fmt.Sprint(c.MaxRetryCredentials)
	}
	auth := fmt.Sprintf("%d 已配置", len(c.APIKeys))
	if len(c.APIKeys) == 0 {
		auth = "无（放行一切）"
	}

	return []ConfigRow{
		{K: "listen", V: c.Addr()},
		{K: "auth-dir", V: dash(c.AuthDir)},
		{K: "api-keys", V: auth, Warn: len(c.APIKeys) == 0},
		{K: "proxy-url", V: dash(c.ProxyURL)},
		{K: "request-log", V: yesNo(c.RequestLog), Warn: c.RequestLog},
		{K: "debug", V: yesNo(c.Debug)},
		{K: "request-retry", V: retry, Warn: c.RequestRetry == 0},
		{K: "max-retry-interval", V: fmt.Sprintf("%d s", c.MaxRetryInterval)},
		{K: "max-retry-credentials", V: creds},
		{K: "models", V: models},
		{K: "management api", V: "已禁用", Warn: true},
		{K: "plugin host", V: "已禁用", Warn: true},
	}
}

// hardeningNotes documents every subsystem build() pins off, so the reason a
// knob is missing from Config is discoverable from the code rather than only
// from the commit that removed it.
const hardeningNotes = `
management API   off  - no /v0/management routes; SecretKey empty and control panel disabled
control panel    off  - suppresses the background GitHub fetch of management.html
plugin host      off  - no dlopen of native plugins into this process
pprof            off  - no debug listener
usage statistics off  - no redis usage/errors queue on the listen port
commercial mode  off  - keeps the request-logging middleware installable
`

// Validate checks the config without mutating it.
func (c *Config) Validate() error {
	var errs []error

	if c.Port <= 0 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d out of range", c.Port))
	}
	if len(c.APIKeys) == 0 && !c.AllowUnauthenticated {
		errs = append(errs, errors.New(
			"no APIKeys set: inbound auth is fail-open, so this would serve your upstream credentials to anyone who can reach the port; set APIKeys or AllowUnauthenticated"))
	}
	// AllowUnauthenticated's own documentation says it is "only meaningful when
	// the listener is not reachable from outside the host" -- and until now that
	// precondition lived solely in a comment, while isLoopbackHost sat unused.
	//
	// The combination it permits is the worst configuration this program can
	// hold: every interface bound, no key required, live subscription
	// credentials behind it. Reaching it took one field and produced "config
	// OK".
	if c.AllowUnauthenticated && len(c.APIKeys) == 0 && !isLoopbackHost(c.Host) {
		errs = append(errs, fmt.Errorf(
			"allow-unauthenticated is set with host %q, which accepts connections from outside this machine: "+
				"that combination serves your upstream credentials to anyone who can reach the port. "+
				"Bind 127.0.0.1, or set api-keys", orAllInterfaces(c.Host)))
	}
	for i, k := range c.APIKeys {
		if strings.TrimSpace(k) == "" {
			errs = append(errs, fmt.Errorf("APIKeys[%d] is blank", i))
		}
	}
	if c.RequestRetry < 0 {
		errs = append(errs, errors.New("RequestRetry must not be negative"))
	}
	if c.MaxRetryInterval < 0 {
		errs = append(errs, errors.New("MaxRetryInterval must not be negative"))
	}
	if c.ProxyURL != "" && !strings.Contains(c.ProxyURL, "://") {
		errs = append(errs, fmt.Errorf("ProxyURL %q needs a scheme (http://, https://, socks5://)", c.ProxyURL))
	}
	for i, k := range c.APIKeys {
		if placeholderAPIKeys[strings.TrimSpace(k)] {
			errs = append(errs, fmt.Errorf(
				"APIKeys[%d] is the placeholder %q from a shipped example: it is published in a public "+
					"repository, so this would be a live relay in front of your subscription credentials "+
					"guarded by a key anyone can read", i, strings.TrimSpace(k)))
		}
	}
	if err := checkManagementPasswordEnv(); err != nil {
		errs = append(errs, err)
	}
	if err := checkStorageBackendEnv(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// placeholderAPIKeys are the example values shipped in this repo and upstream.
//
// CLIProxyAPI refuses to serve a single proxy request when it sees its own
// placeholders (its example-API-key safe mode). That safe mode is an internal
// server option with no sdk/api wrapper, so an SDK embedder does not inherit
// the protection and has to reimplement the check.
var placeholderAPIKeys = map[string]bool{
	"change-me-to-something-random": true,
	"your-api-key-1":                true,
	"your-api-key-2":                true,
	"your-api-key-3":                true,
	"your-api-key":                  true,
	"changeme":                      true,
}

// storageBackendEnvVars select a non-file token store in CLIProxyAPI's own
// binary. slimproxy never reads them: sdk/cliproxy.Builder calls GetTokenStore()
// which lazily constructs a plain FileTokenStore, and RegisterTokenStore has no
// caller anywhere under sdk/.
//
// Left unchecked this diverges silently and expensively: a deployment migrated
// with PGSTORE_DSN still set loads zero credentials out of ./auths, every
// request fails with "no available credentials", and any login performed from
// the dashboard writes JSON files the Postgres-backed deployment will never
// see. Refusing to boot turns that into one readable line.
var storageBackendEnvVars = []string{
	"PGSTORE_DSN", "PGSTORE_SCHEMA", "PGSTORE_LOCAL_PATH",
	"GITSTORE_GIT_URL", "GITSTORE_GIT_TOKEN", "GITSTORE_GIT_USERNAME",
	"GITSTORE_GIT_BRANCH", "GITSTORE_LOCAL_PATH",
	"OBJECTSTORE_ENDPOINT", "OBJECTSTORE_BUCKET",
	"OBJECTSTORE_ACCESS_KEY", "OBJECTSTORE_SECRET_KEY", "OBJECTSTORE_LOCAL_PATH",
}

func checkStorageBackendEnv() error {
	var set []string
	for _, name := range storageBackendEnvVars {
		if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s set, but slimproxy is file-store only: those variables select a Postgres/git/object "+
			"token store in CLIProxyAPI's own binary and are not readable through its SDK, so slimproxy "+
			"would silently load zero credentials from %s instead — unset them, or run CLIProxyAPI directly",
		strings.Join(set, ", "), "auth-dir")
}

// ResolveAuthDir returns the absolute directory credentials are actually loaded
// from, applying the default, ~ expansion, and absolutization in that order.
//
// Exported because anything that reports on the credential pool has to agree
// with what the proxy does. Re-deriving it elsewhere is how a diagnostic ends
// up reading a literal "~" directory in the working tree while the proxy reads
// the real home -- reporting healthy credentials the proxy will never see.
//
// The error is advisory: on failure the best-effort path is still returned, so
// a caller that only wants something to print can ignore it, while a caller
// that needs certainty can report the failure.
func (c *Config) ResolveAuthDir() (string, error) {
	dir := c.AuthDir
	if dir == "" {
		dir = "auths"
	}
	expanded, err := expandHome(dir)
	if err != nil {
		return dir, err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return expanded, err
	}
	return abs, nil
}

// expandHome resolves a leading ~ against the user home directory.
//
// CLIProxyAPI's own auth-dir resolver does this; the SDK path never calls it,
// so "~/.cli-proxy-api" -- the value CLIProxyAPI defaults to and writes logins
// into -- became a literal directory named "~" under the working directory.
// Zero credentials loaded, no error, and the most likely line for a migrating
// operator to carry over verbatim.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// checkManagementPasswordEnv refuses to start when MANAGEMENT_PASSWORD is set.
//
// Pinning cfg.RemoteManagement.SecretKey to "" is not sufficient to keep the
// management API off. internal/api/server.go:331-333 reads MANAGEMENT_PASSWORD
// from the environment, :400 ORs it into hasManagementSecret, and :403 then
// registers the whole management route set -- which :2019 re-asserts on every
// config reload. An exported variable therefore silently restores exactly the
// surface this proxy exists to remove, including GET /v0/management/config,
// which returns every provider API key in plaintext.
//
// Since slimproxy cannot suppress that from outside the module, the only honest
// options are to refuse or to lie in the Configuration panel. It refuses.
func checkManagementPasswordEnv() error {
	if v, ok := os.LookupEnv("MANAGEMENT_PASSWORD"); ok && strings.TrimSpace(v) != "" {
		return errors.New(
			"MANAGEMENT_PASSWORD is set: CLIProxyAPI enables its full management API from that " +
				"variable alone, which slimproxy cannot disable from outside the module — " +
				"unset it, or use CLIProxyAPI directly if you want the management API")
	}
	return nil
}

// Addr renders the listen address for logging.
func (c *Config) Addr() string {
	host := c.Host
	if host == "" {
		host = "0.0.0.0"
	}
	return host + ":" + strconv.Itoa(c.Port)
}

// AllowModel reports whether this proxy should serve requests for model.
//
// TODO(you): decide the matching policy for Config.Models. The current
// behaviour is exact-match, which is safe but brittle: upstream model names
// carry dated suffixes ("claude-sonnet-4-20250514") and this proxy also
// receives names with a thinking suffix appended ("gpt-5.5(high)"), so an
// exact list needs editing every time a provider ships a build.
//
// Approaches worth weighing:
//   - prefix match: "claude-" admits the whole family, including models added
//     later, which may be more access than intended
//   - strip the "(...)" thinking suffix before comparing, so one entry covers
//     every reasoning level of a model
//   - glob or regex: most expressive, but a bad pattern fails open silently
//
// This is a real access-control boundary, not a formatting choice, so it is
// left to whoever knows what this deployment should expose.
func (c *Config) AllowModel(model string) bool {
	if len(c.Models) == 0 {
		return true
	}
	for _, m := range c.Models {
		if m == model {
			return true
		}
	}
	return false
}

// build converts Config into the CLIProxyAPI configuration, pinning every
// subsystem this proxy does not use.
//
// It seeds from ParseConfigBytes rather than a bare &Config{} so CLIProxyAPI's
// own defaulting runs. A zero-valued Config does NOT round-trip: LoadConfig
// rejects it with "credential-in-flight.snapshot-interval must be positive",
// and because the file watcher re-reads the materialized path on every change
// and only logs that failure, the result is hot reload that silently never
// applies. TestMaterializedConfigRoundTrips pins this.
func (c *Config) build() *cliproxyconfig.Config {
	authDir, _ := c.ResolveAuthDir()

	cfg, err := cliproxyconfig.ParseConfigBytes([]byte("port: 0\n"))
	if err != nil || cfg == nil {
		// Defaulting is unavailable; fall back rather than crash. The reload
		// path will be degraded, which the round-trip test reports.
		cfg = &cliproxyconfig.Config{}
	}

	cfg.Host = c.Host
	cfg.Port = c.Port
	cfg.AuthDir = authDir
	cfg.Debug = c.Debug
	cfg.LoggingToFile = c.LogToFile

	cfg.APIKeys = append([]string(nil), c.APIKeys...)
	cfg.ProxyURL = c.ProxyURL
	cfg.RequestLog = c.RequestLog

	cfg.RequestRetry = c.RequestRetry
	cfg.MaxRetryInterval = c.MaxRetryInterval
	cfg.MaxRetryCredentials = c.MaxRetryCredentials

	// --- pinned off; see hardeningNotes ---

	// No management API. An empty SecretKey plus a disabled panel keeps the
	// routes unreachable and stops the background panel fetch from GitHub.
	cfg.RemoteManagement.SecretKey = ""
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.RemoteManagement.DisableAutoUpdatePanel = true

	// No native plugins dlopen'd into this process.
	cfg.Plugins.Enabled = false

	// No pprof listener, no usage queue on the listen port.
	cfg.Pprof.Enable = false
	cfg.UsageStatisticsEnabled = false

	// Keep the request-logging middleware installable; commercial mode removes
	// it at construction time and it cannot be added back by reload.
	cfg.CommercialMode = false

	// Match the shipped CLIProxyAPI template. The ParseConfigBytes seed leaves
	// this false, which silently disables the Antigravity credits fallback:
	// once every free-tier credential is exhausted the executor gates directly
	// on this flag and fails the request instead of falling back.
	cfg.QuotaExceeded.AntigravityCredits = true

	return cfg
}
