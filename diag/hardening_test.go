package diag

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestZeroPortIsUnknownNotFree.
//
// net.Listen on port 0 means "assign me anything" and therefore always
// succeeds, so the probe reported "空闲，可以绑定" about an address the proxy
// will never bind. Reachable without any misconfiguration:
// loadConfigForDiagnosis keeps going when the config file is missing or has a
// type error, leaving Port at zero -- so `slimproxy doctor -config typo.yaml`
// produced a confident finding about nothing.
func TestZeroPortIsUnknownNotFree(t *testing.T) {
	for _, port := range []int{0, -1, 70000} {
		res := Target{Host: "127.0.0.1", Port: port}.checkListenPort(context.Background())
		if res.Level != Unknown {
			t.Errorf("port=%d 得到 %s，应为 UNKNOWN（无法检查不等于没问题）", port, res.Level)
		}
		if res.Remedy == "" {
			t.Errorf("port=%d 没有给出下一步", port)
		}
	}
}

// TestDisabledCredentialIsNotCounted.
//
// A disabled credential parses perfectly and is worth exactly nothing --
// credentials.Recoverable lists disabled and broken as the only two states with
// no value. Counting it let a pool whose sole entry was disabled report PASS,
// on a proxy that would start, accept requests and fail every one.
func TestDisabledCredentialIsNotCounted(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, disabled bool) {
		body, _ := json.Marshal(map[string]any{
			"type": "claude", "email": name, "disabled": disabled,
		})
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("off", true)
	res := Target{AuthDir: dir}.checkCredentials(context.Background())
	if res.Level == Pass {
		t.Errorf("唯一凭据被禁用时报告了 PASS: %s", res.Detail)
	}
	if !strings.Contains(res.Detail, "已禁用") {
		t.Errorf("说明未指出凭据被禁用: %s", res.Detail)
	}

	// A healthy one alongside it downgrades to a warning rather than a failure.
	write("on", false)
	res = Target{AuthDir: dir}.checkCredentials(context.Background())
	if res.Level != Warn {
		t.Errorf("一可用一禁用时得到 %s，应为 WARN: %s", res.Level, res.Detail)
	}
}

// TestHealthyPoolStillPasses guards the premise: a check that never passes is
// as useless as one that always does.
func TestHealthyPoolStillPasses(t *testing.T) {
	dir := t.TempDir()
	body, _ := json.Marshal(map[string]any{"type": "claude", "email": "a@x.com"})
	if err := os.WriteFile(filepath.Join(dir, "a.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if res := (Target{AuthDir: dir}).checkCredentials(context.Background()); res.Level != Pass {
		t.Errorf("健康的凭据池得到 %s: %s", res.Level, res.Detail)
	}
}

// TestTranslatorCheckIsHonestFromOutside.
//
// The finding comes from the translation hooks at request time, so a separate
// `doctor` process has no way to know. Reporting PASS there would be a
// conclusion it never reached -- the exact failure mode the Unknown level
// exists for.
func TestTranslatorCheckIsHonestFromOutside(t *testing.T) {
	res := Target{}.checkTranslator(context.Background())
	if res.Level != Unknown {
		t.Errorf("从外部进程检查得到 %s，应为 UNKNOWN：这个进程无从观察", res.Level)
	}
	if res.Remedy == "" {
		t.Error("应告诉操作者去哪里才能看到答案")
	}
}

// TestTranslatorCheckReportsObservedPairs: inside the serving process the
// answer is real, and an untranslated pair is a failure -- the request reached
// an upstream speaking a different dialect.
func TestTranslatorCheckReportsObservedPairs(t *testing.T) {
	res := Target{
		FromRunningInstance: true,
		Untranslated:        []string{"请求:openai->gemini"},
	}.checkTranslator(context.Background())

	if res.Level != Fail {
		t.Errorf("观察到未翻译的组合应为 FAIL，实际 %s", res.Level)
	}
	if !strings.Contains(res.Detail, "openai->gemini") {
		t.Errorf("说明未点名是哪一对: %s", res.Detail)
	}
	if !strings.Contains(res.Remedy, "routes") {
		t.Errorf("应指向 routes 命令: %s", res.Remedy)
	}
}

// TestTranslatorCheckPassesWhenClean guards the premise.
func TestTranslatorCheckPassesWhenClean(t *testing.T) {
	res := Target{FromRunningInstance: true}.checkTranslator(context.Background())
	if res.Level != Pass {
		t.Errorf("运行中且未观察到问题应为 PASS，实际 %s: %s", res.Level, res.Detail)
	}
}

// TestExternalDoctorHasNoPermanentUnknowns.
//
// Two checks read state that exists only inside the serving process. Including
// them from outside produced two Unknowns on every healthy deployment -- and
// Unknown drives the exit code, so `slimproxy doctor` always exited non-zero.
// A diagnostic that always fails is one people stop reading, which would waste
// the next real Unknown.
func TestExternalDoctorHasNoPermanentUnknowns(t *testing.T) {
	external := Checks(Target{Host: "127.0.0.1", Port: 8317})
	for _, c := range external {
		if c.Name == "translator" || c.Name == "journal" {
			t.Errorf("外部检查不应包含 %q：它永远只能回答「查不了」", c.Name)
		}
	}

	inside := Checks(Target{Host: "127.0.0.1", Port: 8317, FromRunningInstance: true})
	if len(inside) <= len(external) {
		t.Error("运行中的实例应当多检查几项，那里才有真答案")
	}
	var sawJournal bool
	for _, c := range inside {
		if c.Name == "journal" {
			sawJournal = true
		}
	}
	if !sawJournal {
		t.Error("面板内的 doctor 应当检查事件日志写入情况")
	}
}
