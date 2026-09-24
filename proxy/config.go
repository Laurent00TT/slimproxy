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
// The emulation layer itself is slimproxy's fork under third_party/CLIProxyAPI.
// A change that belongs inside it -- in an executor, the registry, a handler --
// is made there and listed in its SLIMPROXY_PATCHES.md; this package reaches
// the result through the Builder like any other embedder.
package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cliproxyconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"

	"github.com/Laurent00TT/slimproxy/i18n"
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

	// ProxyFallbackDirect dials upstream directly whenever ProxyURL's port has
	// no listener, deciding per connection rather than per process. For
	// deployments that alternate between a system-proxy VPN (proxy mandatory)
	// and a TUN VPN (proxy port dead, direct traffic captured transparently)
	// -- the same proxy-url is required under one and fatal under the other,
	// and the engine resolves its proxy once at startup, so no config edit can
	// follow the switch. See the outbound package for the mechanism.
	//
	// Off by default deliberately: for anyone whose proxy is a policy boundary
	// rather than a reachability workaround, silently dialing direct when the
	// proxy is down would be an egress-policy bypass, not a convenience.
	ProxyFallbackDirect bool `yaml:"proxy-fallback-direct"`

	// RequestRetry caps how many times the outer loop will wait for a cooling
	// credential to recover (or honour a 429's retry-after) and try again. It
	// is NOT a general failure-retry count: the loop only re-runs a request
	// while some credential sits in a cooldown window whose recovery deadline
	// fits inside MaxRetryInterval. 529 and dial-class failures never enter
	// cooldown, and the transient-5xx cooldown default (60s) exceeds the 30s
	// interval used here -- so with a single credential every failure class is
	// attempted exactly once regardless of this value. Measured, not inferred:
	// see the TestUpstreamRetry_* characterization tests.
	//
	// CLIProxyAPI ships no default for this; at zero the loop is skipped
	// entirely and even the multi-credential waits above disappear.
	RequestRetry int `yaml:"request-retry"`

	// MaxRetryInterval bounds, in seconds, how long the retry loop will wait
	// for a cooling credential before giving up. Zero disables waiting. Note
	// the interaction documented on RequestRetry: cooldowns longer than this
	// bound (like the 60s transient-5xx default) are never waited for.
	MaxRetryInterval int `yaml:"max-retry-interval"`

	// MaxRetryCredentials caps how many credentials one request may try.
	// Zero means unlimited.
	MaxRetryCredentials int `yaml:"max-retry-credentials"`

	// StreamIdleTimeout is how many seconds a streaming response may go
	// without a single chunk before the proxy severs it and hands the client
	// an explicit timeout to retry against. Zero means the 90-second default;
	// negative disables the guard.
	//
	// Exists because of streams that die silently mid-flight (measured
	// 2026-08-01: a VPN dropping long-lived flows without RST). Upstream keeps
	// no read deadline of its own, so without this the client waits out its
	// own stall detector -- five to nine minutes per occurrence. Healthy
	// streams are never quiet this long: the upstream emits SSE pings through
	// thinking pauses. See proxy/stallguard.go.
	StreamIdleTimeout int `yaml:"stream-idle-timeout"`

	// StreamEarlyFlush is how many seconds a streaming request may produce no
	// output before the proxy commits "200, text/event-stream" early and keeps
	// the connection warm with SSE keep-alive comments. Zero means the
	// 30-second default; negative disables the preamble.
	//
	// Exists because of Cloudflare's ~100-second time-to-first-header limit
	// (fixed on free/Pro plans): requests queue-bound behind tunnel bandwidth
	// were being cut down as 524s while their eventual execution would have
	// succeeded -- 129 crossed 100s on 2026-08-03 alone. The cost is confined
	// to slow requests: one that fails after the preamble delivers its error
	// as an in-stream SSE event instead of an HTTP status. See
	// proxy/earlyflush.go.
	StreamEarlyFlush int `yaml:"stream-early-flush"`

	// ClaudeCodeCacheTTL pins the lifetime of every prompt-cache breakpoint on
	// requests from Claude Code (User-Agent claude-cli/*): "" sends them as
	// the client wrote them, "1h" or "5m" rewrites all of them.
	//
	// Exists because the default five-minute lifetime is measured from the
	// START of the request that last touched the prefix, so a turn that spends
	// longer than that generating -- agentic loops routinely do -- returns to
	// find the whole conversation evicted and writes it again at full price:
	// 18 full rewrites of a ~300k-token prefix in one morning, measured
	// 2026-09-16. Rewritten uniformly rather than only where a ttl is missing
	// because Anthropic rejects a 1h breakpoint placed after a 5m one and the
	// engine flattens such a mix back to 5m. The price is real: a 1h write
	// bills at 2x the base input rate against 1.25x for 5m, so a client that
	// never returns within five minutes anyway pays the premium for nothing --
	// hence off by default and Claude Code only. Applied inside the engine's
	// executor, the one place that sees the request whole and knows who sent
	// it (third_party/CLIProxyAPI/SLIMPROXY_PATCHES.md 第 10-12 条).
	ClaudeCodeCacheTTL string `yaml:"claude-code-cache-ttl"`

	// Lang selects the interface language: "zh", "en", or empty to follow the
	// system locale.
	//
	// A config field rather than a flag or environment variable, because the
	// audience is "me and a few friends" sharing config files by chat: the file
	// is the one artifact that travels, and a language that rides in it arrives
	// set. Applied by the CLI at config load; this package never reads it
	// itself, since by the time a Config exists the strings around it are
	// already being selected.
	Lang string `yaml:"lang"`

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

	// Models is reserved for a model allowlist that is not enforced yet, so
	// Validate refuses a non-empty list. Empty serves everything the registry
	// knows about for the loaded credentials.
	//
	// Refused rather than accepted because the list never reached the serving
	// path: check printed it as an exact-match allowlist and the config summary
	// as "N allow-listed" while every model was served. An access-control knob
	// that reports itself on and does nothing is worse than no knob.
	//
	// TODO(you): enforcing it means deciding the matching policy. Exact match
	// is safe but brittle: upstream names carry dated suffixes
	// ("claude-sonnet-4-20250514") and requests arrive with a thinking suffix
	// appended ("gpt-5.5(high)"), so an exact list needs editing every time a
	// provider ships a build. Prefix match ("claude-") admits models added
	// later, which may be more access than intended; stripping the "(...)"
	// suffix before comparing lets one entry cover every reasoning level; glob
	// or regex is the most expressive, but a bad pattern fails open silently.
	// The check belongs where the engine resolves the model name for every
	// dialect (Gemini's is in the URL path, not the body) -- in the fork, not
	// in a middleware here re-parsing bodies.
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

