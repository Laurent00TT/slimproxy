package diag

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// fakeDNS stands in for the resolver so these tests answer questions about the
// code rather than about the machine running them. Asking the real resolver
// here would make the outcome depend on whether the developer's own box happens
// to be running a fake-ip proxy, which is the condition under test.
type fakeDNS struct {
	answers map[string][]string
	errs    map[string]error
	calls   int
}

func (f *fakeDNS) install(t *testing.T) {
	t.Helper()
	prev := lookupIPv4
	lookupIPv4 = func(_ context.Context, host string) ([]net.IP, error) {
		f.calls++
		if err, ok := f.errs[host]; ok {
			return nil, err
		}
		var ips []net.IP
		for _, s := range f.answers[host] {
			ips = append(ips, net.ParseIP(s))
		}
		return ips, nil
	}
	t.Cleanup(func() { lookupIPv4 = prev })
}

const (
	region1 = "region1.v2.argotunnel.com"
	region2 = "region2.v2.argotunnel.com"
)

// TestTunnelEdgeDNS pins the check against the outage it was written for: the
// tunnel edge resolving into RFC 2544 space while upstream-dns, which only ever
// asks about api.anthropic.com, reported everything healthy.
//
// The cases assert the reason, not just the level -- a Warn that does not name
// both halves of the fix leaves the operator adding a direct rule that can
// never match, which is where the original incident's first hour went.
func TestTunnelEdgeDNS(t *testing.T) {
	resolveErr := &net.DNSError{Err: "no such host", Name: region1, IsNotFound: true}

	for _, tc := range []struct {
		name      string
		target    Target
		answers   map[string][]string
		errs      map[string]error
		cancel    bool
		want      Level
		detail    []string
		notDetail []string
		remedy    []string
		wantCalls int
	}{
		{
			// The incident itself.
			name:   "fake-ip 应答",
			target: Target{TunnelName: "slim"},
			answers: map[string][]string{
				region1: {"198.18.0.11"},
				region2: {"198.18.0.12"},
			},
			want: Warn,
			// The address, paired with the region that answered it: the finding
			// is the evidence, and a level with no address is an assertion.
			detail: []string{"region1 → 198.18.0.11", "region2 → 198.18.0.12", "198.18.0.0/15"},
			// Both halves. A direct rule alone still receives 198.18.x.x and
			// therefore never matches the rule that was just added.
			remedy:    []string{"argotunnel.com", "fake-ip"},
			wantCalls: 2,
		},
		{
			name:   "真实边缘地址",
			target: Target{TunnelName: "slim"},
			answers: map[string][]string{
				region1: {"198.41.192.47"},
				region2: {"198.41.200.113"},
			},
			want:      Pass,
			detail:    []string{"198.41.192.47"},
			notDetail: []string{"198.18."},
			wantCalls: 2,
		},
		{
			// Not resolving is a different finding from resolving to a fake
			// address, and it has a different fix. Reporting it as the latter
			// would point the operator at proxy rules during a DNS outage.
			name:      "解析失败不是劫持",
			target:    Target{TunnelName: "slim"},
			errs:      map[string]error{region1: resolveErr, region2: resolveErr},
			want:      Unknown,
			detail:    []string{"region1", "region2"},
			notDetail: []string{"198.18.0.0/15"},
			remedy:    []string{"nslookup"},
			wantCalls: 2,
		},
		{
			// Reporting an edge-DNS problem on a machine with no tunnel is
			// noise, and the resolver must not even be asked.
			name:      "未配置隧道时不查 DNS",
			target:    Target{},
			want:      Pass,
			detail:    []string{"未配置隧道"},
			wantCalls: 0,
		},
		{
			// A cloudflared config that exists but cannot be read leaves open
			// whether this check applies at all. An open question is not a pass.
			name:      "配置无法理解时是 Unknown",
			target:    Target{TunnelErr: errors.New("config.yml 解析失败")},
			want:      Unknown,
			detail:    []string{"cloudflared"},
			wantCalls: 0,
		},
		{
			// One region unanswered, the other hijacked: the observation
			// outranks the unanswered question.
			name:      "一个区域无应答另一个是 fake-ip",
			target:    Target{TunnelName: "slim"},
			errs:      map[string]error{region1: resolveErr},
			answers:   map[string][]string{region2: {"198.18.0.12"}},
			want:      Warn,
			detail:    []string{"region2 → 198.18.0.12"},
			wantCalls: 2,
		},
		{
			// cloudflared registers against whichever region answers, so one
			// missing region is not what this check exists for -- promoting it
			// to Unknown would fail doctor on a tunnel that comes up fine.
			name:      "一个区域无应答另一个真实",
			target:    Target{TunnelName: "slim"},
			errs:      map[string]error{region1: resolveErr},
			answers:   map[string][]string{region2: {"198.41.200.113"}},
			want:      Pass,
			detail:    []string{"region2 → 198.41.200.113", "region1"},
			wantCalls: 2,
		},
		{
			// A lookup that failed because time ran out establishes nothing;
			// saying "does not resolve" there would be a finding about the
			// clock. Asking the next host would establish nothing either, so
			// exactly one lookup is attempted.
			name:      "取消后不下结论",
			target:    Target{TunnelName: "slim"},
			errs:      map[string]error{region1: context.Canceled, region2: context.Canceled},
			cancel:    true,
			want:      Unknown,
			detail:    []string{"取消"},
			notDetail: []string{"无法解析"},
			wantCalls: 1,
		},
		{
			// A genuine edge address already in hand settles the question this
			// check asks -- fake-ip answers every name under the domain, so one
			// real answer rules it out. Reporting Unknown because the clock ran
			// out afterwards would throw away an observation and, since Unknown
			// drives the exit code, fail doctor on a healthy machine.
			name:    "超时前已看到真实地址",
			target:  Target{TunnelName: "slim"},
			answers: map[string][]string{region1: {"198.41.192.47"}},
			errs:    map[string]error{region2: context.DeadlineExceeded},
			cancel:  true,
			want:    Pass,
			detail:  []string{"region1 → 198.41.192.47"},
			// Silence about region2, not a verdict on it: the lookup ended with
			// the deadline, which says nothing about whether the name resolves.
			notDetail: []string{"region2", "未解析"},
			wantCalls: 2,
		},
		{
			// An empty answer carrying no error is neither a real address nor a
			// fake one; counting it as real would report an address never seen.
			name:      "空应答不算真实地址",
			target:    Target{TunnelName: "slim"},
			answers:   map[string][]string{region1: {}, region2: {}},
			want:      Unknown,
			notDetail: []string{"真实"},
			wantCalls: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dns := &fakeDNS{answers: tc.answers, errs: tc.errs}
			dns.install(t)

			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			res := tc.target.checkTunnelEdgeDNS(ctx)
			if res.Level != tc.want {
				t.Errorf("得到 %s，应为 %s: %s", res.Level, tc.want, res.Detail)
			}
			if dns.calls != tc.wantCalls {
				t.Errorf("解析了 %d 次，应为 %d 次", dns.calls, tc.wantCalls)
			}
			for _, want := range tc.detail {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("说明里没有 %q: %s", want, res.Detail)
				}
			}
			for _, unwanted := range tc.notDetail {
				if strings.Contains(res.Detail, unwanted) {
					t.Errorf("说明里不该出现 %q: %s", unwanted, res.Detail)
				}
			}
			for _, want := range tc.remedy {
				if !strings.Contains(res.Remedy, want) {
					t.Errorf("处置建议里没有 %q: %s", want, res.Remedy)
				}
			}
			// The package's own rule: anything that is not a pass owes the
			// operator a next step.
			if res.Level != Pass && res.Remedy == "" {
				t.Errorf("%s 没有给出下一步", res.Level)
			}
		})
	}
}

