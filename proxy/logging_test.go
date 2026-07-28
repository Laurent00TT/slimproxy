package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestDebugRaisesLogLevel pins the fix for an inverted knob: `debug: true` used
// to leave logrus at Info (util.SetLogLevel is only called on the reload path)
// while gin still dumped its route table. Noise up, diagnostics zero -- and
// this is the knob an operator reaches for during an incident.
func TestDebugRaisesLogLevel(t *testing.T) {
	t.Cleanup(func() { log.SetLevel(log.InfoLevel); log.SetReportCaller(false) })

	if _, _, err := setupLogging(&Config{Debug: true}); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	if got := log.GetLevel(); got != log.DebugLevel {
		t.Errorf("debug:true left logrus at %s", got)
	}

	if _, _, err := setupLogging(&Config{Debug: false}); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	if got := log.GetLevel(); got != log.InfoLevel {
		t.Errorf("debug:false left logrus at %s", got)
	}
}

// TestLogToFileActuallyWrites pins the fix for a dead knob: log-to-file used to
// do nothing at all, because ConfigureLogOutput is only reached from a
// change-detecting reload branch a fresh process never takes.
func TestLogToFileActuallyWrites(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c := &Config{LogToFile: true, LogDir: dir}
	resolved, closer, err := setupLogging(c)
	if err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	// Release the handle before TempDir cleanup: on Windows an open log file
	// blocks removal of its directory.
	t.Cleanup(func() { log.SetOutput(os.Stderr); _ = closer.Close() })
	if resolved != dir {
		abs, _ := filepath.Abs(dir)
		if resolved != abs {
			t.Errorf("resolved dir %s, want %s", resolved, dir)
		}
	}

	log.Info("regression-marker")

	body, err := os.ReadFile(filepath.Join(resolved, "slimproxy.log"))
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	if !strings.Contains(string(body), "regression-marker") {
		t.Errorf("log file exists but does not contain the line: %q", body)
	}
}

// TestLogTargetsAreReportable: the operator-facing strings must name a real
// path, because the previous behaviour was "stderr, silently, regardless".
func TestLogTargetsAreReportable(t *testing.T) {
	off := &Config{}
	if !strings.Contains(off.AppLogTarget(), "stdout") {
		t.Errorf("app log target should say stdout when log-to-file is off, got %q", off.AppLogTarget())
	}
	if off.RequestLogTarget() == "" {
		t.Error("request log target must never be empty")
	}

	on := &Config{LogToFile: true, LogDir: t.TempDir(), RequestLog: true}
	if !strings.Contains(on.AppLogTarget(), "slimproxy.log") {
		t.Errorf("app log target should name the file, got %q", on.AppLogTarget())
	}
	if !filepath.IsAbs(strings.Fields(on.RequestLogTarget())[0]) {
		t.Errorf("request log target should start with an absolute path, got %q", on.RequestLogTarget())
	}
}
