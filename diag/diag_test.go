package diag

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTimeoutBecomesUnknownNotPass is the central rule of this package. A check
// that cannot finish has established nothing; reporting Pass would send the
// operator looking somewhere else entirely.
func TestTimeoutBecomesUnknownNotPass(t *testing.T) {
	c := Check{
		Name:    "hangs",
		Timeout: 50 * time.Millisecond,
		Run: func(ctx context.Context) Result {
			<-ctx.Done()
			// A careless check might report success after being cancelled.
			return Result{Level: Pass, Detail: "finished somehow"}
		},
	}
	res := c.run(context.Background())
	if res.Level != Unknown {
		t.Errorf("timed-out check reported %s, want UNKNOWN", res.Level)
	}
	if res.Remedy == "" {
		t.Error("UNKNOWN without a remedy leaves the operator with no next step")
	}
	if res.Name != "hangs" {
		t.Errorf("name lost: %q", res.Name)
	}
}

func TestPanicBecomesUnknown(t *testing.T) {
	c := Check{
		Name: "explodes",
		Run: func(ctx context.Context) Result {
			panic("boom")
		},
	}
	res := c.run(context.Background())
	if res.Level != Unknown {
		t.Errorf("panicking check reported %s, want UNKNOWN", res.Level)
	}
	if !strings.Contains(res.Detail, "boom") {
		t.Errorf("panic value lost: %q", res.Detail)
	}
}

// TestWorstRanksUnknownAboveWarn: an unanswered question about a process
// holding live credentials deserves more attention than a known-benign warning,
// and doctor's exit code depends on this ordering.
func TestWorstRanksUnknownAboveWarn(t *testing.T) {
	r := Report{Results: []Result{
		{Level: Pass},
		{Level: Warn},
		{Level: Unknown},
	}}
	if got := r.Worst(); got != Unknown {
		t.Errorf("Worst() = %s, want UNKNOWN", got)
	}

	withFail := Report{Results: []Result{{Level: Unknown}, {Level: Fail}}}
	if got := withFail.Worst(); got != Fail {
		t.Errorf("Worst() = %s, want FAIL (fail outranks unknown)", got)
	}

	allPass := Report{Results: []Result{{Level: Pass}, {Level: Pass}}}
	if got := allPass.Worst(); got != Pass {
		t.Errorf("Worst() = %s, want PASS", got)
	}
}

// TestCancelledRunMarksRemainingUnknown: giving up must not leave unrun checks
// looking like they passed.
func TestCancelledRunMarksRemainingUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	checks := []Check{
		{Name: "first", Run: func(context.Context) Result {
			cancel()
			return Result{Level: Pass, Detail: "ran"}
		}},
		{Name: "second", Run: func(context.Context) Result {
			return Result{Level: Pass, Detail: "should not run"}
		}},
		{Name: "third", Run: func(context.Context) Result {
			return Result{Level: Pass, Detail: "should not run"}
		}},
	}
	r := Run(ctx, checks)
	if len(r.Results) != 3 {
		t.Fatalf("got %d results, want one per check", len(r.Results))
	}
	for _, res := range r.Results[1:] {
		if res.Level != Unknown {
			t.Errorf("%s reported %s after cancellation, want UNKNOWN", res.Name, res.Level)
		}
	}
}

