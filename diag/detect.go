package diag

import "github.com/Laurent00TT/slimproxy/tunnel"

// DetectTunnel reports the cloudflared configuration.
//
// A thin forward to the tunnel package, which owns this. Duplicating the
// parsing here is how the auth-dir bug happened: two places deriving the same
// fact, disagreeing, and one of them reporting healthy while the other acted on
// something else.
func DetectTunnel() (tunnel.Config, bool, error) { return tunnel.ReadConfig() }
