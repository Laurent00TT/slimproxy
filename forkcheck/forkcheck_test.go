package forkcheck

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// forkDir is where the committed SDK fork lives, relative to this package.
const forkDir = "../third_party/CLIProxyAPI"

// runForkGuard runs the named tests inside the fork and demands a
// `--- PASS:` line for every one of them.
//
// The exit code alone is not evidence. `go test -run X` prints
// `[no tests to run]` and exits 0 when X matches nothing, and the way an
// upstream rebase loses a patch is by dropping the patch's test file along
// with it -- the one case in which an exit-code guard reports green. So the
// guard runs -v and asserts, per test, that it actually ran and passed.
// Mutation-checked: pointing an expected name at a test that does not exist
// turns the guard red.
func runForkGuard(t *testing.T, what string, pkgs []string, tests []string) {
	t.Helper()
	if _, err := os.Stat(forkDir); err != nil {
		t.Fatalf("fork 目录不见了（%s）：go.mod 的 replace 会让所有构建失败", forkDir)
	}
	args := []string{"test", "-count=1", "-v", "-run", "^(" + strings.Join(tests, "|") + ")$"}
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Dir = forkDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s失败：\n%s", what, out)
	}
	for _, name := range tests {
		if !regexp.MustCompile(`(?m)^--- PASS: ` + regexp.QuoteMeta(name) + ` `).Match(out) {
			t.Errorf("%s：%s 没有 `--- PASS:` 行——这个测试没有跑（测试文件被升级冲掉了？），"+
				"而 go test 对空匹配照样退出 0：\n%s", what, name, out)
		}
	}
}

// TestForkPatchRegressionSuite runs the fork's own patch-regression tests
// (SLIMPROXY_PATCHES.md 第 1-3 条).
//
// Scoped to the two EnsurePublished tests on purpose: the upstream's full
// executor suite contains timing assertions (xai TTFT) that flake on
// Windows' clock granularity, and a guard that cries wolf gets deleted. The
// two tests it does run are exactly the ones that fail when an upstream
// rebase loses the patches -- verified by mutation before this wiring
// existed, when the only thing running them was memory.
func TestForkPatchRegressionSuite(t *testing.T) {
	runForkGuard(t, "fork 的补丁回归测试（EnsurePublished 兜底补丁可能被升级冲掉了）",
		[]string{"./internal/runtime/executor/"},
		[]string{
			"TestClaudeStreamNoTailStillPublishes",
			"TestClaudeStreamWithTailPublishesOnce",
		})
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
	runForkGuard(t, "fork 的 ctx 重挂守卫（GetContextWithCancel 的重挂补丁可能被升级冲掉了，net-leg 三段计时会静默消失）",
		[]string{"./sdk/api/handlers/"},
		[]string{
			"TestContextReparentInheritsRequestValues",
			"TestContextReparentKeepsCancelPropagation",
			"TestContextReparentRespectsExplicitParent",
		})
}

// TestForkCatalogClientGuard runs the fork's guard tests for the model
// catalog refresh patches (SLIMPROXY_PATCHES.md 第 7-9 条).
//
// What it defends: the SDK entry points that start the refresh at all (the
// upstream binary starts it from its own main; the SDK never did), and the
// fetchers dialing through the injected client rather than a bare one that
// ignores proxy-url. Losing the first means the process serves the catalog
// compiled into the binary forever; losing the second means the refresh
// fails every three hours on any host whose route out is the proxy, with a
// log line as the only symptom. The slimproxy-side wiring test
// (proxy/catalog_wiring_test.go) pins that the client Build assembles is the
// one Run starts the refresh with; this one keeps the fork's half honest.
func TestForkCatalogClientGuard(t *testing.T) {
	runForkGuard(t, "fork 的模型目录客户端守卫（目录拉取的客户端注入或 SDK 入口可能被升级冲掉了）",
		[]string{"./internal/registry/", "./sdk/cliproxy/"},
		[]string{
			"TestModelsFetchUsesInjectedClient",
			"TestCodexClientModelsFetchUsesInjectedClient",
			"TestNilClientRestoresDefaultFetcher",
			"TestNewModelCatalogClientHonoursProxyURL",
			"TestNewModelCatalogClientEmptyProxyKeepsDefaultTransport",
			"TestStartModelCatalogUpdaterFetchesWithGivenClient",
		})
}

