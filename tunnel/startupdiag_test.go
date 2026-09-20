package tunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fragments below are the shapes cloudflared actually writes. Timestamps
// and UUIDs are trimmed or invented; the field layout is real, and the field
// layout is what the classifier is being held to.

// incidentLog is the failure this classifier exists for, taken from the run
// that broke the tunnel: Clash/mihomo TUN with fake-ip answering DNS for
// region1/2.v2.argotunnel.com, no proxy rule matching argotunnel.com, and the
// traffic relayed by a node whose UDP relay carries QUIC's first round trip
// and not the rest.
const incidentLog = `2026-09-20T02:46:03Z INF ICMP proxy will use 198.18.0.1 as source for IPv4
2026-09-20T02:46:03Z INF Initial protocol quic
2026-09-20T02:46:03Z INF You requested 4 HA connections but I can give you at most 2.
2026-09-20T02:46:03Z INF Tunnel connection curve preferences: [X25519MLKEM768 CurveP256] connIndex=0 event=0 ip=198.18.0.11
2026-09-20T02:46:13Z INF precheck component="UDP Connectivity" details="QUIC connection successful" status=pass target=region1.v2.argotunnel.com
2026-09-20T02:46:13Z INF precheck component="TCP Connectivity" details="HTTP/2 connection is blocked or unreachable" status=fail target=region1.v2.argotunnel.com
2026-09-20T02:46:13Z ERR Failed to dial a quic connection error="failed to dial to edge with quic: timeout: no recent network activity" connIndex=0 event=0 ip=198.18.0.12
`

// healthyLog is the same tunnel after the proxy was fixed: real anycast edge
// addresses, a registration one second in.
const healthyLog = `2026-09-20T09:12:01Z INF Initial protocol quic
2026-09-20T09:12:01Z INF Tunnel connection curve preferences: [X25519MLKEM768 CurveP256] connIndex=0 event=0 ip=198.41.192.27
2026-09-20T09:12:02Z INF Registered tunnel connection connIndex=0 connection=8d1a event=0 ip=198.41.192.27 location=lax01 protocol=quic
`

// unrelatedFailureLog is a launch that failed for a reason this classifier
// knows nothing about. It must stay unclassified.
const unrelatedFailureLog = `2026-02-02T00:00:00Z ERR Couldn't start tunnel error="failed to unmarshal credentials from file"
2026-02-02T00:00:00Z ERR Cannot determine default origin certificate path
`

