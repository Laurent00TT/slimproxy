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
