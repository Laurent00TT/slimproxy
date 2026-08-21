# 网速开销三段计时 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 每个代理请求在 journal 的 req 事件里带上 `up_ms`（上传腿）与 `wb_ms`（回写阻塞），`slimproxy log` 每行显示、`-stats` 汇总均值。

**Architecture:** gin 中间件 `NetMeterMiddleware` 包装 Request.Body 与 ResponseWriter 做测量，把 `*metrics.NetTimings`（原子字段）放进请求 ctx；`Collector.HandleUsage` 停止丢弃 ctx 参数，从中取出计时合并进 Sample；`journal.FromSample` 投影成事件字段；logcmd 渲染。fork 零改动。

**Tech Stack:** Go / gin / 现有 metrics·journal·proxy 包。

**Spec:** `docs/superpowers/specs/2026-08-21-net-timing-journal-design.md`

## Global Constraints

- 所有新用户可见文案必须 `i18n.T(zh, en)`，且不得在 var/init 里固化 T() 结果（lint 测试会抓）。
- `third_party/` 一行不改。
- 每个任务收尾跑 `go test ./...`（含 forkcheck）通过后才 commit；最终任务补 `go test -race`。
- 提交在 `main`，commit 后核对 `git branch --show-current`（多会话共用工作区）。
- commit message 尾行：`Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`。
- 注册次序硬约束：`NetMeterMiddleware` 必须在 `EarlyFlushMiddleware` **之前**注册（earlyflush 会抽干并替换 body；EarlyFlush 保持最后）。

---

### Task 1: metrics.NetTimings 与 ctx 通道

**Files:**
- Create: `metrics/nettimings.go`
- Test: `metrics/nettimings_test.go`

**Interfaces:**
- Produces: `NewNetTimings(started time.Time) *NetTimings`；`(*NetTimings).MarkBodyDone(now time.Time)`（首次生效）；`(*NetTimings).AddWriteBlock(d time.Duration)`；`(*NetTimings).Upload() time.Duration`；`(*NetTimings).WriteBlock() time.Duration`；`WithNetTimings(ctx context.Context, nt *NetTimings) context.Context`；`NetTimingsFrom(ctx context.Context) *NetTimings`（无则 nil）。

- [ ] **Step 1: 写失败测试**

```go
// metrics/nettimings_test.go
package metrics

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNetTimingsUploadFirstMarkWins(t *testing.T) {
	start := time.Now()
	nt := NewNetTimings(start)
	if nt.Upload() != 0 {
		t.Fatalf("unmarked upload = %v, want 0", nt.Upload())
	}
	nt.MarkBodyDone(start.Add(200 * time.Millisecond))
	nt.MarkBodyDone(start.Add(5 * time.Second)) // 迟到的第二次不得覆盖
	if got := nt.Upload(); got != 200*time.Millisecond {
		t.Fatalf("upload = %v, want 200ms", got)
	}
}

func TestNetTimingsWriteBlockAccumulates(t *testing.T) {
	nt := NewNetTimings(time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); nt.AddWriteBlock(time.Millisecond) }()
	}
	wg.Wait()
	if got := nt.WriteBlock(); got != 50*time.Millisecond {
		t.Fatalf("writeblock = %v, want 50ms", got)
	}
}

func TestNetTimingsContextRoundtrip(t *testing.T) {
	nt := NewNetTimings(time.Now())
	ctx := WithNetTimings(context.Background(), nt)
	if NetTimingsFrom(ctx) != nt {
		t.Fatal("NetTimingsFrom lost the pointer")
	}
	if NetTimingsFrom(context.Background()) != nil {
		t.Fatal("empty context must yield nil")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./metrics/ -run TestNetTimings -v`
Expected: FAIL（`NewNetTimings` 未定义，编译错）

- [ ] **Step 3: 最小实现**

