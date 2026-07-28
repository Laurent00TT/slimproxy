package tunnel

import (
	"strings"
	"testing"
)

// TestParseConfigCollectsEveryRoutedHostname is why Hostnames is a list.
//
// Was: ReadConfig stopped at the first ingress rule that routed somewhere, so a
// second domain added to the same tunnel -- the documented way to serve two
// domains -- never appeared in any status surface. The operator who had just
// added a second domain read the dashboard as "只显示一个" and concluded the
// new domain had not taken effect, when it was routing fine.
func TestParseConfigCollectsEveryRoutedHostname(t *testing.T) {
	body := []byte(`
tunnel: 0f0e0d0c-0b0a-4990-8877-665544332211
ingress:
  - hostname: proxy.example.com
    service: http://127.0.0.1:8317
  - hostname: proxy.example.org
    service: http://127.0.0.1:8317
  - service: http_status:404
`)
	cfg, found, err := parseConfig(body, "test.yml")
	if err != nil || !found {
		t.Fatalf("parseConfig: found=%v err=%v", found, err)
	}
	want := []string{"proxy.example.com", "proxy.example.org"}
	if len(cfg.Hostnames) != len(want) {
		t.Fatalf("Hostnames = %v, want %v", cfg.Hostnames, want)
	}
	for i := range want {
		if cfg.Hostnames[i] != want[i] {
			t.Errorf("Hostnames[%d] = %q, want %q（顺序必须保持 ingress 规则顺序）",
				i, cfg.Hostnames[i], want[i])
		}
	}
}

// TestParseConfigDeduplicatesPathSplitRules.
//
// Was: every routed rule appended its hostname, so cloudflared's documented
// path-splitting -- one hostname spread over several rules with different
// `path:` selectors -- yielded ["a.com", "a.com"], and the top bar claimed
// "+1" for a second domain that does not exist.
func TestParseConfigDeduplicatesPathSplitRules(t *testing.T) {
	body := []byte(`
tunnel: some-tunnel
ingress:
  - hostname: proxy.example.com
    path: /v1/.*
    service: http://127.0.0.1:8317
  - hostname: proxy.example.com
    service: http://127.0.0.1:9000
  - service: http_status:404
`)
	cfg, _, err := parseConfig(body, "test.yml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Hostnames) != 1 || cfg.Hostnames[0] != "proxy.example.com" {
		t.Errorf("Hostnames = %v, want [proxy.example.com]", cfg.Hostnames)
	}
}

// TestParseConfigSkipsBlockedHostnames: a hostname whose only rule is
// http_status:* is a path being refused, not a domain being served, and
// reporting it as reachable would send traffic to a wall.
func TestParseConfigSkipsBlockedHostnames(t *testing.T) {
	body := []byte(`
tunnel: some-tunnel
ingress:
  - hostname: blocked.example.com
    service: http_status:404
  - hostname: live.example.com
    service: http://127.0.0.1:8317
  - service: http_status:404
`)
	cfg, _, err := parseConfig(body, "test.yml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Hostnames) != 1 || cfg.Hostnames[0] != "live.example.com" {
		t.Errorf("Hostnames = %v, want [live.example.com]", cfg.Hostnames)
	}
}

// TestParseConfigRejectsMissingTunnel preserves ReadConfig's three-outcome
// contract across the parseConfig split: a config without a tunnel field is
// "exists but not understandable", never "absent".
func TestParseConfigRejectsMissingTunnel(t *testing.T) {
	_, found, err := parseConfig([]byte("ingress:\n  - service: http_status:404\n"), "test.yml")
	if found || err == nil {
		t.Fatalf("found=%v err=%v, want found=false with an error", found, err)
	}
	if !strings.Contains(err.Error(), "tunnel 字段") {
		t.Errorf("error does not name the missing field: %v", err)
	}
}

// TestParseConfigRejectsFlagLikeTunnelID: the tunnel value becomes an argument
// to cloudflared, so a leading dash would be promoted from data to flag.
func TestParseConfigRejectsFlagLikeTunnelID(t *testing.T) {
	_, found, err := parseConfig([]byte("tunnel: --token=evil\n"), "test.yml")
	if found || err == nil {
		t.Fatalf("found=%v err=%v, want found=false with an error", found, err)
	}
}

// TestJoinHostnames pins the display convention used by every one-line surface
// (journal note, down preamble, diagnostics), and that empty stays empty --
// the "该隧道" fallback is each caller's decision, not this function's.
func TestJoinHostnames(t *testing.T) {
	if got := JoinHostnames([]string{"a.example.com", "b.example.com"}); got != "a.example.com、b.example.com" {
		t.Errorf("JoinHostnames = %q", got)
	}
	if got := JoinHostnames(nil); got != "" {
		t.Errorf("JoinHostnames(nil) = %q, want empty", got)
	}
}
