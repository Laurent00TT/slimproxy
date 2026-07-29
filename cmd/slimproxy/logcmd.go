package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Laurent00TT/slimproxy/journal"
	"github.com/Laurent00TT/slimproxy/metrics"
)

// cmdLog queries the event journal.
//
// The reason this is a command rather than a note in the README telling people
// to use jq: the value of the journal is that requests and state changes share
// one timeline. A burst of 502s means one thing on its own and something else
// entirely next to a tunnel reconnect thirty seconds earlier. Assembling that
// view by hand every time is the part worth writing down once.
func cmdLog(cx *cliContext, args []string) error {
	cmd := lookup("log")
	fs := newFlagSet(cx, cmd)
	bindConfigOnly(fs, cx)
	since := fs.Duration("since", time.Hour, "只看这段时间内的事件")
	status := fs.Int("status", 0, "只看某个上游状态码，例如 429")
	slow := fs.Duration("slow", 0, "只看总时长超过该值的请求")
	route := fs.String("route", "", "按路由过滤（子串，例如 openai）")
	model := fs.String("model", "", "按模型过滤（子串）")
	failed := fs.Bool("failed", false, "只看失败的请求")
	noise := fs.Bool("noise", false, "包含外网扫描等被拒流量（默认排除）")
	limit := fs.Int("n", 50, "最多显示多少条（0 = 不限）")
	stats := fs.Bool("stats", false, "只打印汇总，不列出逐条事件")
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	dir, err := journalDir(cx)
	if err != nil {
		return err
	}

	q := journal.Query{
		Since:        time.Now().Add(-*since),
		Status:       *status,
		MinLatency:   *slow,
		Route:        *route,
		Model:        *model,
		FailedOnly:   *failed,
		IncludeNoise: *noise,
		Limit:        *limit,
	}

	res, err := journal.Read(dir, q)
	if err != nil {
		return err
	}
	events := res.Events
	if len(events) == 0 && res.Noise == 0 {
		fmt.Fprintf(cx.stdout, "在 %s 内没有匹配的事件（目录 %s）\n", shortDuration(*since), dir)
		if !*noise {
			fmt.Fprintf(cx.stdout, "外网扫描等被拒流量默认不显示，加 -noise 查看。\n")
		}
		return nil
	}

	if !*stats && len(events) > 0 {
		writeEventTable(cx, events)
		fmt.Fprintln(cx.stdout)
	}
	summary := journal.Summarise(events)
	summary.Noise += res.Noise
	writeJournalSummary(cx, summary, *since)
	return nil
}

// journalDir resolves where events live, from the same config the proxy uses.
func journalDir(cx *cliContext) (string, error) {
	cfg, _, err := cx.loadConfigForDiagnosis()
	if err != nil {
		return "", err
	}
	// Resolved through the same function the proxy writes with, so a query
	// cannot end up reporting "no events" about a directory nothing writes to.
	dir, err := cfg.ResolvedJournalDir()
	if err != nil {
		return "", fmt.Errorf("无法解析事件日志目录: %w", err)
	}
	if dir == "" {
		return "", fmt.Errorf("事件日志已被 journal-days: %d 关闭", cfg.JournalDays)
	}
	return dir, nil
}

func writeEventTable(cx *cliContext, events []journal.Event) {
	tw := tabwriter.NewWriter(cx.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  时间\t状态\t路由\t模型\t首字\t总时长\t说明")
	for _, e := range events {
		switch e.Kind {
		case journal.KindRequest:
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				e.At.Format("01-02 15:04:05"),
				requestStatus(e),
				dashIfEmpty(e.Route),
				dashIfEmpty(shortModel(e.Model)),
				msOrDash(e.TTFTMs),
				msOrDash(e.LatencyMs),
				requestNote(e))
		case journal.KindReject:
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				e.At.Format("01-02 15:04:05"), e.Status,
				string(e.Src), dashIfEmpty(e.Path), "—", "—",
				fmt.Sprintf("被拒 ×%d", maxInt(e.Count, 1)))
		case journal.KindFidelity:
			// The inbound shape gets the full row too: it is the half of the
			// picture the usage record cannot supply, and squeezing it into
			// request columns would leave the fields that matter unrendered --
			// which is exactly what happened before this branch existed.
			fmt.Fprintf(tw, "  %s\t%s\t%s\n",
				e.At.Format("01-02 15:04:05"), "▸ 入站", fidelityDetail(e))
		default:
			// State events span the whole row: they are the context, and
			// squeezing them into request columns would hide the one line that
			// explains the others.
			fmt.Fprintf(tw, "  %s\t%s\t%s\n",
				e.At.Format("01-02 15:04:05"), stateTag(e), stateDetail(e))
		}
	}
	_ = tw.Flush()
}

