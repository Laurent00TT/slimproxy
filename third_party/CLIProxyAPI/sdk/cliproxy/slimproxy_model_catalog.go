// slimproxy patch: model catalog refresh for SDK embedders
// (SLIMPROXY_PATCHES.md 第 7 条 and 第 9 条).
package cliproxy

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// NewModelCatalogClient builds the HTTP client the model catalog refresh
// should fetch with, from the same proxy-url every executor's transport is
// built from -- util.SetProxy is the one place upstream turns that setting
// into a transport, and going through it is what keeps the two paths equal.
//
// An empty proxy-url leaves the transport nil on purpose: Go's default
// transport then applies, and with it HTTP(S)_PROXY from the environment,
// which is exactly what the executors get in that case.
func NewModelCatalogClient(cfg *config.Config) *http.Client {
	client := &http.Client{}
	if cfg == nil {
		return client
	}
	return util.SetProxy(&cfg.SDKConfig, client)
}

// StartModelCatalogUpdater starts the background refresh of the model catalog
// -- models.json, and the Codex client templates that ride the same schedule
// -- with client doing every fetch.
//
// The upstream binary starts these from its own main (cmd/server/main.go,
// startModelCatalogUpdaters); the SDK never does. An embedder that does not
// call this serves the catalog compiled into the binary for the life of the
// process, so a model released after the build is unknown to it until the
// next rebuild. Idempotent: the registry's updaters start once per process,
// and the client is read at fetch time, so a later call re-points the
// fetches without restarting them.
func StartModelCatalogUpdater(ctx context.Context, client *http.Client) {
	if ctx == nil {
		ctx = context.Background()
	}
	registry.SetModelsHTTPClient(client)
	registry.StartCodexClientModelsUpdater(ctx)
	registry.StartModelsUpdater(ctx)
}
