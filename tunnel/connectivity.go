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
//
// Any field will do here because the input is a connector table, where every
// address column is an edge address. Do not reach for it when reading
// cloudflared's own log: those lines carry addresses that are not the edge
// ("ICMP proxy will use 198.18.0.1 as source"), and edgeFakeIP below exists to
// key on the ip= field instead. The two look mergeable and are not.
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

// ---------------------------------------------------------------------------
// Startup failure classification.
//
// This lives beside fakeIPNet rather than beside confirmStarted because the
// cause that matters most is the one this file already knows how to name: an
// edge address in RFC 2544 space means the answer came from something on this
// machine, not from Cloudflare. manager.go keeps the log reading and the
// wiring into the two failure paths.

// startupCause names the reason a launch never registered a connection.
type startupCause int

const (
	// causeUnknown: nothing in the log matched, and the caller must fall back
	// to printing the tail unchanged.
	//
	// Same rule as Connectivity.Parsed above and identityUnknown in
	// manager.go: a named cause is only ever returned on evidence. A
	// confidently wrong diagnosis sends the operator to fix something that is
	// not broken, which costs more than the raw dump it replaced.
	causeUnknown startupCause = iota
	// causeFakeIPEdge: the edge address is inside RFC 2544 space, so DNS for
	// *.argotunnel.com was answered by a local proxy or VPN in fake-ip mode
	// and the connection never left this machine.
	causeFakeIPEdge
	// causePrecheckFailed: cloudflared's own connectivity pre-check named a
	// failing component. Which one it was is the answer, and it used to be
	// buried in the dump.
	causePrecheckFailed
	// causeEdgePoolCollapsed: cloudflared could offer fewer HA connections than
	// were asked for, meaning it resolved fewer distinct edge addresses than
	// exist. That is a fingerprint of causeFakeIPEdge seen from the other side.
	causeEdgePoolCollapsed
	// causeStillRetrying: dial failures, no registration, process alive. The
	// tunnel is not dead -- and will not become usable on its own either, which
	// is the distinction "it failed" does not make.
	causeStillRetrying
)

// startupDiagnosis is a named cause together with the remedy that follows from
// it, rendered as operator-facing lines.
type startupDiagnosis struct {
	Cause startupCause
	// Lines lead with the cause and end with what to do. They are printed
	// ahead of the log tail because the TUI truncates every line to the panel
	// width and shows a limited number of rows (see tui/view.go's result
	// panel): anything arriving after a dozen timestamped log lines, or past
	// the right edge of one, is not read.
	Lines []string
}

func (d startupDiagnosis) known() bool { return d.Cause != causeUnknown }

// The markers below are cloudflared's own log output, not an interface it
// promises to keep. Each is a whole distinctive phrase or a logfmt key, so a
// rewording upstream makes a matcher stop firing -- dropping the message back
// to the plain log tail -- rather than making it fire on something else.
// Degrading to "here is the log" is recoverable; a wrong cause is not.
const (
	precheckMarker    = "precheck"
	haShortfallMarker = "HA connections but I can give you at most"
	dialFailureMarker = "Failed to dial"
)