// TestUnknownFieldsCatchesRemovedSettings covers the real regression: a config
// written for an older binary still carries settings that no longer exist.
func TestUnknownFieldsCatchesRemovedSettings(t *testing.T) {
	body := []byte(`host: "127.0.0.1"
port: 8317
ui: true
ui-behind-proxy: true
some-typo: 1
models: []
`)
	unknown, err := UnknownFields(body)
	if err != nil {
		t.Fatalf("UnknownFields: %v", err)
	}
	want := map[string]bool{"ui": true, "ui-behind-proxy": true, "some-typo": true}
	for _, k := range unknown {
		if !want[k] {
			t.Errorf("reported %q as unknown, but it is a real field", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("failed to report removed/unknown field %q", k)
	}
}

func TestUnknownFieldsAcceptsAValidConfig(t *testing.T) {
	body := []byte(`host: "127.0.0.1"
port: 8317
api-keys: ["x"]
allow-unauthenticated: false
auth-dir: "auths"
proxy-url: ""
request-retry: 3
max-retry-interval: 30
max-retry-credentials: 0
debug: false
request-log: false
log-to-file: false
models: []
`)
	unknown, err := UnknownFields(body)
	if err != nil {
		t.Fatalf("UnknownFields: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("valid config reported unknown fields: %v", unknown)
	}
}

func TestUnknownFieldsReportsSyntaxError(t *testing.T) {
	if _, err := UnknownFields([]byte("host: [unclosed\n")); err == nil {
		t.Error("malformed YAML accepted")
	}
}

// TestListenPortFreeVersusTaken pins the distinction the check exists for.
func TestListenPortFreeVersusTaken(t *testing.T) {
	// A port nothing is on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	free := Target{Host: "127.0.0.1", Port: port}
	if res := free.checkListenPort(context.Background()); res.Level != Pass {
		t.Errorf("free port reported %s: %s", res.Level, res.Detail)
	}

	// Now hold it with something that is not slimproxy.
	held, err := net.Listen("tcp", free.addr())
	if err != nil {
		t.Fatalf("re-listen: %v", err)
	}
	defer func() { _ = held.Close() }()
	go func() {
		for {
			c, aerr := held.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close() // never answers /healthz
		}
	}()

	res := free.checkListenPort(context.Background())
	if res.Level != Fail {
		t.Errorf("port held by a foreign process reported %s, want FAIL: %s", res.Level, res.Detail)
	}
	if res.Remedy == "" {
		t.Error("FAIL without a remedy")
	}
}

func TestCredentialsEmptyDirIsFail(t *testing.T) {
	dir := t.TempDir()
	tgt := Target{AuthDir: dir}
	res := tgt.checkCredentials(context.Background())
	if res.Level != Fail {
		t.Errorf("empty auth-dir reported %s, want FAIL: %s", res.Level, res.Detail)
	}
}

func TestCredentialsMissingDirIsFail(t *testing.T) {
	tgt := Target{AuthDir: filepath.Join(t.TempDir(), "nope")}
	res := tgt.checkCredentials(context.Background())
	if res.Level != Fail {
		t.Errorf("missing auth-dir reported %s, want FAIL", res.Level)
	}
}

func TestCredentialsCountsValidAndInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.json"), []byte(`{"type":"oauth"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A non-JSON file must be ignored rather than counted as broken.
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Target{AuthDir: dir}.checkCredentials(context.Background())
	if res.Level != Warn {
		t.Errorf("mixed dir reported %s, want WARN: %s", res.Level, res.Detail)
	}
	if !strings.Contains(res.Detail, "broken.json") {
		t.Errorf("invalid file not named: %s", res.Detail)
	}
	if strings.Contains(res.Detail, "README.txt") {
		t.Errorf("non-JSON file counted: %s", res.Detail)
	}
}

// TestTunnelNotConfiguredIsNotAFailure: no tunnel is a legitimate deployment,
// not a broken one.
func TestTunnelNotConfiguredIsNotAFailure(t *testing.T) {
	res := Target{}.checkTunnel(context.Background())
	if res.Level != Pass {
		t.Errorf("absent tunnel reported %s, want PASS: %s", res.Level, res.Detail)
	}
}

func TestFakeIPDetection(t *testing.T) {
	for _, ip := range []string{"198.18.0.101", "198.18.1.98", "198.19.255.255"} {
		if !fakeIPNet.Contains(net.ParseIP(ip)) {
			t.Errorf("%s should be inside the RFC 2544 range", ip)
		}
	}
	for _, ip := range []string{"198.17.255.255", "198.20.0.0", "104.21.90.181", "127.0.0.1"} {
		if fakeIPNet.Contains(net.ParseIP(ip)) {
			t.Errorf("%s should be outside the RFC 2544 range", ip)
		}
	}
}

// TestListenPortHealthyProxyIsPass: without this, doctor tells the operator to
// kill their own running proxy.
func TestListenPortHealthyProxyIsPass(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", srv.URL, err)
	}
	port, _ := strconv.Atoi(portStr)

	res := Target{Host: host, Port: port}.checkListenPort(context.Background())
	if res.Level != Pass {
		t.Errorf("healthy proxy on the port reported %s: %s", res.Level, res.Detail)
	}
	if strings.Contains(res.Remedy, "结束占用") {
		t.Error("told the operator to kill their own running proxy")
	}
}

// TestListenPortNon200IsFail exercises the branch where something answers HTTP
// but is not slimproxy.
func TestListenPortNon200IsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	res := Target{Host: host, Port: port}.checkListenPort(context.Background())
	if res.Level != Fail {
		t.Errorf("foreign HTTP server on the port reported %s: %s", res.Level, res.Detail)
	}
}