func TestClassifyStartupLog(t *testing.T) {
	tests := []struct {
		name  string
		log   string
		alive bool
		want  startupCause
		// contains are substrings the message must carry: the address, the
		// component, the half of the remedy that is easy to leave out.
		contains []string
		// absent guards against the advice that has already been measured not
		// to work, and against a cause being claimed on the wrong evidence.
		absent []string
	}{
		{
			name:  "fake-ip edge is named, with both halves of the remedy",
			log:   incidentLog,
			alive: true,
			want:  causeFakeIPEdge,
			contains: []string{
				"198.18.0.11", "198.18.0.12", // the ip= fields, quoted back
				"fake-ip",
				"两步缺一不可",           // routing direct alone leaves DNS returning 198.18.x.x
				"TCP Connectivity", // cloudflared's own failing row, named
			},
			// Verified on this incident: TCP 7844 completed TLS through the
			// proxy but never negotiated ALPN h2, and one HTTP/2 preface frame
			// failed 6/6. Offering it as the fix would cost another afternoon.
			// The flag itself is barred, not just the word, so a reworded
			// "try HTTP/2 instead" cannot slip past this.
			absent: []string{"http2", "--protocol"},
		},
		{
			name:  "a healthy launch is never diagnosed",
			log:   healthyLog,
			alive: true,
			want:  causeUnknown,
		},
		{
			name:  "an unrelated failure falls back to the log tail",
			log:   unrelatedFailureLog,
			alive: true,
			want:  causeUnknown,
		},
		{
			name: "a 198.18 source address alone is not an edge address",
			// The ICMP line carries an RFC 2544 address and means only that
			// the default route is the proxy's. Diagnosing fake-ip off it
			// would be a cause with no evidence behind it.
			log:   "2026-09-20T02:46:03Z INF ICMP proxy will use 198.18.0.1 as source for IPv4\n" + unrelatedFailureLog,
			alive: true,
			want:  causeUnknown,
		},
		{
			name: "precheck failure names the component",
			log: `2026-05-02T08:00:00Z INF Initial protocol quic
2026-05-02T08:00:10Z INF precheck component="UDP Connectivity" details="QUIC connection failed" status=fail target=region1.v2.argotunnel.com
2026-05-02T08:00:10Z INF precheck component="TCP Connectivity" details="HTTP/2 connection successful" status=pass target=region1.v2.argotunnel.com
2026-05-02T08:00:11Z ERR Failed to dial a quic connection error="timeout: no recent network activity" connIndex=0 event=0 ip=198.41.200.23
`,
			alive: true,
			want:  causePrecheckFailed,
			contains: []string{
				"UDP Connectivity",
				"region1.v2.argotunnel.com",
				// Mentioned only as a way to confirm the diagnosis, and only
				// here, where the precheck really does show TCP passing.
				"--protocol http2 只用于确认",
			},
			absent: []string{"198.18"},
		},
		{
			name: "both transports failing does not single out QUIC",
			log: `2026-05-02T08:00:10Z INF precheck component="UDP Connectivity" details="QUIC connection failed" status=fail target=region1.v2.argotunnel.com
2026-05-02T08:00:10Z INF precheck component="TCP Connectivity" details="HTTP/2 connection is blocked or unreachable" status=fail target=region1.v2.argotunnel.com
`,
			alive:    true,
			want:     causePrecheckFailed,
			contains: []string{"UDP Connectivity", "TCP Connectivity", "7844"},
			// TCP failed its own pre-check here, so switching to it is not a
			// step worth naming even as a confirmation.
			absent: []string{"http2", "--protocol"},
		},
		{
			name: "HA shortfall alone is reported as a collapsed address pool",
			log: `2026-05-02T08:00:00Z INF Initial protocol quic
2026-05-02T08:00:00Z INF You requested 4 HA connections but I can give you at most 1.
`,
			alive: true,
			want:  causeEdgePoolCollapsed,
			// Both numbers, in their own slots: read by position out of the
			// sentence, so a swap would be invisible to a looser assertion.
			contains: []string{"请求 4 条", "只拿到 1 条", "nslookup"},
		},
		{
			name: "dial failures with a live process mean retrying, not dead",
			log: `2026-03-01T00:00:00Z INF Initial protocol quic
2026-03-01T00:00:05Z ERR Failed to dial a quic connection error="dial udp 198.41.192.27:7844: i/o timeout" connIndex=0 event=0 ip=198.41.192.27
2026-03-01T00:00:07Z INF Retrying connection in up to 2s connIndex=0
`,
			alive:    true,
			want:     causeStillRetrying,
			contains: []string{"不会因此退出", "i/o timeout"},
		},
		{
			name: "the same log from a process that exited claims nothing",
			// "it is still retrying" is a statement about a live process. On
			// the exit path it would be false, and a false cause is worse than
			// the raw tail.
			log: `2026-03-01T00:00:05Z ERR Failed to dial a quic connection error="dial udp 198.41.192.27:7844: i/o timeout" connIndex=0 event=0 ip=198.41.192.27
`,
			alive: false,
			want:  causeUnknown,
		},
		{
			name: "a reworded upstream log degrades to no diagnosis",
			// Every marker here is cloudflared's own wording, which it does
			// not promise to keep. A format change must make the matchers stop
			// firing, never fire on something else.
			log: `2026-05-02T08:00:00Z INF connectivity precheck: UDP Connectivity FAILED (region1.v2.argotunnel.com)
2026-05-02T08:00:00Z INF You asked for 4 HA connections; 2 available.
2026-05-02T08:00:01Z ERR could not open a quic connection to the edge
`,
			alive: true,
			want:  causeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := classifyStartupLog(tt.log, tt.alive)
			if d.Cause != tt.want {
				t.Fatalf("cause = %v, want %v\n%s", d.Cause, tt.want, strings.Join(d.Lines, "\n"))
			}
			if tt.want == causeUnknown && len(d.Lines) != 0 {
				t.Errorf("未命名任何原因却给出了文字：\n%s", strings.Join(d.Lines, "\n"))
			}
			text := strings.Join(d.Lines, "\n")
			for _, want := range tt.contains {
				if !strings.Contains(text, want) {
					t.Errorf("诊断里没有 %q：\n%s", want, text)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(text, bad) {
					t.Errorf("诊断里不该出现 %q：\n%s", bad, text)
				}
			}
		})
	}
}