// classifyStartupLog reads one launch's output and names why it did not connect.
//
// alive says whether the child was still running when the log was read. Only
// causeStillRetrying depends on it: claiming "it is still retrying" about a
// process that has exited would be worse than saying nothing.
func classifyStartupLog(log string, alive bool) startupDiagnosis {
	// A region holding a registration is not a failure this function has any
	// business explaining. confirmStarted never calls it in that case; the
	// guard is here so that a future caller cannot make it lie.
	if strings.Contains(log, connectedMarker) {
		return startupDiagnosis{}
	}

	var (
		fakeEdges []string
		failed    []string
		passed    []string
		target    string
		haWanted  string
		haOffered string
		dialFails int
		dialErr   string
	)
	for _, line := range strings.Split(log, "\n") {
		if ip := edgeFakeIP(line); ip != "" {
			fakeEdges = addUnique(fakeEdges, ip)
		}
		if strings.Contains(line, precheckMarker) {
			if comp := logField(line, "component"); comp != "" {
				switch logField(line, "status") {
				case "fail":
					failed = addUnique(failed, comp)
					if t := logField(line, "target"); t != "" {
						target = t
					}
				case "pass":
					passed = addUnique(passed, comp)
				}
			}
		}
		if strings.Contains(line, haShortfallMarker) {
			// Only overwrite on a successful parse: a second marker line that
			// does not parse must not erase the numbers the first one gave.
			if w, o := haShortfall(line); w != "" {
				haWanted, haOffered = w, o
			}
		}
		if strings.Contains(line, dialFailureMarker) {
			dialFails++
			// Kept as the last one rather than the first: the transport can
			// change between retries, and the most recent attempt is the one
			// the operator is about to reproduce.
			if e := logField(line, "error"); e != "" {
				dialErr = e
			}
		}
	}

	// Ordered by how specific the evidence is, not by how alarming it looks. A
	// fake-ip edge explains the pre-check rows and the HA shortfall as well, so
	// it must not be reported as one of them.
	switch {
	case len(fakeEdges) > 0:
		return fakeIPDiagnosis(fakeEdges, failed, haWanted, haOffered)
	case len(failed) > 0:
		return precheckDiagnosis(failed, passed, target)
	case haWanted != "":
		return edgePoolDiagnosis(haWanted, haOffered)
	case dialFails > 0 && alive:
		return retryingDiagnosis(dialFails, dialErr)
	}
	return startupDiagnosis{}
}

// fakeIPDiagnosis is the case that cost an afternoon: region1/2.v2.argotunnel.com
// resolved to 198.18.0.11 and .12, no proxy rule matched argotunnel.com, and the
// traffic was relayed by a node whose UDP relay carries QUIC's first stateless
// round trip but not the rest of the handshake.
//
// It deliberately does not offer --protocol http2 as the remedy. Measured on
// this incident, TCP 7844 completed a TLS handshake through the proxy but never
// negotiated ALPN h2, and writing a single HTTP/2 preface frame failed 6 times
// out of 6. Suggesting it here would send the operator down a path that does
// not work and looks like it should.
func fakeIPDiagnosis(edges, failedChecks []string, haWanted, haOffered string) startupDiagnosis {
	lines := []string{
		fmt.Sprintf(i18n.T(
			"原因：边缘地址 %s 落在 198.18.0.0/15（RFC 2544 保留段）",
			"Cause: edge address %s is inside 198.18.0.0/15 (RFC 2544 reserved space)"),
			strings.Join(edges, ", ")),
		i18n.T(
			"  真实 Cloudflare 边缘不在此段：本机代理或 VPN 的 fake-ip 在应答 *.argotunnel.com 的 DNS",
			"  a real edge is never there: a local proxy or VPN in fake-ip mode is answering DNS for *.argotunnel.com"),
	}
	if haWanted != "" {
		lines = append(lines, fmt.Sprintf(i18n.T(
			"  佐证：请求 %s 条 HA 连接只拿到 %s 条，边缘地址池已塌缩",
			"  corroborated: %s HA connections requested, only %s available -- the edge address pool collapsed"),
			haWanted, haOffered))
	}
	if len(failedChecks) > 0 {
		lines = append(lines, fmt.Sprintf(i18n.T(
			"  cloudflared 自检失败项：%s",
			"  cloudflared's own pre-check failing: %s"), strings.Join(failedChecks, ", ")))
	}
	lines = append(lines,
		i18n.T(
			"处理：让 *.argotunnel.com 直连，同时把它加入 fake-ip 排除列表",
			"Fix: route *.argotunnel.com direct, and exempt it from fake-ip as well"),
		i18n.T(
			"  两步缺一不可：只加直连规则时 DNS 仍返回 198.18.x.x，规则永远匹配不上",
			"  both halves are needed: with only a direct rule DNS still answers 198.18.x.x and the rule never matches"),
		i18n.T(
			"  详见 deploy/TUNNEL.md 的 \"If it never connects\"",
			"  see \"If it never connects\" in deploy/TUNNEL.md"))
	return startupDiagnosis{Cause: causeFakeIPEdge, Lines: lines}
}