```go
// metrics/nettimings.go
package metrics

import (
	"context"
	"sync/atomic"
	"time"
)

// NetTimings measures the two network legs of one inbound request that the
// usage record cannot see: how long the client (and the tunnel in front of it)
// took to deliver the request body, and how much time response writes spent
// blocked on the way back. The upstream leg between them is the record's own
// Latency/TTFT.
//
// Written from the request goroutine and read from the usage manager's
// dispatch goroutine, which is why every field is atomic: the publish races
// the request's final client writes, so a snapshot may miss the last write --
// an undercount, never an overcount.
type NetTimings struct {
	started time.Time
	// bodyDoneNs is elapsed nanoseconds from started to the body's EOF,
	// 0 while unseen. Clamped to at least 1 so "instant" stays distinguishable
	// from "never finished".
	bodyDoneNs atomic.Int64
	// writeBlockNs accumulates time spent inside client Write/Flush calls. In
	// a pipelined stream this is the only measurable form of return-path
	// pressure: "upstream done -> client done" is ~0 by construction.
	writeBlockNs atomic.Int64
}

// NewNetTimings starts the clock at started (the middleware's entry, i.e.
// headers parsed, body not yet read).
func NewNetTimings(started time.Time) *NetTimings {
	return &NetTimings{started: started}
}

// MarkBodyDone records the body's EOF. First call wins: a body replayed from
// memory later (earlyflush replaces the drained body with a bytes.Reader)
// must not overwrite the network reading.
func (n *NetTimings) MarkBodyDone(now time.Time) {
	ns := now.Sub(n.started).Nanoseconds()
	if ns < 1 {
		ns = 1
	}
	n.bodyDoneNs.CompareAndSwap(0, ns)
}

// AddWriteBlock accumulates one client-write duration.
func (n *NetTimings) AddWriteBlock(d time.Duration) {
	if d > 0 {
		n.writeBlockNs.Add(int64(d))
	}
}

// Upload is the measured upload leg, 0 when the body never reached EOF.
func (n *NetTimings) Upload() time.Duration {
	return time.Duration(n.bodyDoneNs.Load())
}

// WriteBlock is the accumulated client-write blocking so far.
func (n *NetTimings) WriteBlock() time.Duration {
	return time.Duration(n.writeBlockNs.Load())
}

// netTimingsKey is unexported so only this package mints the context entry.
type netTimingsKey struct{}

// WithNetTimings hangs the timings on a request context. The value rides the
// same context the fork already threads to usage publication (the
// ResponseHeaders mechanism is the precedent), which is what lets
// HandleUsage recover it without any fork change.
func WithNetTimings(ctx context.Context, nt *NetTimings) context.Context {
	return context.WithValue(ctx, netTimingsKey{}, nt)
}

// NetTimingsFrom recovers the timings, nil when the request never passed the
// meter (direct engine tests, health checks).
func NetTimingsFrom(ctx context.Context) *NetTimings {
	nt, _ := ctx.Value(netTimingsKey{}).(*NetTimings)
	return nt
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./metrics/ -run TestNetTimings -v`
Expected: PASS（三个测试）

- [ ] **Step 5: Commit**

```bash
git add metrics/nettimings.go metrics/nettimings_test.go
git commit -m "Give metrics a per-request net-leg stopwatch"
```

---

### Task 2: proxy.NetMeterMiddleware（body 与 writer 包装）

**Files:**
- Create: `proxy/netmeter.go`
- Test: `proxy/netmeter_test.go`

**Interfaces:**
- Consumes: Task 1 的全部导出（`metrics.NewNetTimings` 等）。
- Produces: `NetMeterMiddleware() gin.HandlerFunc`（Task 5 注册用）。

- [ ] **Step 1: 写失败测试**

```go
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

// slowReader 按块吐 body，模拟隧道慢上传。
type slowReader struct {
	chunks []string
	delay  time.Duration
	i      int
}

func (r *slowReader) Read(p []byte) (int, error) {
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

	req := httptest.NewRequest("POST", "/echo", &slowReader{
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./proxy/ -run TestNetMeter -v`
Expected: FAIL（`NetMeterMiddleware` 未定义，编译错）

- [ ] **Step 3: 最小实现**

