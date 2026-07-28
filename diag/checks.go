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
)

// fakeIPNet is RFC 2544 benchmark space, 198.18.0.0/15.
//
// It is never a real destination. Local proxies and VPNs in fake-ip mode hand
// it out as a synthetic answer and then intercept the connection, which is
// invisible to an application unless it looks. Two separate outages in this
// project traced back to it: cloudflared could not reach the tunnel edge over
// QUIC, and the proxy intermittently could not reach api.anthropic.com.
var fakeIPNet = &net.IPNet{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)}

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
			Detail: "只有运行中的实例知道事件日志的写入情况",
			Remedy: "在面板里运行 /doctor",
		}
	}
	if t.JournalDir == "" {
		return Result{
			Level:  Warn,
			Detail: "事件日志未启用，无法回溯历史请求",
			Remedy: "把 journal-days 设为正数（或删掉该项用默认值 7）重新启动",
		}
	}
	if t.JournalErr != nil {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("事件日志写入失败: %v", t.JournalErr),
			Remedy: "检查 " + t.JournalDir + " 的权限与磁盘空间",
			Err:    t.JournalErr,
		}
	}
	if t.JournalDropped > 0 {
		return Result{
			Level: Warn,
			Detail: fmt.Sprintf("有 %d 条事件因写入队列已满被丢弃（磁盘跟不上请求速率）",
				t.JournalDropped),
			Remedy: "这段时间的回溯记录不完整；若持续出现，检查磁盘性能",
		}
	}
	return Result{Level: Pass, Detail: "事件日志写入正常: " + t.JournalDir}
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
			Detail: "只有运行中的实例能观察到未翻译的协议组合，本次从外部检查无法判断",
			Remedy: "在面板里运行 /doctor，或查看日志中是否有「没有为 X → Y 注册翻译器」",
		}
	}
	if len(t.Untranslated) == 0 {
		return Result{Level: Pass, Detail: "没有观察到缺少翻译器的协议组合"}
	}
	return Result{
		Level: Fail,
		Detail: fmt.Sprintf("%d 个协议组合没有翻译器，报文被原样转发: %s",
			len(t.Untranslated), strings.Join(t.Untranslated, ", ")),
		Remedy: "运行 \"slimproxy routes\" 查看支持的组合；把客户端改用受支持的协议，" +
			"或改用与凭据匹配的上游",
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
			Detail: fmt.Sprintf("端口 %d 不是合法端口，无法检查监听状态", t.Port),
			Remedy: "在配置里设置 1-65535 之间的 port；若配置文件本身有问题，先看 config-fields 这一项",
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
				Detail: fmt.Sprintf("检查被取消或超时，未能确定 %s 的状态", addr),
				Remedy: "重新运行",
				Err:    ctx.Err(),
			}
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return Result{
				Level:  Unknown,
				Detail: fmt.Sprintf("探测 %s 超时，无法判断是否被占用", addr),
				Remedy: "重试；持续超时通常意味着防火墙在丢包而非端口空闲",
				Err:    err,
			}
		}

		// Nothing answered. Confirm we could actually bind, because "refused"
		// and "cannot bind" are different problems with the same symptom.
		ln, bindErr := net.Listen("tcp", addr)
		if bindErr != nil {
			return Result{
				Level:  Fail,
				Detail: fmt.Sprintf("%s 无法连接也无法绑定: %v", addr, bindErr),
				Remedy: "检查地址是否合法、端口是否被系统保留、是否需要提升权限",
				Err:    bindErr,
			}
		}
		_ = ln.Close()
		// "可以绑定", not "可以启动": binding is what was verified. Whether the
		// port is reachable from elsewhere depends on firewall rules this check
		// never touches.
		return Result{
			Level:  Pass,
			Detail: fmt.Sprintf("%s 空闲，可以绑定", addr),
		}
	}
	_ = conn.Close()

	// Something is listening. Is it us?
	healthy, herr := probeHealthz(ctx, addr)
	switch {
	case healthy:
		return Result{Level: Pass, Detail: fmt.Sprintf("%s 已被 slimproxy 占用且健康", addr)}
	case herr != nil:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("%s 被占用，但 /healthz 无响应: %v", addr, herr),
			Remedy: "另一个程序占用了该端口；换 -port 或结束占用它的进程",
			Err:    herr,
		}
	default:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("%s 被占用，/healthz 返回了非 200", addr),
			Remedy: "另一个程序占用了该端口；换 -port 或结束占用它的进程",
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
			Detail: fmt.Sprintf("无法解析 auth-dir: %v", t.AuthDirErr),
			Remedy: "检查 auth-dir 的写法；无法确定目录时不能判断凭据是否可用",
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
				Detail: fmt.Sprintf("auth-dir %s 不存在", dir),
				Remedy: "启动 slimproxy 会创建它；之后需要放入至少一个凭据文件",
				Err:    err,
			}
		}
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf("无法读取 auth-dir %s: %v", dir, err),
			Remedy: "检查目录权限",
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
			bad = append(bad, e.Name()+"（无法读取）")
			continue
		}
		var probe struct {
			Disabled bool `json:"disabled"`
		}
		if json.Unmarshal(body, &probe) != nil {
			bad = append(bad, e.Name()+"（不是合法 JSON）")
			continue
		}
		// A disabled credential parses perfectly and is worth exactly nothing:
		// credentials.Recoverable treats disabled and broken as the only two
		// states with no value. Counting it as usable let a pool whose sole
		// entry was disabled report PASS, on a proxy that would start, accept
		// requests, and fail every one -- the precise situation this check
		// exists to catch.
		if probe.Disabled {
			bad = append(bad, e.Name()+"（已禁用）")
			continue
		}
		ok = append(ok, e.Name())
	}

	switch {
	case len(ok) == 0 && len(bad) == 0:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("auth-dir %s 中没有凭据文件", dir),
			Remedy: "放入一个 OAuth 凭据 JSON；没有凭据时代理会接受请求但每一个都会失败",
		}
	case len(ok) == 0:
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("%d 个凭据文件全部不可用: %s", len(bad), strings.Join(bad, ", ")),
			Remedy: "重新生成或启用这些凭据；代理会接受请求但每一个都会失败",
		}
	case len(bad) > 0:
		return Result{
			Level:  Warn,
			Detail: fmt.Sprintf("%d 个凭据可用，%d 个不可用: %s", len(ok), len(bad), strings.Join(bad, ", ")),
			Remedy: "移除、重新生成或启用这些凭据文件",
		}
	default:
		return Result{Level: Pass, Detail: fmt.Sprintf("%d 个凭据文件可解析（未校验有效期与冷却状态）", len(ok))}
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
			Detail: "未提供配置文件路径",
			Remedy: "用 -config 指定配置文件后重试",
		}
	}
	// Problems the loader already found. Reported here rather than aborting the
	// whole run, because the checks that do not read the config are still worth
	// running -- often they are the ones that matter on a fresh machine.
	if len(t.ConfigErrs) > 0 {
		return Result{
			Level:  Fail,
			Detail: "配置存在问题: " + strings.Join(t.ConfigErrs, "; "),
			Remedy: "出错的字段会退回零值使用（例如 port 会变成 0），必须修正",
		}
	}
	body, err := os.ReadFile(t.ConfigPath)
	if err != nil {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("无法读取配置 %s: %v", t.ConfigPath, err),
			Remedy: "确认路径正确、文件可读",
			Err:    err,
		}
	}
	unknown, err := UnknownFields(body)
	if err != nil {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("配置无法解析: %v", err),
			Remedy: "修正 YAML 语法",
			Err:    err,
		}
	}
	if len(unknown) > 0 {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("配置中有本二进制不认识的字段: %s", strings.Join(unknown, ", ")),
			Remedy: "字段名写错，或这个二进制比配置旧——重新构建后再试",
		}
	}
	return Result{Level: Pass, Detail: "配置字段与本二进制匹配"}
}

