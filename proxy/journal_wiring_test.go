package proxy

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestJournalDoesNotRequireFileLogging.
//
// The journal used to live under the application log directory and inherit its
// enabling condition, so `log-to-file: false` -- an ordinary setting, and the
// one in the operator's own config -- silently produced no event stream. Panel
// mode forces file logging on, so it worked there and nowhere else; the
// resulting hole in the record is indistinguishable from a quiet period.
func TestJournalDoesNotRequireFileLogging(t *testing.T) {
	c := Config{Port: 8317, APIKeys: []string{"k"}, LogDir: t.TempDir(), LogToFile: false}

	dir, err := c.resolvedJournalDir()
	if err != nil {
		t.Fatalf("resolvedJournalDir: %v", err)
	}
	if dir == "" {
		t.Fatal("log-to-file 为 false 时事件日志被关掉了——这两件事无关")
	}
	if filepath.Base(dir) != JournalDirName {
		t.Errorf("目录 = %q，应以 %q 结尾", dir, JournalDirName)
	}
}

// TestJournalCanBeTurnedOffExplicitly: someone who does not want it has to say
// so, and an unset field means "on" because that is the useful default.
func TestJournalCanBeTurnedOffExplicitly(t *testing.T) {
	off := Config{LogDir: t.TempDir(), JournalDays: JournalDisabled}
	if dir, _ := off.resolvedJournalDir(); dir != "" {
		t.Errorf("journal-days=%d 应关闭事件日志，实际 %q", JournalDisabled, dir)
	}

	unset := Config{LogDir: t.TempDir()}
	if dir, _ := unset.resolvedJournalDir(); dir == "" {
		t.Error("未设置 journal-days 时应启用，未设置不等于「不要」")
	}
}

// TestJournalAndQueryAgreeOnTheDirectory: the writer and the reader must
// resolve the same path, or a query reports "no events" about a directory
// nothing writes to.
func TestJournalAndQueryAgreeOnTheDirectory(t *testing.T) {
	c := Config{LogDir: t.TempDir()}
	writerDir, err := c.resolvedJournalDir()
	if err != nil {
		t.Fatal(err)
	}
	readerDir, err := c.ResolvedJournalDir()
	if err != nil {
		t.Fatal(err)
	}
	if writerDir != readerDir {
		t.Errorf("写入端 %q 与查询端 %q 不一致", writerDir, readerDir)
	}
	if !strings.HasSuffix(readerDir, JournalDirName) {
		t.Errorf("查询端目录 %q 不对", readerDir)
	}
}
