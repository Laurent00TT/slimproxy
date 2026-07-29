package proxy

import (
	"os"
	"path/filepath"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestHealthTrackingDoesNotDependOnJournalling pins where the tracker is
// constructed, which is the whole substance of the fix.
//
// Everything that watched requests used to be registered inside the journal
// branch, so turning off the event stream also turned off any chance of being
// told about an outage -- two unrelated decisions wired to one switch. The
// journal's own directory resolution was changed once already for exactly this
// reason; putting the alarm back in there would be the same mistake a second
// time.
func TestHealthTrackingDoesNotDependOnJournalling(t *testing.T) {
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
			if rt.Health == nil {
				t.Error("没有健康跟踪：连续失败将像那两天一样悄无声息")
			}
			if tc.days == JournalDisabled && rt.Journal != nil {
				t.Fatal("测试没有真的关掉 journal，这一轮没有验证到任何东西")
			}
		})
	}
}
