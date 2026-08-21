// proxy/netmeter_test.go
package proxy

import (
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Laurent00TT/slimproxy/journal"
	"github.com/Laurent00TT/slimproxy/metrics"
)

// chunkedSlowReader 按块吐 body，模拟隧道慢上传。
type chunkedSlowReader struct {
	chunks []string
	delay  time.Duration
	i      int
}

func (r *chunkedSlowReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

// slowRecorder delays every Write so the meter has something real to
// measure: on this host an httptest recorder write takes ~0ns, which the
// accumulator correctly drops (the spec mandates undercount-never-overcount,
// so there is no floor to make fast writes visible).
type slowRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
}

func (r *slowRecorder) Write(b []byte) (int, error) {
	time.Sleep(r.delay)
	return r.ResponseRecorder.Write(b)
}

func TestNetMeterMeasuresUploadAndWriteBlock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var seen *metrics.NetTimings
	r := gin.New()
	r.Use(NetMeterMiddleware())
	r.POST("/echo", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			t.Errorf("body read: %v", err)
		}
		seen = metrics.NetTimingsFrom(c.Request.Context())
		c.String(200, strings.Repeat("x", 1024))
	})

	req := httptest.NewRequest("POST", "/echo", &chunkedSlowReader{
		chunks: []string{"aaaa", "bbbb", "cccc"},
		delay:  20 * time.Millisecond,
	})
	w := &slowRecorder{ResponseRecorder: httptest.NewRecorder(), delay: 5 * time.Millisecond}
	r.ServeHTTP(w, req)

	if seen == nil {
		t.Fatal("handler saw no NetTimings in its context")
	}
	// 3 块 × 20ms：上传腿至少 60ms（含调度抖动的下界断言）。
	if got := seen.Upload(); got < 60*time.Millisecond {
		t.Fatalf("upload = %v, want >= 60ms", got)
	}
	// slowRecorder injects 5ms per write, c.String should call Write at least once.
	if got := seen.WriteBlock(); got < 5*time.Millisecond {
		t.Fatalf("writeblock = %v, want >= 5ms", got)
	}
}

func TestNetMeterLeavesBodylessRequestsAlone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var seen *metrics.NetTimings
	r := gin.New()
	r.Use(NetMeterMiddleware())
	r.GET("/healthz", func(c *gin.Context) {
		seen = metrics.NetTimingsFrom(c.Request.Context())
		c.String(200, "ok")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if seen == nil {
		t.Fatal("handler saw no NetTimings in its context")
	}
	// 无 body 的请求 Upload 允许为 0 或 1ns（立即 EOF 的钳位值），
	// 只要求不 panic、不虚报大数。
	if seen.Upload() > time.Millisecond {
		t.Fatalf("bodyless upload = %v, want ~0", seen.Upload())
	}
}

// meteredUpload 把一次慢上传请求穿过 first→second 两层中间件，返回 handler
// ctx 里测得的上传腿。两个次序契约测试共用：慢在网络侧（chunkedSlowReader），
// 抽干层装错位置时测到的只会是内存重放（~0）。
func meteredUpload(t *testing.T, first, second gin.HandlerFunc) time.Duration {
	t.Helper()
	var seen *metrics.NetTimings
	r := gin.New()
	r.Use(first, second)
	r.POST("/v1/messages", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		seen = metrics.NetTimingsFrom(c.Request.Context())
		c.String(200, "{}")
	})
	body := `{"stream":true,"messages":[]}`
	req := httptest.NewRequest("POST", "/v1/messages",
		&chunkedSlowReader{chunks: []string{body[:8], body[8:]}, delay: 30 * time.Millisecond})
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if seen == nil {
		t.Fatal("handler saw no NetTimings")
	}
	return seen.Upload()
}

// 次序契约：NetMeter 必须在 EarlyFlush 之外。earlyflush 会把网络 body 抽干
// 换成 bytes.Reader；装反时上传腿测到的是内存重放（~0），数字静默变谎。
// 两个方向都测：正确次序测出慢上传，错误次序测不出——后者一旦开始"测得出"，
// 说明 earlyflush 的抽干行为变了，本契约要重审。
func TestNetMeterOrderAgainstEarlyFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ef := EarlyFlushMiddleware(time.Hour, earlyFlushNotes{})
	if got := meteredUpload(t, NetMeterMiddleware(), ef); got < 60*time.Millisecond {
		t.Fatalf("meter-first upload = %v, want >= 60ms", got)
	}
	if got := meteredUpload(t, ef, NetMeterMiddleware()); got >= 60*time.Millisecond {
		t.Fatalf("meter-behind-earlyflush upload = %v, want ~0 (the contract this test pins)", got)
	}
}