// TestStartupFailurePutsTheCauseBeforeTheTail pins the ordering the TUI forces.
//
// The output panel truncates every line to the panel width and shows a bounded
// number of rows, so a cause printed after twelve lines of timestamped log is a
// cause nobody reads -- which is exactly how this incident was diagnosed by
// hand instead of by the program.
func TestStartupFailurePutsTheCauseBeforeTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloudflared.log")
	if err := os.WriteFile(path, []byte(incidentLog), 0o600); err != nil {
		t.Fatal(err)
	}

	msg := startupFailure("启动失败", path, 0, true).Error()
	lines := strings.Split(msg, "\n")
	if lines[0] != "启动失败" {
		t.Errorf("第一行 = %q，应是调用方给的标题", lines[0])
	}
	if !strings.HasPrefix(lines[1], "原因：") {
		t.Errorf("第二行 = %q，应当直接给出原因", lines[1])
	}

	cause := strings.Index(msg, "198.18.0.11")
	tail := strings.Index(msg, "ICMP proxy will use")
	if cause < 0 || tail < 0 {
		t.Fatalf("原因或日志尾部缺失：\n%s", msg)
	}
	if cause > tail {
		t.Error("原因排在日志尾部之后，等于没写")
	}
}

// TestStartupFailureFallsBackToTheTail: when nothing classifies, the message
// must be what it was before -- a headline and the log -- and must not invent
// a "原因：" line to look helpful.
func TestStartupFailureFallsBackToTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloudflared.log")
	if err := os.WriteFile(path, []byte(unrelatedFailureLog), 0o600); err != nil {
		t.Fatal(err)
	}

	msg := startupFailure("启动失败", path, 0, true).Error()
	if strings.Contains(msg, "原因：") {
		t.Errorf("没有证据却给出了原因：\n%s", msg)
	}
	if !strings.Contains(msg, "failed to unmarshal credentials") {
		t.Errorf("日志尾部没有保留：\n%s", msg)
	}
}

// TestStartupFailureReadsOnlyThisRun: the log is append-only, so the previous
// run's evidence is still in the file. Classifying it would explain a failure
// that is over, and in the worst case name a cause that was fixed hours ago.
func TestStartupFailureReadsOnlyThisRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloudflared.log")
	if err := os.WriteFile(path, []byte(incidentLog+unrelatedFailureLog), 0o600); err != nil {
		t.Fatal(err)
	}

	msg := startupFailure("启动失败", path, int64(len(incidentLog)), true).Error()
	if strings.Contains(msg, "原因：") {
		t.Errorf("上一次运行的证据被当成了本次的原因：\n%s", msg)
	}
	// The tail is printed as evidence, so it obeys the same rule. This run
	// wrote two lines; padding them out to twelve with the previous run's
	// output puts a fake-ip edge address under a headline it has nothing to do
	// with, and the operator reads the tail, not the offset arithmetic.
	if strings.Contains(msg, "198.18.0.11") {
		t.Errorf("日志尾部把上一次运行的输出也贴了出来：\n%s", msg)
	}
	if !strings.Contains(msg, "failed to unmarshal credentials") {
		t.Errorf("本次运行的日志尾部丢了：\n%s", msg)
	}
}

// TestStartupFailureSaysWhenThisRunLoggedNothing: an empty tail block reads as
// a rendering fault. That cloudflared wrote nothing at all is a finding of its
// own -- it never got as far as logging, which points at the binary or its
// arguments rather than at the network.
func TestStartupFailureSaysWhenThisRunLoggedNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloudflared.log")
	if err := os.WriteFile(path, []byte(incidentLog), 0o600); err != nil {
		t.Fatal(err)
	}

	msg := startupFailure("启动失败", path, int64(len(incidentLog)), true).Error()
	if !strings.Contains(msg, "本次启动没有写入任何日志") {
		t.Errorf("本次运行无输出时没有说明，只留下空白：\n%s", msg)
	}
	if strings.Contains(msg, "ICMP proxy will use") {
		t.Errorf("本次运行无输出，却贴出了上一次的日志：\n%s", msg)
	}
}

// TestConfirmStartedNamesTheCauseOnImmediateExit wires the two together: the
// exit path must carry the diagnosis too, not only the timeout path.
func TestConfirmStartedNamesTheCauseOnImmediateExit(t *testing.T) {
	m := testManager(t)
	logPath := filepath.Join(m.StateDir, "cloudflared.log")
	if err := os.WriteFile(logPath, []byte(incidentLog), 0o600); err != nil {
		t.Fatal(err)
	}
	m.verify = func(record) (identity, error) { return identityGone, nil }

	err := m.confirmStarted(context.Background(), record{PID: 1234, LogPath: logPath}, 0)
	if err == nil {
		t.Fatal("退出的子进程被报告为启动成功")
	}
	for _, want := range []string{"启动后随即退出", "原因：", "198.18.0.11", "ICMP proxy will use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("消息里没有 %q：\n%v", want, err)
		}
	}
}

