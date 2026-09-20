package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laurent00TT/slimproxy/tunnel"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// fakeIPNet is RFC 2544 benchmark space, 198.18.0.0/15.
//
// It is never a real destination. Local proxies and VPNs in fake-ip mode hand
// it out as a synthetic answer and then intercept the connection, which is
// invisible to an application unless it looks. Two separate outages in this
// project traced back to it: cloudflared could not reach the tunnel edge over
// QUIC, and the proxy intermittently could not reach api.anthropic.com.
var fakeIPNet = &net.IPNet{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)}

// edgeHostSuffix is the part every edge hostname shares.
const edgeHostSuffix = ".v2.argotunnel.com"

// tunnelEdgeHosts are the names cloudflared resolves to find the Cloudflare
// edge. They are fixed, not derived from the tunnel's own hostname, which is
// what makes them answerable before anything has been started.
var tunnelEdgeHosts = []string{"region1" + edgeHostSuffix, "region2" + edgeHostSuffix}

// edgeLabel names one edge host inside a report line.
//
// Short, because the panel truncates every line to its width (see tui/view.go's
// result panel): two 25-cell hostnames push the addresses past the right edge,
// and an operator told "the edge is in reserved space" with no address on screen
// has been handed a conclusion and shown none of it. Nothing is lost by
// dropping the suffix -- the remedy names *.argotunnel.com, which is the string
// that goes into the proxy rules.
func edgeLabel(host string) string { return strings.TrimSuffix(host, edgeHostSuffix) }

// lookupIPv4 is the one place this package asks DNS a question.
//
// A variable so the DNS checks can be exercised without a live resolver. A test
// that called the real one would be grading the developer's network rather than
// the code, and would pass or fail depending on whether their own machine is
// running a fake-ip proxy -- which is the exact condition under test.
var lookupIPv4 = func(ctx context.Context, host string) ([]net.IP, error) {
	return (&net.Resolver{}).LookupIP(ctx, "ip4", host)
}

// Target describes what to inspect. It mirrors the parts of the slimproxy
// config the checks need, so this package does not depend on the proxy package.
type Target struct {
	Host string
	Port int
	// AuthDir must be the RESOLVED directory (~ expanded, absolutized), the
	// same one the proxy loads from. Passing the raw config string here would
	// make this package inspect a different directory than the one that counts.
	AuthDir string
	// AuthDirErr records a failure to resolve AuthDir, which makes any finding
	// about the credential pool unreliable rather than negative.
	AuthDirErr error
	ConfigPath string
	// ConfigErrs carries problems found while loading the config -- type
	// mismatches, a missing file. The config is still usable for the checks
	// that do not depend on it, so these are reported rather than fatal.
	ConfigErrs []string

	// TunnelName is the cloudflared tunnel to query. Empty means either no
	// tunnel is configured or its configuration could not be read -- which of
	// the two is carried by TunnelErr, never inferred from this being empty.
	TunnelName string
	// TunnelHostname is the public hostname -- all of them, pre-joined for
	// display, when the ingress has several. Used to report what would be
	// reachable. Empty is fine.
	TunnelHostname string
	// TunnelErr is set when a cloudflared configuration exists but could not be
	// understood. Distinct from "not configured": see DetectTunnel.
	TunnelErr error

	// Untranslated lists protocol pairs this process has observed passing
	// through with no translator, as "方向:from->to".
	//
	// Only a running instance can supply it -- the finding comes from the
	// translation hooks at request time, not from anything on disk. An empty
	// slice from a separate process means "nothing to report from here",
	// not "nothing happened", which is why the check says so.
	Untranslated []string
	// FromRunningInstance reports whether Untranslated could have been
	// populated at all.
	FromRunningInstance bool

	// JournalDropped counts events the journal could not keep up with, and
	// JournalErr is its most recent write failure.
	//
	// A journal that silently stops recording is worse than no journal: the
	// absence of events later reads as an absence of problems.
	JournalDropped int64
	JournalErr     error
	JournalDir     string
}

func (t Target) addr() string {
	host := t.Host
	if host == "" {
		// An empty host means the proxy binds every interface. Probing 0.0.0.0
		// is not meaningful, so this dials the loopback -- which is a narrower
		// question than the one the config asks. ProbeAddr exists so callers
		// can say which address was actually tested instead of implying the
		// configured one was.
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, fmt.Sprint(t.Port))
}

// ProbeAddr is the address the port check actually connects to.
func (t Target) ProbeAddr() string { return t.addr() }

