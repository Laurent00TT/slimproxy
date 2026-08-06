package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/Laurent00TT/slimproxy/i18n"
	"github.com/Laurent00TT/slimproxy/proxy"
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

	upstream, note := upstreamBuild()
	fmt.Fprintf(cx.stdout, "CLIProxyAPI %s\n", upstream)
	if note != "" {
		fmt.Fprint(cx.stdout, note)
	}
	return nil
}

// upstreamBuild reports the CLIProxyAPI version this binary was built against,
// plus a note when that needs explaining.
//
// Two kinds of replace get opposite treatment. The committed fork under
// third_party/ ships in this repository with its patch list, so the build is
// as reproducible as a published version -- the version line names the
// upstream release it is based on and flags the patches, and the note points
// at the list. Any OTHER replace target is a working copy somewhere on the
// build machine, and earns the original distrustful wording: two builds of
// "the same" slimproxy could then genuinely differ.
func upstreamBuild() (version string, note string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return i18n.T("未知（构建信息不可用）", "unknown (build info unavailable)"), ""
	}
	for _, d := range info.Deps {
		if !strings.Contains(d.Path, "CLIProxyAPI") {
			continue
		}
		if d.Replace == nil {
			return d.Version, ""
		}
		if strings.HasPrefix(d.Replace.Path, "./third_party/") {
			base := d.Version
			if base == "" || base == "(devel)" {
				base = d.Replace.Path
			}
			return base + " " + i18n.T("+本地补丁", "+local patches"), i18n.T(
				"\n注意：上游带本地补丁（third_party/CLIProxyAPI，清单与升级流程见其中的 SLIMPROXY_PATCHES.md）。\n",
				"\nnote: the upstream carries local patches (third_party/CLIProxyAPI; list and upgrade recipe in its SLIMPROXY_PATCHES.md).\n")
		}
		target := d.Replace.Path
		if d.Replace.Version != "" && d.Replace.Version != "(devel)" {
			target += " " + d.Replace.Version
		}
		return target, i18n.T(
			"\n注意：上游是本地 checkout（go.mod 的 replace 指令），不是已发布版本。\n      构建结果取决于这台机器上那个目录的内容。\n",
			"\nnote: the upstream is a local checkout (a go.mod replace directive), not a released version.\n      The build depends on whatever that directory holds on this machine.\n")
	}
	return i18n.T("未链接", "not linked"), ""
}
