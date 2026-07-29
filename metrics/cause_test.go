package metrics

import (
	"strings"
	"testing"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestCauseFromRealTransportErrors.
//
// The wordings below are what the Go standard library actually produces, both
// platform spellings included -- the audit found 108 failures carrying nothing
// but `ok:false`, and every one of them had text like this sitting unread in
// Record.Fail.Body.
func TestCauseFromRealTransportErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
		want Cause
	}{
		{"dns", `dial tcp: lookup api.anthropic.com: no such host`, CauseDNS},
		{"dns servfail", `lookup api.anthropic.com on 10.0.0.1:53: server misbehaving`, CauseDNS},
		{"refused windows", `dial tcp 127.0.0.1:1: connectex: No connection could be made because the target machine actively refused it.`, CauseConnect},
		{"refused unix", `dial tcp 127.0.0.1:1: connect: connection refused`, CauseConnect},
		{"unreachable", `dial tcp 198.18.0.42:443: connect: network is unreachable`, CauseConnect},
		{"reset", `read tcp 10.0.0.2:1234->1.2.3.4:443: read: connection reset by peer`, CauseConnect},
		{"eof", `Post "https://api.anthropic.com/v1/messages": EOF`, CauseConnect},
		{"tls handshake", `net/http: TLS handshake timeout`, CauseTLS},
		{"cert", `x509: certificate signed by unknown authority`, CauseTLS},
		{"io timeout", `dial tcp 198.18.0.42:443: i/o timeout`, CauseTimeout},
		{"deadline", `context deadline exceeded`, CauseTimeout},
		{"canceled", `context canceled`, CauseCanceled},
		{"unknown wording", `something nobody has seen before`, CauseOther},
		{"empty", ``, CauseOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CauseFromText(tc.text); got != tc.want {
				t.Errorf("CauseFromText(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestTLSTimeoutIsTLSNotTimeout pins a precedence that a reordering would
// silently flip.
//
// "net/http: TLS handshake timeout" matches both tables. TLS is the more
// specific and more actionable reading -- it points at interception or a broken
// middlebox, where "timeout" would send someone looking at latency.
func TestTLSTimeoutIsTLSNotTimeout(t *testing.T) {
	if got := CauseFromText("net/http: TLS handshake timeout"); got != CauseTLS {
		t.Errorf("= %q, want %q：TLS 阶段的超时应归为 TLS，否则会被当成网络慢", got, CauseTLS)
	}
}

// TestCanceledIsNotAFailureMode.
//
// A client pressing Ctrl-C produces context.Canceled. Filing that with the
// timeouts would make a busy interactive session read as an outage, which
// inverts the signal the health ladder is built on.
func TestCanceledIsNotAFailureMode(t *testing.T) {
	if got := CauseFromText("context canceled"); got != CauseCanceled {
		t.Errorf("= %q, want %q", got, CauseCanceled)
	}
	if CauseFromText("context canceled") == CauseFromText("context deadline exceeded") {
		t.Error("取消与超时被归为同一类：客户端主动断开会被当成故障")
	}
}

// TestStatusShortCircuits: a status code means the upstream answered, and
// classifying its body would produce nonsense like "connect" for a 429 whose
// message happens to mention a connection.
func TestStatusShortCircuits(t *testing.T) {
	if got := CauseFrom(429, "dial tcp 1.2.3.4:443: connect: connection refused"); got != CauseUpstream {
		t.Errorf("CauseFrom(429, ...) = %q, want %q", got, CauseUpstream)
	}
	if got := CauseFrom(0, "dial tcp 1.2.3.4:443: connect: connection refused"); got != CauseConnect {
		t.Errorf("CauseFrom(0, ...) = %q, want %q：无状态码时必须读文本", got, CauseConnect)
	}
}

// TestCauseNeverCarriesTheText is the containment guarantee.
//
// Fail.Body is the Go error verbatim: dial target, port, platform syscall name,
// sometimes a local path. It is read here and must not survive the trip -- the
// journal promises to hold no response bodies, and a failure message is one.
func TestCauseNeverCarriesTheText(t *testing.T) {
	const leaky = `Post "https://api.anthropic.com/v1/messages": dial tcp 198.18.0.42:443: ` +
		`connectex: A connection attempt failed; C:\Users\someone\auths\token.json`

	c := CauseFromText(leaky)
	for _, marker := range []string{"198.18.0.42", "connectex", "C:\\Users", "443", "anthropic"} {
		if strings.Contains(string(c), marker) {
			t.Errorf("分类结果 %q 里带上了原始错误的片段 %q", c, marker)
		}
		if strings.Contains(c.Display(), marker) {
			t.Errorf("分类的显示文案 %q 里带上了原始错误的片段 %q", c.Display(), marker)
		}
	}
	// And it still classified rather than giving up.
	if c != CauseConnect {
		t.Errorf("= %q, want %q", c, CauseConnect)
	}
}

// TestSampleFromFillsCause is the wiring: the field existing is worthless if
// SampleFrom keeps dropping Fail.Body, which is precisely the original bug.
func TestSampleFromFillsCause(t *testing.T) {
	s := SampleFrom(cliproxyusage.Record{
		Provider: "claude",
		Failed:   true,
		Fail: cliproxyusage.Failure{
			// No status: this is what a dial failure looks like on the way in.
			Body: `dial tcp 198.18.0.42:443: connectex: no connection could be made`,
		},
	})
	if s.Cause != CauseConnect {
		t.Errorf("Sample.Cause = %q, want %q：Fail.Body 又一次被丢掉了", s.Cause, CauseConnect)
	}
	if s.Status != 0 {
		t.Errorf("Sample.Status = %d，传输层失败不应有状态码", s.Status)
	}
}

// TestSuccessHasNoCause keeps the field meaningful: a cause on a successful
// request would make "any cause present" useless as a failure filter.
func TestSuccessHasNoCause(t *testing.T) {
	s := SampleFrom(cliproxyusage.Record{Provider: "claude", Failed: false})
	if s.Cause != CauseNone {
		t.Errorf("成功请求的 Cause = %q, want 空", s.Cause)
	}
}

// TestFailureWithoutTextStillSaysSomething.
//
// "Failed with no reason given" is itself a finding -- it is how the original
// gap presented -- so it must be distinguishable from success rather than
// collapsing to the same empty value.
func TestFailureWithoutTextStillSaysSomething(t *testing.T) {
	s := SampleFrom(cliproxyusage.Record{Provider: "claude", Failed: true})
	if s.Cause == CauseNone {
		t.Error("失败但无错误文本的记录 Cause 为空，与成功无法区分")
	}
	if s.Cause != CauseOther {
		t.Errorf("Cause = %q, want %q", s.Cause, CauseOther)
	}
}
