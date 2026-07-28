package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Laurent00TT/slimproxy/proxy"
)

// `slimproxy test` is the only command that establishes a request actually
// completes -- everything doctor checks stops short of the translation layer,
// the inbound key and the upstream's willingness. So its own failure reporting
// has to be precise: each outcome sends the operator somewhere different.

// testConfigAt writes a config pointing at addr.
func testConfigAt(t *testing.T, addr string) string {
	t.Helper()
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("无法拆分地址 %q", addr)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "slimproxy.yaml")
	body := "host: \"" + host + "\"\nport: " + port +
		"\napi-keys: [\"k-real-not-placeholder-2244\"]\nauth-dir: \"" +
		filepath.ToSlash(filepath.Join(dir, "auths")) + "\"\nmodels: []\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeUpstream stands in for a running slimproxy.
func fakeUpstream(t *testing.T, models []string, chat http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			ID string `json:"id"`
		}
		out := struct {
			Data []entry `json:"data"`
		}{}
		for _, m := range models {
			out.Data = append(out.Data, entry{ID: m})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	if chat != nil {
		mux.HandleFunc("/v1/chat/completions", chat)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTestCommandReportsSuccess(t *testing.T) {
	srv := fakeUpstream(t, []string{"claude-opus-5"}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"total_tokens":9}}`))
	})

	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"test", "-config", testConfigAt(t, strings.TrimPrefix(srv.URL, "http://"))}); err != nil {
		t.Fatalf("test: %v", err)
	}
	got := out.String()
	for _, want := range []string{"成功", "claude-opus-5", "stop", "OK"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q:\n%s", want, got)
		}
	}
}

// TestTestCommandFlagsEmptyTwoHundred.
//
// A 200 carrying no content is the exact shape an upstream policy refusal took
// before proxy/refusal.go rewrote it. Reporting that as plain success would
// make this command endorse the bug it is best placed to catch.
func TestTestCommandFlagsEmptyTwoHundred(t *testing.T) {
	srv := fakeUpstream(t, []string{"m"}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`))
	})

	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"test", "-config", testConfigAt(t, strings.TrimPrefix(srv.URL, "http://"))}); err != nil {
		t.Fatalf("test: %v", err)
	}
	if !strings.Contains(out.String(), "回复为空") {
		t.Errorf("空回复未被指出:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "content_filter") {
		t.Errorf("finish_reason 未显示，而它正是区分「被策略拒绝」和「链路坏了」的依据:\n%s", out.String())
	}
}

// TestTestCommandDistinguishesFailures pins that each status sends the operator
// somewhere different. Collapsing them into "请求失败" would waste the one
// command that can tell them apart.
func TestTestCommandDistinguishesFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"限流", http.StatusTooManyRequests, "链路本身是通的"},
		{"上游错误", http.StatusBadGateway, "doctor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeUpstream(t, []string{"m"}, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"x"}`))
			})

			cx, _, _ := newTestContext()
			err := dispatch(cx, []string{"test", "-config", testConfigAt(t, strings.TrimPrefix(srv.URL, "http://"))})
			if err == nil {
				t.Fatalf("状态 %d 应作为失败上报", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 = %v，应包含 %q", err, tc.want)
			}
		})
	}
}

// TestTestCommandSaysTheProxyIsDown.
//
// The most likely reason this command fails is that nothing is listening. The
// message must say so rather than surfacing a raw dial error.
func TestTestCommandSaysTheProxyIsDown(t *testing.T) {
	cx, _, _ := newTestContext()
	err := dispatch(cx, []string{"test", "-config", testConfigAt(t, "127.0.0.1:1")})
	if err == nil {
		t.Fatal("连不上时应报错")
	}
	if !strings.Contains(err.Error(), "没有在运行") {
		t.Errorf("错误信息 = %v，应指出代理可能没在运行", err)
	}
}

// TestTestCommandOnUnauthorizedDoesNotSuggestAModel.
//
// A 401 never reached model selection, so pointing at -model sends the operator
// to change something that cannot matter.
func TestTestCommandOnUnauthorizedDoesNotSuggestAModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid API key"}`))
	}))
	defer srv.Close()

	cx, _, _ := newTestContext()
	err := dispatch(cx, []string{"test", "-config", testConfigAt(t, strings.TrimPrefix(srv.URL, "http://"))})
	if err == nil {
		t.Fatal("401 应作为失败上报")
	}
	if strings.Contains(err.Error(), "-model") {
		t.Errorf("401 时不该建议指定模型: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("应点明是认证失败: %v", err)
	}
}

// TestReachableAddrRewritesWildcard: a config binding every interface does not
// name an address to connect to.
func TestReachableAddrRewritesWildcard(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::"} {
		got := reachableAddr(&proxy.Config{Host: host, Port: 8317})
		if !strings.HasPrefix(got, "127.0.0.1:") {
			t.Errorf("host %q 解析为 %q，应回落到环回地址", host, got)
		}
	}
}

// TestVersionNamesThePublishedUpstream.
//
// Was the inverse assertion: go.mod once replaced CLIProxyAPI with a working
// copy on this machine, and the version output had to disclose that two
// builds of "the same" slimproxy could differ. The replace is gone -- builds
// must now resolve a published upstream version, and the disclaimer must NOT
// appear, or version output would cast doubt on builds that are in fact
// reproducible.
func TestVersionNamesThePublishedUpstream(t *testing.T) {
	cx, out, _ := newTestContext()
	if err := dispatch(cx, []string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, proxy.Version) {
		t.Errorf("未打印版本号 %q:\n%s", proxy.Version, got)
	}
	if !strings.Contains(got, "CLIProxyAPI v7") {
		t.Errorf("未提及已发布的上游版本:\n%s", got)
	}
	if strings.Contains(got, "本地 checkout") {
		t.Errorf("go.mod 已无 replace，输出不应再声称依赖本机目录:\n%s", got)
	}
}