// Checks returns the diagnostic set applicable to a target.
//
// Two of the checks read state that exists only inside the serving process --
// which protocol pairs went untranslated, whether the journal is writing. From
// any other process they can only answer "cannot tell", so they are omitted
// rather than included as permanent Unknowns.
//
// The distinction matters more than it looks. Unknown outranks Warn here on
// purpose, and it drives the exit code, so two unanswerable checks made
// `slimproxy doctor` exit non-zero on every healthy deployment. A diagnostic
// that always fails is one people stop reading -- and the next real Unknown
// would arrive into an audience that had already learned to ignore it.
func Checks(t Target) []Check {
	checks := []Check{
		{Name: "listen-port", Timeout: 4 * time.Second, Run: t.checkListenPort},
		{Name: "credentials", Timeout: 4 * time.Second, Run: t.checkCredentials},
		{Name: "config-fields", Timeout: 4 * time.Second, Run: t.checkConfigFields},
		{Name: "upstream-dns", Timeout: 6 * time.Second, Run: t.checkUpstreamDNS},
		{Name: "upstream-reach", Timeout: 8 * time.Second, Run: t.checkUpstreamReach},
		{Name: "tunnel-edge-dns", Timeout: 6 * time.Second, Run: t.checkTunnelEdgeDNS},
		{Name: "tunnel", Timeout: 15 * time.Second, Run: t.checkTunnel},
	}
	if t.FromRunningInstance {
		checks = append(checks,
			Check{Name: "translator", Timeout: 2 * time.Second, Run: t.checkTranslator},
			Check{Name: "journal", Timeout: 2 * time.Second, Run: t.checkJournal},
		)
	}
	return checks
}

// checkJournal reports whether the event stream is actually being written.
//
// The journal is what later questions are answered from, so it failing is a
// failure that hides itself: nothing is missing at the time, and weeks later
// the gap looks like a quiet period.
func (t Target) checkJournal(context.Context) Result {
	if !t.FromRunningInstance {
		return Result{
			Level:  Unknown,
			Detail: i18n.T("只有运行中的实例知道事件日志的写入情况", "only a running instance knows whether the journal is being written"),
			Remedy: i18n.T("在面板里运行 /doctor", "run /doctor from the dashboard"),
		}
	}
	if t.JournalDir == "" {
		return Result{
			Level:  Warn,
			Detail: i18n.T("事件日志未启用，无法回溯历史请求", "the event journal is disabled; past requests cannot be reviewed"),
			Remedy: i18n.T("把 journal-days 设为正数（或删掉该项用默认值 7）重新启动", "set journal-days to a positive number (or delete it for the default 7) and restart"),
		}
	}
	if t.JournalErr != nil {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("事件日志写入失败: %v", "journal writes are failing: %v"), t.JournalErr),
			Remedy: i18n.T("检查 ", "check permissions and disk space on ") + t.JournalDir + i18n.T(" 的权限与磁盘空间", ""),
			Err:    t.JournalErr,
		}
	}
	if t.JournalDropped > 0 {
		return Result{
			Level: Warn,
			Detail: fmt.Sprintf(i18n.T("有 %d 条事件因写入队列已满被丢弃（磁盘跟不上请求速率）", "%d events were dropped because the write queue filled (disk cannot keep up with the request rate)"),
				t.JournalDropped),
			Remedy: i18n.T("这段时间的回溯记录不完整；若持续出现，检查磁盘性能", "the review record for this period is incomplete; if it persists, check disk performance"),
		}
	}
	return Result{Level: Pass, Detail: i18n.T("事件日志写入正常: ", "journal writes are healthy: ") + t.JournalDir}
}

