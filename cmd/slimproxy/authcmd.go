package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Laurent00TT/slimproxy/credentials"
	"github.com/Laurent00TT/slimproxy/fsperm"
)

// cmdAuth dispatches the auth sub-verbs.
func cmdAuth(cx *cliContext, args []string) error {
	cmd := lookup("auth")
	if len(args) == 0 {
		return fmt.Errorf("%w: auth 需要一个子命令\n\n用法: %s", errUsage, cmd.usage)
	}

	verb, rest := args[0], args[1:]
	switch verb {
	case "list", "ls":
		return cmdAuthList(cx, rest)
	case "add":
		return cmdAuthAdd(cx, rest)
	case "rm", "remove":
		return cmdAuthRemove(cx, rest)
	case "-h", "--help", "help":
		fmt.Fprintf(cx.stdout, "用法: %s\n\n%s\n\n子命令:\n"+
			"  list           列出凭据及其状态\n"+
			"  add <provider> 通过 OAuth 添加一个凭据\n"+
			"  rm <标识>      删除一个凭据\n\n可用 provider:\n", cmd.usage, cmd.summary)
		tw := tabwriter.NewWriter(cx.stdout, 0, 0, 2, ' ', 0)
		for _, p := range credentials.Providers() {
			fmt.Fprintf(tw, "  %s\t%s\n", p.Name, p.Summary)
		}
		_ = tw.Flush()
		return nil
	default:
		return fmt.Errorf("%w: 未知的 auth 子命令 %q（可用: list / add / rm）", errUsage, verb)
	}
}

// authSettings is what the auth commands need from the configuration.
type authSettings struct {
	// Dir is the resolved credential directory -- never the raw config string,
	// because the proxy expands ~ and absolutizes before loading, so operating
	// on the literal value would touch a different directory.
	Dir string
	// ProxyURL is the upstream proxy the serving path uses. Login must use the
	// same one or it reaches the provider directly.
	ProxyURL string
}

func authSettingsFor(cx *cliContext) (authSettings, error) {
	cfg, _, err := cx.loadConfigForDiagnosis()
	if err != nil {
		return authSettings{}, err
	}
	dir, err := cfg.ResolveAuthDir()
	if err != nil {
		return authSettings{}, fmt.Errorf("无法解析 auth-dir: %w", err)
	}
	return authSettings{Dir: dir, ProxyURL: cfg.ProxyURL}, nil
}

