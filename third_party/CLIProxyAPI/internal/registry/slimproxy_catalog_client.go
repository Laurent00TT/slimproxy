// slimproxy patch: the catalog fetchers dial with an injectable client
// (SLIMPROXY_PATCHES.md 第 9 条).
//
// Both remote catalog fetchers -- models.json and the Codex client templates
// -- built a bare &http.Client{} per refresh. That client knows nothing about
// proxy-url: every executor's transport goes through util.SetProxy, the
// fetchers' never did. On a host whose only route out is that proxy the
// refresh therefore failed at startup and every three hours while requests
// flowed normally, and because the failure is a Warnf the process kept
// serving the catalog compiled into the binary without anyone being told.
//
// The client is injected rather than derived here because the embedder owns
// the outbound policy: slimproxy replaces the proxy address with a loopback
// relay's at build time, and a client built from the raw setting would be the
// one outbound path in the process that does not follow a VPN switch.
package registry

import (
	"net/http"
	"sync/atomic"
)

var catalogClient atomic.Pointer[http.Client]

// SetModelsHTTPClient sets the client every subsequent catalog fetch dials
// with. It is read at fetch time rather than at updater start, so it may be
// set before or after StartModelsUpdater and re-pointed while the updater
// runs. nil restores the default: a fresh client per refresh on Go's default
// transport, environment proxies honoured.
func SetModelsHTTPClient(client *http.Client) {
	catalogClient.Store(client)
}

// catalogHTTPClient is what the fetchers dial with. An injected client that
// carries no Timeout of its own is still bounded: the fetchers wrap every
// request in a modelsFetchTimeout context, so the default's Timeout is a
// belt on top of those braces, not the only limit.
func catalogHTTPClient() *http.Client {
	if c := catalogClient.Load(); c != nil {
		return c
	}
	return &http.Client{Timeout: modelsFetchTimeout}
}