// TestStartupTimeoutHeadlineOnlyClaimsWhatWasObserved.
//
// identityUnknown means the liveness query failed, not that the process is
// fine: on Windows that is tasklist being unavailable or refusing the query.
// The loop's old wording asserted "it never exited, it kept retrying" in that
// case too, which is the process state inferred from our own inability to read
// it -- and it is the one claim that sends an operator looking at the network
// when the thing to look at is the child.
func TestStartupTimeoutHeadlineOnlyClaimsWhatWasObserved(t *testing.T) {
	alive := startupTimeoutHeadline(true)
	if !strings.Contains(alive, "一直在重试") {
		t.Errorf("确认存活时没有说明它仍在重试：%s", alive)
	}

	unknown := startupTimeoutHeadline(false)
	if strings.Contains(unknown, "没有退出") || strings.Contains(unknown, "一直在重试") {
		t.Errorf("进程状态未确认，却断言了它的状态：%s", unknown)
	}
	if !strings.Contains(unknown, "无法确认") {
		t.Errorf("没有说明进程状态未能确认：%s", unknown)
	}
}

// TestLogFieldReadsWholeFieldsOnly. cloudflared writes logfmt, and several of
// its keys end with the letters of another key. Matching "ip=" as a bare
// substring reads some other field's value and attributes it to the edge
// address -- which is how a classifier starts naming causes out of nothing.
func TestLogFieldReadsWholeFieldsOnly(t *testing.T) {
	const line = `2026-09-20T02:46:13Z INF precheck component="TCP Connectivity" details="HTTP/2 connection is blocked or unreachable" status=fail target=region1.v2.argotunnel.com connIndex=0 ip=198.18.0.12`

	tests := []struct{ key, want string }{
		{"ip", "198.18.0.12"},
		{"component", "TCP Connectivity"},
		{"details", "HTTP/2 connection is blocked or unreachable"},
		{"status", "fail"},
		{"target", "region1.v2.argotunnel.com"},
		{"connIndex", "0"},
		{"absent", ""},
		// "connIndex=0" ends in "ndex=0", and "dex" is not a field here.
		{"dex", ""},
	}
	for _, tt := range tests {
		if got := logField(line, tt.key); got != tt.want {
			t.Errorf("logField(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}

	// A key spelled inside a quoted value is prose, not a field. details= and
	// error= carry free-form text, and reading an address out of one of them
	// would let a sentence about an address decide the diagnosis.
	const prose = `INF precheck component="UDP Connectivity" details="edge ip=198.18.0.77 was unreachable" status=fail`
	if got := logField(prose, "ip"); got != "" {
		t.Errorf("引号内的 ip= 被当成字段读出：%q", got)
	}
	if got := logField(prose, "status"); got != "fail" {
		t.Errorf("引号之后的字段读不到了：status = %q，应为 fail", got)
	}
	if got := edgeFakeIP(prose); got != "" {
		t.Errorf("从引号内的地址判定了 fake-ip：%q", got)
	}
}

// TestEdgeFakeIPOnlyTrustsTheIPField is the same rule at the level that
// matters: the address that decides the diagnosis must be cloudflared's edge
// address, not any RFC 2544 address on the line.
func TestEdgeFakeIPOnlyTrustsTheIPField(t *testing.T) {
	tests := []struct{ line, want string }{
		{"INF Tunnel connection curve preferences: [x] connIndex=0 event=0 ip=198.18.0.11", "198.18.0.11"},
		{"INF ICMP proxy will use 198.18.0.1 as source for IPv4", ""},
		{"INF Registered tunnel connection connIndex=0 ip=198.41.192.27 location=lax01", ""},
		{"INF something ip=not-an-address", ""},
	}
	for _, tt := range tests {
		if got := edgeFakeIP(tt.line); got != tt.want {
			t.Errorf("edgeFakeIP(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

// TestHAShortfallRefusesRewordedSentences: the numbers are read by position,
// so a reworded sentence must yield nothing rather than whatever words happen
// to land there.
func TestHAShortfallRefusesRewordedSentences(t *testing.T) {
	wanted, offered := haShortfall("2026-09-20T02:46:03Z INF You requested 4 HA connections but I can give you at most 2.")
	if wanted != "4" || offered != "2" {
		t.Errorf("真实语句解析为 (%q, %q)，应为 (4, 2)", wanted, offered)
	}
	for _, line := range []string{
		"INF You requested some HA connections but I can give you at most a few.",
		"INF You requested 4 HA connections but I can give you at most as many as possible",
		"INF requested",
	} {
		if w, o := haShortfall(line); w != "" || o != "" {
			t.Errorf("改写后的语句仍被解析为 (%q, %q)：%s", w, o, line)
		}
	}
}