func cmdAuthList(cx *cliContext, args []string) error {
	cmd := lookup("auth")
	fs := newFlagSet(cx, cmd)
	bindConfigOnly(fs, cx)
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	set, err := authSettingsFor(cx)
	if err != nil {
		return err
	}
	creds, err := credentials.List(set.Dir)
	if err != nil {
		return err
	}

	fmt.Fprintf(cx.stdout, "凭据目录 %s\n\n", set.Dir)
	if len(creds) == 0 {
		fmt.Fprintf(cx.stdout, "  没有凭据。代理会接受请求，但每一个都会在上游失败。\n"+
			"  用 \"slimproxy auth add <provider>\" 添加一个。\n")
		return nil
	}

	now := time.Now()
	tw := tabwriter.NewWriter(cx.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  名称\tPROVIDER\t账号\t状态\t到期")
	usable, recoverable := 0, 0
	for _, c := range creds {
		if c.Usable(now) {
			usable++
		}
		if c.Recoverable(now) {
			recoverable++
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			c.Name, orDash(c.Provider), orDash(c.Account), c.Status(now), expiryText(c, now))
	}
	_ = tw.Flush()

	fmt.Fprintf(cx.stdout, "\n  共 %d 个，其中 %d 个当前可用\n", len(creds), usable)

	// The alarm is gated on Recoverable, not Usable, because those answer
	// different questions and only one of them is the question being asked.
	//
	// "代理会接受请求但全部失败" is a claim about a pool with nothing left to
	// load. A credential whose access token expired an hour ago is not usable
	// this instant, but the proxy loads it and refreshes it on its own -- so
	// gating on Usable raised the alarm on a deployment that was about to work
	// fine, while `auth rm` in this same file has always used Recoverable for
	// exactly this sentence.
	switch {
	case recoverable == 0:
		fmt.Fprintf(cx.stdout, "  ⚠ 没有可加载的凭据：代理会接受请求但全部失败\n")
	case usable == 0:
		fmt.Fprintf(cx.stdout, "  ⚠ 当前没有可直接服务的凭据；%d 个待自动刷新\n", recoverable)
	}
	for _, c := range creds {
		if c.Err != nil {
			fmt.Fprintf(cx.stdout, "  ⚠ %s: %v\n", c.Name, c.Err)
		}
	}
	return nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// expiryText renders the expiry as something relative, which is what an
// operator actually reads it for.
func expiryText(c credentials.Credential, now time.Time) string {
	if c.Err != nil {
		return "—"
	}
	if c.Expires.IsZero() {
		return "未记录"
	}
	d := c.Expires.Sub(now)
	switch {
	case d < 0:
		return fmt.Sprintf("%s 前", roughDuration(-d))
	default:
		return fmt.Sprintf("%s 后", roughDuration(d))
	}
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "不到 1 分钟"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%.1f 小时", d.Hours())
	default:
		return fmt.Sprintf("%.1f 天", d.Hours()/24)
	}
}

func cmdAuthAdd(cx *cliContext, args []string) error {
	cmd := lookup("auth")
	fs := newFlagSet(cx, cmd)
	bindConfigOnly(fs, cx)
	noBrowser := fs.Bool("no-browser", false, "不自动打开浏览器，改为打印授权链接")
	port := fs.Int("callback-port", 0, "OAuth 回调使用的本地端口（0 = 由 provider 决定）")
	rest, err := cx.parsePositional(fs, cmd, args, 1)
	if err != nil {
		return skipHandled(err)
	}
	if len(rest) == 0 {
		return fmt.Errorf("%w: 需要指定 provider（可用: %s）",
			errUsage, strings.Join(credentials.ProviderNames(), ", "))
	}
	provider := rest[0]

	// Validated before anything is created or announced: a typo used to print
	// "正在为 nonesuch 启动授权流程" and create the credential directory before
	// failing.
	if !credentials.KnownProvider(provider) {
		return fmt.Errorf("%w: 不支持的 provider %q（可用: %s）",
			errUsage, provider, strings.Join(credentials.ProviderNames(), ", "))
	}

	set, err := authSettingsFor(cx)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(set.Dir, 0o700); err != nil {
		return fmt.Errorf("无法创建凭据目录 %s: %w", set.Dir, err)
	}
	// About to receive OAuth tokens in plaintext. On Windows the mode above is
	// not access control -- see fsperm.
	if rerr := fsperm.Restrict(set.Dir); rerr != nil {
		fmt.Fprintf(cx.stderr, "slimproxy: 未能收紧凭据目录权限（本机其他用户可能可读）: %v\n", rerr)
	}

	// Deliberately not signal.NotifyContext: it would take over SIGINT, and
	// nothing downstream observes the context -- the SDK's OAuth wait selects
	// on its callback channels only. Installing a handler nobody honours turns
	// Ctrl-C into a no-op and leaves the command unkillable until the flow
	// times out. Default signal behaviour ends the process, and an interrupted
	// login leaves nothing behind to clean up.
	ctx := context.Background()

	fmt.Fprintf(cx.stdout, "正在为 %s 启动授权流程，凭据将写入 %s\n", provider, set.Dir)
	if set.ProxyURL != "" {
		fmt.Fprintf(cx.stdout, "经由代理 %s\n", set.ProxyURL)
	}
	if *noBrowser {
		fmt.Fprintf(cx.stdout, "（-no-browser：请手动打开下面打印的链接）\n")
	}
	// The callback listener is the SDK's, and it binds every interface rather
	// than loopback. Saying so is cheap; a silent open port is not.
	fmt.Fprintf(cx.stdout, "授权期间本机会临时监听一个回调端口，请勿在不可信网络中长时间停留。\n")
	fmt.Fprintln(cx.stdout)

	path, err := credentials.Login(ctx, credentials.LoginRequest{
		Provider:     provider,
		AuthDir:      set.Dir,
		ProxyURL:     set.ProxyURL,
		NoBrowser:    *noBrowser,
		CallbackPort: *port,
		Prompt:       terminalPrompt(cx),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errors.New("授权已取消")
		}
		return fmt.Errorf("授权失败: %w", err)
	}

	fmt.Fprintf(cx.stdout, "\n凭据已写入 %s\n", path)
	fmt.Fprintf(cx.stdout, "文件监听器会自动加载它，无需重启代理。\n")
	return nil
}

// terminalPrompt asks the operator for input on stdin.
//
// A closed stdin returns an error rather than an empty string: an OAuth flow
// that reads "" as an authorisation code produces a failure far from its cause.
func terminalPrompt(cx *cliContext) func(string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	return func(prompt string) (string, error) {
		fmt.Fprint(cx.stdout, prompt)
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) != "" {
				return strings.TrimSpace(line), nil
			}
			return "", fmt.Errorf("读取输入失败（授权流程需要交互式终端）: %w", err)
		}
		return strings.TrimSpace(line), nil
	}
}

