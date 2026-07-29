package proxy

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Keeping the application log where it was put.
//
// logrus's standard logger is a process-wide singleton and CLIProxyAPI writes
// to it too -- including by taking it over. internal/api/server_reload.go calls
// logging.ConfigureLogOutput on a config reload, which ends in
// log.SetOutput(logWriter) pointing every subsequent line at logs/main.log: a
// third log file, in a different format, with none of the rotation limits
// setupLogging chose. From that moment `slimproxy > run.log` collects nothing,
// and nothing says why. That is the real cause of run.log dying mid-session on
// 07-26 with the process still healthy and still serving.
//
// setupLogging runs once, in Build. There is no reload hook to run it again, so
// before this the takeover was permanent.
//
// Reasserting rather than detecting-then-fixing, which would be the better
// design if it were available: logrus guards Logger.Out with a mutex on write
// and offers no reader, so comparing against it from this goroutine is a data
// race. An unconditional SetOutput is idempotent, cheap, and correct; the cost
// is that the takeover cannot be reported, only undone.
//
// The same reason keeps the loop from being clever about frequency. It cannot
// know whether anything happened, so it does the same small thing on every
// tick.

// pinnedLogOutput is where setupLogging last pointed logrus.
//
// Package-level state, and appropriately so: the thing being guarded is a
// process singleton, so "where should its output go" is a process-wide question
// with exactly one answer at a time. Threading it through Runtime would suggest
// a per-instance answer that cannot exist -- two Runtimes in one process share
// the logger whether they like it or not.
var pinnedLogOutput atomic.Pointer[io.Writer]

// setLogOutput points logrus at w and remembers it for the pin loop.
//
// Every SetOutput in this package goes through here. A direct call would set a
// destination the loop does not know about and would then overwrite within
// seconds -- worse than not guarding at all, because the log would appear to
// work and then move.
func setLogOutput(w io.Writer) {
	pinnedLogOutput.Store(&w)
	log.SetOutput(w)
}

// logPinInterval is how long the log can be pointed somewhere else.
//
// Lines written inside the window are not lost -- they land in the file
// CLIProxyAPI redirected to -- so this trades a bounded amount of misplacement
// against taking the logger's mutex, which every log line also needs. Five
// seconds keeps both small.
// A var rather than a const only so tests can shorten it; nothing in the
// program changes it.
var logPinInterval = 5 * time.Second

// keepLogOutputPinned restores the log destination on a timer.
func keepLogOutputPinned(ctx context.Context) {
	ticker := time.NewTicker(logPinInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if w := pinnedLogOutput.Load(); w != nil {
			log.SetOutput(*w)
		}
	}
}
