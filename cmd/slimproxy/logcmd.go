package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Laurent00TT/slimproxy/i18n"
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
	since := fs.Duration("since", time.Hour, i18n.T("只看这段时间内的事件", "only events within this period"))
	status := fs.Int("status", 0, i18n.T("只看某个上游状态码，例如 429", "only one upstream status code, e.g. 429"))
	slow := fs.Duration("slow", 0, i18n.T("只看总时长超过该值的请求", "only requests slower than this in total"))
	route := fs.String("route", "", i18n.T("按路由过滤（子串，例如 openai）", "filter by route (substring, e.g. openai)"))
	model := fs.String("model", "", i18n.T("按模型过滤（子串）", "filter by model (substring)"))
	failed := fs.Bool("failed", false, i18n.T("只看失败的请求", "failed requests only"))
	noise := fs.Bool("noise", false, i18n.T("包含外网扫描等被拒流量（默认排除）", "include refused outside traffic such as scans (excluded by default)"))
	limit := fs.Int("n", 50, i18n.T("最多显示多少条（0 = 不限）", "show at most this many (0 = unlimited)"))
	stats := fs.Bool("stats", false, i18n.T("只打印汇总，不列出逐条事件", "summary only, no per-event rows"))
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
		fmt.Fprintf(cx.stdout, i18n.T("在 %s 内没有匹配的事件（目录 %s）\n", "no matching events within %s (directory %s)\n"), shortDuration(*since), dir)
		// Only advertise -noise when it would actually change the answer.
		// Suggesting it after an explicit -status search would be a second lie
		// on top of the one just fixed: nothing was hidden, so there is nothing
		// for the flag to reveal.
		if q.SuppressesNoise() {
			fmt.Fprint(cx.stdout, i18n.T("外网扫描等被拒流量默认不显示，加 -noise 查看。\n", "refused outside traffic (scans) is hidden by default; add -noise to see it.\n"))
		}
		return nil
	}

	if !*stats && len(events) > 0 {
		writeEventTable(cx, events)
		fmt.Fprintln(cx.stdout)
	}
	summary := journal.Summarise(events)
	summary.NoiseHidden = res.Noise
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
		return "", fmt.Errorf(i18n.T("无法解析事件日志目录: %w", "cannot resolve the event journal directory: %w"), err)
	}
	if dir == "" {
		return "", fmt.Errorf(i18n.T("事件日志已被 journal-days: %d 关闭", "the event journal is disabled by journal-days: %d"), cfg.JournalDays)
	}
	return dir, nil
}