// checkTranslator reports protocol pairs that were forwarded without
// translation.
//
// The registry returns an unregistered pair's body unchanged, so a request in
// one dialect reaches an upstream speaking another and the resulting error
// looks like an ordinary upstream failure. Nothing on disk records this; the
// only witness is the process that served the request.
func (t Target) checkTranslator(context.Context) Result {
	if !t.FromRunningInstance {
		// Unreachable through Checks, which omits this one outside the serving
		// process. Kept because the function is callable directly, and
		// answering "fine" to a question it cannot see would be worse than
		// saying so.
		return Result{
			Level:  Unknown,
			Detail: i18n.T("只有运行中的实例能观察到未翻译的协议组合，本次从外部检查无法判断", "only a running instance can observe untranslated protocol pairs; this external check cannot tell"),
			Remedy: i18n.T("在面板里运行 /doctor，或查看日志中是否有「没有为 X → Y 注册翻译器」", "run /doctor from the dashboard, or look for \"no translator registered for X → Y\" in the log"),
		}
	}
	if len(t.Untranslated) == 0 {
		return Result{Level: Pass, Detail: i18n.T("没有观察到缺少翻译器的协议组合", "no protocol pairs missing a translator were observed")}
	}
	return Result{
		Level: Fail,
		Detail: fmt.Sprintf(i18n.T("%d 个协议组合没有翻译器，报文被原样转发: %s", "%d protocol pairs have no translator; payloads forwarded untouched: %s"),
			len(t.Untranslated), strings.Join(t.Untranslated, ", ")),
		Remedy: i18n.T(
			"运行 \"slimproxy routes\" 查看支持的组合；把客户端改用受支持的协议，或改用与凭据匹配的上游",
			"run \"slimproxy routes\" to see supported pairs; switch the client to a supported protocol, or to an upstream matching the credentials"),
	}
}

// checkListenPort distinguishes three states that look alike from a log file:
// the port is free, slimproxy already holds it, or something else does.
func (t Target) checkListenPort(ctx context.Context) Result {
	// Port 0 means "assign me anything", so net.Listen on it always succeeds --
	// and the probe below would report "空闲，可以绑定" about an address the
	// proxy will never use. Reachable without any misconfiguration:
	// loadConfigForDiagnosis deliberately keeps going when the config file is
	// missing or has a type error, and leaves Port at zero.
	if t.Port <= 0 || t.Port > 65535 {
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf(i18n.T("端口 %d 不是合法端口，无法检查监听状态", "port %d is not a valid port; the listen state cannot be checked"), t.Port),
			Remedy: i18n.T("在配置里设置 1-65535 之间的 port；若配置文件本身有问题，先看 config-fields 这一项", "set port between 1-65535 in the config; if the config itself is broken, see config-fields first"),
		}
	}

	addr := t.addr()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		// A dial that failed because we ran out of time establishes nothing.
		// Treating it as "nothing is listening" would let a Ctrl-C during
		// doctor produce a confident, wrong answer.
		if ctx.Err() != nil {
			return Result{
				Level:  Unknown,
				Detail: fmt.Sprintf(i18n.T("检查被取消或超时，未能确定 %s 的状态", "the check was canceled or timed out; the state of %s is undetermined"), addr),
				Remedy: i18n.T("重新运行", "run it again"),
				Err:    ctx.Err(),
			}
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return Result{
				Level:  Unknown,
				Detail: fmt.Sprintf(i18n.T("探测 %s 超时，无法判断是否被占用", "probing %s timed out; whether it is in use cannot be determined"), addr),
				Remedy: i18n.T("重试；持续超时通常意味着防火墙在丢包而非端口空闲", "retry; persistent timeouts usually mean a firewall is dropping packets, not that the port is free"),
				Err:    err,
			}
		}

		// Nothing answered. Confirm we could actually bind, because "refused"
		// and "cannot bind" are different problems with the same symptom.
		ln, bindErr := net.Listen("tcp", addr)
		if bindErr != nil {
			return Result{
				Level:  Fail,
				Detail: fmt.Sprintf(i18n.T("%s 无法连接也无法绑定: %v", "%s can be neither connected to nor bound: %v"), addr, bindErr),
				Remedy: i18n.T("检查地址是否合法、端口是否被系统保留、是否需要提升权限", "check that the address is valid, the port is not system-reserved, and whether elevation is needed"),
				Err:    bindErr,
			}
		}
		_ = ln.Close()
		// "可以绑定", not "可以启动": binding is what was verified. Whether the
		// port is reachable from elsewhere depends on firewall rules this check
		// never touches.
		return Result{
			Level:  Pass,
			Detail: fmt.Sprintf(i18n.T("%s 空闲，可以绑定", "%s is free and can be bound"), addr),
		}
	}
	_ = conn.Close()

	// Something is listening. Is it us?
	healthy, herr := probeHealthz(ctx, addr)
	switch {
	case healthy:
		return Result{Level: Pass, Detail: fmt.Sprintf(i18n.T("%s 已被 slimproxy 占用且健康", "%s is held by slimproxy and healthy"), addr)}
	case herr != nil:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("%s 被占用，但 /healthz 无响应: %v", "%s is in use, but /healthz does not answer: %v"), addr, herr),
			Remedy: i18n.T("另一个程序占用了该端口；换 -port 或结束占用它的进程", "another program holds the port; pick another with -port or end that process"),
			Err:    herr,
		}
	default:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("%s 被占用，/healthz 返回了非 200", "%s is in use and /healthz returned non-200"), addr),
			Remedy: i18n.T("另一个程序占用了该端口；换 -port 或结束占用它的进程", "another program holds the port; pick another with -port or end that process"),
		}
	}
}

