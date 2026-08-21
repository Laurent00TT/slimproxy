package metrics

import (
	"context"
	"sync"
	"testing"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
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

	// nil ctx 不得 panic：fork 的 safeInvoke 虽会兜住，但样本会随 panic 一起
	// 丢——这里必须照常记录（三段为零）。
	c.HandleUsage(nil, cliproxyusage.Record{Model: "nilctx"})
	if got.Model != "nilctx" {
		t.Fatalf("nil ctx 的样本丢了：observed %q", got.Model)
	}
	if got.Upload != 0 || got.WriteBlock != 0 {
		t.Fatalf("nil ctx produced Upload=%v WriteBlock=%v, want zeros", got.Upload, got.WriteBlock)
	}
}