// precheckDiagnosis names the component cloudflared itself reported broken.
func precheckDiagnosis(failed, passed []string, target string) startupDiagnosis {
	head := fmt.Sprintf(i18n.T(
		"原因：cloudflared 自检未通过：%s",
		"Cause: cloudflared's own pre-check failed: %s"), strings.Join(failed, ", "))
	if target != "" {
		head += fmt.Sprintf(i18n.T("（目标 %s）", " (target %s)"), target)
	}
	d := startupDiagnosis{Cause: causePrecheckFailed, Lines: []string{head}}

	udpFailed := anyContains(failed, "UDP")
	switch {
	case udpFailed && anyContains(passed, "TCP"):
		// The one shape where naming the transport is safe: cloudflared's
		// initial protocol is QUIC and it does not fall back on its own, so it
		// retries a transport that is never going to work.
		d.Lines = append(d.Lines,
			i18n.T(
				"处理：QUIC/UDP 到边缘不通，cloudflared 不会自行回退，只会一直重试；让 *.argotunnel.com 绕开本机代理",
				"Fix: QUIC/UDP cannot reach the edge and cloudflared never falls back on its own, it just retries; take *.argotunnel.com off the local proxy"),
			// Confirmation only. HTTP/2 through a proxy has been measured on
			// this project at roughly one drop every few minutes, each one a
			// window of 502s -- and in the fake-ip incident it did not connect
			// at all.
			i18n.T(
				"  --protocol http2 只用于确认这个判断，不是修复：经代理的 HTTP/2 会反复掉线",
				"  --protocol http2 only confirms the diagnosis, it is not the fix: HTTP/2 through a proxy keeps dropping"))
	case udpFailed && anyContains(failed, "TCP"):
		d.Lines = append(d.Lines, i18n.T(
			"处理：UDP 与 TCP（7844）都到不了边缘，先查本机代理或防火墙是否拦下了 *.argotunnel.com",
			"Fix: neither UDP nor TCP (7844) reaches the edge -- check whether a local proxy or firewall intercepts *.argotunnel.com"))
	default:
		d.Lines = append(d.Lines, i18n.T(
			"处理：先修复该项再重试 tunnel up；排查步骤见 deploy/TUNNEL.md",
			"Fix: repair that component, then run tunnel up again; deploy/TUNNEL.md has the steps"))
	}
	return d
}

// edgePoolDiagnosis reports the HA shortfall on its own.
//
// Without a fake-ip address in the log this is a symptom, not a mechanism, so
// the remedy is the lookup that settles it rather than a change to make.
func edgePoolDiagnosis(wanted, offered string) startupDiagnosis {
	return startupDiagnosis{Cause: causeEdgePoolCollapsed, Lines: []string{
		fmt.Sprintf(i18n.T(
			"原因：边缘地址池塌缩：请求 %s 条 HA 连接，只拿到 %s 条",
			"Cause: the edge address pool collapsed: %s HA connections requested, only %s available"), wanted, offered),
		// Named as the likeliest explanation, not as the finding. A shortfall
		// on its own is also what a genuinely small edge pool looks like, and
		// this log carried no address to tell the two apart -- so the next
		// line is a lookup that settles it, not a change to make.
		i18n.T(
			"  最常见的原因是 *.argotunnel.com 的 DNS 被本机接管（fake-ip 代理），但日志里没有地址可以坐实",
			"  the usual cause is DNS for *.argotunnel.com being answered locally (a fake-ip proxy), but no address in this log confirms it"),
		i18n.T(
			"处理：nslookup region1.v2.argotunnel.com，确认返回的是真实边缘地址而不是 198.18.x.x",
			"Fix: run nslookup region1.v2.argotunnel.com and check the answer is a real edge address, not 198.18.x.x"),
	}}
}

