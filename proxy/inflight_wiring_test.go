package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// TestInFlightDoesNotDependOnJournalling pins where the middleware is
// registered, which is the whole reason it sits outside the journal branch.
//
// The same mistake has been made twice in this file already -- everything that
// watched requests was wired into the journal branch, so `log-to-file: false`
// silently bought a dashboard with no alarm attached. What is running right now
// is a property of the display, not of the diary, and putting it up there would
// mean turning off the event log emptied the panel's only live section.
//
// Also checks that the tracker the middleware writes to is the one the snapshot
// reads from. They are reached through different paths -- the middleware takes
// what Build hands it, the dashboard calls Snapshot -- and two instances would
// fail in the quietest possible way: everything works, nothing ever appears.
func TestInFlightDoesNotDependOnJournalling(t *testing.T) {
	for _, tc := range []struct {
		name string
		days int
	}{
		{"journalling on", 7},
		{"journalling off", JournalDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetLevel(log.InfoLevel) })

			rt, err := Build(Config{
				Host:        "127.0.0.1",
				Port:        freeTestPort(t),
				APIKeys:     []string{"k"},
				AuthDir:     filepath.Join(t.TempDir(), "auths"),
				LogDir:      t.TempDir(),
				JournalDays: tc.days,
			}, t.TempDir())
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			f := rt.Stats.InFlight()
			if f == nil {
				t.Fatal("没有 in-flight 追踪器：中间件会拿着 nil 构造，第一个请求就 panic")
			}

			now := time.Now()
			f.Begin("/v1/messages", now)

			snap := rt.Stats.Snapshot(now)
			if len(snap.Pending) != 1 {
				t.Errorf("写进追踪器的请求没有出现在快照里（%d 个）："+
					"中间件写的和面板读的不是同一个实例，面板会永远是空的", len(snap.Pending))
			}
		})
	}
}
