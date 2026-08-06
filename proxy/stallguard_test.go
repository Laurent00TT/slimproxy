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

// waitForDrops blocks until the drop counter reaches want, or fails the test.
//
// Order matters in the blocked-forward tests: reading the stream would reopen
// the forward path and let the pump exit through the OUTER select instead of
// the branch under test, so the report must be awaited while the pump still
// has no reader -- its only possible exit then is the inner Done branch.
func waitForDrops(t *testing.T, mu *sync.Mutex, drops *[]bool, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(*drops)
		mu.Unlock()
		if n >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("2s 内 clientDrop 只上报了 %d 次，want %d", n, want)
		case <-time.After(time.Millisecond):
		}
	}
}

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
	var noTails, drops int
	var mu sync.Mutex
	g := &stallGuard{
		inner: fake,
		idle:  80 * time.Millisecond,
		notes: stallGuardNotes{
			stalled: func(_ stallStreamMeta, d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				reported = append(reported, d)
			},
			noTail: func(stallStreamMeta) { mu.Lock(); noTails++; mu.Unlock() },
			clientDrop: func(stallStreamMeta, bool) { mu.Lock(); drops++; mu.Unlock() },
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
	if noTails != 0 || drops != 0 {
		t.Errorf("stall 退出还额外报了 noTail=%d clientDrop=%d：一次结局只该有一个名字", noTails, drops)
	}
}

// TestHealthyStreamReportsNothing: the reporting path must not fire for a
// stream that ends normally -- tail and all -- or the journal fills with
// phantom endings.
func TestHealthyStreamReportsNothing(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 3)}
	var stalls, noTails, drops int
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
		stalled:    func(stallStreamMeta, time.Duration) { mu.Lock(); stalls++; mu.Unlock() },
		noTail:     func(stallStreamMeta) { mu.Lock(); noTails++; mu.Unlock() },
		clientDrop: func(stallStreamMeta, bool) { mu.Lock(); drops++; mu.Unlock() },
	}}

	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_stop", `{"type":"message_stop"}`)}
	close(fake.chunks)

	result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	collectUntilClosed(t, result.Chunks, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if stalls != 0 || noTails != 0 || drops != 0 {
		t.Fatalf("完整结束的流触发了上报：stall=%d noTail=%d clientDrop=%d", stalls, noTails, drops)
	}
}

// sseEvent renders one SSE event the way the claude passthrough delivers it:
// event line, data line, blank terminator -- one chunk per event.
func sseEvent(name, data string) []byte {
	return []byte("event: " + name + "\ndata: " + data + "\n\n")
}

// TestNoTailReportedOnCleanEOFWithoutUsage is the silent gap made explicit:
// a stream that starts normally and ends cleanly WITHOUT the tail
// message_delta leaves no usage record anywhere -- the SDK's reporter only
// publishes on a line with a top-level usage field. This is the exact shape of
// the two requests found missing from two days of journal (request_id
// c00cb3b6 and 8d93f755): message_start, some deltas, EOF, access log 200.
//
// message_start is in the feed deliberately: it carries usage NESTED under
// "message", and the guard counting that as the tail would blind the check on
// every stream's first event.
func TestNoTailReportedOnCleanEOFWithoutUsage(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 3)}
	var metas []stallStreamMeta
	var drops int
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
		noTail:     func(m stallStreamMeta) { mu.Lock(); metas = append(metas, m); mu.Unlock() },
		clientDrop: func(stallStreamMeta, bool) { mu.Lock(); drops++; mu.Unlock() },
	}}

	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3,"output_tokens":1}}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_stop", `{"type":"message_stop"}`)}
	close(fake.chunks)

	result, err := g.ExecuteStream(context.Background(),
		&coreauth.Auth{ID: "auth-id-1", Label: "friend-key"},
		cliproxyexecutor.Request{Model: "claude-opus-5"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	got := collectUntilClosed(t, result.Chunks, 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("收到 %d 个 chunk，want 3：上报不能改变转发的字节", len(got))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(metas) != 1 {
		t.Fatalf("noTail 上报了 %d 次，want 1——不自报，这种流就是 journal 里的零记录", len(metas))
	}
	// The meta is what turns the event from "something happened" into "this
	// model, this credential, this far in" -- the questions that previously
	// needed the access log.
	if metas[0].model != "claude-opus-5" {
		t.Errorf("meta.model = %q, want claude-opus-5", metas[0].model)
	}
	if metas[0].auth != "friend-key" {
		t.Errorf("meta.auth = %q, want label 优先于 ID", metas[0].auth)
	}
	if metas[0].started.IsZero() {
		t.Error("meta.started 是零值——事件算不出流龄")
	}
	if drops != 0 {
		t.Errorf("干净 EOF 还报了 %d 次 clientDrop", drops)
	}
}

// TestNoTailQuietWhenStreamErrors: a stream that ends in a chunk-level error
// was published by the SDK's PublishFailure -- the books are settled, and a
// noTail on top would cry wolf on every failed request.
func TestNoTailQuietWhenStreamErrors(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 3)}
	var noTails int
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
		noTail: func(stallStreamMeta) { mu.Lock(); noTails++; mu.Unlock() },
	}}

	// message_start first, so the ledger is armed and the quiet outcome is
	// the error suppression specifically -- not the unarmed short-circuit.
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("upstream fell over")}
	close(fake.chunks)

	result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	collectUntilClosed(t, result.Chunks, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if noTails != 0 {
		t.Fatalf("带错误结束的流报了 %d 次 noTail：PublishFailure 已经记过账了", noTails)
	}
}

