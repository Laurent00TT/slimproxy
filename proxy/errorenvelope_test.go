package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// realDialError produces a genuine transport failure rather than a hand-written
// imitation of one.
//
// The point of the test suite is that the leak was platform-specific wording
// nobody had enumerated; a literal from this file would only ever prove the
// code recognises literals from this file. A refused connection to a closed
// local port is the fastest way to get the real thing, and it produces
// "connectex" on Windows and "connect: connection refused" elsewhere -- the
// exact divergence being guarded against.
func realDialError(t *testing.T) error {
	t.Helper()

	// Bind then close, so the port is known to be free and nothing else claims
	// it in between.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法绑定本地端口: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	conn, err := net.Dial("tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Skip("端口被别的进程占用，无法制造连接失败")
	}
	// Wrap it the way net/http does, so the text carries the URL as well.
	return fmt.Errorf("Post %q: %w", "https://api.anthropic.com/v1/messages", err)
}

// serve runs one request through the middleware with a handler that reproduces
// what upstream does with a failed request.
func serve(t *testing.T, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.Use(ErrorEnvelopeMiddleware())
	r.POST("/v1/messages", handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	r.ServeHTTP(rec, req)
	return rec
}

// upstreamStyleFailure writes a body the way sdk WriteErrorResponse does: the Go
// error's text, verbatim, in the message field.
func upstreamStyleFailure(status int, err error) gin.HandlerFunc {
	return func(c *gin.Context) {
		body := map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": err.Error(),
			},
		}
		c.JSON(status, body)
	}
}

// TestDialFailureDoesNotReachCaller is the regression this file exists for.
//
// Removing ErrorEnvelopeMiddleware from serve makes this fail on the IP
// assertion, which is how it was confirmed to be testing something.
func TestDialFailureDoesNotReachCaller(t *testing.T) {
	dialErr := realDialError(t)
	leaked := dialErr.Error()
	t.Logf("上游会原样写出的文本: %s", leaked)

	rec := serve(t, upstreamStyleFailure(http.StatusInternalServerError, dialErr))
	got := rec.Body.String()

	if ipLiteral.MatchString(got) {
		t.Errorf("响应体仍含 IP 字面量，本机网络拓扑泄露给了调用方: %s", got)
	}
	for _, marker := range []string{"connectex", "connection refused", "dial tcp", "127.0.0.1"} {
		if strings.Contains(strings.ToLower(got), marker) {
			t.Errorf("响应体含传输层细节 %q: %s", marker, got)
		}
	}
	if !strings.Contains(got, "upstream unreachable") {
		t.Errorf("调用方拿不到失败类别，无法判断是否该重试: %s", got)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d，脱敏不应改变状态码", rec.Code)
	}
}