// TestListenPortCancelledIsUnknown: a dial that failed because time ran out
// establishes nothing, and must not be reported as "the port is free".
func TestListenPortCancelledIsUnknown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := Target{Host: "127.0.0.1", Port: port}.checkListenPort(ctx)
	if res.Level != Unknown {
		t.Errorf("cancelled probe reported %s (%s), want UNKNOWN", res.Level, res.Detail)
	}
}

// TestCredentialsUnresolvableDirIsUnknown: not knowing which directory to look
// in is a different answer from looking and finding nothing.
func TestCredentialsUnresolvableDirIsUnknown(t *testing.T) {
	res := Target{AuthDirErr: errors.New("cannot expand ~")}.checkCredentials(context.Background())
	if res.Level != Unknown {
		t.Errorf("unresolvable auth-dir reported %s, want UNKNOWN", res.Level)
	}
}

// TestConfigErrsSurface pins that loader problems reach the report rather than
// aborting the run.
func TestConfigErrsSurface(t *testing.T) {
	res := Target{
		ConfigPath: "anything.yaml",
		ConfigErrs: []string{"line 2: cannot unmarshal !!str into int"},
	}.checkConfigFields(context.Background())
	if res.Level != Fail {
		t.Errorf("config type error reported %s, want FAIL", res.Level)
	}
	if !strings.Contains(res.Detail, "cannot unmarshal") {
		t.Errorf("the underlying error was not shown: %s", res.Detail)
	}
}

func TestConfigFieldsWithoutPathIsUnknown(t *testing.T) {
	res := Target{}.checkConfigFields(context.Background())
	if res.Level != Unknown {
		t.Errorf("missing config path reported %s, want UNKNOWN", res.Level)
	}
}

// TestTunnelDetectErrorIsUnknownNotPass is the CRITICAL case: a cloudflared
// config that exists but cannot be read must never be reported as "no tunnel
// configured", because that is a PASS.
func TestTunnelDetectErrorIsUnknownNotPass(t *testing.T) {
	res := Target{TunnelErr: errors.New("config.yml 解析失败")}.checkTunnel(context.Background())
	if res.Level != Unknown {
		t.Errorf("broken tunnel config reported %s, want UNKNOWN: %s", res.Level, res.Detail)
	}
	if strings.Contains(res.Detail, "未配置") {
		t.Errorf("a broken config was described as absent: %s", res.Detail)
	}
}

// TestLevelForConnectors pins the classification checkTunnel derives its level
// from. Parsing itself now lives in the tunnel package; what stays here is the
// decision, and its two rules: output we could not read is Unknown (never a
// count), and a parsed zero is Fail (never Pass) -- a tunnel with no
// connections is exactly the state that looks alive while the hostname 502s.
func TestLevelForConnectors(t *testing.T) {
	if got := levelForConnectors(0, false); got != Unknown {
		t.Errorf("unparseable output = %s, want UNKNOWN", got)
	}
	if got := levelForConnectors(0, true); got != Fail {
		t.Errorf("zero connections = %s, want FAIL", got)
	}
	if got := levelForConnectors(3, true); got != Pass {
		t.Errorf("healthy tunnel = %s, want PASS", got)
	}
}