// StreamIdleSummary renders the stream-idle knob for an operator.
//
// Seconds, matching how the knob is written and how every doc describes it --
// a %s on the duration would print "1m30s" for a default the documentation
// calls 90s, and no reader should have to reconcile those.
func (c Config) StreamIdleSummary() string {
	switch {
	case c.StreamIdleTimeout < 0:
		return i18n.T("关闭（流可以无限期挂住）", "off (streams may hang indefinitely)")
	case c.StreamIdleTimeout == 0:
		return fmt.Sprintf(i18n.T("%d s（默认）", "%d s (default)"), int(defaultStreamIdle/time.Second))
	default:
		return fmt.Sprintf("%d s", c.StreamIdleTimeout)
	}
}

// streamIdle resolves the stream-idle knob to a duration. Zero result means
// the guard is off; the yaml zero value means "use the default", mirroring
// upstream's transient-cooldown convention where only a negative disables.
func (c Config) streamIdle() time.Duration {
	switch {
	case c.StreamIdleTimeout < 0:
		return 0
	case c.StreamIdleTimeout == 0:
		return defaultStreamIdle
	default:
		return time.Duration(c.StreamIdleTimeout) * time.Second
	}
}

// StreamEarlyFlushSummary renders the early-flush knob for an operator.
// Seconds, for the same reason StreamIdleSummary uses them.
func (c Config) StreamEarlyFlushSummary() string {
	switch {
	case c.StreamEarlyFlush < 0:
		return i18n.T("关闭（慢请求可能被边缘按 ~100s 斩成 524）", "off (slow requests may be severed as 524s at the edge's ~100s)")
	case c.StreamEarlyFlush == 0:
		return fmt.Sprintf(i18n.T("%d s（默认）", "%d s (default)"), int(defaultEarlyFlush/time.Second))
	default:
		return fmt.Sprintf("%d s", c.StreamEarlyFlush)
	}
}