// TestTunnelEdgeDNSRunsBeforeTunnel.
//
// The point of this check is to answer before cloudflared is started, so it has
// to appear in the set and it has to come ahead of the tunnel check -- the
// operator reads the report top to bottom, and the edge-DNS finding is the
// explanation for the tunnel finding below it.
func TestTunnelEdgeDNSRunsBeforeTunnel(t *testing.T) {
	edge, tunnelAt := -1, -1
	for i, c := range Checks(Target{Host: "127.0.0.1", Port: 8317}) {
		switch c.Name {
		case "tunnel-edge-dns":
			edge = i
			if c.Timeout <= 0 {
				t.Error("没有设置超时：DNS 查询必须有界")
			}
		case "tunnel":
			tunnelAt = i
		}
	}
	if edge < 0 {
		t.Fatal("检查集里没有 tunnel-edge-dns，这次事故仍然无人发现")
	}
	if tunnelAt < 0 || edge > tunnelAt {
		t.Errorf("tunnel-edge-dns 排在 tunnel 之后（%d > %d），解释跑到了结论后面", edge, tunnelAt)
	}
}

// TestUpstreamDNSSharesTheResolverSeam guards that the two DNS checks keep
// asking through one function. Two ways of resolving in one package is how they
// end up disagreeing, and only one of them gets fixed.
func TestUpstreamDNSSharesTheResolverSeam(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{"api.anthropic.com": {"198.18.0.7"}}}
	dns.install(t)

	res := Target{}.checkUpstreamDNS(context.Background())
	if dns.calls == 0 {
		t.Fatal("upstream-dns 绕开了 lookupIPv4，这个测试测到的是本机网络")
	}
	if res.Level != Warn {
		t.Errorf("api.anthropic.com 解析到 fake-ip 段得到 %s，应为 WARN: %s", res.Level, res.Detail)
	}
}