func probeHealthz(ctx context.Context, addr string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return false, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}

// checkCredentials reports whether the pool has anything usable in it.
//
// An empty auth-dir is Fail, not Warn: the proxy starts, accepts requests, and
// then fails every one of them upstream. That is a broken deployment wearing a
// healthy-looking listener.
func (t Target) checkCredentials(ctx context.Context) Result {
	if t.AuthDirErr != nil {
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf(i18n.T("无法解析 auth-dir: %v", "cannot resolve auth-dir: %v"), t.AuthDirErr),
			Remedy: i18n.T("检查 auth-dir 的写法；无法确定目录时不能判断凭据是否可用", "check how auth-dir is written; without the directory, credential usability cannot be judged"),
			Err:    t.AuthDirErr,
		}
	}
	dir := t.AuthDir
	if dir == "" {
		dir = "auths"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{
				Level:  Fail,
				Detail: fmt.Sprintf(i18n.T("auth-dir %s 不存在", "auth-dir %s does not exist"), dir),
				Remedy: i18n.T("启动 slimproxy 会创建它；之后需要放入至少一个凭据文件", "starting slimproxy creates it; at least one credential file must then be placed inside"),
				Err:    err,
			}
		}
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf(i18n.T("无法读取 auth-dir %s: %v", "cannot read auth-dir %s: %v"), dir, err),
			Remedy: i18n.T("检查目录权限", "check directory permissions"),
			Err:    err,
		}
	}

	var ok, bad []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			bad = append(bad, e.Name()+i18n.T("（无法读取）", " (unreadable)"))
			continue
		}
		var probe struct {
			Disabled bool `json:"disabled"`
		}
		if json.Unmarshal(body, &probe) != nil {
			bad = append(bad, e.Name()+i18n.T("（不是合法 JSON）", " (not valid JSON)"))
			continue
		}
		// A disabled credential parses perfectly and is worth exactly nothing:
		// credentials.Recoverable treats disabled and broken as the only two
		// states with no value. Counting it as usable let a pool whose sole
		// entry was disabled report PASS, on a proxy that would start, accept
		// requests, and fail every one -- the precise situation this check
		// exists to catch.
		if probe.Disabled {
			bad = append(bad, e.Name()+i18n.T("（已禁用）", " (disabled)"))
			continue
		}
		ok = append(ok, e.Name())
	}

	switch {
	case len(ok) == 0 && len(bad) == 0:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("auth-dir %s 中没有凭据文件", "auth-dir %s contains no credential files"), dir),
			Remedy: i18n.T("放入一个 OAuth 凭据 JSON；没有凭据时代理会接受请求但每一个都会失败", "place an OAuth credential JSON inside; with none, the proxy accepts requests and fails every one"),
		}
	case len(ok) == 0:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("%d 个凭据文件全部不可用: %s", "all %d credential files are unusable: %s"), len(bad), strings.Join(bad, ", ")),
			Remedy: i18n.T("重新生成或启用这些凭据；代理会接受请求但每一个都会失败", "regenerate or enable these credentials; the proxy accepts requests and fails every one"),
		}
	case len(bad) > 0:
		return Result{
			Level:  Warn,
			Detail: fmt.Sprintf(i18n.T("%d 个凭据可用，%d 个不可用: %s", "%d credentials usable, %d not: %s"), len(ok), len(bad), strings.Join(bad, ", ")),
			Remedy: i18n.T("移除、重新生成或启用这些凭据文件", "remove, regenerate or enable these credential files"),
		}
	default:
		return Result{Level: Pass, Detail: fmt.Sprintf(i18n.T("%d 个凭据文件可解析（未校验有效期与冷却状态）", "%d credential files parse (validity and cooldown not verified)"), len(ok))}
	}
}

