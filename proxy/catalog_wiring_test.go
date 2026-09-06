package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// The seam is replaced for the whole test binary, not per test: two other
// tests in this package drive Runtime.Run, and left alone the real updater
// would dial GitHub from a suite CONTRIBUTING.md promises is hermetic. The
// recorder keeps exactly what the wiring test needs -- which client Run
// handed over -- and does nothing else.
var catalogStarts struct {
	mu      sync.Mutex
	clients []*http.Client
}

func init() {
	startCatalogUpdater = func(_ context.Context, c *http.Client) {
		catalogStarts.mu.Lock()
		defer catalogStarts.mu.Unlock()
		catalogStarts.clients = append(catalogStarts.clients, c)
	}
}

func catalogStartedWith(c *http.Client) bool {
	catalogStarts.mu.Lock()
	defer catalogStarts.mu.Unlock()
	for _, got := range catalogStarts.clients {
		if got == c {
			return true
		}
	}
	return false
}

// catalogProxyOf answers which proxy the runtime's catalog client would dial
// through for the catalog URL. Asked of the transport rather than read off
// the config, because the transport is what dials.
func catalogProxyOf(t *testing.T, rt *Runtime) string {
	t.Helper()
	if rt.catalog == nil {
		t.Fatal("Build left no catalog client on the runtime; Run would start the refresh with nil and the fetchers would fall back to their proxy-blind default")
	}
	transport, ok := rt.catalog.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("catalog client transport is %T, not *http.Transport", rt.catalog.Transport)
	}
	req := httptest.NewRequest(http.MethodGet, "https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json", nil)
	u, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy: %v", err)
	}
	if u == nil {
		return ""
	}
	return u.String()
}

func buildForCatalog(t *testing.T, proxyURL string, fallback bool) *Runtime {
	t.Helper()
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })
	rt, err := Build(Config{
		Host:                "127.0.0.1",
		Port:                freeTestPort(t),
		APIKeys:             []string{"k"},
		AuthDir:             filepath.Join(t.TempDir(), "auths"),
		LogDir:              t.TempDir(),
		JournalDays:         JournalDisabled,
		ProxyURL:            proxyURL,
		ProxyFallbackDirect: fallback,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(rt.closeRelay)
	return rt
}

// TestCatalogClientFollowsRelay: with proxy-fallback-direct on, the catalog
// refresh must dial through the relay -- the address the engine was given --
// not the raw proxy-url. A client built from the raw URL would be the one
// outbound path in the process that does not follow a VPN switch: requests
// would flow and the refresh would fail, on exactly the schedule that makes
// it look like a flaky upstream rather than a wiring gap.
func TestCatalogClientFollowsRelay(t *testing.T) {
	raw := fmt.Sprintf("http://127.0.0.1:%d", freeTestPort(t))
	rt := buildForCatalog(t, raw, true)
	if rt.relay == nil {
		t.Fatal("no relay was started")
	}
	got := catalogProxyOf(t, rt)
	if got != rt.relay.URL() {
		t.Errorf("catalog client proxies via %q, want the relay %q", got, rt.relay.URL())
	}
	if got == raw {
		t.Errorf("catalog client still names the raw proxy-url %s; it was built before the relay substitution", raw)
	}
}

// TestCatalogClientUsesRawProxyURLWithoutFallback: without the flag, the raw
// proxy-url is the engine's path and must be the refresh's too.
func TestCatalogClientUsesRawProxyURLWithoutFallback(t *testing.T) {
	raw := fmt.Sprintf("http://127.0.0.1:%d", freeTestPort(t))
	rt := buildForCatalog(t, raw, false)
	if got := catalogProxyOf(t, rt); got != raw {
		t.Errorf("catalog client proxies via %q, want %q", got, raw)
	}
}

// TestCatalogClientInheritsEnvironmentWithoutProxyURL: no proxy-url means
// Go's default transport, so HTTPS_PROXY from the environment applies to the
// refresh exactly as it applies to the executors. An explicit transport here
// would silently make the refresh the only thing ignoring that variable.
func TestCatalogClientInheritsEnvironmentWithoutProxyURL(t *testing.T) {
	rt := buildForCatalog(t, "", false)
	if rt.catalog == nil {
		t.Fatal("Build left no catalog client on the runtime")
	}
	if rt.catalog.Transport != nil {
		t.Errorf("catalog client has an explicit transport %T with no proxy-url configured", rt.catalog.Transport)
	}
}

// TestRunStartsCatalogUpdaterWithBuildClient pins the hand-off: the client
// Build assembled is the one Run starts the refresh with. Each half is
// trivially correct on its own; what this catches is the two drifting apart
// -- Run passing nil, or a second client built from the raw URL.
func TestRunStartsCatalogUpdaterWithBuildClient(t *testing.T) {
	raw := fmt.Sprintf("http://127.0.0.1:%d", freeTestPort(t))
	rt := buildForCatalog(t, raw, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rt.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("代理没有在 10 秒内停止")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for !catalogStartedWith(rt.catalog) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !catalogStartedWith(rt.catalog) {
		t.Fatal("Run did not start the catalog refresh with the client Build assembled; the refresh either never starts or dials on a path the engine is not on")
	}
}