// Cell budgets for one report line, derived from tui/view.go: the panel is
// capped at 100 columns, so inner = 100 - 2 borders - 2*hpad = 96, and every
// line is truncated to it. reportLines prefixes a detail with level and check
// name (8 + 1 + 16 + 1 cells) and a remedy with "         → " (11 cells).
//
// This is the widest the panel ever gets. Anything past these numbers is not
// merely wrapped somewhere else, it is gone -- which is how the original
// incident was reported as "启动隧道失败" above a log tail whose decisive field
// had been cut off the right edge.
const (
	panelDetailCells = 96 - 26
	panelRemedyCells = 96 - 11
)

// TestEdgeDNSFindingSurvivesTheTruncation.
//
// The check can be perfectly correct and still useless: the operator reads it
// through a panel that cuts every line, in whichever of the two languages they
// run. So the address must arrive before the cut, and so must both halves of
// the fix -- a direct rule without the fake-ip exemption is matched against
// 198.18.x.x and never fires, which is the hour this whole check exists to save.
func TestEdgeDNSFindingSurvivesTheTruncation(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{
		region1: {"198.18.0.11"},
		region2: {"198.18.0.12"},
	}}
	dns.install(t)

	for _, lang := range []struct {
		name string
		lang i18n.Lang
	}{{"中文", i18n.Zh}, {"english", i18n.En}} {
		t.Run(lang.name, func(t *testing.T) {
			i18n.Set(lang.lang)
			t.Cleanup(func() { i18n.Set(i18n.Zh) })

			res := Target{TunnelName: "slim"}.checkTunnelEdgeDNS(context.Background())
			detail := cut(res.Detail, panelDetailCells)
			for _, want := range []string{"198.18.0.0/15", "198.18.0.11"} {
				if !strings.Contains(detail, want) {
					t.Errorf("面板宽度内看不到 %q，只剩下 %q", want, detail)
				}
			}
			// Both halves, in the readable part. "argotunnel" is the name to
			// route direct; "fake-ip" is the list to exempt it from.
			remedy := cut(res.Remedy, panelRemedyCells)
			for _, want := range []string{"argotunnel", "fake-ip"} {
				if !strings.Contains(remedy, want) {
					t.Errorf("面板宽度内的处置建议少了 %q，只剩下 %q", want, remedy)
				}
			}
		})
	}
}

// cut truncates to a cell count the way the panel does.
//
// runewidth is what tui/theme.go measures with, so this counts the same cells
// the terminal will. Counting runes instead would pass this test on the
// Chinese strings and still lose half of them on screen. The condition is
// pinned the way theme.go pins it: the package default counts ·, — and → as
// two cells on a Chinese Windows console, and the panel does not.
func cut(s string, cells int) string {
	cond := &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: true}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := cond.RuneWidth(r)
		if used+w > cells {
			break
		}
		used += w
		b.WriteRune(r)
	}
	return b.String()
}
