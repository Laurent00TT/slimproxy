package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"

	"github.com/Laurent00TT/slimproxy/metrics"
)

// stallFakeExecutor mirrors ClaudeExecutor's exported method set exactly, so
// the reflection gate in ensureStallGuard accepts it. Its stream is whatever
// the test feeds into chunks; upCtx captures the context the guard hands down,
// which is how tests observe the sever.
type stallFakeExecutor struct {
	chunks chan cliproxyexecutor.StreamChunk
	upCtx  context.Context
}

func (f *stallFakeExecutor) Identifier() string { return "claude" }

func (f *stallFakeExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}

func (f *stallFakeExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	f.upCtx = ctx
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: f.chunks}, nil
}

func (f *stallFakeExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}

func (f *stallFakeExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}

func (f *stallFakeExecutor) HttpRequest(_ context.Context, _ *coreauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (f *stallFakeExecutor) PrepareRequest(_ *http.Request, _ *coreauth.Auth) error { return nil }

func collectUntilClosed(t *testing.T, ch <-chan cliproxyexecutor.StreamChunk, deadline time.Duration) []cliproxyexecutor.StreamChunk {
	t.Helper()
	var got []cliproxyexecutor.StreamChunk
	timeout := time.After(deadline)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, c)
		case <-timeout:
			t.Fatalf("流在 %s 内没有关闭；已收 %d 个 chunk", deadline, len(got))
		}
	}
}

// TestStallGuardSeversSilentStream is the feature: a stream that goes quiet
// must end with an explicit timeout error instead of hanging until the client
// gives up.
func TestStallGuardSeversSilentStream(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 4)}
	g := &stallGuard{inner: fake, idle: 80 * time.Millisecond}

	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("a")}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("b")}
	// ...and then silence: the channel stays open but nothing more arrives.

	result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	// The guard cancels the upstream context when it fires; the inner stream
	// closing is what lets the pump's drain finish. A real executor does this
	// on its own; the fake needs the nudge -- armed before collecting, or the
	// drain and the collector deadlock waiting on each other.
	go func() {
		<-fake.upCtx.Done()
		close(fake.chunks)
	}()

	got := collectUntilClosed(t, result.Chunks, 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("收到 %d 个 chunk，want 3（两个数据 + 一个超时错误）", len(got))
	}
	if got[0].Err != nil || got[1].Err != nil {
		t.Errorf("数据 chunk 不该带错误: %v %v", got[0].Err, got[1].Err)
	}
	last := got[2].Err
	if last == nil {
		t.Fatal("最后一个 chunk 该是看门狗的超时错误，得到的是数据")
	}
	if !strings.Contains(last.Error(), "stalled") || !strings.Contains(last.Error(), "timeout") {
		t.Errorf("错误文本 %q 缺少 stalled/timeout 标记", last.Error())
	}
	select {
	case <-fake.upCtx.Done():
	case <-time.After(time.Second):
		t.Error("看门狗触发后上游 context 没有被取消——死流不会被收走")
	}
}

// TestStallGuardQuietWhenChunksFlow: a live stream must pass through
// untouched, however long it runs relative to the idle window in total.
func TestStallGuardQuietWhenChunksFlow(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk)}
	// A wide margin on purpose: the assertion is "many short gaps do not add
	// up to a stall", which needs gap << idle, not gap close to it. At 30ms
	// against a 100ms window one descheduled producer on a loaded machine --
	// Windows' timer granularity alone is ~15ms -- turns a correct guard into
	// a red test.
	const gap = 20 * time.Millisecond
	g := &stallGuard{inner: fake, idle: 2 * time.Second}

	result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(gap) // cumulative 100ms; each gap is 1% of the window
			fake.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte{byte(i)}}
		}
		close(fake.chunks)
	}()

	got := collectUntilClosed(t, result.Chunks, 2*time.Second)
	if len(got) != 5 {
		t.Fatalf("收到 %d 个 chunk，want 5", len(got))
	}
	for i, c := range got {
		if c.Err != nil {
			t.Errorf("chunk %d 带了错误 %v：活跃的流不该被看门狗碰", i, c.Err)
		}
	}
}