// 同一契约的另一半：FidelityProbe 在 c.Next() 之前 io.ReadAll 整个 body 再从
// 内存回放（fidelity.go）。它被采样的请求（/v1/messages 最多每分钟一条）若
// 排在 meter 之前，up_ms 同样静默归零——正是终评抓到的注册次序 bug。
// 每个方向新建一个 probe：采样闸门是 per-instance 的，首个请求必被采样。
func TestNetMeterOrderAgainstFidelityProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newProbe := func() gin.HandlerFunc {
		jw, err := journal.Open(t.TempDir(), 1)
		if err != nil {
			t.Fatalf("journal.Open: %v", err)
		}
		t.Cleanup(func() { _ = jw.Close() })
		return FidelityProbeMiddleware(jw)
	}
	if got := meteredUpload(t, NetMeterMiddleware(), newProbe()); got < 60*time.Millisecond {
		t.Fatalf("meter-first upload = %v, want >= 60ms", got)
	}
	if got := meteredUpload(t, newProbe(), NetMeterMiddleware()); got >= 60*time.Millisecond {
		t.Fatalf("meter-behind-fidelity upload = %v, want ~0 (the contract this test pins)", got)
	}
}

// spec 的「handler panic 时包装不泄漏」：panic 穿过 meter 的两层包装向上冒，
// 不悬挂、不吞 panic，已测得的计时仍可读（少算不多算，panic 前读完的 body
// 照记）。meter 自身无 goroutine 无锁，这条契约防的是将来有人往包装里加。
func TestNetMeterSurvivesHandlerPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var seen *metrics.NetTimings
	r := gin.New()
	r.Use(NetMeterMiddleware())
	r.POST("/boom", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		seen = metrics.NetTimingsFrom(c.Request.Context())
		_, _ = c.Writer.Write([]byte("partial"))
		panic("handler exploded")
	})

	req := httptest.NewRequest("POST", "/boom", &chunkedSlowReader{
		chunks: []string{"aaaa", "bbbb"},
		delay:  20 * time.Millisecond,
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic 被中间件吞掉了——上层的 recovery/日志再也看不到它")
			}
		}()
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()

	if seen == nil {
		t.Fatal("handler saw no NetTimings")
	}
	if got := seen.Upload(); got < 40*time.Millisecond {
		t.Fatalf("panic 后已测得的上传腿丢了：%v, want >= 40ms", got)
	}
	if seen.WriteBlock() < 0 {
		t.Fatal("writeblock unreadable")
	}
}

// 注册次序的真身在 proxy.go，gin 语义测试锁不住它：有人把 NetMeter 挪回
// fidelity/earlyflush 之后，上面两条测试照绿。此测直接断言源文件里的注册
// 顺序（源码级守卫在本仓库有先例：forkcheck 对 go.mod 与补丁文档做同样的
// 事）。前提是 WithMiddleware 选项按出现顺序追加——上面的 gin 语义测试与
// fork 的 append 实现共同背书。
func TestNetMeterRegisteredBeforeBodyDrainers(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	s := string(src)
	meter := strings.Index(s, "cliproxyapi.WithMiddleware(NetMeterMiddleware())")
	fidelity := strings.Index(s, "cliproxyapi.WithMiddleware(FidelityProbeMiddleware(")
	early := strings.Index(s, "cliproxyapi.WithMiddleware(EarlyFlushMiddleware(")
	if meter < 0 || fidelity < 0 || early < 0 {
		t.Fatalf("registration sites not found (meter=%d fidelity=%d early=%d)——"+
			"注册方式改了的话请同步改本守卫", meter, fidelity, early)
	}
	if meter > fidelity {
		t.Error("NetMeter 注册在 FidelityProbe 之后：被采样请求的 up_ms 会静默归零")
	}
	if meter > early {
		t.Error("NetMeter 注册在 EarlyFlush 之后：所有流式请求的 up_ms 会静默归零")
	}
	if last := strings.LastIndex(s, "cliproxyapi.WithMiddleware("); last != early {
		t.Error("EarlyFlush 不再是最后一个注册的中间件——它的 writer 必须离 handler 最近，见 earlyflush.go")
	}
}
