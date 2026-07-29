package metrics

import (
	"sync"
	"testing"
	"time"
)

// TestTokenIsolatesConcurrentRequestsOnOnePath pins why Begin returns a token
// instead of keying by path.
//
// Two calls to /v1/messages overlapping is the normal case here, not the edge
// one. Keyed by path, the first to finish would clear the entry belonging to
// the one still running, and the panel would show a long request vanishing at
// the moment an unrelated short one completed.
func TestTokenIsolatesConcurrentRequestsOnOnePath(t *testing.T) {
	f := NewInFlight()
	base := time.Now()

	slow := f.Begin("/v1/messages", base)
	fast := f.Begin("/v1/messages", base.Add(time.Second))

	f.End(fast)

	live := f.Snapshot()
	if len(live) != 1 {
		t.Fatalf("结束一个后应剩 1 个，实际 %d", len(live))
	}
	if !live[0].At.Equal(base) {
		t.Errorf("剩下的应是先开始的那个，实际是 %v 开始的", live[0].At)
	}

	f.End(slow)
	if n := len(f.Snapshot()); n != 0 {
		t.Errorf("全部结束后应为空，实际 %d", n)
	}
}

// TestSnapshotIsOldestFirst pins the ordering the display's truncation relies
// on: it keeps the head of this slice, and the entry worth keeping is the one
// closest to being stuck.
func TestSnapshotIsOldestFirst(t *testing.T) {
	f := NewInFlight()
	base := time.Now()
	// Inserted out of order on purpose: a map iteration that happened to come
	// out in insertion order must not be able to pass this by luck.
	f.Begin("/c", base.Add(2*time.Second))
	f.Begin("/a", base)
	f.Begin("/b", base.Add(time.Second))

	got := f.Snapshot()
	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("数量 %d，应为 %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Fatalf("顺序 %v，应为 %v——截断保留头部，顺序反了就会丢掉最久的那个",
				pathsOf(got), want)
		}
	}
}

func pathsOf(ps []Pending) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Path
	}
	return out
}

// TestEndIsIdempotent: the middleware defers End, and any future early-return
// path that also calls it must not corrupt the map.
func TestEndIsIdempotent(t *testing.T) {
	f := NewInFlight()
	tok := f.Begin("/v1/messages", time.Now())
	f.End(tok)
	f.End(tok)
	if n := len(f.Snapshot()); n != 0 {
		t.Errorf("应为空，实际 %d", n)
	}
}

// TestPendingSurvivesTheEmptyCollector pins the early return in
// Collector.Snapshot.
//
// It returns as soon as it sees no samples, and "no samples" is precisely the
// state of a proxy that has started its first request and not yet finished one
// -- the single moment when the in-flight list is the only thing there is to
// report. Filling Pending in after that check would leave the first request of
// every session invisible, which is the defect this whole feature exists to
// remove.
func TestPendingSurvivesTheEmptyCollector(t *testing.T) {
	c := NewCollector()
	c.InFlight().Begin("/v1/messages", time.Now())

	snap := c.Snapshot(time.Now())
	if len(snap.Pending) != 1 {
		t.Fatalf("还没有已完成请求时 Pending 丢了：%d 个，应为 1", len(snap.Pending))
	}
	if snap.Total != 0 {
		t.Errorf("Total = %d，应为 0", snap.Total)
	}
}

// TestConcurrentUse is the -race check. Begin and End run on request
// goroutines while the render loop calls Snapshot once a second.
func TestConcurrentUse(t *testing.T) {
	f := NewInFlight()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.End(f.Begin("/v1/messages", time.Now()))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = f.Snapshot()
		}
	}()

	wg.Wait()
	if n := len(f.Snapshot()); n != 0 {
		t.Errorf("全部结束后应为空，实际 %d", n)
	}
}