```go
// proxy/netmeter.go
package proxy

// Measuring the two network legs the journal could not see.
//
// A slow request has three suspects -- the client/tunnel delivering the body,
// the upstream thinking, the return path draining -- and until now only the
// middle one was on record (usage Latency/TTFT). Attribution meant manually
// reconciling the access log's total against the journal's ms. This meter
// closes that gap from inside the process: body EOF minus arrival is the
// upload leg, and cumulative time spent inside client writes is the only form
// of return-path pressure that stays measurable while the stream is pipelined
// ("upstream done -> client done" is ~0 by construction and would be a field
// that never fires).
//
// Registration order is load-bearing: this must run BEFORE EarlyFlushMiddleware,
// whose bodySniffer drains the network body and replaces it with a bytes.Reader
// -- a meter behind it would clock the in-memory replay at ~0. EarlyFlush also
// must stay the last registration (its writer closest to the handler), so this
// one cannot be last. See the registration site in proxy.go.

import (
	"io"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// NetMeterMiddleware stamps every request with a NetTimings and installs the
// two probes. Unconditional and cheap (two small wrappers, one ctx value):
// only requests that end in a usage record ever surface the numbers, so
// non-proxy paths carry a meter nobody reads.
func NetMeterMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		nt := metrics.NewNetTimings(time.Now())
		c.Request.Body = &meterBody{inner: c.Request.Body, nt: nt}
		c.Writer = &meterWriter{ResponseWriter: c.Writer, nt: nt}
		c.Request = c.Request.WithContext(metrics.WithNetTimings(c.Request.Context(), nt))
		c.Next()
	}
}

// meterBody marks the timings when the network body reaches EOF. A body the
// handler never fully reads (abort, reject) simply leaves Upload at 0 --
// absent, not wrong.
type meterBody struct {
	inner io.ReadCloser
	nt    *metrics.NetTimings
}

func (b *meterBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if err == io.EOF {
		b.nt.MarkBodyDone(time.Now())
	}
	return n, err
}

func (b *meterBody) Close() error { return b.inner.Close() }

// meterWriter accumulates the time client writes spend blocked. Embedding
// gin.ResponseWriter forwards everything not measured (Status, Written,
// Hijack, ...), the same shape earlyFlushWriter relies on.
type meterWriter struct {
	gin.ResponseWriter
	nt *metrics.NetTimings
}

func (w *meterWriter) Write(b []byte) (int, error) {
	t := time.Now()
	n, err := w.ResponseWriter.Write(b)
	w.nt.AddWriteBlock(time.Since(t))
	return n, err
}

func (w *meterWriter) WriteString(s string) (int, error) {
	t := time.Now()
	n, err := w.ResponseWriter.WriteString(s)
	w.nt.AddWriteBlock(time.Since(t))
	return n, err
}

func (w *meterWriter) Flush() {
	t := time.Now()
	w.ResponseWriter.Flush()
	w.nt.AddWriteBlock(time.Since(t))
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./proxy/ -run TestNetMeter -v`
Expected: PASS（两个测试）

- [ ] **Step 5: Commit**

```bash
git add proxy/netmeter.go proxy/netmeter_test.go
git commit -m "Meter the upload and write-block legs of each request"
```

---

### Task 3: Sample 字段与 HandleUsage 的 ctx 合并

**Files:**
- Modify: `metrics/collector.go`（Sample 结构体 Latency 字段之后；`HandleUsage` 第一形参与函数体）
- Test: `metrics/nettimings_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `NetTimingsFrom`。
- Produces: `Sample.Upload time.Duration`、`Sample.WriteBlock time.Duration`（Task 4 投影用）。

- [ ] **Step 1: 写失败测试**

在 `metrics/nettimings_test.go` 追加（`cliproxyusage` 的 import 别名照 collector.go 现有写法）：

```go
func TestHandleUsageMergesNetTimings(t *testing.T) {
	c := NewCollector()
	var got Sample
	c.Observe(func(s Sample) { got = s })

	nt := NewNetTimings(time.Now())
	nt.MarkBodyDone(nt.started.Add(300 * time.Millisecond))
	nt.AddWriteBlock(40 * time.Millisecond)
	ctx := WithNetTimings(context.Background(), nt)

	c.HandleUsage(ctx, cliproxyusage.Record{Model: "m", Latency: time.Second})
	if got.Upload != 300*time.Millisecond {
		t.Fatalf("Upload = %v, want 300ms", got.Upload)
	}
	if got.WriteBlock != 40*time.Millisecond {
		t.Fatalf("WriteBlock = %v, want 40ms", got.WriteBlock)
	}

	// 没经过中间件的请求（直连引擎测试、无 ctx 场景）安静地保持零值。
	c.HandleUsage(context.Background(), cliproxyusage.Record{Model: "m"})
	if got.Upload != 0 || got.WriteBlock != 0 {
		t.Fatalf("bare ctx produced Upload=%v WriteBlock=%v, want zeros", got.Upload, got.WriteBlock)
	}
}
```

注：`Observe` 若签名不同（见 collector.go 现有 `c.observe` 的设置方法），照现有测试里给 observer 赋值的方式改写这两行，其余断言不变。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./metrics/ -run TestHandleUsageMergesNetTimings -v`
Expected: FAIL（`Sample` 无 Upload 字段，编译错）

- [ ] **Step 3: 实现**

`Sample` 结构体中 `Latency` 字段之后加：

