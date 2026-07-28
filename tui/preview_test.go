package tui

import (
	"testing"
	"time"

	"github.com/momo/slimproxy/credentials"
	"github.com/momo/slimproxy/metrics"
	"github.com/momo/slimproxy/tunnel"
)

// TestRenderFailureStates prints the frames that only appear when something is
// wrong, for the same reason TestRenderPreview exists: these are assertions
// about legibility, which a width check cannot make. Run with -v to look.
//
// Every state here was a defect once -- a fabricated 200, a green dot over an
// unreadable connection count, an expired credential rendered as time
// remaining, a warning hidden behind the running label.
func TestRenderFailureStates(t *testing.T) {
	t.Run("健康", func(t *testing.T) {
		t.Log("\n" + sampleModel(90, 22).View())
	})

	t.Run("各种坏状态", func(t *testing.T) {
		m := sampleModel(90, 22)
		// tunnel: running, connection count unreadable
		m.tunnel.st.ConnectionsErr = &stringError{"cert.pem 已过期"}
		m.tunnel.connKnown = false
		m.tunnel.st.ConnectionsKnown = false
		m.tunnel.at = fixedNow.Add(-3 * time.Minute) // stale reading
		// creds: one healthy, one expired-but-refreshable, one broken
		m.creds = summariseCreds(credsMsg{
			at: fixedNow,
			creds: []credentials.Credential{
				{Name: "live.json", Provider: "claude", Account: "a@x.com", Expires: fixedNow.Add(8 * time.Hour)},
				{Name: "stale.json", Provider: "claude", Account: "b@x.com", Expires: fixedNow.Add(-72 * time.Hour)},
				{Name: "broken.json", Err: &stringError{"不是合法 JSON"}},
			},
		}, fixedNow)
		m.creds.at = fixedNow
		// requests: real upstream status codes
		m.stats.Recent = []metrics.Sample{
			{At: fixedNow, Account: "openai@acct", Target: "claude", Model: "claude-opus-5", TTFT: 812 * time.Millisecond, Tokens: 1900},
			{At: fixedNow.Add(-9 * time.Second), Account: "openai@acct", Target: "claude", Model: "claude-opus-5", Failed: true, Status: 429},
			{At: fixedNow.Add(-20 * time.Second), Account: "claude@acct", Target: "claude", Model: "claude-opus-5", Failed: true, Status: 401},
			{At: fixedNow.Add(-31 * time.Second), Account: "openai@acct", Target: "claude", Model: "claude-opus-5", Failed: true},
		}
		t.Log("\n" + m.View())
	})

	t.Run("命令执行中按 q", func(t *testing.T) {
		m := sampleModel(90, 22)
		m.running = "正在启动 cloudflared 并等待连接注册…"
		m, _ = press(t, m, key("q"))
		t.Log("\n" + m.View())
	})

	t.Run("敌意模型名", func(t *testing.T) {
		m := sampleModel(90, 22)
		m.stats.Recent = []metrics.Sample{{
			At: fixedNow, Account: "openai@acct", Target: "claude",
			Model: "claude\x1b[2J\nopus", TTFT: time.Second, Tokens: 100,
		}}
		m.tunnel.st.State = tunnel.Running
		t.Log("\n" + m.View())
	})
}
