package diag

import (
	"context"
	"strings"
	"testing"

	"github.com/Laurent00TT/slimproxy/i18n"
)

const anthropicHost = "api.anthropic.com"

// TestUpstreamDNSFakeIPUnderProxy pins upstream-dns against the advice it used
// to give: a fake-ip answer was a WARN whose remedy was to route
// api.anthropic.com direct. With proxy-url set the dial never used that answer,
// and from a region Anthropic does not serve, direct was 403 on every request,
// each one suspending the only credential's model for 30 minutes.
//
// Whether proxy-url counts as a proxy is the engine's call, not "is it
// non-empty": "direct" is a value the engine reads as no proxy at all, and a
// scheme it rejects leaves the dial on this machine's resolver too.
func TestUpstreamDNSFakeIPUnderProxy(t *testing.T) {
	const fake = "198.18.0.7"
	for _, tc := range []struct {
		name      string
		proxyURL  string
		answer    string
		want      Level
		detail    []string
		notDetail []string
		remedy    []string
	}{
		{
			name:     "http 代理下的 fake-ip",
			proxyURL: "http://127.0.0.1:7897",
			answer:   fake,
			want:     Pass,
			// The observation stays on the line; the pass is explained, not
			// silent, so an operator who knows fake-ip is on sees why it is fine.
			detail: []string{"fake-ip", fake, "proxy-url", "代理自行解析"},
		},
		{
			name:     "socks5 代理下的 fake-ip",
			proxyURL: "socks5://127.0.0.1:7898",
			answer:   fake,
			want:     Pass,
			detail:   []string{"fake-ip", "proxy-url"},
		},
		{
			// proxy-url can carry credentials and doctor output gets pasted
			// into issues, so the line names the setting, never its value.
			name:      "代理地址里的口令不上屏",
			proxyURL:  "http://user:hunter2@127.0.0.1:7897",
			answer:    fake,
			want:      Pass,
			notDetail: []string{"hunter2", "127.0.0.1:7897"},
		},
		{
			name:     "未配置代理",
			proxyURL: "",
			answer:   fake,
			want:     Warn,
			detail:   []string{fake, "198.18.0.0/15"},
			remedy:   []string{anthropicHost, "服务地区的节点", "403"},
		},
		{
			name:     "direct 在引擎里就是不走代理",
			proxyURL: "direct",
			answer:   fake,
			want:     Warn,
			remedy:   []string{"服务地区的节点"},
		},
		{
			name:     "引擎不认的 scheme 不算代理",
			proxyURL: "ftp://127.0.0.1:21",
			answer:   fake,
			want:     Warn,
			remedy:   []string{"服务地区的节点"},
		},
		{
			name:      "代理下的真实地址照旧",
			proxyURL:  "http://127.0.0.1:7897",
			answer:    "160.79.104.10",
			want:      Pass,
			detail:    []string{"真实地址", "160.79.104.10"},
			notDetail: []string{"fake-ip"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			(&fakeDNS{answers: map[string][]string{anthropicHost: {tc.answer}}}).install(t)

			res := Target{ProxyURL: tc.proxyURL}.checkUpstreamDNS(context.Background())
			if res.Level != tc.want {
				t.Errorf("得到 %s，应为 %s: %s", res.Level, tc.want, res.Detail)
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
			if res.Level != Pass && res.Remedy == "" {
				t.Errorf("%s 没有给出下一步", res.Level)
			}
		})
	}
}

// TestUpstreamDNSNeverAdvisesDirect: on the deployment this was written for, a
// direct route to Anthropic is refused, and a refusal costs the credential 30
// minutes. The check must not suggest one in any outcome or either language --
// asserted on the words, because the old remedy was one reasonable-sounding
// clause ("让 api.anthropic.com 直连可消除这一层") inside an otherwise correct line.
func TestUpstreamDNSNeverAdvisesDirect(t *testing.T) {
	(&fakeDNS{answers: map[string][]string{anthropicHost: {"198.18.0.7"}}}).install(t)

	for _, lang := range []struct {
		name string
		lang i18n.Lang
	}{{"中文", i18n.Zh}, {"english", i18n.En}} {
		for _, route := range []struct{ name, proxyURL string }{
			{"无代理", ""},
			{"经代理", "http://127.0.0.1:7897"},
		} {
			t.Run(lang.name+"/"+route.name, func(t *testing.T) {
				i18n.Set(lang.lang)
				t.Cleanup(func() { i18n.Set(i18n.Zh) })

				res := Target{ProxyURL: route.proxyURL}.checkUpstreamDNS(context.Background())
				text := strings.ToLower(res.Detail + " " + res.Remedy)
				for _, word := range []string{"直连", "direct", "绕过", "bypass", "exclud"} {
					if strings.Contains(text, word) {
						t.Errorf("%s 的输出建议了直连（含 %q）: %s / %s", res.Level, word, res.Detail, res.Remedy)
					}
				}
			})
		}
	}
}

// TestUpstreamDNSReasonSurvivesTheTruncation: the dashboard cuts every line at
// its width (see panelDetailCells). A pass that shows "fake-ip" and loses the
// reason reads as a finding someone forgot to grade, and a warning that loses
// the node reads as "do something about DNS" -- the old direct advice is the
// first thing that comes to mind there.
//
// The widest address in the range is used on purpose: 198.18.0.7 leaves room
// that 198.19.255.255 does not.
func TestUpstreamDNSReasonSurvivesTheTruncation(t *testing.T) {
	const widest = "198.19.255.255"
	(&fakeDNS{answers: map[string][]string{anthropicHost: {widest}}}).install(t)

	for _, lang := range []struct {
		name string
		lang i18n.Lang
		node string
	}{{"中文", i18n.Zh, "节点"}, {"english", i18n.En, "node"}} {
		t.Run(lang.name, func(t *testing.T) {
			i18n.Set(lang.lang)
			t.Cleanup(func() { i18n.Set(i18n.Zh) })

			pass := Target{ProxyURL: "http://127.0.0.1:7897"}.checkUpstreamDNS(context.Background())
			shown := cut(pass.Detail, panelDetailCells)
			for _, want := range []string{"fake-ip", widest, "proxy-url"} {
				if !strings.Contains(shown, want) {
					t.Errorf("面板宽度内看不到 %q，只剩下 %q", want, shown)
				}
			}

			warn := Target{}.checkUpstreamDNS(context.Background())
			remedy := cut(warn.Remedy, panelRemedyCells)
			for _, want := range []string{anthropicHost, lang.node} {
				if !strings.Contains(remedy, want) {
					t.Errorf("面板宽度内的处置建议少了 %q，只剩下 %q", want, remedy)
				}
			}
		})
	}
}