// checkUpstreamDNS looks for DNS interception on the upstream endpoint.
func (t Target) checkUpstreamDNS(ctx context.Context) Result {
	const host = "api.anthropic.com"
	ips, err := (&net.Resolver{}).LookupIP(ctx, "ip4", host)
	if err != nil {
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf("无法解析 %s: %v", host, err),
			Remedy: "检查 DNS 配置；解析不了就无法判断是否被劫持",
			Err:    err,
		}
	}
	for _, ip := range ips {
		if fakeIPNet.Contains(ip) {
			return Result{
				Level: Warn,
				Detail: fmt.Sprintf("%s 解析为 %s，属于 RFC 2544 保留段（198.18.0.0/15）",
					host, ip),
				Remedy: "本机代理软件在用 fake-ip 劫持 DNS。请求会经由该代理，长连接可能不稳；" +
					"在代理规则中让 api.anthropic.com 直连可消除这一层",
			}
		}
	}
	return Result{Level: Pass, Detail: fmt.Sprintf("%s 解析到真实地址（%s）", host, ips[0])}
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
				Detail: "检查被取消或超时，未能确定上游可达性",
				Remedy: "重新运行",
				Err:    ctx.Err(),
			}
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return Result{
				Level:  Unknown,
				Detail: fmt.Sprintf("无法解析 %s，可达性未知", addr),
				Remedy: "先解决 DNS（见 upstream-dns 一项）",
				Err:    err,
			}
		}
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("无法连接 %s: %v", addr, err),
			Remedy: "上游不可达时每个请求都会失败。检查网络与代理设置",
			Err:    err,
		}
	}
	_ = conn.Close()
	return Result{Level: Pass, Detail: fmt.Sprintf("%s TCP 可连接（未验证 TLS）", addr)}
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
			Detail: fmt.Sprintf("存在 cloudflared 配置但无法理解: %v", t.TunnelErr),
			Remedy: "修正 ~/.cloudflared/config.yml，或删除它以表示确实没有隧道",
			Err:    t.TunnelErr,
		}
	}
	if t.TunnelName == "" {
		return Result{Level: Pass, Detail: "未配置隧道（跳过）"}
	}

	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		return Result{
			Level:  Unknown,
			Detail: "找不到 cloudflared 可执行文件",
			Remedy: "安装 cloudflared 或将其加入 PATH；否则无法判断隧道状态",
			Err:    err,
		}
	}

	out, err := exec.CommandContext(ctx, bin, "tunnel", "info", t.TunnelName).CombinedOutput()
	text := string(out)
	if err != nil {
		if strings.Contains(strings.ToLower(text), "not found") {
			return Result{
				Level:  Fail,
				Detail: fmt.Sprintf("隧道 %q 不存在", t.TunnelName),
				Remedy: "运行 cloudflared tunnel create " + t.TunnelName,
				Err:    err,
			}
		}
		// Include err, not just the captured output: a command killed by the
		// context timeout produces no output at all, and reporting an empty
		// string after "失败:" tells the operator nothing.
		detail := fmt.Sprintf("查询隧道状态失败: %v", err)
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
			Remedy: "该查询需要网络和 cert.pem；无法查询时不能断定隧道是否正常",
			Err:    err,
		}
	}

	if strings.Contains(text, "does not have any active connection") {
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("隧道 %q 存在但没有任何活动连接", t.TunnelName),
			Remedy: "cloudflared 未运行，或连接反复断开；此状态下外网访问返回 502",
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
			Detail: "无法解析 cloudflared 的输出（可能是版本差异）",
			Remedy: fmt.Sprintf("手动运行 cloudflared tunnel info %s 自行查看", t.TunnelName),
		}
	case Fail:
		// The one state this check exists for: a process that looks alive in a
		// task list while the hostname serves 502.
		return Result{
			Level:  Fail,
			Detail: fmt.Sprintf("隧道 %q 的连接表中没有任何连接", t.TunnelName),
			Remedy: "cloudflared 未运行或连接已全部断开；此状态下外网访问返回 502",
		}
	}

	detail := fmt.Sprintf("隧道 %q 有 %d 个活动连接", t.TunnelName, conn.Count)
	if t.TunnelHostname != "" {
		detail += "，对外主机名 " + t.TunnelHostname
	}
	if fake := conn.FakeIP; fake != "" {
		return Result{
			Level:  Warn,
			Detail: detail + fmt.Sprintf("，但边缘地址 %s 属于保留段", fake),
			Remedy: "隧道流量经由本机代理，长连接可能反复断开；让 *.argotunnel.com 直连可消除",
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