```go
	// Upload and WriteBlock are the request's two network legs, measured by
	// proxy.NetMeterMiddleware and recovered from the publish context --
	// which is why they are absent (zero) on records that never passed the
	// meter. Latency sits between them: body-in, think, body-out.
	Upload     time.Duration
	WriteBlock time.Duration
```

`HandleUsage` 改为使用 ctx（把 `_ context.Context` 改名 `ctx`），`sample := SampleFrom(r)` 之后：

```go
	// The one use this process makes of the publish context. The fork threads
	// the request context through PublishRecord (its ResponseHeaders mechanism
	// depends on the same fact); if a fork upgrade ever severs that, these
	// fields silently stop appearing -- TestHandleUsageMergesNetTimings is the
	// slimproxy-side sentinel, and the spec records the residual risk.
	if nt := NetTimingsFrom(ctx); nt != nil {
		sample.Upload = nt.Upload()
		sample.WriteBlock = nt.WriteBlock()
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./metrics/ -v`
Expected: PASS（新旧全部）

- [ ] **Step 5: Commit**

```bash
git add metrics/collector.go metrics/nettimings_test.go
git commit -m "Recover the net legs from the usage publish context"
```

---

### Task 4: journal.Event 字段与 FromSample 投影

**Files:**
- Modify: `journal/event.go`（`LatencyMs` 之后）
- Modify: `journal/writer.go`（`FromSample`）
- Test: `journal/writer_test.go`（追加）

**Interfaces:**
- Consumes: Task 3 的 `Sample.Upload` / `Sample.WriteBlock`。
- Produces: `Event.UploadMs int64` (`json:"up_ms,omitempty"`)、`Event.WriteBlockMs int64` (`json:"wb_ms,omitempty"`)（Task 6 渲染用）。

- [ ] **Step 1: 写失败测试**

`journal/writer_test.go` 追加（Sample 构造照该文件现有 FromSample 测试的写法补齐必填字段）：

```go
func TestFromSampleProjectsNetLegs(t *testing.T) {
	e := FromSample(metrics.Sample{
		Model:      "claude-x",
		Latency:    2 * time.Second,
		Upload:     900 * time.Millisecond,
		WriteBlock: 150 * time.Millisecond,
	})
	if e.UploadMs != 900 {
		t.Fatalf("UploadMs = %d, want 900", e.UploadMs)
	}
	if e.WriteBlockMs != 150 {
		t.Fatalf("WriteBlockMs = %d, want 150", e.WriteBlockMs)
	}
	// 亚毫秒读数落整为 0 并因 omitempty 缺席——绝不写出一个撒谎的 0ms 字段。
	e = FromSample(metrics.Sample{Model: "claude-x", Upload: 300 * time.Microsecond})
	if e.UploadMs != 0 {
		t.Fatalf("sub-ms UploadMs = %d, want 0", e.UploadMs)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./journal/ -run TestFromSampleProjectsNetLegs -v`
Expected: FAIL（Event 无 UploadMs，编译错）

- [ ] **Step 3: 实现**

`journal/event.go` 的 `LatencyMs int64 \`json:"ms,omitempty"\`` 之后：

```go
	// UploadMs and WriteBlockMs bracket LatencyMs: the client/tunnel leg
	// delivering the body, and the cumulative blocking of writes back to the
	// client. Absent on events written before 2026-08 and on requests that
	// never passed the meter -- a reader must treat missing as "not measured",
	// never as "instant".
	UploadMs     int64 `json:"up_ms,omitempty"`
	WriteBlockMs int64 `json:"wb_ms,omitempty"`
```

`journal/writer.go` 的 `FromSample`，`if s.Latency > 0 {...}` 块之后：

```go
	if s.Upload > 0 {
		e.UploadMs = s.Upload.Milliseconds()
	}
	if s.WriteBlock > 0 {
		e.WriteBlockMs = s.WriteBlock.Milliseconds()
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./journal/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add journal/event.go journal/writer.go journal/writer_test.go
git commit -m "Project the net legs onto the journal's request events"
```

---

### Task 5: 注册中间件（次序即约束）+ 组合回归测试

**Files:**
- Modify: `proxy/proxy.go`（最后一个 `WithServerOptions` 块，`InFlightMiddleware` 注册之前）
- Test: `proxy/netmeter_test.go`（追加组合测试）

**Interfaces:**
- Consumes: Task 2 的 `NetMeterMiddleware`。

- [ ] **Step 1: 写失败测试（先锁次序契约）**

`proxy/netmeter_test.go` 追加：

