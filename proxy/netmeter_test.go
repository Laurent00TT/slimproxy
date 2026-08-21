// proxy/netmeter_test.go
package proxy

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

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

// 次序契约：NetMeter 必须在 EarlyFlush 之外。earlyflush 会把网络 body 抽干
// 换成 bytes.Reader；装反时上传腿测到的是内存重放（~0），数字静默变谎。
// 两个方向都测：正确次序测出慢上传，错误次序测不出——后者一旦开始"测得出"，
// 说明 earlyflush 的抽干行为变了，本契约要重审。
func TestNetMeterOrderAgainstEarlyFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upload := func(first, second gin.HandlerFunc) time.Duration {
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

	ef := EarlyFlushMiddleware(time.Hour, earlyFlushNotes{})
	if got := upload(NetMeterMiddleware(), ef); got < 60*time.Millisecond {
		t.Fatalf("meter-first upload = %v, want >= 60ms", got)
	}
	if got := upload(ef, NetMeterMiddleware()); got >= 60*time.Millisecond {
		t.Fatalf("meter-behind-earlyflush upload = %v, want ~0 (the contract this test pins)", got)
	}
}
