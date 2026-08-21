package forkcheck

import (
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// forkDir is where the committed SDK fork lives, relative to this package.
const forkDir = "../third_party/CLIProxyAPI"

// TestForkPatchRegressionSuite runs the fork's own patch-regression tests.
//
// Scoped by -run to the two EnsurePublished tests on purpose: the upstream's
// full executor suite contains timing assertions (xai TTFT) that flake on
// Windows' clock granularity, and a guard that cries wolf gets deleted. The
// two tests it does run are exactly the ones that fail when an upstream
// rebase loses the patches -- verified by mutation before this wiring
// existed, when the only thing running them was memory.
func TestForkPatchRegressionSuite(t *testing.T) {
	if _, err := os.Stat(forkDir); err != nil {
		t.Fatalf("fork 目录不见了（%s）：go.mod 的 replace 会让所有构建失败", forkDir)
	}
	cmd := exec.Command("go", "test", "-count=1",
		"-run", "TestClaudeStream(NoTailStillPublishes|WithTailPublishesOnce)",
		"./internal/runtime/executor/")
	cmd.Dir = forkDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fork 的补丁回归测试失败——EnsurePublished 兜底补丁可能被升级冲掉了：\n%s", out)
	}
}

// TestForkContextReparentGuard runs the fork's guard tests for the
// GetContextWithCancel reparenting patch (SLIMPROXY_PATCHES.md 第 5/6 条)。
//
// What it defends: the executor ctx must inherit the request ctx's value
// chain, or metrics.NetTimings dies at the fork boundary and every request
// publishes with up_ms/wb_ms absent -- silently, because the slimproxy-side
// unit tests each cover their own half of the hop and stay green. The
// slimproxy-side wiring test (proxy/netmeter_wiring_test.go) exercises the
// same property through the real usage manager; this one keeps the fork's own
// three-way contract (values inherited, cancellation preserved, explicit
// parents respected) running even when someone touches only the fork.
func TestForkContextReparentGuard(t *testing.T) {
	if _, err := os.Stat(forkDir); err != nil {
		t.Fatalf("fork 目录不见了（%s）：go.mod 的 replace 会让所有构建失败", forkDir)
	}
	cmd := exec.Command("go", "test", "-count=1",
		"-run", "TestContextReparent(InheritsRequestValues|KeepsCancelPropagation|RespectsExplicitParent)",
		"./sdk/api/handlers/")
	cmd.Dir = forkDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fork 的 ctx 重挂守卫失败——GetContextWithCancel 的重挂补丁可能被升级冲掉了，"+
			"net-leg 三段计时会静默消失：\n%s", out)
	}
}

// TestForkBaselineVersionMatchesPatchDoc pins the version bookkeeping nothing
// else enforces: with a directory replace active, go.mod's require version is
// pure annotation (the build always comes from third_party/), and `slimproxy
// version` prints it as the fork's baseline. A require bump without a fork
// rebuild -- e.g. a routine `go get -u` -- would make version output name a
// baseline the binary does not contain, precisely in the incident-triage
// moment the command exists for.
func TestForkBaselineVersionMatchesPatchDoc(t *testing.T) {
	gomod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`github\.com/router-for-me/CLIProxyAPI/v7 (v[0-9.]+)`).FindSubmatch(gomod)
	if m == nil {
		t.Fatal("go.mod 里找不到 CLIProxyAPI 的 require 行")
	}
	version := string(m[1])

	doc, err := os.ReadFile(forkDir + "/SLIMPROXY_PATCHES.md")
	if err != nil {
		t.Fatalf("read SLIMPROXY_PATCHES.md: %v", err)
	}
	if !regexp.MustCompile(regexp.QuoteMeta(version)).Match(doc) {
		t.Fatalf("go.mod require 的 %s 没有出现在 SLIMPROXY_PATCHES.md 里——"+
			"要么 require 被顺手升了而 fork 没有重建，要么升级后忘了改文档；"+
			"`slimproxy version` 会把这个版本号当作 fork 的基线报出去", version)
	}
}
