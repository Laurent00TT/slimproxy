package tunnel

import (
	"net"
	"testing"
)

// realTunnelInfo mirrors the exact shape of `cloudflared tunnel info` output
// (2026.7.x). The values are fictional -- UUIDs are made up and the origin IP
// is from RFC 5737 test space -- only the layout is real, and the layout is
// what the parser is being held to.
const realTunnelInfo = `NAME:     slimproxy
ID:       0f0e0d0c-0b0a-4990-8877-665544332211
CREATED:  2026-07-25 15:30:40

CONNECTOR ID                         CREATED              ARCHITECTURE  VERSION   ORIGIN IP     EDGE
11111111-2222-4333-8444-555555555555 2026-07-25T15:33:27Z windows_amd64 2026.7.3  203.0.113.43 lax10
66666666-7777-4888-9999-aaaaaaaaaaaa 2026-07-25T15:37:12Z windows_amd64 2026.7.3  203.0.113.43 lax01
`

func TestParseTunnelInfoCountsConnectors(t *testing.T) {
	c := ParseTunnelInfo(realTunnelInfo)
	if !c.Parsed {
		t.Fatal("real output reported as unparseable")
	}
	// The "ID:" line carries a UUID too, but only two fields, so it must not
	// be counted.
	if c.Count != 2 {
		t.Errorf("Count = %d, want 2", c.Count)
	}
}

// TestParseTunnelInfoRejectsUnknownFormat is the rule that keeps a format
// change from silently becoming "zero connections", which callers treat as an
// outage -- or worse, as healthy.
func TestParseTunnelInfoRejectsUnknownFormat(t *testing.T) {
	c := ParseTunnelInfo("some future format nobody here has seen")
	if c.Parsed {
		t.Error("unrecognised output reported as successfully parsed")
	}
	if c.Count != 0 {
		t.Errorf("Count = %d on unparseable output, want 0", c.Count)
	}
}

func TestParseTunnelInfoIdleTunnel(t *testing.T) {
	c := ParseTunnelInfo("Your tunnel X does not have any active connection.")
	if !c.Parsed {
		t.Error("an explicit idle message must count as parsed")
	}
	if !c.Idle || c.Count != 0 {
		t.Errorf("idle tunnel parsed as %+v", c)
	}
}

func TestParseTunnelInfoEmptyTable(t *testing.T) {
	c := ParseTunnelInfo("CONNECTOR ID   CREATED   ARCHITECTURE   VERSION\n")
	if !c.Parsed || c.Count != 0 {
		t.Errorf("well-formed empty table parsed as %+v, want parsed with zero", c)
	}
}

// TestParseTunnelInfoFindsFakeIP: an edge address in RFC 2544 space means the
// tunnel runs through a local proxy, where long-lived connections drop.
func TestParseTunnelInfoFindsFakeIP(t *testing.T) {
	withFake := `CONNECTOR ID                         CREATED   ARCH  VER  ORIGIN IP     EDGE
b2c02360-c41d-4a76-918b-bee20be4c3b2 x         y     z    198.18.1.98   nrt10
`
	c := ParseTunnelInfo(withFake)
	if c.FakeIP == "" {
		t.Error("fake-ip edge address not detected")
	}
	if ParseTunnelInfo(realTunnelInfo).FakeIP != "" {
		t.Error("a real edge address was flagged as fake-ip")
	}
}

func TestFakeIPRangeBoundaries(t *testing.T) {
	for _, ip := range []string{"198.18.0.0", "198.18.1.98", "198.19.255.255"} {
		if !fakeIPNet.Contains(net.ParseIP(ip)) {
			t.Errorf("%s should be inside RFC 2544 space", ip)
		}
	}
	for _, ip := range []string{"198.17.255.255", "198.20.0.0", "104.21.90.181"} {
		if fakeIPNet.Contains(net.ParseIP(ip)) {
			t.Errorf("%s should be outside RFC 2544 space", ip)
		}
	}
}

// TestParseTasklistRowIsLanguageIndependent: identifying "no such process" by
// blacklisting the English "INFO:" breaks on a German Windows, which prints
// "INFORMATION:" -- that string does not contain "INFO:".
func TestParseTasklistRowIsLanguageIndependent(t *testing.T) {
	real := `"cloudflared.exe","24600","Console","1","40,172 K"`
	name, ok := parseTasklistRow(real, 24600)
	if !ok || name != "cloudflared.exe" {
		t.Errorf("real row parsed as (%q, %v)", name, ok)
	}

	// Wrong PID in the row must not match, however well-formed.
	if _, ok := parseTasklistRow(real, 999); ok {
		t.Error("a row for another PID was accepted")
	}

	for _, msg := range []string{
		"INFO: No tasks are running which match the specified criteria.",
		"INFORMATION: Es werden keine Aufgaben ausgeführt.",
		"信息: 没有运行的任务匹配指定标准。",
		"",
	} {
		if name, ok := parseTasklistRow(msg, 24600); ok {
			t.Errorf("informational message parsed as a process: %q -> %q", msg, name)
		}
	}
}