func cmdAuthRemove(cx *cliContext, args []string) error {
	cmd := lookup("auth")
	fs := newFlagSet(cx, cmd)
	bindConfigOnly(fs, cx)
	yes := fs.Bool("y", false, "跳过确认")
	force := fs.Bool("force", false, "即使这是最后一个可用凭据也删除")
	rest, err := cx.parsePositional(fs, cmd, args, 1)
	if err != nil {
		return skipHandled(err)
	}
	if len(rest) == 0 {
		return fmt.Errorf("%w: 需要指定要删除的凭据（名称或账号）", errUsage)
	}
	id := rest[0]

	set, err := authSettingsFor(cx)
	if err != nil {
		return err
	}
	creds, err := credentials.List(set.Dir)
	if err != nil {
		return err
	}
	target, err := credentials.Find(creds, id)
	if err != nil {
		return err
	}

	fmt.Fprintf(cx.stdout, "将删除:\n  %s\n  provider %s，账号 %s，状态 %s\n  文件 %s\n\n",
		target.Name, orDash(target.Provider), orDash(target.Account),
		target.Status(time.Now()), target.File)

	if !*yes {
		ok, err := confirm(cx, "确认删除？此操作不可撤销 [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(cx.stdout, "已取消，未删除任何文件。")
			return nil
		}
	}

	// The resolved credential, not the identifier: re-resolving after the
	// prompt would let the pool change in between, so the file described above
	// and the file deleted could be different ones. The clock is read here
	// too -- a credential that expired while the operator was reading the
	// prompt should count as expired when the guard runs.
	now := time.Now()
	if err := credentials.RemoveResolved(creds, target, *force, now); err != nil {
		if errors.Is(err, credentials.ErrLastUsable) {
			return fmt.Errorf("%w\n若确实要删除，加 -force", err)
		}
		return err
	}
	fmt.Fprintf(cx.stdout, "已删除 %s\n", target.File)

	// Say what the pool looks like now. "已删除 X" alone leaves the operator to
	// discover an empty pool from a failing request.
	remaining := 0
	for _, c := range creds {
		if c.File != target.File && c.Recoverable(now) {
			remaining++
		}
	}
	if remaining == 0 {
		fmt.Fprintf(cx.stdout, "⚠ 凭据池已空：代理会接受请求但每一个都会失败\n")
	} else {
		fmt.Fprintf(cx.stdout, "剩余 %d 个凭据\n", remaining)
	}
	return nil
}

// stdinIsTerminal reports whether stdin can be prompted.
//
// Checked before reading rather than after: a redirected stdin that supplies
// nothing and never closes -- a pipe whose writer is still open, a service
// manager's inherited handle -- would otherwise block the command forever on a
// question nobody can see. Measured: an `auth rm` without -y hung until it was
// killed. Tests do not catch this, because a test binary's stdin returns EOF
// immediately.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// confirm asks a yes/no question.
//
// A non-interactive stdin is an error, not a default. Defaulting to "no" would
// make the command silently do nothing in a script; defaulting to "yes" would
// delete without anyone agreeing. Scripts pass -y.
func confirm(cx *cliContext, question string) (bool, error) {
	if !stdinIsTerminal() {
		return false, errors.New("需要确认，但标准输入不是交互式终端；" +
			"在脚本中请显式加 -y")
	}
	fmt.Fprint(cx.stdout, question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("读取确认输入失败: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