// TestSlowConsumerIsNotAStall: the idle window times waits on the upstream,
// not waits on the reader. A reader that drains slowly must not get its
// healthy stream severed.
func TestSlowConsumerIsNotAStall(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 3)}
	g := &stallGuard{inner: fake, idle: 80 * time.Millisecond}

	// Upstream delivers instantly...
	for i := 0; i < 3; i++ {
		fake.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte{byte(i)}}
	}
	close(fake.chunks)

	result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	// ...and the reader sits on its hands for twice the idle window before
	// touching the stream.
	time.Sleep(160 * time.Millisecond)

	got := collectUntilClosed(t, result.Chunks, 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("收到 %d 个 chunk，want 3", len(got))
	}
	for i, c := range got {
		if c.Err != nil {
			t.Errorf("chunk %d 带了错误 %v：慢消费不是上游 stall", i, c.Err)
		}
	}
}

// TestStallIsReportedByTheGuardItself is the observability contract, and it
// exists because the obvious version of this test was measuring nothing.
//
// The first version asserted metrics.CauseFromText(stall error) == timeout and
// passed -- but that string never reaches the usage pipeline. Severing works
// by cancelling the upstream context, so the inner executor reports its own
// "context canceled" and the request is filed under CauseCanceled: the bucket
// for a deliberate Ctrl-C, excluded from health alerting. Stalls would have
// been invisible in the journal while a green test claimed otherwise.
//
// So the guard reports itself, and this pins that it does.
func TestStallIsReportedByTheGuardItself(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 1)}
	var reported []time.Duration
	var mu sync.Mutex
	g := &stallGuard{
		inner: fake,
		idle:  80 * time.Millisecond,
		onStall: func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, d)
		},
	}

	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("a")}

	result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	go func() {
		<-fake.upCtx.Done()
		close(fake.chunks)
	}()
	collectUntilClosed(t, result.Chunks, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 {
		t.Fatalf("看门狗上报了 %d 次，want 1——stall 不上报就只会被记成客户端取消", len(reported))
	}
	if reported[0] != 80*time.Millisecond {
		t.Errorf("上报的窗口 = %v, want 80ms", reported[0])
	}
}

// TestHealthyStreamReportsNothing: the reporting path must not fire for a
// stream that ends normally, or the journal fills with phantom stalls.
func TestHealthyStreamReportsNothing(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 2)}
	var fired int
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, onStall: func(time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		fired++
	}}

	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("a")}
	close(fake.chunks)

	result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	collectUntilClosed(t, result.Chunks, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if fired != 0 {
		t.Fatalf("正常结束的流触发了 %d 次上报", fired)
	}
}

// TestStallErrorAvoidsMisclassifyingWords keeps the client-facing wording from
// drifting into a term the taxonomy reads as something else. This is about the
// SSE frame a human reads, not the usage pipeline -- see the test above for
// why those are different things.
func TestStallErrorAvoidsMisclassifyingWords(t *testing.T) {
	text := errStreamStalled(defaultStreamIdle).Error()
	if got := metrics.CauseFromText(text); got != metrics.CauseTimeout {
		t.Errorf("CauseFromText(%q) = %q, want %q", text, got, metrics.CauseTimeout)
	}
	for _, bad := range []string{"canceled", "cancelled", "dial ", "refused", "eof"} {
		if strings.Contains(strings.ToLower(text), bad) {
			t.Errorf("错误文本含有 %q，会被读成另一类失败: %s", bad, text)
		}
	}
}

