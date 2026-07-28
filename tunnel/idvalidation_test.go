package tunnel

import "testing"

// TestTunnelIDCannotBecomeAFlag.
//
// The id is passed to cloudflared as a command-line argument. exec.Command uses
// no shell, so this is not shell injection -- it is argument injection, and
// that is enough: `tunnel: "--token=<attacker's>"` runs a tunnel under someone
// else's Cloudflare account with this machine behind it.
func TestTunnelIDCannotBecomeAFlag(t *testing.T) {
	for _, bad := range []string{
		"--token=eyJhIjoi",
		"-config=/tmp/evil.yml",
		"--url=http://evil",
		" --token=x",
		"",
		"has space",
		"semi;colon",
	} {
		if validTunnelID(bad) {
			t.Errorf("%q 被接受了，但它会改变 cloudflared 命令的含义", bad)
		}
	}
}

// TestRealTunnelIdentifiersStillWork guards against a pattern so strict it
// breaks ordinary configurations.
func TestRealTunnelIdentifiersStillWork(t *testing.T) {
	for _, good := range []string{
		"b7c1e2f0-1111-2222-3333-444455556666",
		"my-tunnel",
		"prod.tunnel_01",
		"a",
	} {
		if !validTunnelID(good) {
			t.Errorf("%q 是合法的隧道标识，不应被拒绝", good)
		}
	}
}
