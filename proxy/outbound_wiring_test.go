package proxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// TestFallbackDirectRoutesEngineThroughRelay: with proxy-fallback-direct on,
// the engine must be handed the relay's loopback address, not the raw
// proxy-url. The engine resolves its proxy exactly once, so if the raw URL
// leaks through here the whole feature silently does not exist -- the same
// class of wiring gap the in-flight and health tests exist to catch.
func TestFallbackDirectRoutesEngineThroughRelay(t *testing.T) {
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })

	raw := fmt.Sprintf("http://127.0.0.1:%d", freeTestPort(t))
	rt, err := Build(Config{
		Host:                "127.0.0.1",
		Port:                freeTestPort(t),
		APIKeys:             []string{"k"},
		AuthDir:             filepath.Join(t.TempDir(), "auths"),
		LogDir:              t.TempDir(),
		JournalDays:         JournalDisabled,
		ProxyURL:            raw,
		ProxyFallbackDirect: true,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.closeRelay()

	if rt.relay == nil {
		t.Fatal("no relay was started; the engine is dialing the raw proxy-url and the fallback does not exist")
	}

	// The relay must actually be listening where its URL claims.
	addr := strings.TrimPrefix(rt.relay.URL(), "http://")
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("relay URL %s is not listening: %v", rt.relay.URL(), err)
	}
	_ = c.Close()

	// The materialized config is what the engine (and its file watcher) read;
	// it must carry the relay's address, not the raw one.
	body, err := os.ReadFile(rt.ConfigPath)
	if err != nil {
		t.Fatalf("reading effective config: %v", err)
	}
	if !strings.Contains(string(body), rt.relay.URL()) {
		t.Errorf("effective config does not point at the relay %s", rt.relay.URL())
	}
	if strings.Contains(string(body), raw) {
		t.Errorf("effective config still names the raw proxy-url %s; the engine would resolve it once and never follow a VPN switch", raw)
	}
}

// TestNoRelayWithoutTheFlag: the flag off must mean the raw proxy-url reaches
// the engine untouched -- an always-on relay would silently change the
// meaning of an existing setting.
func TestNoRelayWithoutTheFlag(t *testing.T) {
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })

	raw := fmt.Sprintf("http://127.0.0.1:%d", freeTestPort(t))
	rt, err := Build(Config{
		Host:        "127.0.0.1",
		Port:        freeTestPort(t),
		APIKeys:     []string{"k"},
		AuthDir:     filepath.Join(t.TempDir(), "auths"),
		LogDir:      t.TempDir(),
		JournalDays: JournalDisabled,
		ProxyURL:    raw,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rt.relay != nil {
		t.Fatal("a relay was started without proxy-fallback-direct")
	}
	body, err := os.ReadFile(rt.ConfigPath)
	if err != nil {
		t.Fatalf("reading effective config: %v", err)
	}
	if !strings.Contains(string(body), raw) {
		t.Errorf("effective config lost the raw proxy-url %s", raw)
	}
}

func TestValidateProxyFallbackDirect(t *testing.T) {
	base := Config{Port: 8317, APIKeys: []string{"k"}}

	c := base
	c.ProxyFallbackDirect = true
	if err := c.Validate(); err == nil {
		t.Error("fallback with no proxy-url validated; the flag names nothing to fall back from")
	}

	c.ProxyURL = "socks5://127.0.0.1:1080"
	if err := c.Validate(); err == nil {
		t.Error("fallback with a socks5 proxy-url validated; the relay chains with a plain CONNECT and cannot speak socks5")
	}

	c.ProxyURL = "http://10.0.0.8:7897"
	if err := c.Validate(); err == nil {
		t.Error("fallback with a non-loopback proxy-url validated; the loopback-tuned probe would misread its round-trip as down and silently bypass it")
	}

	c.ProxyURL = "http://127.0.0.1:7897"
	if err := c.Validate(); err != nil {
		t.Errorf("fallback with a loopback http proxy-url refused: %v", err)
	}

	// Without the flag, socks5 stays as accepted as it always was.
	c = base
	c.ProxyURL = "socks5://127.0.0.1:1080"
	if err := c.Validate(); err != nil {
		t.Errorf("socks5 proxy-url without the flag refused: %v", err)
	}
}