// checkConfigFields catches the failure mode where a config carries settings
// this binary does not know about.
//
// This project has hit it: an older binary silently ignored a newly added
// field, so the config looked correct, -check reported no error, and the
// setting simply did nothing.
func (t Target) checkConfigFields(ctx context.Context) Result {
	if t.ConfigPath == "" {
		return Result{
			Level:  Unknown,
			Detail: i18n.T("未提供配置文件路径", "no config file path was given"),
			Remedy: i18n.T("用 -config 指定配置文件后重试", "point -config at the config file and retry"),
		}
	}
	// Problems the loader already found. Reported here rather than aborting the
	// whole run, because the checks that do not read the config are still worth
	// running -- often they are the ones that matter on a fresh machine.
	if len(t.ConfigErrs) > 0 {
		return Result{
			Level:  Fail,
			Detail: i18n.T("配置存在问题: ", "the config has problems: ") + strings.Join(t.ConfigErrs, "; "),
			Remedy: i18n.T("出错的字段会退回零值使用（例如 port 会变成 0），必须修正", "broken fields fall back to zero values (port becomes 0, for example); they must be fixed"),
		}
	}
	body, err := os.ReadFile(t.ConfigPath)
	if err != nil {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("无法读取配置 %s: %v", "cannot read config %s: %v"), t.ConfigPath, err),
			Remedy: i18n.T("确认路径正确、文件可读", "confirm the path is right and the file is readable"),
			Err:    err,
		}
	}
	unknown, err := UnknownFields(body)
	if err != nil {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("配置无法解析: %v", "the config does not parse: %v"), err),
			Remedy: i18n.T("修正 YAML 语法", "fix the YAML syntax"),
			Err:    err,
		}
	}
	if len(unknown) > 0 {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("配置中有本二进制不认识的字段: %s", "the config carries fields this binary does not know: %s"), strings.Join(unknown, ", ")),
			Remedy: i18n.T("字段名写错，或这个二进制比配置旧——重新构建后再试", "a field name is misspelled, or this binary is older than the config -- rebuild and retry"),
		}
	}
	return Result{Level: Pass, Detail: i18n.T("配置字段与本二进制匹配", "config fields match this binary")}
}

// checkUpstreamDNS looks for DNS interception on the upstream endpoint.
func (t Target) checkUpstreamDNS(ctx context.Context) Result {
	const host = "api.anthropic.com"
	ips, err := lookupIPv4(ctx, host)
	if err != nil {
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf(i18n.T("无法解析 %s: %v", "cannot resolve %s: %v"), host, err),
			Remedy: i18n.T("检查 DNS 配置；解析不了就无法判断是否被劫持", "check DNS configuration; without resolution, hijacking cannot be judged"),
			Err:    err,
		}
	}
	for _, ip := range ips {
		if fakeIPNet.Contains(ip) {
			return Result{
				Level: Warn,
				Detail: fmt.Sprintf(i18n.T("%s 解析为 %s，属于 RFC 2544 保留段（198.18.0.0/15）", "%s resolves to %s, inside the RFC 2544 reserved range (198.18.0.0/15)"),
					host, ip),
				Remedy: i18n.T(
					"本机代理软件在用 fake-ip 劫持 DNS。请求会经由该代理，长连接可能不稳；在代理规则中让 api.anthropic.com 直连可消除这一层",
					"a local proxy is hijacking DNS via fake-ip. Requests go through that proxy and long-lived connections may be unstable; excluding api.anthropic.com in the proxy rules removes this layer"),
			}
		}
	}
	return Result{Level: Pass, Detail: fmt.Sprintf(i18n.T("%s 解析到真实地址（%s）", "%s resolves to a real address (%s)"), host, ips[0])}
}

// checkUpstreamReach establishes whether the upstream is actually usable.
func (t Target) checkUpstreamReach(ctx context.Context) Result {
	const addr = "api.anthropic.com:443"
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		// Distinguish "upstream is down" from "this check could not run".
		// Reporting the latter as Fail contradicts upstream-dns, which reports
		// the same root cause as Unknown.
		if ctx.Err() != nil {
			return Result{
				Level:  Unknown,
				Detail: i18n.T("检查被取消或超时，未能确定上游可达性", "the check was canceled or timed out; upstream reachability is undetermined"),
				Remedy: i18n.T("重新运行", "run it again"),
				Err:    ctx.Err(),
			}
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return Result{
				Level:  Unknown,
				Detail: fmt.Sprintf(i18n.T("无法解析 %s，可达性未知", "cannot resolve %s; reachability unknown"), addr),
				Remedy: i18n.T("先解决 DNS（见 upstream-dns 一项）", "fix DNS first (see the upstream-dns item)"),
				Err:    err,
			}
		}
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("无法连接 %s: %v", "cannot connect to %s: %v"), addr, err),
			Remedy: i18n.T("上游不可达时每个请求都会失败。检查网络与代理设置", "with the upstream unreachable every request fails. Check the network and proxy settings"),
			Err:    err,
		}
	}
	_ = conn.Close()
	return Result{Level: Pass, Detail: fmt.Sprintf(i18n.T("%s TCP 可连接（未验证 TLS）", "%s accepts TCP (TLS not verified)"), addr)}
}