```go
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
			&slowReader{chunks: []string{body[:8], body[8:]}, delay: 30 * time.Millisecond})
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
```

- [ ] **Step 2: 跑测试确认通过（这一步先绿：契约测试不依赖注册）**

Run: `go test ./proxy/ -run TestNetMeterOrder -v`
Expected: PASS。若 FAIL，先按失败信息修 Task 2 的实现，不得进入下一步。

- [ ] **Step 3: 注册**

`proxy/proxy.go` 最后一个 `WithServerOptions`（含 `InFlightMiddleware` 的那块），在 `cliproxyapi.WithMiddleware(InFlightMiddleware(...))` 之前插入：

```go
		// Before EarlyFlush, and the order is load-bearing: earlyflush drains
		// and replaces the request body (bodySniffer.readAll), so a meter
		// registered behind it would clock the in-memory replay at ~0ms
		// instead of the tunnel upload -- and EarlyFlush must itself stay
		// last (its writer closest to the handler; see its comment below).
		// TestNetMeterOrderAgainstEarlyFlush pins this.
		cliproxyapi.WithMiddleware(NetMeterMiddleware()),
```

- [ ] **Step 4: 全量测试**

Run: `go test ./...`
Expected: PASS（含 forkcheck、i18n lint）

- [ ] **Step 5: Commit**

```bash
git add proxy/proxy.go proxy/netmeter_test.go
git commit -m "Install the net meter ahead of the early-flush drain"
```

---

### Task 6: log 命令渲染与 -stats 均值

**Files:**
- Modify: `journal/reader.go`（`Summary` 结构体与 `Summarise`）
- Modify: `cmd/slimproxy/logcmd.go`（`writeEventTable` 表头与 KindRequest/KindReject 行、新增 `netCell`、`writeJournalSummary`）
- Test: `journal/reader_test.go`、`cmd/slimproxy/logcmd_net_test.go`（新建）

**Interfaces:**
- Consumes: Task 4 的 `Event.UploadMs` / `Event.WriteBlockMs`。
- Produces: `Summary.AvgUploadMs int64`、`Summary.AvgWriteBlockMs int64`；`netCell(e journal.Event) string`。

- [ ] **Step 1: 写失败测试**

`journal/reader_test.go` 追加：

```go
func TestSummariseAveragesNetLegs(t *testing.T) {
	ok := true
	s := Summarise([]Event{
		{Kind: KindRequest, OK: &ok, UploadMs: 100, WriteBlockMs: 30},
		{Kind: KindRequest, OK: &ok, UploadMs: 300},
		{Kind: KindRequest, OK: &ok}, // 旧事件无字段，不得拉低均值
	})
	if s.AvgUploadMs != 200 {
		t.Fatalf("AvgUploadMs = %d, want 200", s.AvgUploadMs)
	}
	if s.AvgWriteBlockMs != 30 {
		t.Fatalf("AvgWriteBlockMs = %d, want 30", s.AvgWriteBlockMs)
	}
}
```

`cmd/slimproxy/logcmd_net_test.go` 新建：

```go
package main

import (
	"testing"

	"github.com/Laurent00TT/slimproxy/journal"
)

func TestNetCell(t *testing.T) {
	e := journal.Event{Kind: journal.KindRequest, UploadMs: 900, WriteBlockMs: 150}
	if got := netCell(e); got != "传900ms┊写150ms" && got != "up 900ms┊wr 150ms" {
		t.Fatalf("netCell = %q", got)
	}
	if got := netCell(journal.Event{Kind: journal.KindRequest, UploadMs: 1200}); got != "传1.2s" && got != "up 1.2s" {
		t.Fatalf("upload-only netCell = %q", got)
	}
	if got := netCell(journal.Event{Kind: journal.KindRequest}); got != "—" {
		t.Fatalf("empty netCell = %q, want —", got)
	}
}
```

（双语两种可接受值并列断言，与 i18n lint 的既有做法一致；若仓库测试统一固定单语言环境，照 `logstate_test.go` 的现有断言风格改成单值。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./journal/ -run TestSummariseAveragesNetLegs -v; go test ./cmd/slimproxy/ -run TestNetCell -v`
Expected: 两者 FAIL（字段/函数未定义，编译错）

- [ ] **Step 3: 实现**

`journal/reader.go` `Summary` 的 `SlowestMs`/`P50Ms` 声明后加：

```go
	// AvgUploadMs and AvgWriteBlockMs average the two net legs over the
	// matches that carry them. Events from before the meter (or that never
	// passed it) are excluded from the denominator -- an absent reading is
	// "not measured", and averaging it in as zero would flatter the tunnel.
	AvgUploadMs     int64
	AvgWriteBlockMs int64