// TestReplacementIsWellFormed guards the contract with the caller: a client
// parsing the error schema must not break on the substitute.
func TestReplacementIsWellFormed(t *testing.T) {
	rec := serve(t, upstreamStyleFailure(http.StatusInternalServerError, realDialError(t)))

	var ce claudeError
	if err := json.Unmarshal(rec.Body.Bytes(), &ce); err != nil {
		t.Fatalf("替换后的响应体不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if ce.Type != "error" || ce.Error.Type != "api_error" {
		t.Errorf("替换后的信封结构不符合 Anthropic 错误 schema: %+v", ce)
	}
	if ce.Error.Message == "" {
		t.Error("替换后 message 为空，调用方拿不到任何信息")
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	// A stale Content-Length from the original body would truncate or hang the
	// client, which is a worse failure than the one being fixed.
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n != rec.Body.Len() {
			t.Errorf("Content-Length = %q，实际 body %d 字节", cl, rec.Body.Len())
		}
	}
}

// TestUpstreamFailurePassesThrough.
//
// The reason this is not simply "replace every failure body": Anthropic's own
// errors carry information the caller acts on -- which parameter was invalid,
// that the model is overloaded and a retry is worth making. Dropping those
// would trade a leak for a different kind of broken.
func TestUpstreamFailurePassesThrough(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			"overloaded", http.StatusServiceUnavailable,
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		},
		{
			"invalid request", http.StatusBadRequest,
			`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than 0"}}`,
		},
		{
			"rate limited", http.StatusTooManyRequests,
			`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`,
		},
		{
			// Upstream's own 500 is passed through: it went through
			// DirectResponse, so the body is Anthropic's, not ours.
			"upstream 500", http.StatusInternalServerError,
			`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, func(c *gin.Context) {
				c.Data(tc.status, "application/json", []byte(tc.body))
			})
			var want, got any
			_ = json.Unmarshal([]byte(tc.body), &want)
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("响应不是合法 JSON: %v", err)
			}
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Errorf("上游错误被改写了\n原文: %s\n实际: %s", tc.body, rec.Body.String())
			}
		})
	}
}

// TestLocalErrorWearingUpstreamClothesIsStillReplaced.
//
// The collision that makes structure alone useless: the SDK stamps "api_error"
// on locally generated failures too, so a body can satisfy the schema and the
// type enumeration while still carrying a dial error. This is the case an
// allowlist built only on shape would wave through.
func TestLocalErrorWearingUpstreamClothesIsStillReplaced(t *testing.T) {
	leaky := `{"type":"error","error":{"type":"api_error","message":` +
		`"Post \"https://api.anthropic.com/v1/messages\": dial tcp 198.18.0.42:443: connectex: A connection attempt failed"}}`

	rec := serve(t, func(c *gin.Context) {
		c.Data(http.StatusInternalServerError, "application/json", []byte(leaky))
	})
	got := rec.Body.String()

	if strings.Contains(got, "198.18.0.42") {
		t.Errorf("合法 schema 掩护下的 IP 泄露没有被拦住: %s", got)
	}
	if strings.Contains(strings.ToLower(got), "connectex") {
		t.Errorf("平台文案泄露: %s", got)
	}
}

// TestUnknownFailureShapeIsReplaced is the deny-by-default premise.
//
// A body in no recognised shape is not evidence of safety. The original leak
// came from a path nobody had enumerated, so the default for the unrecognised
// has to be replacement -- otherwise the next unenumerated path leaks exactly
// as this one did.
func TestUnknownFailureShapeIsReplaced(t *testing.T) {
	cases := []struct{ name, body string }{
		{"plain text", `dial tcp 10.0.0.1:443: i/o timeout`},
		{"html", `<html><body>502 Bad Gateway: 192.168.1.1</body></html>`},
		{"foreign json", `{"err":"connect failed to 172.16.0.1:8080"}`},
		{"empty", ``},
		{"unknown error type", `{"type":"error","error":{"type":"weird_error","message":"ok"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, func(c *gin.Context) {
				c.Data(http.StatusBadGateway, "application/json", []byte(tc.body))
			})
			got := rec.Body.String()
			if ipLiteral.MatchString(got) {
				t.Errorf("未知形态的响应体原样透传，IP 泄露: %s", got)
			}
			var ce claudeError
			if err := json.Unmarshal(rec.Body.Bytes(), &ce); err != nil || ce.Type != "error" {
				t.Errorf("替换后不是合法错误信封: %s", got)
			}
		})
	}
}

// TestSuccessIsUntouched.
//
// A rewrite that reached 2xx would corrupt every answer the proxy delivers, so
// this asserts the decision gate rather than trusting it.
func TestSuccessIsUntouched(t *testing.T) {
	const answer = `{"content":[{"type":"text","text":"here is 10.0.0.1 in a legitimate answer"}]}`
	rec := serve(t, func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(answer))
	})
	if rec.Body.String() != answer {
		t.Errorf("成功响应被改写了\n原文: %s\n实际: %s", answer, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d", rec.Code)
	}
}

// TestStreamIsNotBuffered.
//
// Buffering SSE would convert a live token stream into one delivery at the end
// -- a regression a user would feel on every single request, unlike the leak
// this file fixes. The failure path that matters is written as JSON before the
// stream opens, so nothing is lost by leaving streams alone.
func TestStreamIsNotBuffered(t *testing.T) {
	r := gin.New()
	r.Use(ErrorEnvelopeMiddleware())

	flushed := make(chan struct{}, 4)
	r.GET("/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		for i := 0; i < 3; i++ {
			_, _ = c.Writer.WriteString("data: chunk\n\n")
			c.Writer.Flush()
			flushed <- struct{}{}
		}
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if len(flushed) != 3 {
		t.Errorf("只观察到 %d 次 flush，流式响应被缓冲了", len(flushed))
	}
	if got := strings.Count(rec.Body.String(), "data: chunk"); got != 3 {
		t.Errorf("收到 %d 个分片，want 3: %q", got, rec.Body.String())
	}
}