// ClaudeCodeCacheTTLSummary renders the cache-lifetime knob for an operator.
func (c Config) ClaudeCodeCacheTTLSummary() string {
	if strings.TrimSpace(c.ClaudeCodeCacheTTL) == "" {
		return i18n.T("不改写（按 Claude Code 发来的，默认 5m）", "as sent by Claude Code (5m by default)")
	}
	return fmt.Sprintf(i18n.T("Claude Code 的全部断点改写为 %s", "every Claude Code breakpoint rewritten to %s"), strings.TrimSpace(c.ClaudeCodeCacheTTL))
}

// streamEarlyFlush resolves the early-flush knob to a duration, with the same
// zero/negative convention as streamIdle.
func (c Config) streamEarlyFlush() time.Duration {
	switch {
	case c.StreamEarlyFlush < 0:
		return 0
	case c.StreamEarlyFlush == 0:
		return defaultEarlyFlush
	default:
		return time.Duration(c.StreamEarlyFlush) * time.Second
	}
}

// orAllInterfaces names the empty host for a message an operator reads.
func orAllInterfaces(host string) string {
	if strings.TrimSpace(host) == "" {
		return "(empty = all interfaces)"
	}
	return host
}

// isLoopbackHost reports whether the bind address is loopback-only.
// fallbackProxyIsLoopback answers whether proxy-url names a loopback host.
// Parse failures read as "not loopback": the relay will refuse the URL at
// startup anyway, and validation should not vouch for what it cannot parse.
func fallbackProxyIsLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

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
			return i18n.T("开启", "on")
		}
		return i18n.T("关闭", "off")
	}
	dash := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "—"
		}
		return s
	}
	models := i18n.T("全部", "all")
	if len(c.Models) > 0 {
		// Diagnostics also render invalid configs; do not imply enforcement.
		models = fmt.Sprintf(i18n.T("%d 项（尚不支持，拒绝启动）", "%d listed (unsupported; startup refused)"), len(c.Models))
	}
	retry := fmt.Sprint(c.RequestRetry)
	if c.RequestRetry == 0 {
		retry = i18n.T("0（重试循环关闭）", "0 (retry loop off)")
	}
	creds := i18n.T("不限", "unlimited")
	if c.MaxRetryCredentials > 0 {
		creds = fmt.Sprint(c.MaxRetryCredentials)
	}
	auth := fmt.Sprintf(i18n.T("%d 已配置", "%d configured"), len(c.APIKeys))
	if len(c.APIKeys) == 0 {
		auth = i18n.T("无（放行一切）", "none (everything admitted)")
	}
	stall := c.StreamIdleSummary()

	return []ConfigRow{
		{K: "listen", V: c.Addr()},
		{K: "auth-dir", V: dash(c.AuthDir)},
		{K: "api-keys", V: auth, Warn: len(c.APIKeys) == 0},
		{K: "proxy-url", V: dash(c.ProxyURL)},
		{K: "proxy-fallback-direct", V: yesNo(c.ProxyFallbackDirect)},
		{K: "request-log", V: yesNo(c.RequestLog), Warn: c.RequestLog},
		{K: "debug", V: yesNo(c.Debug)},
		{K: "request-retry", V: retry, Warn: c.RequestRetry == 0},
		{K: "max-retry-interval", V: fmt.Sprintf("%d s", c.MaxRetryInterval)},
		{K: "max-retry-credentials", V: creds},
		{K: "stream-idle-timeout", V: stall, Warn: c.StreamIdleTimeout < 0},
		{K: "stream-early-flush", V: c.StreamEarlyFlushSummary(), Warn: c.StreamEarlyFlush < 0},
		{K: "claude-code-cache-ttl", V: c.ClaudeCodeCacheTTLSummary()},
		{K: "models", V: models, Warn: len(c.Models) > 0},
		{K: "management api", V: i18n.T("已禁用", "disabled"), Warn: true},
		{K: "plugin host", V: i18n.T("已禁用", "disabled"), Warn: true},
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
	if c.Lang != "" && c.Lang != "zh" && c.Lang != "en" {
		// Validated here so i18n.Parse can stay error-free: a bad value dies at
		// startup with this message, so every later Parse call sees input this
		// has already ruled on.
		errs = append(errs, fmt.Errorf("lang %q unknown: use \"zh\", \"en\", or leave it out to follow the system locale", c.Lang))
	}
	switch strings.TrimSpace(c.ClaudeCodeCacheTTL) {
	case "", "5m", "1h":
	default:
		// The engine forwards the value verbatim and Anthropic would reject
		// the request -- every Claude Code request -- at run time. Refused
		// here instead, naming the two lifetimes that exist.
		errs = append(errs, fmt.Errorf(
			"claude-code-cache-ttl %q unknown: Anthropic's prompt cache offers \"5m\" and \"1h\"; leave it empty to send breakpoints as Claude Code wrote them", c.ClaudeCodeCacheTTL))
	}
	if len(c.Models) > 0 {
		// Nothing enforces the list (see Models). Accepting it would put an
		// allowlist in the operator's config and in check's output while every
		// model the credentials expose is served.
		errs = append(errs, fmt.Errorf(
			"models lists %d entries, but the model allowlist is not enforced yet: every model would still be served. "+
				"Remove the entries (models: [] serves everything)", len(c.Models)))
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
	if c.ProxyFallbackDirect {
		switch {
		case c.ProxyURL == "":
			// A contradiction, not a no-op: the flag says "fall back from the
			// proxy" and the config names no proxy to fall back from. Someone
			// removing proxy-url has left a stale flag behind; saying so at
			// startup beats a setting that silently does nothing.
			errs = append(errs, errors.New(
				"proxy-fallback-direct is set but proxy-url is empty: there is no proxy to fall back from; set proxy-url or remove the flag"))
		case !strings.HasPrefix(c.ProxyURL, "http://"):
			// The fallback relay chains with a plain HTTP CONNECT. Accepting a
			// socks5 or https proxy-url here would speak the wrong protocol to
			// it -- refused at startup rather than discovered per request.
			errs = append(errs, fmt.Errorf(
				"proxy-fallback-direct only supports an http:// proxy-url (got %q): the relay probes and chains with a plain CONNECT", c.ProxyURL))
		case !fallbackProxyIsLoopback(c.ProxyURL):
			// The probe equates "up" with "TCP dial completes within 250ms",
			// which is only a faithful reading of "listening" on loopback.
			// Against a distant proxy, round-trip time alone would read as
			// absence, and every request would silently go direct -- for a
			// deployment whose proxy is an egress policy, that is the bypass
			// the flag's default-off exists to prevent.
			errs = append(errs, fmt.Errorf(
				"proxy-fallback-direct requires a loopback proxy-url (got %q): the listen probe is tuned for loopback and would misread a remote proxy's round-trip as \"down\", silently sending traffic direct", c.ProxyURL))
		}
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

	// Carried by the fork's ClaudeCodeConfig (SLIMPROXY_PATCHES.md 第 10 条)
	// so it rides the same materialize/reload path as every other knob.
	cfg.ClaudeCode.CacheTTL = strings.TrimSpace(c.ClaudeCodeCacheTTL)

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