func writeEventTable(cx *cliContext, events []journal.Event) {
	tw := tabwriter.NewWriter(cx.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, i18n.T("  时间\t状态\t路由\t模型\t首字\t总时长\t网络\t说明", "  time\tstatus\troute\tmodel\tttft\ttotal\tnet\tnotes"))
	for _, e := range events {
		switch e.Kind {
		case journal.KindRequest:
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				e.At.Format("01-02 15:04:05"),
				requestStatus(e),
				dashIfEmpty(e.Route),
				dashIfEmpty(shortModel(e.Model)),
				msOrDash(e.TTFTMs),
				msOrDash(e.LatencyMs),
				netCell(e),
				requestNote(e))
		case journal.KindReject:
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				e.At.Format("01-02 15:04:05"), e.Status,
				string(e.Src), dashIfEmpty(e.Path), "—", "—", "—",
				fmt.Sprintf(i18n.T("被拒 ×%d", "refused ×%d"), maxInt(e.Count, 1)))
		case journal.KindFidelity:
			// The inbound shape gets the full row too: it is the half of the
			// picture the usage record cannot supply, and squeezing it into
			// request columns would leave the fields that matter unrendered --
			// which is exactly what happened before this branch existed.
			fmt.Fprintf(tw, "  %s\t%s\t%s\n",
				e.At.Format("01-02 15:04:05"), i18n.T("▸ 入站", "▸ inbound"), fidelityDetail(e))
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
	fmt.Fprintf(cx.stdout, i18n.T("  最近 %s：%d 个请求", "  last %s: %d requests"), shortDuration(since), s.Requests)
	if s.Failed > 0 {
		fmt.Fprintf(cx.stdout, i18n.T("，%d 个失败", ", %d failed"), s.Failed)
		var parts []string
		for code, n := range s.ByStatus {
			label := fmt.Sprint(code)
			if code == 0 {
				label = i18n.T("无状态码", "no status")
			}
			parts = append(parts, fmt.Sprintf("%s×%d", label, n))
		}
		fmt.Fprintf(cx.stdout, "（%s）", strings.Join(parts, " "))
	}
	if s.Canceled > 0 {
		fmt.Fprintf(cx.stdout, i18n.T("，%d 个客户端取消", ", %d canceled by the client"), s.Canceled)
	}
	fmt.Fprintln(cx.stdout)

	if s.P50Ms > 0 {
		fmt.Fprintf(cx.stdout, i18n.T("  时延 p50 %s，最慢 %s\n", "  latency p50 %s, slowest %s\n"), msOrDash(s.P50Ms), msOrDash(s.SlowestMs))
	}
	if s.AvgUploadMs > 0 || s.AvgWriteBlockMs > 0 {
		fmt.Fprintf(cx.stdout, i18n.T("  网络 上传均值 %s，回写阻塞均值 %s\n", "  network: avg upload %s, avg write-block %s\n"), msOrDash(s.AvgUploadMs), msOrDash(s.AvgWriteBlockMs))
	}
	// The cache verdict, stated rather than left to be computed from two
	// columns. On a subscription this is what decides whether a long
	// conversation costs what it should.
	switch {
	case s.CacheRead > 0:
		fmt.Fprintf(cx.stdout, i18n.T("  缓存：命中 %s token", "  cache: %s tokens read"), compactTokens(s.CacheRead))
		if s.CacheCreation > 0 {
			fmt.Fprintf(cx.stdout, i18n.T("，新建 %s", ", %s written"), compactTokens(s.CacheCreation))
		}
		if s.CacheMissed > 0 {
			fmt.Fprintf(cx.stdout, i18n.T("；成功请求中 %d 个未读写缓存", "; %d successful requests neither read nor wrote cache"), s.CacheMissed)
		}
		fmt.Fprintln(cx.stdout)
	case s.CacheWanted > 0:
		fmt.Fprintf(cx.stdout, i18n.T("  ⚠ 缓存：%d 个请求要求缓存但一次都没命中——长对话会按全价重复计费\n", "  ⚠ cache: %d requests asked for caching and hit nothing -- long conversations bill at full price every turn\n"), s.CacheWanted)
	}

	if s.MaxQuota >= 0 {
		// The number that decides anything on a subscription: requests per
		// minute says how busy it was, this says whether there is anything
		// left.
		fmt.Fprintf(cx.stdout, i18n.T("  5 小时窗口用量峰值 %.0f%%\n", "  5-hour window peak usage %.0f%%\n"), s.MaxQuota*100)
	}
	// Two different statements, and the wording has to match which one is true:
	// withheld rows are news to the reader, listed ones are already on screen
	// and only need labelling.
	if s.NoiseHidden > 0 {
		fmt.Fprintf(cx.stdout, i18n.T("  另有 %d 次外部被拒请求未列出（扫描噪音，加 -noise 查看）\n", "  %d more refused outside requests not listed (scan noise; add -noise)\n"), s.NoiseHidden)
	}
	if s.Noise > 0 {
		fmt.Fprintf(cx.stdout, i18n.T("  上面有 %d 次是外部被拒请求（扫描噪音）\n", "  %d of the rows above are refused outside requests (scan noise)\n"), s.Noise)
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

// netCell renders the two net legs, or a dash when the event predates the
// meter. The upstream leg is not repeated here -- it is the 总时长 column.
func netCell(e journal.Event) string {
	var parts []string
	if e.UploadMs > 0 {
		parts = append(parts, i18n.T("传", "up ")+msOrDash(e.UploadMs))
	}
	if e.WriteBlockMs > 0 {
		parts = append(parts, i18n.T("写", "wr ")+msOrDash(e.WriteBlockMs))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "┊")
}

// requestNote carries what does not fit a column: cache activity, which
// credential served it, and how full the quota window was.
func requestNote(e journal.Event) string {
	var parts []string
	// Cache first: it is the number that decides whether a long conversation
	// costs what it should.
	if e.CacheRead > 0 {
		parts = append(parts, fmt.Sprintf(i18n.T("缓存命中 %s", "cache read %s"), compactTokens(e.CacheRead)))
	} else if e.CacheCreation > 0 {
		parts = append(parts, fmt.Sprintf(i18n.T("建缓存 %s", "cache write %s"), compactTokens(e.CacheCreation)))
	}
	if e.Auth != "" {
		parts = append(parts, shortAuth(e.Auth))
	}
	if e.Quota5h != nil {
		parts = append(parts, fmt.Sprintf(i18n.T("配额 %.0f%%", "quota %.0f%%"), *e.Quota5h*100))
	}
	return strings.Join(parts, " · ")
}

// fidelityDetail renders an inbound-shape observation.
func fidelityDetail(e journal.Event) string {
	var parts []string
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	parts = append(parts, i18n.T("客户端 ", "client ")+orUnknownClient(e.UAClient))
	if e.UAClient == "claude-cli" {
		parts = append(parts, i18n.T("不改写透传", "passed through unrewritten"))
	} else if e.UAClient != "" {
		parts = append(parts, i18n.T("system 会被改写", "system gets rewritten"))
	}
	if e.SystemBlocks > 0 {
		parts = append(parts, fmt.Sprintf(i18n.T("system %d 块", "system %d blocks"), e.SystemBlocks))
	}
	if e.CacheControls > 0 {
		parts = append(parts, fmt.Sprintf("cache_control ×%d", e.CacheControls))
	}
	if e.Messages > 0 {
		parts = append(parts, fmt.Sprintf(i18n.T("%d 条消息", "%d messages"), e.Messages))
	}
	if e.Tools > 0 {
		parts = append(parts, fmt.Sprintf(i18n.T("%d 个工具", "%d tools"), e.Tools))
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
		return i18n.T("未知", "unknown")
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
		return i18n.T("▸ 代理", "▸ proxy")
	case journal.KindTunnel:
		return i18n.T("▸ 隧道", "▸ tunnel")
	case journal.KindCred:
		return i18n.T("▸ 凭据", "▸ creds")
	case journal.KindDoctor:
		return i18n.T("▸ 诊断", "▸ doctor")
	default:
		return "▸ " + string(e.Kind)
	}
}

func stateDetail(e journal.Event) string {
	var parts []string
	if e.State != "" {
		parts = append(parts, e.State)
	}
	// The stall guard's health events carry the stream they are about --
	// model, credential, how far in. Rendered here, or the fields would exist
	// only for jq readers and the whole point of stamping them (answering
	// "which model, whose key" without the raw JSONL) would be lost. Absent
	// on every other state event, so their rows are unchanged.
	if e.Model != "" {
		parts = append(parts, shortModel(e.Model))
	}
	if e.Auth != "" {
		parts = append(parts, shortAuth(e.Auth))
	}
	if e.LatencyMs > 0 {
		// Second precision, not shortDuration's minutes: stream ages worth
		// showing start well under a minute, and "0 分钟" reads as a bug.
		parts = append(parts, (time.Duration(e.LatencyMs) * time.Millisecond).Truncate(time.Second).String())
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
		return fmt.Sprintf(i18n.T("%d 天", "%dd"), int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf(i18n.T("%d 小时", "%dh"), int(d.Hours()))
	default:
		return fmt.Sprintf(i18n.T("%d 分钟", "%dm"), int(d.Minutes()))
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