// TestForkCacheTTLGuard runs the fork's guard tests for the Claude Code
// cache-lifetime patch (SLIMPROXY_PATCHES.md 第 10-12 条).
//
// What it defends: the two one-line call sites in the claude executor that
// rewrite every Claude Code breakpoint to claude-code.cache-ttl BEFORE the
// ordering normalizer runs. Lose either and the knob goes silently inert on
// that path -- the request still succeeds, the cache still works at 5m, and
// the only symptom is the full-prefix rewrite after every long turn coming
// back (the measured 2026-09-16 pattern the knob exists to end). The
// slimproxy-side test (proxy/config_test.go TestClaudeCodeCacheTTLReachesEngine)
// pins that the value reaches the engine's config; these pin that the engine
// acts on it.
func TestForkCacheTTLGuard(t *testing.T) {
	runForkGuard(t, "fork 的 Claude Code 缓存 TTL 守卫（executor 里的改写调用点可能被升级冲掉了，长轮次后整段前缀重写会回来）",
		[]string{"./internal/runtime/executor/"},
		[]string{
			"TestClaudeCodeCacheTTL_PinsEveryBreakpoint",
			"TestClaudeCodeCacheTTL_SurvivesOrderingNormalizer",
			"TestClaudeCodeCacheTTL_LeavesOtherClientsAlone",
			"TestClaudeCodeCacheTTL_UnsetKnobIsANoOp",
			"TestClaudeCodeCacheTTL_NothingToDoReturnsSameBytes",
			"TestClaudeCodeCacheTTLConfigKey",
			"TestClaudeStreamCarriesConfiguredCacheTTLUpstream",
			"TestClaudeExecuteCarriesConfiguredCacheTTLUpstream",
			"TestClaudeStreamWithoutKnobSendsBreakpointsAsWritten",
		})
}

// TestForkClaudeExtrasGuard runs the fork's guard tests for the pinned Claude
// models (SLIMPROXY_PATCHES.md 第 13-14 条).
//
// What it defends: the two call sites that merge the pinned entries into the
// catalog when it is read. Lose GetClaudeModels' and the model is never
// registered -- a local 502 "unknown provider" for a model Anthropic serves,
// the exact 2026-09-23 incident this exists for. Lose LookupStaticModelInfo's
// and the thinking layer stops finding the entry wherever it falls back on
// the static catalog. Both were mutation-checked red. The shape test pins the
// levels-only thinking block: min/max would put budget_tokens on the wire,
// which Opus 5.5 rejects with a 400.
func TestForkClaudeExtrasGuard(t *testing.T) {
	runForkGuard(t, "fork 的固定 Claude 模型守卫（目录读取时的补缺调用点可能被升级冲掉了，Opus 5.5 会退回本地 502）",
		[]string{"./internal/registry/"},
		[]string{
			"TestClaudeExtrasFillCatalogGap",
			"TestClaudeExtrasSurviveRemoteRefresh",
			"TestClaudeExtrasYieldToRemoteEntry",
			"TestClaudeExtraOpus55IsLevelOnly",
		})
}

// TestForkUtlsSetupGuard runs the fork's guard tests for the bounded, retried
// connection setup of the utls round tripper (SLIMPROXY_PATCHES.md 第 15-16 条).
//
// What it defends: the claude and codex executors reach their upstreams
// through this round tripper, which upstream set up with no deadline and no
// retry. Lose the patch and a dead proxy node goes back to failing the
// request ~5s in (an EOF inside the handshake) or hanging it ~60s -- both
// before any response byte, both costing Claude Code a resend of the whole
// body -- with nothing in slimproxy's journal beyond a cause enum to say so.
// The not-retried test pins the other side of the boundary: an error after
// the request was written must never be retried, since that can bill it
// twice -- its subtests cover a reset stream and a path that dies under the
// request, and run with it. Two more pin what the patch must not do to a
// connection once it is up: cut a long response at the setup bound, or keep
// the connection a request leaves behind open for good (the PING health check
// would otherwise keep it alive through Clash and the node). Every test was
// mutation-checked red against the bug it names.
func TestForkUtlsSetupGuard(t *testing.T) {
	runForkGuard(t, "fork 的 utls 建连守卫（建连的限时/重试补丁可能被升级冲掉了，死节点会重新让请求 5s EOF 或挂 60s）",
		[]string{"./internal/runtime/executor/helps/"},
		[]string{
			"TestUtlsSetupGivesUpAfterAttemptBudget",
			"TestUtlsSetupRetriesAfterHandshakeEOF",
			"TestUtlsSetupExhaustedKeepsJournalCause",
			"TestUtlsSetupStopsWhenRequestEndsDuringBackoff",
			"TestUtlsRequestErrorAfterSetupIsNotRetried",
			"TestUtlsSetupWaitersFollowTheirOwnContext",
			"TestUtlsConnectionDetectsSilentDeathMidResponse",
			"TestUtlsAttemptBoundDoesNotCapTheResponse",
			"TestUtlsIdleConnectionIsClosed",
		})
}

// TestForkCPATraceUnwrapGuard runs the fork's guard test for the CPA trace
// writer's Unwrap (SLIMPROXY_PATCHES.md 第 17-18 条).
//
// What it defends: the engine's CPA trace writer sits between every slimproxy
// writer and net/http, so without its Unwrap the early-flush middleware's full
// duplex switch fails for every request, and a desperation preamble written
// mid-upload makes net/http discard the rest of the body -- the 20 uploads
// killed on 2026-09-23. The slimproxy-side test
// (proxy/earlyflush_duplex_test.go TestEarlyFlushFullDuplexThroughBuiltChain)
// catches the same loss end to end through Build; this one names the writer.
func TestForkCPATraceUnwrapGuard(t *testing.T) {
	runForkGuard(t, "fork 的 CPA trace writer Unwrap 守卫（补丁文件可能被升级冲掉了，desperation 预发头会重新掐断上传）",
		[]string{"./internal/logging/"},
		[]string{
			"TestCPATraceWriterPassesFullDuplexThrough",
		})
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