// retryingDiagnosis separates "still retrying" from "dead".
//
// They want different actions, and the message did not distinguish them:
// cloudflared does not exit on a dial failure, so a launch that will never
// succeed still leaves a live process to point at.
func retryingDiagnosis(dialFails int, dialErr string) startupDiagnosis {
	d := startupDiagnosis{Cause: causeStillRetrying, Lines: []string{
		fmt.Sprintf(i18n.T(
			"原因：%d 次拨号失败且没有任何连接注册；cloudflared 不会因此退出，只会一直退避重试",
			"Cause: %d dial failures and no registration; cloudflared does not exit on this, it just backs off and retries"), dialFails),
	}}
	if dialErr != "" {
		d.Lines = append(d.Lines, fmt.Sprintf(i18n.T(
			"  最近一次拨号错误：%s", "  most recent dial error: %s"), dialErr))
	}
	d.Lines = append(d.Lines, i18n.T(
		"处理：再等下去不会自行恢复；先修通到 *.argotunnel.com 的网络路径，再重试 tunnel up",
		"Fix: waiting will not recover it; repair the network path to *.argotunnel.com, then run tunnel up again"))
	return d
}

// edgeFakeIP returns the edge address of one log line when it is in RFC 2544
// space.
//
// Keyed on cloudflared's own ip= field rather than on any address in the line.
// "ICMP proxy will use 198.18.0.1 as source for IPv4" carries such an address
// too, and means only that the default route points at the proxy -- not that
// the edge was resolved to it. Claiming the fake-ip cause from that line would
// be a diagnosis with nothing behind it.
func edgeFakeIP(line string) string {
	v := logField(line, "ip")
	if v == "" {
		return ""
	}
	if ip := net.ParseIP(v); ip == nil || !fakeIPNet.Contains(ip) {
		return ""
	}
	return v
}

// logField reads one logfmt key=value field out of a cloudflared log line.
//
// Two rules, both guarding the same mistake -- reading something that is not
// this field and attributing it to this one, which is how a classifier starts
// naming causes out of nothing:
//
//   - the key must start at a field boundary, because "ip=" also occurs at the
//     end of other keys ("connIndex=0" ends in "ndex=0");
//   - a match inside a quoted value does not count, because cloudflared quotes
//     free-form prose into details= and error=, and prose can contain anything
//     including another key's spelling.
//
// A line whose quotes never close -- the log's final line while cloudflared is
// still writing it -- yields nothing for the fields after the break. Silence is
// the right failure here; a value read out of half a line is not.
func logField(line, key string) string {
	pat := key + "="
	quoted := false
	for i := 0; i+len(pat) <= len(line); i++ {
		if line[i] == '"' {
			quoted = !quoted
			continue
		}
		if quoted || line[i:i+len(pat)] != pat {
			continue
		}
		if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
			return logValue(line[i+len(pat):])
		}
	}
	return ""
}

// logValue reads a logfmt value: quoted when it contains spaces, bare
// otherwise. A value whose closing quote is missing -- a truncated final line,
// which the log can end on while cloudflared is still writing -- yields the
// remainder rather than nothing.
func logValue(s string) string {
	if strings.HasPrefix(s, "\"") {
		if end := strings.IndexByte(s[1:], '"'); end >= 0 {
			return s[1 : 1+end]
		}
		return strings.TrimSpace(s[1:])
	}
	if end := strings.IndexAny(s, " \t"); end >= 0 {
		return s[:end]
	}
	return s
}

// haShortfall reads the two numbers out of
// "You requested 4 HA connections but I can give you at most 2."
//
// Both must look like numbers or neither is reported. If the sentence is ever
// reworded the words move, and quoting whatever happens to sit in those
// positions would state a count upstream never gave.
func haShortfall(line string) (wanted, offered string) {
	f := strings.Fields(line)
	for i, w := range f {
		if w == "requested" && i+1 < len(f) {
			wanted = f[i+1]
		}
	}
	if len(f) > 0 {
		offered = strings.TrimRight(f[len(f)-1], ".")
	}
	if !isNumber(wanted) || !isNumber(offered) {
		return "", ""
	}
	return wanted, offered
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func addUnique(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

func anyContains(list []string, sub string) bool {
	for _, e := range list {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}
