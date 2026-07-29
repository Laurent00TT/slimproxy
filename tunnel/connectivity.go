package tunnel

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// fakeIPNet is RFC 2544 benchmark space, 198.18.0.0/15.
//
// Never a real destination. Local proxies and VPNs in fake-ip mode answer DNS
// with it and then intercept the connection. A tunnel whose edge address falls
// in this range is running through such a proxy, and its long-lived connections
// tend to drop repeatedly -- observed in this project as roughly one drop every
// few minutes, each one a window of 502s.
var fakeIPNet = &net.IPNet{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)}

// Connectivity is what `cloudflared tunnel info` reports.
type Connectivity struct {
	// Count is the number of edge connections.
	Count int
	// Parsed is false when the output could not be read. It must not be
	// conflated with Count == 0: a format change upstream would otherwise turn
	// a healthy tunnel into a reported outage, or the reverse.
	Parsed bool
	// Idle is true when cloudflared explicitly said there are no connections.
	Idle bool
	// FakeIP names an edge address inside RFC 2544 space, when one appears.
	FakeIP string
}

// ParseTunnelInfo reads `cloudflared tunnel info` output.
//
// Exported so the diagnostics package uses this one implementation rather than
// keeping its own. Two places deriving the same fact from the same text is how
// they end up disagreeing, and only one of them gets fixed.
func ParseTunnelInfo(text string) Connectivity {
	if strings.Contains(text, "does not have any active connection") {
		return Connectivity{Parsed: true, Idle: true}
	}
	// The header is the only stable marker that this is a connector table at
	// all. Without it, returning a count would be inventing a number.
	if !strings.Contains(text, "CONNECTOR ID") {
		return Connectivity{}
	}

	c := Connectivity{Parsed: true}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		// A connector row starts with a UUID and carries further columns; the
		// "ID:" summary line holds a UUID too but has only two fields.
		if len(f) >= 4 && len(f[0]) == 36 && strings.Count(f[0], "-") == 4 {
			c.Count++
		}
	}
	if ip := findFakeIP(text); ip != "" {
		c.FakeIP = ip
	}
	return c
}

// findFakeIP returns the first RFC 2544 address in the text, if any.
func findFakeIP(text string) string {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ','
	}) {
		if ip := net.ParseIP(strings.TrimSpace(field)); ip != nil && fakeIPNet.Contains(ip) {
			return field
		}
	}
	return ""
}

// queryConnectivity runs `cloudflared tunnel info` and parses it.
func (m *Manager) queryConnectivity(ctx context.Context, tunnelID string) (Connectivity, error) {
	bin, err := m.resolveBinary()
	if err != nil {
		return Connectivity{}, err
	}
	out, err := exec.CommandContext(ctx, bin, "tunnel", "info", tunnelID).CombinedOutput()
	text := string(out)
	if err != nil {
		// Include err itself: a command killed by the context timeout produces
		// no output, and reporting an empty string after the colon says nothing.
		detail := err.Error()
		if line := firstLine(text); line != "" {
			detail += "（" + line + "）"
		}
		return Connectivity{}, fmt.Errorf(i18n.T("查询失败: %s。该查询需要网络和 cert.pem", "query failed: %s. This query needs network access and cert.pem"), detail)
	}
	return ParseTunnelInfo(text), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
