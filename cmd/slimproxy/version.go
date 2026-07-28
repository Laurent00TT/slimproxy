package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/momo/slimproxy/proxy"
)

// cmdVersion prints what this binary is.
//
// Exists because "which version are you on" is the first question asked when
// something is wrong, and until now the answer was only visible in the
// dashboard header -- unreachable from a script, a bug report, or a machine
// where the panel does not start.
func cmdVersion(cx *cliContext, args []string) error {
	cmd := lookup("version")
	fs := newFlagSet(cx, cmd)
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	fmt.Fprintf(cx.stdout, "slimproxy %s\n", proxy.Version)
	fmt.Fprintf(cx.stdout, "go        %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	upstream, replaced := upstreamBuild()
	fmt.Fprintf(cx.stdout, "CLIProxyAPI %s\n", upstream)
	if replaced {
		// Worth stating plainly: the emulation layer came from a working copy
		// on this machine rather than a published version, so two builds of
		// "the same" slimproxy can behave differently.
		fmt.Fprintf(cx.stdout, "\n注意：上游是本地 checkout（go.mod 的 replace 指令），不是已发布版本。\n"+
			"      构建结果取决于这台机器上那个目录的内容。\n")
	}
	return nil
}

// upstreamBuild reports the CLIProxyAPI version this binary was built against,
// and whether it came from a replace directive.
func upstreamBuild() (version string, replaced bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "未知（构建信息不可用）", false
	}
	for _, d := range info.Deps {
		if !strings.Contains(d.Path, "CLIProxyAPI") {
			continue
		}
		if d.Replace != nil {
			target := d.Replace.Path
			if d.Replace.Version != "" && d.Replace.Version != "(devel)" {
				target += " " + d.Replace.Version
			}
			return target, true
		}
		return d.Version, false
	}
	return "未链接", false
}