// TestOversizedBodyIsReleased.
//
// The bound exists so a pathological body cannot be held in memory. Releasing
// it is the deliberate trade: refusing to answer would be a worse failure than
// the leak, and a body this size is not a failure envelope in any known shape.
func TestOversizedBodyIsReleased(t *testing.T) {
	big := strings.Repeat("x", maxEnvelopeBody+1024)
	rec := serve(t, func(c *gin.Context) {
		c.Data(http.StatusInternalServerError, "application/json", []byte(big))
	})
	if rec.Body.Len() != len(big) {
		t.Errorf("超限响应体长度 = %d, want %d：内容被截断了", rec.Body.Len(), len(big))
	}
}

// TestClassificationTellsCallerWhatToDo.
//
// The categories are the entire reason the original text is read before being
// dropped. A caller retrying a DNS failure and a caller retrying a timeout want
// different behaviour, and this is the only channel left carrying that.
func TestClassificationTellsCallerWhatToDo(t *testing.T) {
	cases := []struct{ name, original, want string }{
		{"dns", `dial tcp: lookup api.anthropic.com: no such host`, "dns resolution failed"},
		{"tls", `net/http: TLS handshake timeout`, "tls"},
		{"refused", `dial tcp 127.0.0.1:1: connectex: No connection could be made`, "connection failed"},
		{"timeout", `context deadline exceeded`, "upstream timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFailure(http.StatusInternalServerError, tc.original)
			if !strings.Contains(got, tc.want) {
				t.Errorf("classifyFailure(%q) = %q，应含 %q", tc.original, got, tc.want)
			}
			if ipLiteral.MatchString(got) || strings.Contains(strings.ToLower(got), "connectex") {
				t.Errorf("分类文案本身泄露了细节: %q", got)
			}
		})
	}
}

// TestGuideDocumentsEveryCategory.
//
// The categories are the caller's entire view of a failure, and they exist in
// two hand-written copies -- these strings and the table in the guide. An
// operator reading the guide is trying to work out what a client actually
// reported, so a category that drifted out of the table would send them looking
// for a fault that is not there. Pinned in this direction (code is the source,
// docs must cover it) because adding a category is the easy thing to forget.
func TestGuideDocumentsEveryCategory(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "docs", "GUIDE.md"))
	if err != nil {
		t.Fatalf("读取指南失败: %v", err)
	}
	// Every transport wording classifyFailure recognises, and what it yields.
	samples := []string{
		"lookup api.anthropic.com: no such host",
		"net/http: TLS handshake timeout",
		"dial tcp: connection refused",
		"context deadline exceeded",
	}
	seen := map[string]bool{}
	for _, s := range samples {
		seen[classifyFailure(http.StatusInternalServerError, s)] = true
	}
	seen[classifyFailure(http.StatusServiceUnavailable, "")] = true

	for msg := range seen {
		if !strings.Contains(string(guide), msg) {
			t.Errorf("分类文案 %q 没有出现在 docs/GUIDE.md 的对照表里："+
				"用户看到它时查不到含义", msg)
		}
	}
}

// TestSafeMessageRejectsMachineText pins the shape rules directly, since they
// are what stands between an unenumerated error format and the caller.
func TestSafeMessageRejectsMachineText(t *testing.T) {
	unsafe := []string{
		"dial tcp 198.18.0.42:443: connectex: failed",
		`open C:\Users\someone\.slimproxy\auths\token.json: permission denied`,
		`Post "https://api.anthropic.com/v1/messages": EOF`,
		"api.anthropic.com:443 unreachable",
		strings.Repeat("a", 201),
		"",
		"上游不可达", // non-ASCII: upstream text is English
	}
	for _, m := range unsafe {
		if safeMessage(m) {
			t.Errorf("safeMessage(%q) = true，机器生成的文本被判为可放行", m)
		}
	}

	safe := []string{
		"Overloaded",
		"Internal server error",
		"max_tokens: must be greater than 0",
		"Number of request tokens has exceeded your per-minute rate limit",
	}
	for _, m := range safe {
		if !safeMessage(m) {
			t.Errorf("safeMessage(%q) = false，上游真实文案被误判", m)
		}
	}
}
