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
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if seen == nil {
		t.Fatal("handler saw no NetTimings in its context")
	}
	// 3 块 × 20ms：上传腿至少 60ms（含调度抖动的下界断言）。
	if got := seen.Upload(); got < 60*time.Millisecond {
		t.Fatalf("upload = %v, want >= 60ms", got)
	}
	// httptest 的写不阻塞，但每次 Write 仍应被计入（>0 即可）。
	if seen.WriteBlock() <= 0 {
		t.Fatalf("writeblock = %v, want > 0", seen.WriteBlock())
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