// TestClientDropReportedUnaccounted: the request context dying while the
// upstream stream is still open must be said out loud, with the ledger state
// attached -- before the tail, whether the SDK published anything depends on
// where the cancel caught it, so this event is the only trace the request is
// guaranteed to leave.
func TestClientDropReportedUnaccounted(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 2)}
	var drops []bool
	var noTails int
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
		noTail:     func(stallStreamMeta) { mu.Lock(); noTails++; mu.Unlock() },
		clientDrop: func(_ stallStreamMeta, accounted bool) { mu.Lock(); drops = append(drops, accounted); mu.Unlock() },
	}}

	reqCtx, hangUp := context.WithCancel(context.Background())
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)}

	result, err := g.ExecuteStream(reqCtx, &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	// Drain the delivered chunks so the pump is back waiting on the upstream
	// when the hang-up lands.
	for i := 0; i < 2; i++ {
		select {
		case <-result.Chunks:
		case <-time.After(time.Second):
			t.Fatal("1s 内没有收到转发的 chunk")
		}
	}
	hangUp()

	got := collectUntilClosed(t, result.Chunks, 2*time.Second)
	if len(got) != 0 {
		t.Fatalf("挂断后还收到 %d 个 chunk", len(got))
	}
	select {
	case <-fake.upCtx.Done():
	case <-time.After(time.Second):
		t.Error("挂断后上游 context 没有被取消——上游连接会被白白吊着")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(drops) != 1 {
		t.Fatalf("clientDrop 上报了 %d 次，want 1", len(drops))
	}
	if drops[0] {
		t.Error("usage 尾行从未出现，accounted 却是 true——事件的整个价值就在这个标志")
	}
	if noTails != 0 {
		t.Errorf("挂断退出还报了 %d 次 noTail：一次结局只该有一个名字", noTails)
	}
}

// TestClientDropWhileForwardBlocked exercises the OTHER drop exit: the cancel
// catching the pump mid-forward, parked on a send the client will never
// drain. That is the common real shape -- a dropped client stops reading
// first -- and it is a different select branch than the drained-then-cancel
// path above; a mutation deleting its report survived the whole suite until
// this test existed.
func TestClientDropWhileForwardBlocked(t *testing.T) {
	// Unbuffered on purpose: the test's send completes only when the pump has
	// RECEIVED the chunk, so the hang-up below deterministically lands while
	// the pump is parked on the blocked forward.
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk)}
	var drops []bool
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
		clientDrop: func(_ stallStreamMeta, accounted bool) { mu.Lock(); drops = append(drops, accounted); mu.Unlock() },
	}}

	reqCtx, hangUp := context.WithCancel(context.Background())
	result, err := g.ExecuteStream(reqCtx, &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`)}
	// Nobody reads result.Chunks: the pump is now stuck forwarding. The
	// report is awaited BEFORE any read -- see waitForDrops for why.
	hangUp()
	waitForDrops(t, &mu, &drops, 1)

	got := collectUntilClosed(t, result.Chunks, 2*time.Second)
	if len(got) != 0 {
		t.Fatalf("挂断后收到 %d 个 chunk，want 0", len(got))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(drops) != 1 {
		t.Fatalf("clientDrop 上报了 %d 次，want 1——卡在转发上的挂断没人报，journal 就少一条", len(drops))
	}
	if drops[0] {
		t.Error("尾行从未过流，accounted 却是 true")
	}
}

// TestClientDropAccountedAtReceiveNotForward pins the ledger's bookkeeping
// point: accounted flips when the tail is RECEIVED from the upstream, not
// when it is forwarded -- the SDK published before it ever sent the chunk, so
// a tail stuck in a blocked forward is still a settled account.
func TestClientDropAccountedAtReceiveNotForward(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk)}
	var drops []bool
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
		clientDrop: func(_ stallStreamMeta, accounted bool) { mu.Lock(); drops = append(drops, accounted); mu.Unlock() },
	}}

	reqCtx, hangUp := context.WithCancel(context.Background())
	result, err := g.ExecuteStream(reqCtx, &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`)}
	select {
	case <-result.Chunks:
	case <-time.After(time.Second):
		t.Fatal("1s 内没有收到 message_start")
	}
	// The unbuffered send synchronizes: once it returns, the pump HAS the
	// tail. The client then stops reading and hangs up with that tail still
	// stuck in the forward; the report is awaited before any read.
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`)}
	hangUp()
	waitForDrops(t, &mu, &drops, 1)
	collectUntilClosed(t, result.Chunks, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(drops) != 1 {
		t.Fatalf("clientDrop 上报了 %d 次，want 1", len(drops))
	}
	if !drops[0] {
		t.Error("尾行已被接收（虽未转发出去），accounted 却是 false——SDK 在发送前就已发布，这笔账是记了的")
	}
}

// TestUnarmedStreamStaysSilent: a stream that never speaks the claude dialect
// -- a translated route, whose usage lines the translators nest or rename --
// must report nothing at all. Before the arming gate, every successful
// gemini/responses/interactions-format stream would have journalled a phantom
// notail; blindness there is the accepted price of a journal with no false
// entries.
func TestUnarmedStreamStaysSilent(t *testing.T) {
	geminiish := []byte(`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"totalTokenCount":7}}` + "\n\n")

	t.Run("clean close", func(t *testing.T) {
		fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 1)}
		var noTails, drops int
		var mu sync.Mutex
		g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
			noTail:     func(stallStreamMeta) { mu.Lock(); noTails++; mu.Unlock() },
			clientDrop: func(stallStreamMeta, bool) { mu.Lock(); drops++; mu.Unlock() },
		}}
		fake.chunks <- cliproxyexecutor.StreamChunk{Payload: geminiish}
		close(fake.chunks)

		result, err := g.ExecuteStream(context.Background(), &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		collectUntilClosed(t, result.Chunks, 2*time.Second)

		mu.Lock()
		defer mu.Unlock()
		if noTails != 0 || drops != 0 {
			t.Fatalf("未上膛的流上报了 noTail=%d clientDrop=%d：翻译路由的每次成功都会变成幻影事件", noTails, drops)
		}
	})

	t.Run("hang-up", func(t *testing.T) {
		fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 1)}
		var noTails, drops int
		var mu sync.Mutex
		g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
			noTail:     func(stallStreamMeta) { mu.Lock(); noTails++; mu.Unlock() },
			clientDrop: func(stallStreamMeta, bool) { mu.Lock(); drops++; mu.Unlock() },
		}}
		fake.chunks <- cliproxyexecutor.StreamChunk{Payload: geminiish}

		reqCtx, hangUp := context.WithCancel(context.Background())
		result, err := g.ExecuteStream(reqCtx, &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		select {
		case <-result.Chunks:
		case <-time.After(time.Second):
			t.Fatal("1s 内没有收到转发的 chunk")
		}
		hangUp()
		collectUntilClosed(t, result.Chunks, 2*time.Second)

		mu.Lock()
		defer mu.Unlock()
		if noTails != 0 || drops != 0 {
			t.Fatalf("未上膛的流上报了 noTail=%d clientDrop=%d", noTails, drops)
		}
	})
}

// TestClientDropAccountedAfterTail: same hang-up, but the tail already passed
// through -- the event must say the books are settled, or every routine
// esc-cancel reads like a lost record.
func TestClientDropAccountedAfterTail(t *testing.T) {
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk, 2)}
	var drops []bool
	var mu sync.Mutex
	g := &stallGuard{inner: fake, idle: time.Hour, notes: stallGuardNotes{
		clientDrop: func(_ stallStreamMeta, accounted bool) { mu.Lock(); drops = append(drops, accounted); mu.Unlock() },
	}}

	reqCtx, hangUp := context.WithCancel(context.Background())
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`)}
	fake.chunks <- cliproxyexecutor.StreamChunk{Payload: sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`)}

	result, err := g.ExecuteStream(reqCtx, &coreauth.Auth{}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-result.Chunks:
		case <-time.After(time.Second):
			t.Fatal("1s 内没有收到转发的 chunk")
		}
	}
	hangUp()
	collectUntilClosed(t, result.Chunks, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(drops) != 1 {
		t.Fatalf("clientDrop 上报了 %d 次，want 1", len(drops))
	}
	if !drops[0] {
		t.Error("尾行已经过流，accounted 却是 false——会把记好账的取消误报成疑似丢账")
	}
}

// TestScanLedgerSignals pins the per-line predicate to the SDK's
// (helps.ParseClaudeStreamUsage): data-prefix stripping, valid JSON, and --
// the load-bearing part -- usage at the TOP level only, with message_start
// arming the ledger instead of settling it. The translated-format rows are
// the arming gate's reason to exist: none of them may register either way.
func TestScanLedgerSignals(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantOpens   bool
		wantSettles bool
	}{
		{"尾部 message_delta", "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n", false, true},
		{"message_start 上膛但不结账", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n", true, false},
		{"文本增量", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"usage\"}}\n\n", false, false},
		{"data: 后无空格", "data:{\"usage\":{}}\n\n", false, true},
		{"裸 JSON 行（无 data: 前缀）", "{\"usage\":{\"output_tokens\":1}}", false, true},
		{"[DONE] 哨兵", "data: [DONE]\n\n", false, false},
		{"非法 JSON 里出现 usage 字样", "data: {\"usage\":oops}\n\n", false, false},
		{"event 行里出现 usage 字样", "event: usage\ndata: {\"type\":\"ping\"}\n\n", false, false},
		{"多行事件的第二个 data 行带 usage", "event: x\ndata: {\"type\":\"x\"}\ndata: {\"usage\":{}}\n\n", false, true},
		{"空 payload", "", false, false},
		{"gemini 翻译流的 usageMetadata 两不沾", "data: {\"candidates\":[],\"usageMetadata\":{\"totalTokenCount\":7}}\n\n", false, false},
		{"responses 翻译流的嵌套 response.usage 两不沾", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"total_tokens\":9}}}\n\n", false, false},
	}
	for _, tc := range cases {
		opens, settles := scanLedgerSignals([]byte(tc.payload))
		if opens != tc.wantOpens || settles != tc.wantSettles {
			t.Errorf("%s: scanLedgerSignals = (opens=%v, settles=%v), want (%v, %v)", tc.name, opens, settles, tc.wantOpens, tc.wantSettles)
		}
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
// wrap. The notes plumbing is asserted at every step because it is what a
// refactor loses silently: the pump nil-guards every callback, so dropped
// wiring compiles, passes every direct-construction test, and journals
// nothing -- the whole feature off with the suite green (a mutation doing
// exactly that survived until these assertions existed).
func TestEnsureStallGuardWrapsAndReasserts(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	fake := &stallFakeExecutor{chunks: make(chan cliproxyexecutor.StreamChunk)}
	mgr.RegisterExecutor(fake)

	notes := stallGuardNotes{
		stalled:    func(stallStreamMeta, time.Duration) {},
		noTail:     func(stallStreamMeta) {},
		clientDrop: func(stallStreamMeta, bool) {},
	}
	requireWired := func(t *testing.T, when string) *stallGuard {
		t.Helper()
		exec, ok := mgr.Executor("claude")
		if !ok {
			t.Fatalf("%s：claude executor 不见了", when)
		}
		g, wrapped := exec.(*stallGuard)
		if !wrapped {
			t.Fatalf("%s：没有安装包装", when)
		}
		if g.notes.stalled == nil || g.notes.noTail == nil || g.notes.clientDrop == nil {
			t.Fatalf("%s：notes 接线丢了 (stalled=%v noTail=%v clientDrop=%v)——三类事件会静默消失",
				when, g.notes.stalled != nil, g.notes.noTail != nil, g.notes.clientDrop != nil)
		}
		return g
	}

	ensureStallGuard(mgr, 90*time.Second, notes)
	requireWired(t, "首次 ensure 后")

	ensureStallGuard(mgr, 90*time.Second, notes)
	g := requireWired(t, "第二次 ensure 后")
	if _, double := g.inner.(*stallGuard); double {
		t.Fatal("包装被套了两层：ensure 不幂等")
	}

	// Upstream replaces the executor out from under us.
	mgr.RegisterExecutor(fake)
	exec, _ := mgr.Executor("claude")
	if _, wrapped := exec.(*stallGuard); wrapped {
		t.Fatal("测试前提坏了：RegisterExecutor 应该已经换成了裸 executor")
	}
	ensureStallGuard(mgr, 90*time.Second, notes)
	requireWired(t, "上游覆盖后重新 ensure")
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

	ensureStallGuard(mgr, 90*time.Second, stallGuardNotes{})
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

	ensureStallGuard(mgr, 80*time.Millisecond, stallGuardNotes{})

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