```

`Summarise` 的 `case KindRequest:` 内、局部累加变量与收尾：

```go
	// 函数开头 var lat []int64 旁：
	var upSum, upN, wbSum, wbN int64
	// case KindRequest 内：
	if e.UploadMs > 0 {
		upSum += e.UploadMs
		upN++
	}
	if e.WriteBlockMs > 0 {
		wbSum += e.WriteBlockMs
		wbN++
	}
	// return 前：
	if upN > 0 {
		s.AvgUploadMs = upSum / upN
	}
	if wbN > 0 {
		s.AvgWriteBlockMs = wbSum / wbN
	}
```

`cmd/slimproxy/logcmd.go`：

表头行改为（多一列 网络）：

```go
	fmt.Fprintln(tw, i18n.T("  时间\t状态\t路由\t模型\t首字\t总时长\t网络\t说明", "  time\tstatus\troute\tmodel\tttft\ttotal\tnet\tnotes"))
```

KindRequest 行的 `msOrDash(e.LatencyMs),` 与 `requestNote(e))` 之间插入 `netCell(e),`（格式串补一个 `\t`）；KindReject 行在最后一个说明单元格前补一个 `"—",`（格式串同步补 `\t`，保持列对齐）。

新增：

```go
// netCell renders the two net legs, or a dash when the event predates the
// meter. The upstream leg is not repeated here -- it is the 总时长 column.
func netCell(e journal.Event) string {
	var parts []string
	if e.UploadMs > 0 {
		parts = append(parts, i18n.T("传", "up ")+msOrDash(e.UploadMs))
	}
	if e.WriteBlockMs > 0 {
		parts = append(parts, i18n.T("写", "wr ")+msOrDash(e.WriteBlockMs))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "┊")
}
```

`writeJournalSummary` 的时延 p50 行之后：

```go
	if s.AvgUploadMs > 0 || s.AvgWriteBlockMs > 0 {
		fmt.Fprintf(cx.stdout, i18n.T("  网络 上传均值 %s，回写阻塞均值 %s\n", "  network: avg upload %s, avg write-block %s\n"), msOrDash(s.AvgUploadMs), msOrDash(s.AvgWriteBlockMs))
	}
```

- [ ] **Step 4: 跑测试确认通过（含既有表格断言的连带修复）**

Run: `go test ./journal/ ./cmd/slimproxy/`
Expected: PASS。若既有测试断言了旧表头/旧列数而红，按新列更新那些断言——这是本任务的一部分，不是回归。

- [ ] **Step 5: Commit**

```bash
git add journal/reader.go cmd/slimproxy/logcmd.go journal/reader_test.go cmd/slimproxy/logcmd_net_test.go
git commit -m "Show the net legs in log rows and the stats summary"
```

---

### Task 7: 全量验证与竞态检查

**Files:** 无新改动（只验证；有红则回相应任务修）

- [ ] **Step 1: 竞态与全量**

Run: `go test -race ./... && go vet ./... && go build -o slimproxy.new.exe .`
Expected: 全绿，构建成功

- [ ] **Step 2: 真机冒烟（可选但推荐；改动落地 ≠ 验证）**

用新二进制重启 serve 后 `./slimproxy.exe test`，再 `./slimproxy.exe log -since 10m`，确认新请求行出现 网络 列且数值合理（本机直连上传应为 ~0 或个位 ms）。**重启会斩断在飞请求，执行前确认无人在用。**

- [ ] **Step 3: Commit（若冒烟中有微调）**

```bash
git add -A && git commit -m "Verify the net-leg meter end to end"
```

---

## Self-Review（已跑）

- **Spec 覆盖**：三段定义→T1/T2；ctx 合并→T3；journal 字段→T4；注册次序与 earlyflush 交互→T5；每行显示+均值→T6；-race/哨兵→T3 测试注释+T7。fork 零改动全程成立。
- **占位符**：Step 均带真实代码；两处「照现有写法」是对既有测试基建的引用（Observe 赋值方式、单双语断言风格），实现者以所在文件为准，非空缺。
- **类型一致性**：`NetTimings`/`NetTimingsFrom`/`Sample.Upload`/`Event.UploadMs`(`up_ms`)/`Summary.AvgUploadMs`/`netCell` 在各任务间名称与类型一致。
