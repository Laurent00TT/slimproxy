package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/Laurent00TT/slimproxy/i18n"
	"github.com/Laurent00TT/slimproxy/proxy"
)

// cliContext carries what every command needs, so a command body deals with its
// own arguments and nothing else.
type cliContext struct {
	configPath string
	stateDir   string
	port       int // 0 means "use whatever the config says"

	stdout io.Writer
	stderr io.Writer
}

// command is one entry in the CLI.
type command struct {
	name string
	// summaryZh and summaryEn hold both translations, rendered by summary().
	// Not a single field holding i18n.T's result: this registry is populated in
	// init, which runs before main has selected the language, so a summary
	// baked here would be stuck in the default language for the process's life.
	summaryZh string
	summaryEn string
	usage     string // shown by `slimproxy <name> -h`
	run       func(cx *cliContext, args []string) error
}

// summary is the one line shown in `slimproxy help`.
func (c *command) summary() string { return i18n.T(c.summaryZh, c.summaryEn) }

// commands is the registry. Order here is the order help prints.
//
// Populated in init rather than as a literal: help needs lookup, and lookup
// reads this, which the compiler rejects as an initialisation cycle when the
// registry is a package-level literal.
var commands []*command

func init() {
	commands = []*command{
		{
			name:      "serve",
			summaryZh: "启动代理；在终端里同时打开全屏面板（无参数运行时的默认行为)",
			summaryEn: "start the proxy; opens the full-screen panel in a terminal (the default with no arguments)",
			usage:     "slimproxy serve [-config FILE] [-state DIR] [-port N] [-no-tui]",
			run:       cmdServe,
		},
		{
			// The three inspection commands name each other on purpose. They
			// look interchangeable from the outside -- and status/doctor really
			// do share one check set -- so each summary says what it does that
			// the other two do not.
			name:      "check",
			summaryZh: "只读配置：校验并打印将要运行的内容（不联网，秒回）",
			summaryEn: "read-only: validate the config and print what would run (no network, instant)",
			usage:     "slimproxy check [-config FILE] [-state DIR] [-port N]",
			run:       cmdCheck,
		},
		{
			name:      "init",
			summaryZh: "生成一份可直接运行的配置（含随机 api-key）",
			summaryEn: "write a ready-to-run config (with a generated api-key)",
			usage:     "slimproxy init [-config FILE] [-init-auth-dir DIR] [-force]",
			run:       cmdInit,
		},
		{
			name:      "status",
			summaryZh: "与 doctor 同一套检查：只报告不建议，恒退出 0（适合脚本轮询）",
			summaryEn: "the doctor checks, report-only: no remedies, always exit 0 (for script polling)",
			usage:     "slimproxy status [-config FILE] [-state DIR] [-port N]",
			run:       cmdStatus,
		},
		{
			name:      "doctor",
			summaryZh: "与 status 同一套检查：附修复建议，有问题时退出非 0（适合排查）",
			summaryEn: "the status checks with remedies, non-zero exit on problems (for troubleshooting)",
			usage:     "slimproxy doctor [-config FILE] [-state DIR] [-port N]",
			run:       cmdDoctor,
		},
		{
			name:      "auth",
			summaryZh: "管理上游凭据（list / add / rm）",
			summaryEn: "manage upstream credentials (list / add / rm)",
			usage:     "slimproxy auth <list|add|rm> [args]",
			run:       cmdAuth,
		},
		{
			name:      "tunnel",
			summaryZh: "管理对外隧道（status / up / down）",
			summaryEn: "manage the public tunnel (status / up / down)",
			usage:     "slimproxy tunnel <status|up|down> [-state DIR] [-detach]",
			run:       cmdTunnel,
		},
		{
			name:      "routes",
			summaryZh: "列出哪些客户端协议能翻译到哪些上游协议",
			summaryEn: "list which client protocols translate to which upstreams",
			usage:     "slimproxy routes",
			run:       cmdRoutes,
		},
		{
			name:      "log",
			summaryZh: "查询事件日志：请求、失败、以及当时的隧道/凭据状态",
			summaryEn: "query the event journal: requests, failures, and the tunnel/credential state at the time",
			usage:     "slimproxy log [-since 1h] [-failed] [-status N] [-slow 10s] [-route X] [-n 50]",
			run:       cmdLog,
		},
		{
			name:      "test",
			summaryZh: "发一个真实请求走通整条链路（会消耗上游配额）",
			summaryEn: "send one real request through the whole chain (consumes upstream quota)",
			usage:     "slimproxy test [-config FILE] [-model NAME] [-prompt TEXT] [-timeout N]",
			run:       cmdTest,
		},
		{
			name:      "version",
			summaryZh: "打印版本与构建信息",
			summaryEn: "print version and build information",
			usage:     "slimproxy version",
			run:       cmdVersion,
		},
		{
			name:      "help",
			summaryZh: "显示命令列表，或某个命令的用法",
			summaryEn: "show the command list, or one command\u2019s usage",
			usage:     "slimproxy help [COMMAND]",
			run:       cmdHelp,
		},
	}
}