func writeJournalSummary(cx *cliContext, s journal.Summary, since time.Duration) {
	fmt.Fprintf(cx.stdout, "  最近 %s：%d 个请求", shortDuration(since), s.Requests)
	if s.Failed > 0 {
		fmt.Fprintf(cx.stdout, "，%d 个失败", s.Failed)
		var parts []string
		for code, n := range s.ByStatus {
			label := fmt.Sprint(code)
			if code == 0 {
				label = "无状态码"
			}
			parts = append(parts, fmt.Sprintf("%s×%d", label, n))
		}
		fmt.Fprintf(cx.stdout, "（%s）", strings.Join(parts, " "))
	}
	fmt.Fprintln(cx.stdout)

	if s.P50Ms > 0 {
		fmt.Fprintf(cx.stdout, "  时延 p50 %s，最慢 %s\n", msOrDash(s.P50Ms), msOrDash(s.SlowestMs))
	}
	// The cache verdict, stated rather than left to be computed from two
	// columns. On a subscription this is what decides whether a long
	// conversation costs what it should.
	switch {
	case s.CacheRead > 0:
		fmt.Fprintf(cx.stdout, "  缓存：命中 %s token", compactTokens(s.CacheRead))
		if s.CacheCreation > 0 {
			fmt.Fprintf(cx.stdout, "，新建 %s", compactTokens(s.CacheCreation))
		}
		if s.CacheMissed > 0 {
			fmt.Fprintf(cx.stdout, "；%d 个请求带 cache_control 但没有命中", s.CacheMissed)
		}
		fmt.Fprintln(cx.stdout)
	case s.CacheWanted > 0:
		fmt.Fprintf(cx.stdout, "  ⚠ 缓存：%d 个请求要求缓存但一次都没命中——长对话会按全价重复计费\n", s.CacheWanted)
	}

	if s.MaxQuota >= 0 {
		// The number that decides anything on a subscription: requests per
		// minute says how busy it was, this says whether there is anything
		// left.
		fmt.Fprintf(cx.stdout, "  5 小时窗口用量峰值 %.0f%%\n", s.MaxQuota*100)
	}
	if s.Noise > 0 {
		fmt.Fprintf(cx.stdout, "  另有 %d 次外部被拒请求（扫描噪音，未计入上面的失败数）\n", s.Noise)
	}
}

// requestStatus renders the status column.
//
// The cause fallback is the point of the column existing. A transport failure
// carries no HTTP status, so this printed a bare "err" for the largest category
// of failure in the file -- and a week of "err" answers nothing, which is the
// opposite of what the journal is for. A status code is preferred when there is
// one: it is more specific, and Cause is only ever "upstream" beside it.
func requestStatus(e journal.Event) string {
	if e.OK != nil && *e.OK {
		return "ok"
	}
	if e.Status > 0 {
		return fmt.Sprint(e.Status)
	}
	if e.Cause != "" && e.Cause != metrics.CauseOther {
		return string(e.Cause)
	}
	return "err"
}

// requestNote carries what does not fit a column: cache activity, which
// credential served it, and how full the quota window was.
func requestNote(e journal.Event) string {
	var parts []string
	// Cache first: it is the number that decides whether a long conversation
	// costs what it should.
	if e.CacheRead > 0 {
		parts = append(parts, fmt.Sprintf("缓存命中 %s", compactTokens(e.CacheRead)))
	} else if e.CacheCreation > 0 {
		parts = append(parts, fmt.Sprintf("建缓存 %s", compactTokens(e.CacheCreation)))
	}
	if e.Auth != "" {
		parts = append(parts, shortAuth(e.Auth))
	}
	if e.Quota5h != nil {
		parts = append(parts, fmt.Sprintf("配额 %.0f%%", *e.Quota5h*100))
	}
	return strings.Join(parts, " · ")
}

// fidelityDetail renders an inbound-shape observation.
func fidelityDetail(e journal.Event) string {
	var parts []string
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	parts = append(parts, "客户端 "+orUnknownClient(e.UAClient))
	if e.UAClient == "claude-cli" {
		parts = append(parts, "不改写透传")
	} else if e.UAClient != "" {
		parts = append(parts, "system 会被改写")
	}
	if e.SystemBlocks > 0 {
		parts = append(parts, fmt.Sprintf("system %d 块", e.SystemBlocks))
	}
	if e.CacheControls > 0 {
		parts = append(parts, fmt.Sprintf("cache_control ×%d", e.CacheControls))
	}
	if e.Messages > 0 {
		parts = append(parts, fmt.Sprintf("%d 条消息", e.Messages))
	}
	if e.Tools > 0 {
		parts = append(parts, fmt.Sprintf("%d 个工具", e.Tools))
	}
	if e.Thinking {
		parts = append(parts, "thinking")
	}
	if e.BodyKB > 0 {
		parts = append(parts, fmt.Sprintf("%dKB", e.BodyKB))
	}
	return strings.Join(parts, " · ")
}

func orUnknownClient(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

func compactTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func stateTag(e journal.Event) string {
	switch e.Kind {
	case journal.KindProxy:
		return "▸ 代理"
	case journal.KindTunnel:
		return "▸ 隧道"
	case journal.KindCred:
		return "▸ 凭据"
	case journal.KindDoctor:
		return "▸ 诊断"
	default:
		return "▸ " + string(e.Kind)
	}
}

func stateDetail(e journal.Event) string {
	var parts []string
	if e.State != "" {
		parts = append(parts, e.State)
	}
	if e.Level != "" {
		parts = append(parts, e.Level)
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	return strings.Join(parts, " · ")
}

// shortAuth trims a credential filename to something that fits a column.
func shortAuth(s string) string {
	s = strings.TrimSuffix(s, ".json")
	if i := strings.IndexByte(s, '@'); i > 0 {
		return s[:i]
	}
	return s
}

// shortModel drops the vendor prefix, which is already implied by the route.
func shortModel(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimPrefix(s, "claude-")
}

func msOrDash(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func shortDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