// checkTunnelEdgeDNS catches fake-ip interception of the Cloudflare edge before
// anything tries to start the tunnel.
//
// The outage this exists for: region1/region2.v2.argotunnel.com resolved to
// 198.18.0.11 and 198.18.0.12, no proxy rule matched argotunnel.com, and the
// traffic fell through to a relay that carried QUIC's first stateless round
// trip but not the rest of the handshake. cloudflared retried forever instead
// of exiting, the startup grace killed it, and all the operator saw was
// "启动隧道失败" over a width-truncated log tail. upstream-dns stayed green the
// whole time: it only ever asks about api.anthropic.com, which was resolving
// perfectly well.
//
// Checking this by hand on such a machine does not work. A TUN in fake-ip mode
// takes over port 53 outright, so nslookup returns the synthetic address no
// matter which resolver is named -- on the day of the outage 1.1.1.1, 8.8.8.8
// and an address running no resolver at all each answered 198.18.0.11. The
// answer the Go resolver gets here is the answer cloudflared will get.
func (t Target) checkTunnelEdgeDNS(ctx context.Context) Result {
	// Relevance is decided the way checkTunnel decides it, so the two can never
	// disagree about whether a tunnel exists. A config that exists but cannot be
	// read leaves that question open, and an open question is not a pass.
	if t.TunnelErr != nil {
		return Result{
			Level: Unknown,
			Detail: fmt.Sprintf(i18n.T("存在 cloudflared 配置但无法理解，无从判断是否需要检查边缘地址: %v",
				"a cloudflared config exists but could not be understood; whether the edge matters here is undetermined: %v"), t.TunnelErr),
			Remedy: i18n.T("先修好 ~/.cloudflared/config.yml（见 tunnel 一项）再重跑", "fix ~/.cloudflared/config.yml first (see the tunnel item), then run this again"),
			Err:    t.TunnelErr,
		}
	}
	if t.TunnelName == "" {
		// No tunnel, no edge to reach. A finding about Cloudflare's edge on a
		// machine that never talks to it is noise, and noise is what gets the
		// rest of the report skimmed.
		return Result{Level: Pass, Detail: i18n.T("未配置隧道（跳过）", "no tunnel configured (skipped)")}
	}

	var fake, resolved, unresolved []string
	for _, host := range tunnelEdgeHosts {
		ips, err := lookupIPv4(ctx, host)
		if err != nil {
			if ctx.Err() != nil {
				// A lookup that ended because the deadline did says nothing
				// about DNS, and neither would the next one. Recording this
				// host as "does not resolve" would be a finding about the
				// clock, so the loop stops and the conclusion below rests on
				// whatever was observed before time ran out.
				break
			}
			// Not resolving and resolving to a synthetic address are different
			// findings with different fixes. Collapsing them would send the
			// operator into the proxy rules over a plain DNS outage, or the
			// reverse -- and the reverse is the expensive direction.
			unresolved = append(unresolved, edgeLabel(host))
			continue
		}
		var hit string
		for _, ip := range ips {
			if fakeIPNet.Contains(ip) {
				hit = ip.String()
				break
			}
		}
		switch {
		case hit != "":
			fake = append(fake, edgeLabel(host)+" → "+hit)
		case len(ips) > 0:
			// "resolved", not "reachable": nothing here dialled the address.
			resolved = append(resolved, edgeLabel(host)+" → "+ips[0].String())
		default:
			// An empty answer with no error is neither a real address nor a fake
			// one. Counting it as real would report an address never seen.
			unresolved = append(unresolved, edgeLabel(host))
		}
	}

	if len(fake) > 0 {
		// Warn rather than Fail, for the same reason checkTunnel reports its own
		// fake-ip branch as Warn: what was observed is interception, not
		// failure. Some relays do carry the handshake, and the check that gets
		// to call the tunnel broken is the one that looked at the tunnel.
		return Result{
			Level: Warn,
			Detail: fmt.Sprintf(i18n.T("边缘地址落在 RFC 2544 保留段 198.18.0.0/15: %s",
				"edge is in RFC 2544 space 198.18.0.0/15: %s"), strings.Join(fake, ", ")),
			// The action first, then why both halves are needed, then where to
			// read more: the panel truncates this line to its width, so what
			// the operator has to do must arrive before the cut, and the doc
			// pointer -- the one part still findable without this line -- is
			// what gets sacrificed to it. The cause is already on the detail
			// line above; restating it here would spend the readable width
			// saying nothing new. The direct rule alone is the trap: fake-ip
			// answers first, so the rule is matched against 198.18.x.x and
			// never fires.
			Remedy: i18n.T(
				"让 *.argotunnel.com 直连，并加入 fake-ip 排除列表；只加直连规则永远匹配不上，见 deploy/TUNNEL.md",
				"route *.argotunnel.com direct AND exempt it from fake-ip; a direct rule alone never matches, see deploy/TUNNEL.md"),
		}
	}

	if len(resolved) > 0 {
		// cloudflared registers against whichever region answers, so one region
		// missing is not the condition this check exists for. Promoting it to
		// Unknown would make doctor exit non-zero on a tunnel that comes up
		// fine. A real answer is also a real observation: fake-ip answers every
		// name under the domain, so seeing one genuine edge address rules the
		// interception out even when the run was cut short afterwards.
		detail := fmt.Sprintf(i18n.T("边缘地址真实: %s", "real edge addresses: %s"), strings.Join(resolved, ", "))
		if len(unresolved) > 0 {
			detail += fmt.Sprintf(i18n.T("（%s 未解析，用能解析的区域即可）",
				" (%s did not resolve; whichever region answers is enough)"), strings.Join(unresolved, ", "))
		}
		return Result{Level: Pass, Detail: detail}
	}

	// Nothing genuine and nothing synthetic was seen. A context that died first
	// explains that emptiness, and it is the only explanation this check is
	// entitled to give -- "DNS is broken" is a conclusion it did not reach.
	if ctx.Err() != nil {
		return Result{
			Level:  Unknown,
			Detail: i18n.T("检查被取消或超时，边缘地址的解析结果未知", "the check was canceled or timed out; the edge resolution is undetermined"),
			Remedy: i18n.T("重新运行", "run it again"),
			Err:    ctx.Err(),
		}
	}

	return Result{
		Level: Unknown,
		Detail: fmt.Sprintf(i18n.T("边缘地址无法解析（%s），无从判断是否被劫持",
			"the edge addresses do not resolve (%s); whether they are intercepted cannot be judged"), strings.Join(unresolved, ", ")),
		// Not resolving is not the same as not being hijacked, which is why this
		// is Unknown and not Pass. The nslookup half is here because it is the
		// first thing anyone reaches for and the one thing that cannot work: a
		// fake-ip TUN owns port 53, so naming a public resolver changes nothing.
		Remedy: i18n.T(
			"先修 DNS；这种机器上 nslookup 复核没有意义，fake-ip 的 TUN 接管了 53 端口",
			"fix DNS first; nslookup proves nothing on such a machine -- a fake-ip TUN owns port 53"),
	}
}

