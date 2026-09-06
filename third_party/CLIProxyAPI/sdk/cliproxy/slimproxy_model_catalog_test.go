// slimproxy patch: guard tests for the SDK-side catalog refresh entry points
// (SLIMPROXY_PATCHES.md 第 8 条 and 第 9 条). The root module's forkcheck
// package runs them from `go test ./...`.
package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// catalogProxyFor answers which proxy the client's transport would pick for
// a catalog URL -- the transport's decision, not the config's, because the
// config is what the test set and the transport is what dials.
func catalogProxyFor(t *testing.T, client *http.Client) string {
	t.Helper()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("catalog client transport is %T, not *http.Transport", client.Transport)
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

// TestNewModelCatalogClientHonoursProxyURL: the catalog client must dial
// through the configured proxy-url, the way every executor does.
func TestNewModelCatalogClientHonoursProxyURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.ProxyURL = "http://127.0.0.1:7897"
	client := NewModelCatalogClient(cfg)
	if got := catalogProxyFor(t, client); got != cfg.ProxyURL {
		t.Fatalf("catalog client proxies via %q, want %q -- the refresh would leave the process on a different path than the requests", got, cfg.ProxyURL)
	}
}

// TestNewModelCatalogClientEmptyProxyKeepsDefaultTransport: no proxy-url
// must mean Go's default transport (environment proxies honoured), not an
// explicit direct transport that would ignore HTTPS_PROXY where the
// executors respect it.
func TestNewModelCatalogClientEmptyProxyKeepsDefaultTransport(t *testing.T) {
	if tr := NewModelCatalogClient(&config.Config{}).Transport; tr != nil {
		t.Fatalf("empty proxy-url produced an explicit transport %T; environment proxies would be bypassed", tr)
	}
	if tr := NewModelCatalogClient(nil).Transport; tr != nil {
		t.Fatalf("nil config produced an explicit transport %T", tr)
	}
}

// failingTransport refuses every request without touching the network and
// counts the attempts. The startup refresh runs on a goroutine the moment the
// updater starts; the count is how the test observes it went through the
// client it was handed rather than through one of the fetchers' own.
type failingTransport struct{ hits atomic.Int32 }

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.hits.Add(1)
	return nil, errors.New("test transport: no network")
}

// TestStartModelCatalogUpdaterFetchesWithGivenClient pins the hand-off from
// the SDK entry point to the registry's fetchers. The once-guards inside the
// registry make this the only test in the package that may start the
// updaters; the failing transport keeps it off the network.
func TestStartModelCatalogUpdaterFetchesWithGivenClient(t *testing.T) {
	stub := &failingTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartModelCatalogUpdater(ctx, &http.Client{Transport: stub})

	deadline := time.Now().Add(10 * time.Second)
	for stub.hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.hits.Load() == 0 {
		t.Fatal("the startup refresh never dialed through the client handed to StartModelCatalogUpdater; " +
			"either the updaters did not start or they fetched with a client of their own")
	}
}
