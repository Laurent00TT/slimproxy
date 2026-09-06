// slimproxy patch: guard tests for the catalog-client injection (see
// SLIMPROXY_PATCHES.md 第 9 条). Both remote fetchers must dial with the
// client handed to SetModelsHTTPClient, and nil must hand them back their
// default. An upstream rebase that restores the fetchers' private
// `&http.Client{}` compiles cleanly and fails only here -- and in production
// only on the host whose route out is the proxy the bare client ignores.
// The root module's forkcheck package runs these from `go test ./...`.
package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// recordingTransport counts what passes through it and forwards unchanged.
// Identity is the assertion, not behaviour: the injected client must be the
// one dialing, whatever it happens to be configured to do.
type recordingTransport struct {
	hits atomic.Int32
	next http.RoundTripper
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.hits.Add(1)
	return r.next.RoundTrip(req)
}

// serveCatalog serves body as the catalog and counts how often it was asked.
// The embedded catalogs are served verbatim so the fetchers' own validation
// passes and a nil return can only mean the dial itself did not happen.
func serveCatalog(t *testing.T, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

func injectRecordingClient(t *testing.T) *recordingTransport {
	t.Helper()
	rec := &recordingTransport{next: http.DefaultTransport}
	SetModelsHTTPClient(&http.Client{Transport: rec})
	t.Cleanup(func() { SetModelsHTTPClient(nil) })
	return rec
}

func pinModelsURLs(t *testing.T, urls ...string) {
	t.Helper()
	previous := modelsURLs
	modelsURLs = urls
	t.Cleanup(func() { modelsURLs = previous })
}

func pinCodexClientModelsURLs(t *testing.T, urls ...string) {
	t.Helper()
	previous := codexClientModelsURLs
	codexClientModelsURLs = urls
	t.Cleanup(func() { codexClientModelsURLs = previous })
}

// TestModelsFetchUsesInjectedClient: the models.json fetch must go through
// the injected client. This is the assertion that fails when the fetcher
// builds its own client again.
func TestModelsFetchUsesInjectedClient(t *testing.T) {
	srv, served := serveCatalog(t, embeddedModelsJSON)
	pinModelsURLs(t, srv.URL)
	rec := injectRecordingClient(t)

	parsed, from := fetchModelsFromRemote(context.Background())
	if parsed == nil || from != srv.URL {
		t.Fatalf("fetch did not succeed from the test server: parsed=%v from=%q", parsed != nil, from)
	}
	if served.Load() == 0 {
		t.Fatal("the test server was never asked; the fetch went somewhere else")
	}
	if rec.hits.Load() == 0 {
		t.Fatal("the injected client was not used: the fetcher dialed with a client of its own, " +
			"which is exactly the proxy-blind path the patch removes")
	}
}

// TestCodexClientModelsFetchUsesInjectedClient: same contract for the Codex
// client templates, which upstream refreshes on the same schedule through a
// second, equally proxy-blind fetcher.
func TestCodexClientModelsFetchUsesInjectedClient(t *testing.T) {
	srv, served := serveCatalog(t, embeddedCodexClientModelsJSON)
	pinCodexClientModelsURLs(t, srv.URL)
	rec := injectRecordingClient(t)

	data, from := fetchCodexClientModelsFromRemote(context.Background())
	if data == nil || from != srv.URL {
		t.Fatalf("fetch did not succeed from the test server: data=%v from=%q", data != nil, from)
	}
	if served.Load() == 0 {
		t.Fatal("the test server was never asked; the fetch went somewhere else")
	}
	if rec.hits.Load() == 0 {
		t.Fatal("the injected client was not used by the Codex client models fetcher")
	}
}

// TestNilClientRestoresDefaultFetcher: nil must mean "back to the default",
// not "nil pointer at the next refresh" and not "keep the previous client".
func TestNilClientRestoresDefaultFetcher(t *testing.T) {
	srv, served := serveCatalog(t, embeddedModelsJSON)
	pinModelsURLs(t, srv.URL)
	rec := injectRecordingClient(t)
	SetModelsHTTPClient(nil)

	parsed, _ := fetchModelsFromRemote(context.Background())
	if parsed == nil {
		t.Fatal("fetch with the default client failed against the test server")
	}
	if served.Load() == 0 {
		t.Fatal("the test server was never asked with the default client")
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("a nil client did not restore the default: the previously injected client was still dialing (%d hits)", rec.hits.Load())
	}
}