// checkTunnel reports the three states a tunnel can be in, which a single
// boolean cannot express: not configured, configured but not running, or
// running with zero connections -- the last being the one that looks healthy in
// a process list while serving 502 to the internet.
func (t Target) checkTunnel(ctx context.Context) Result {
	// A configuration that exists but cannot be read is not the same as no
	// configuration. Reporting the former as the latter is how a broken tunnel
	// shows up green while the public hostname serves 502.
	if t.TunnelErr != nil {
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf(i18n.T("存在 cloudflared 配置但无法理解: %v", "a cloudflared config exists but could not be understood: %v"), t.TunnelErr),
			Remedy: i18n.T("修正 ~/.cloudflared/config.yml，或删除它以表示确实没有隧道", "fix ~/.cloudflared/config.yml, or delete it to state there really is no tunnel"),
			Err:    t.TunnelErr,
		}
	}
	if t.TunnelName == "" {
		return Result{Level: Pass, Detail: i18n.T("未配置隧道（跳过）", "no tunnel configured (skipped)")}
	}

	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		return Result{
			Level:  Unknown,
			Detail: i18n.T("找不到 cloudflared 可执行文件", "cloudflared executable not found"),
			Remedy: i18n.T("安装 cloudflared 或将其加入 PATH；否则无法判断隧道状态", "install cloudflared or add it to PATH; without it the tunnel state cannot be judged"),
			Err:    err,
		}
	}

	out, err := exec.CommandContext(ctx, bin, "tunnel", "info", t.TunnelName).CombinedOutput()
	text := string(out)
	if err != nil {
		if strings.Contains(strings.ToLower(text), "not found") {
			return Result{
				Level:  Fail,
				Detail: fmt.Sprintf(i18n.T("隧道 %q 不存在", "tunnel %q does not exist"), t.TunnelName),
				Remedy: i18n.T("运行 ", "run ") + "cloudflared tunnel create " + t.TunnelName,
				Err:    err,
			}
		}
		// Include err, not just the captured output: a command killed by the
		// context timeout produces no output at all, and reporting an empty
		// string after "失败:" tells the operator nothing.
		detail := fmt.Sprintf(i18n.T("查询隧道状态失败: %v", "querying the tunnel state failed: %v"), err)
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			if line := strings.TrimSpace(text[:i]); line != "" {
				detail += "（" + line + "）"
			}
		} else if line := strings.TrimSpace(text); line != "" {
			detail += "（" + line + "）"
		}
		return Result{
			Level:  Unknown,
			Detail: detail,
			Remedy: i18n.T("该查询需要网络和 cert.pem；无法查询时不能断定隧道是否正常", "the query needs network access and cert.pem; without it the tunnel cannot be pronounced healthy"),
			Err:    err,
		}
	}

	if strings.Contains(text, "does not have any active connection") {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("隧道 %q 存在但没有任何活动连接", "tunnel %q exists but has no active connections"), t.TunnelName),
			Remedy: i18n.T("cloudflared 未运行，或连接反复断开；此状态下外网访问返回 502", "cloudflared is not running, or connections keep dropping; in this state public access returns 502"),
		}
	}

	conn := tunnel.ParseTunnelInfo(text)
	switch levelForConnectors(conn.Count, conn.Parsed) {
	case Unknown:
		// The output did not look like anything this code knows how to read.
		// Reporting "0 connections" would be a fabricated number, and reporting
		// PASS would be worse.
		return Result{
			Level:  Unknown,
			Detail: i18n.T("无法解析 cloudflared 的输出（可能是版本差异）", "cannot parse cloudflared output (possibly a version difference)"),
			Remedy: fmt.Sprintf(i18n.T("手动运行 cloudflared tunnel info %s 自行查看", "run cloudflared tunnel info %s by hand and inspect it yourself"), t.TunnelName),
		}
	case Fail:
		// The one state this check exists for: a process that looks alive in a
		// task list while the hostname serves 502.
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf(i18n.T("隧道 %q 的连接表中没有任何连接", "tunnel %q has no connections in its connection table"), t.TunnelName),
			Remedy: i18n.T("cloudflared 未运行或连接已全部断开；此状态下外网访问返回 502", "cloudflared is not running or every connection has dropped; in this state public access returns 502"),
		}
	}

	detail := fmt.Sprintf(i18n.T("隧道 %q 有 %d 个活动连接", "tunnel %q has %d active connections"), t.TunnelName, conn.Count)
	if t.TunnelHostname != "" {
		detail += i18n.T("，对外主机名 ", ", public hostname ") + t.TunnelHostname
	}
	if fake := conn.FakeIP; fake != "" {
		return Result{
			Level:  Warn,
			Detail: detail + fmt.Sprintf(i18n.T("，但边缘地址 %s 属于保留段", ", but edge address %s is in the reserved range"), fake),
			Remedy: i18n.T("隧道流量经由本机代理，长连接可能反复断开；让 *.argotunnel.com 直连可消除", "tunnel traffic is going through a local proxy and long-lived connections may keep dropping; excluding *.argotunnel.com from the proxy removes this layer"),
		}
	}
	return Result{Level: Pass, Detail: detail}
}

// levelForConnectors turns a connector count into a level.
//
// Split out from checkTunnel so the classification can be tested without a
// cloudflared binary. The two rules that matter: output we could not parse is
// Unknown (never a count), and a parsed zero is Fail (never Pass) -- a tunnel
// with no connections is exactly the state that looks alive in a process list
// while the public hostname serves 502.
func levelForConnectors(n int, parsed bool) Level {
	switch {
	case !parsed:
		return Unknown
	case n == 0:
		return Fail
	default:
		return Pass
	}
}