func lookup(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

// errUsage marks a failure that is the caller's fault rather than a runtime
// problem, so main can decide to print usage alongside it.
var errUsage = errors.New("usage")

// dispatch routes one invocation to a command.
//
// Two calling conventions are accepted on purpose. The subcommand form is the
// new one; the flag form (-check, -init, -routes) is what every existing script
// and every line of the current documentation uses. Dropping it would break
// them silently -- the binary would start serving instead of validating, which
// is the worst possible way for `slimproxy -check` to fail.
//
// Both forms end up in the same command function. There is no second
// implementation to drift.
func dispatch(cx *cliContext, args []string) error {
	if len(args) == 0 {
		return cmdServe(cx, nil)
	}

	first := args[0]
	if !strings.HasPrefix(first, "-") {
		cmd := lookup(first)
		if cmd == nil {
			return fmt.Errorf(i18n.T("%w: 未知命令 %q\n\n%s", "%w: unknown command %q\n\n%s"), errUsage, first, commandList())
		}
		return cmd.run(cx, args[1:])
	}

	return dispatchLegacy(cx, args)
}

// dispatchLegacy maps the original flag-only interface onto commands.
//
// It parses the whole flag set once, because the old interface allowed the
// selector and its options in any order (`-check -config x` and
// `-config x -check` both worked), then forwards to the command the selector
// names with the options it consumed.
func dispatchLegacy(cx *cliContext, args []string) error {
	fs := flag.NewFlagSet("slimproxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // all output is ours; see cliContext.parse
	configPath := fs.String("config", defaultConfigPath, "path to the slimproxy config file")
	stateDir := fs.String("state", ".", "directory for the generated effective CLIProxyAPI config")
	port := fs.Int("port", 0, "override the configured port")
	check := fs.Bool("check", false, "validate the config and print what would run, then exit")
	routes := fs.Bool("routes", false, "list the translator routes this build can serve, then exit")
	doInit := fs.Bool("init", false, "write a ready-to-run config with generated keys, then exit")
	initAuth := fs.String("init-auth-dir", "auths", "auth-dir to put in the generated config")
	force := fs.Bool("force", false, "with -init, overwrite an existing config")
	// serve's own option, accepted here because "no command means serve" is
	// what the flag-only form promises: `slimproxy -port 9999` already reaches
	// serve, and `slimproxy -no-tui` -- the form the guide gives service
	// managers -- was the one serve option this dispatcher rejected.
	noTUI := fs.Bool("no-tui", false, "with serve, no full-screen panel, logs only")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(cx.stdout, commandList())
			return nil
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	// flag.Parse stops at the first non-flag argument and leaves the rest in
	// Args(). Ignoring them is how `slimproxy -config X check` would silently
	// START SERVING instead of validating: no selector is set, so the switch
	// below falls through to serve, binding a port and loading credentials on a
	// command the user meant as a dry run.
	if fs.NArg() > 0 {
		if lookup(fs.Arg(0)) != nil {
			return fmt.Errorf(i18n.T("%w: 子命令 %q 必须写在选项前面，例如: slimproxy %s -config ...", "%w: subcommand %q must come before options, e.g.: slimproxy %s -config ..."),
				errUsage, fs.Arg(0), fs.Arg(0))
		}
		return fmt.Errorf(i18n.T("%w: 无法识别的参数 %q\n\n%s", "%w: unrecognised argument %q\n\n%s"), errUsage, fs.Arg(0), commandList())
	}

	// Two selectors are a contradiction, not a precedence question. Picking the
	// first would run `-check -init` as init -- writing a config file when the
	// user asked to validate one, and exiting 0.
	selected := []string{}
	if *doInit {
		selected = append(selected, "-init")
	}
	if *routes {
		selected = append(selected, "-routes")
	}
	if *check {
		selected = append(selected, "-check")
	}
	if len(selected) > 1 {
		return fmt.Errorf(i18n.T("%w: %s 不能同时使用，请一次只选一个", "%w: %s cannot be combined; pick one at a time"), errUsage, strings.Join(selected, i18n.T(" 与 ", " and ")))
	}

	// An option the chosen command cannot use was almost certainly a mistake.
	// The old interface accepted and ignored these; saying so costs nothing and
	// is the difference between "my flag did nothing" and "my flag was wrong".
	serving := !*doInit && !*routes && !*check
	if unused := unusedLegacyOptions(fs, *doInit, serving); len(unused) > 0 {
		fmt.Fprintf(cx.stderr, i18n.T("slimproxy: 忽略了与本次操作无关的选项: %s\n", "slimproxy: ignored options irrelevant to this operation: %s\n"), strings.Join(unused, " "))
	}

	// Options travel as arguments, not through the context: a command must
	// behave identically whichever form invoked it, and rebuilding the list is
	// what keeps a dropped option observable in a test.
	shared := []string{"-config", *configPath, "-state", *stateDir}
	if *port != 0 {
		shared = append(shared, "-port", fmt.Sprint(*port))
	}

	switch {
	case *doInit:
		sub := []string{"-config", *configPath, "-init-auth-dir", *initAuth}
		if *force {
			sub = append(sub, "-force")
		}
		return cmdInit(cx, sub)
	case *routes:
		return cmdRoutes(cx, nil)
	case *check:
		return cmdCheck(cx, shared)
	default:
		if *noTUI {
			shared = append(shared, "-no-tui")
		}
		return cmdServe(cx, shared)
	}
}

const defaultConfigPath = "slimproxy.yaml"

// initOnlyOptions are meaningful only when generating a config.
var initOnlyOptions = map[string]bool{"init-auth-dir": true, "force": true}

// unusedLegacyOptions reports options the user set that the selected operation
// will not read. Uses fs.Visit, which walks only the flags actually present on
// the command line -- Lookup would report every registered flag.
func unusedLegacyOptions(fs *flag.FlagSet, isInit, isServe bool) []string {
	var unused []string
	fs.Visit(func(f *flag.Flag) {
		switch {
		case initOnlyOptions[f.Name] && !isInit:
			unused = append(unused, "-"+f.Name)
		case isInit && (f.Name == "state" || f.Name == "port"):
			// init writes a file; it neither binds a port nor materializes an
			// effective config into the state directory.
			unused = append(unused, "-"+f.Name)
		case f.Name == "no-tui" && !isServe:
			// Only serve opens a panel; under a selector the flag does nothing.
			unused = append(unused, "-"+f.Name)
		}
	})
	return unused
}

// bindConfigOnly attaches just -config.
//
// For commands that read the config to find the credential directory and
// nothing else. They were using bindCommon, which also offers -state and
// -port -- neither of which they read, so `auth list -port 9999` was accepted
// and silently ignored, and `auth -h` advertised two options that do nothing.
//
// This package already holds that a flag which quietly fails to apply is worse
// than one that is rejected; unusedLegacyOptions says so for the old
// flag-only interface. Not offering them is the same rule, applied earlier.
func bindConfigOnly(fs *flag.FlagSet, cx *cliContext) {
	fs.StringVar(&cx.configPath, "config", defaultConfigPath, "path to the slimproxy config file")
}

// bindCommon attaches the options shared by the commands that read a config.
//
// Defaults are constants, deliberately not the context's current values. If a
// caller forgets to pass an option through, the value reverts to the default
// and a test can see it -- whereas seeding from the context would silently
// paper over the omission, since dispatchLegacy has already written there.
func bindCommon(fs *flag.FlagSet, cx *cliContext) {
	fs.StringVar(&cx.configPath, "config", defaultConfigPath, "path to the slimproxy config file")
	fs.StringVar(&cx.stateDir, "state", ".", "directory for the generated effective CLIProxyAPI config")
	fs.IntVar(&cx.port, "port", 0, "override the configured port")
}

// newFlagSet returns a flag set whose output this package owns.
//
// io.Discard rather than stderr: the flag package prints usage from inside
// Parse and then returns ErrHelp, so leaving it connected makes every -h print
// the screen twice and every bad flag print its message twice.
func newFlagSet(cx *cliContext, cmd *command) *flag.FlagSet {
	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parsePositional is parse for commands that legitimately take arguments, and
// it returns them.
//
// max bounds them, so a typo still surfaces: `auth rm a b` is a mistake worth
// reporting, not a silent removal of a.
//
// Leading positional arguments are pulled off before the flag set sees them.
// The flag package stops at the first non-flag argument and treats everything
// after it as positional, so without this `auth add claude -config x` would
// report three positional arguments and reject a spelling that every other CLI
// accepts. Both orders work as a result.
func (cx *cliContext) parsePositional(fs *flag.FlagSet, cmd *command, args []string, max int) ([]string, error) {
	// Everything after "--" is positional by convention and must never reach the
	// flag package, or `auth rm -- -weird-name` would be read as an undefined
	// flag.
	var afterDashDash []string
	for i, a := range args {
		if a == "--" {
			afterDashDash = append(afterDashDash, args[i+1:]...)
			args = args[:i]
			break
		}
	}

	// Alternate between collecting positional arguments and letting the flag
	// package consume options. One pass is not enough: the flag package stops
	// at the first non-flag argument, so `auth rm -y NAME -config X` would
	// leave "-config X" sitting behind NAME and be rejected as extra
	// positional arguments -- while pointing at a perfectly valid option.
	var positional []string
	rest := args
	for iterations := 0; len(rest) > 0; iterations++ {
		if iterations > len(args)+1 {
			// Cannot happen: each pass consumes at least one element. Bounded
			// anyway, because an infinite loop here would hang the CLI.
			break
		}
		i := 0
		for i < len(rest) && !strings.HasPrefix(rest[i], "-") {
			i++
		}
		positional = append(positional, rest[:i]...)
		if err := cx.parseFlags(fs, cmd, rest[i:]); err != nil {
			return nil, err
		}
		rest = fs.Args()
	}

	positional = append(positional, afterDashDash...)
	if len(positional) > max {
		// The count is deliberately not reported: an extra argument makes the
		// flag package stop parsing, so everything after it -- including valid
		// options -- lands here too, and "收到 4 个" for one stray word reads
		// like a different mistake than the one that was made.
		return nil, fmt.Errorf(i18n.T("%w: %s 最多接受 %d 个参数；无法识别: %s\n\n用法: %s", "%w: %s takes at most %d arguments; unrecognised: %s\n\nusage: %s"),
			errUsage, cmd.name, max,
			strings.Join(positional[max:], " "), cmd.usage)
	}
	return positional, nil
}

// parse completes argument handling for a command: flags, -h, and the leftover
// arguments flag.Parse silently stops at.
func (cx *cliContext) parse(fs *flag.FlagSet, cmd *command, args []string) error {
	if err := cx.parseFlags(fs, cmd, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf(i18n.T("%w: %s 不接受参数 %q\n\n用法: %s", "%w: %s takes no argument %q\n\nusage: %s"),
			errUsage, cmd.name, fs.Arg(0), cmd.usage)
	}
	return nil
}

// parseFlags handles the flag set and -h, leaving positional arguments to the
// caller's policy.
func (cx *cliContext) parseFlags(fs *flag.FlagSet, cmd *command, args []string) error {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(cx.stdout, i18n.T("用法: %s\n\n%s\n\n选项:\n", "usage: %s\n\n%s\n\noptions:\n"), cmd.usage, cmd.summary())
		fs.SetOutput(cx.stdout)
		fs.PrintDefaults()
		return errHandled
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

// errHandled means the command already produced its output and should exit
// successfully. Distinct from nil so a command body can return immediately
// without the caller mistaking it for "carry on".
var errHandled = errors.New("handled")

// loadConfigFor reads and applies the port override, which every config-reading
// command needs in the same order.
func (cx *cliContext) loadConfigFor() (*proxy.Config, error) {
	cfg, err := loadConfig(cx.configPath)
	if err != nil {
		return nil, err
	}
	if cx.port != 0 {
		cfg.Port = cx.port
	}
	return cfg, nil
}

func commandList() string {
	var b strings.Builder
	b.WriteString(i18n.T("slimproxy — 在 CLIProxyAPI 之上的小型代理\n\n用法:\n  slimproxy [命令] [选项]\n\n命令:\n", "slimproxy — a small proxy on top of CLIProxyAPI\n\nusage:\n  slimproxy [command] [options]\n\ncommands:\n"))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	// Registry order, not alphabetical: serve first because it is the default,
	// help last because it is the fallback.
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary())
	}
	_ = tw.Flush()
	b.WriteString(i18n.T("\n不带命令运行等同于 serve。\n", "\nrunning with no command is the same as serve.\n"))
	b.WriteString(i18n.T("旧式写法（-check / -init / -routes）仍然可用。\n", "the legacy spellings (-check / -init / -routes) still work.\n"))
	b.WriteString(i18n.T("\n用 \"slimproxy help COMMAND\" 查看某个命令的用法。\n", "\nuse \"slimproxy help COMMAND\" for one command\u2019s usage.\n"))
	return b.String()
}

func cmdHelp(cx *cliContext, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(cx.stdout, commandList())
		return nil
	}
	cmd := lookup(args[0])
	if cmd == nil {
		return fmt.Errorf(i18n.T("%w: 未知命令 %q\n\n%s", "%w: unknown command %q\n\n%s"), errUsage, args[0], commandList())
	}

	// Delegated rather than summarised again.
	//
	// auth and tunnel dispatch sub-verbs and print their own help, listing the
	// verbs and (for auth) the available providers. Printing the one-line
	// summary here instead meant `slimproxy help auth` returned strictly less
	// than `slimproxy auth -h` -- and `help <command>` is what someone reaches
	// for first, having just learned it works for `check`.
	if hasSubVerbs[cmd.name] {
		return skipHandled(cmd.run(cx, []string{"-h"}))
	}

	fmt.Fprintf(cx.stdout, i18n.T("用法: %s\n\n%s\n", "usage: %s\n\n%s\n"), cmd.usage, cmd.summary())
	return nil
}

// hasSubVerbs marks commands that dispatch a second word and document it
// themselves.
var hasSubVerbs = map[string]bool{"auth": true, "tunnel": true}

func newContext() *cliContext {
	return &cliContext{
		configPath: defaultConfigPath,
		stateDir:   ".",
		stdout:     os.Stdout,
		stderr:     os.Stderr,
	}
}