// TestEnsureStallGuardWrapsAndReasserts: upstream re-registers a bare executor
// on every auth update; the sweep must wrap it again, and must not wrap a
// wrap.
func TestEnsureStallGuardWrapsAndReasserts(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk)}
	mgr.RegisterExecutor(fake)

	ensureStallGuard(mgr, 90*time.Second, nil)
	exec, ok := mgr.Executor("claude")
	if !ok {
		t.Fatal("claude executor 不见了")
	}
	if _, wrapped := exec.(*stallGuard); !wrapped {
		t.Fatal("ensureStallGuard 没有安装包装")
	}

	ensureStallGuard(mgr, 90*time.Second, nil)
	exec, _ = mgr.Executor("claude")
	g, wrapped := exec.(*stallGuard)
	if !wrapped {
		t.Fatal("第二次 ensure 后包装消失了")
	}
	if _, double := g.inner.(*stallGuard); double {
		t.Fatal("包装被套了两层：ensure 不幂等")
	}

	// Upstream replaces the executor out from under us.
	mgr.RegisterExecutor(fake)
	exec, _ = mgr.Executor("claude")
	if _, wrapped := exec.(*stallGuard); wrapped {
		t.Fatal("测试前提坏了：RegisterExecutor 应该已经换成了裸 executor")
	}
	ensureStallGuard(mgr, 90*time.Second, nil)
	exec, _ = mgr.Executor("claude")
	if _, wrapped := exec.(*stallGuard); !wrapped {
		t.Fatal("上游覆盖后 ensure 没有重新包装")
	}
}

// widerFakeExecutor has one exported method the guard does not forward.
type widerFakeExecutor struct{ stallFakeExecutor }

func (w *widerFakeExecutor) ExtraCapability() {}

// TestMethodSetGateRefusesWiderExecutors: wrapping an executor with methods
// the wrapper lacks would silently amputate them -- the gate must refuse and
// leave the executor bare.
func TestMethodSetGateRefusesWiderExecutors(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	mgr.RegisterExecutor(&widerFakeExecutor{stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk)}})

	ensureStallGuard(mgr, 90*time.Second, nil)
	exec, _ := mgr.Executor("claude")
	if _, wrapped := exec.(*stallGuard); wrapped {
		t.Fatal("方法集更宽的 executor 被包装了：额外能力会被静默剥掉")
	}
}

// TestStallGuardThroughConductor runs the whole path the production request
// takes: Manager.ExecuteStream -> guard -> conductor stream wrapper, with the
// model registry and a registered credential, and asserts the consumer sees
// data then the explicit stall error.
func TestStallGuardThroughConductor(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	mgr.SetRetryConfig(3, 30*time.Second, 0)
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 2)}
	mgr.RegisterExecutor(fake)
	if _, err := mgr.Register(context.Background(), &coreauth.Auth{ID: "claude-stall-probe", Provider: "claude"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	cliproxy.GlobalModelRegistry().RegisterClient("claude-stall-probe", "claude", []*cliproxy.ModelInfo{
		{ID: "claude-opus-5", Object: "model", Type: "claude"},
	})
	t.Cleanup(func() { cliproxy.GlobalModelRegistry().UnregisterClient("claude-stall-probe") })

	ensureStallGuard(mgr, 80*time.Millisecond, nil)

	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: hello")}
	// then silence

	result, err := mgr.ExecuteStream(context.Background(), []string{"claude"},
		cliproxyexecutor.Request{Model: "claude-opus-5"}, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if result == nil || result.Chunks == nil {
		t.Fatal("没有拿到流")
	}
	go func() {
		<-fake.upCtx.Done()
		close(fake.chunks)
	}()

	var sawData, sawStall bool
	deadline := time.After(3 * time.Second)
	for !sawStall {
		select {
		case c, ok := <-result.Chunks:
			if !ok {
				if !sawStall {
					t.Fatal("流关闭了但没有出现看门狗的超时错误")
				}
				break
			}
			if c.Err != nil {
				if !strings.Contains(c.Err.Error(), "stalled") {
					t.Fatalf("错误 chunk 文本 %q 不是看门狗的", c.Err.Error())
				}
				sawStall = true
				continue
			}
			sawData = true
		case <-deadline:
			t.Fatalf("3s 内没有看到 stall 错误（sawData=%v）——看门狗没起作用", sawData)
		}
	}
	if !sawData {
		t.Error("stall 错误之前应该先看到正常数据 chunk")
	}
}
