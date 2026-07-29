package proxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Laurent00TT/slimproxy/fsperm"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

// setupLogging installs the logging configuration that CLIProxyAPI's own binary
// performs in main() and that sdk/cliproxy.Builder does not.
//
// Three knobs were dead before this existed:
//
//   - Debug never raised the logrus level, because util.SetLogLevel is only
//     called on the reload path. Every log.Debugf in the executors, conductor,
//     thinking pipeline and retry logic was discarded -- while gin still
//     printed its own [GIN-debug] route dump. The knob was inverted: noise up,
//     diagnostics zero.
//   - LogToFile did nothing, because logging.ConfigureLogOutput is only reached
//     from a change-detecting reload branch that a freshly started process never
//     takes.
//   - logrus stayed on stderr with the stock formatter, so redirecting stdout
//     captured nothing and gin wrote outside logrus entirely.
//
// Returns the resolved application-log directory ("" when logging to stdout) so
// the caller can report it instead of leaving the operator to guess, plus a
// closer for the file handle.
//
// The closer matters more than it looks: lumberjack holds the log file open,
// and on Windows an open handle blocks deletion of the containing directory.
// A long-lived process does not care, but anything that starts and stops the
// proxy repeatedly -- tests, an embedding host -- needs a way to let go.
// logOption adjusts where setupLogging sends output.
type logOption func(*logSettings)

type logSettings struct {
	// noStdout keeps application logs off stdout entirely.
	noStdout bool
}

// withoutStdout removes stdout from the log destinations.
//
// Exists for the full-screen dashboard, which owns the terminal: a logrus line
// written to stdout lands in the middle of the rendered frame and stays there
// until the next full repaint. There is no way to have both -- the alternate
// screen is a single writer surface -- so the caller that takes the terminal
// has to take the logs off it too.
//
// Callers must ensure logs have somewhere else to go; see setupLogging, which
// turns on file logging rather than letting this discard them.
func withoutStdout() logOption {
	return func(s *logSettings) { s.noStdout = true }
}

func setupLogging(c *Config, opts ...logOption) (string, io.Closer, error) {
	var set logSettings
	for _, o := range opts {
		o(&set)
	}
	// Taking stdout away without a file to replace it would silently discard
	// every log line the process produces. Turning file logging on is the only
	// outcome that keeps them, so it is done here rather than left to whoever
	// remembered to also set the flag.
	if set.noStdout && !c.LogToFile {
		c.LogToFile = true
	}

	if c.Debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetReportCaller(c.Debug)

	// gin writes through its own writers by default, which bypasses logrus
	// entirely -- level filtering, formatting and any downstream collector all
	// miss it. Route everything through the same logger.
	gin.DefaultWriter = log.StandardLogger().WriterLevel(log.InfoLevel)
	gin.DefaultErrorWriter = log.StandardLogger().WriterLevel(log.ErrorLevel)
	gin.DebugPrintFunc = func(format string, values ...any) {
		log.Debugf(format, values...)
	}

	if !c.LogToFile {
		// stdout, not stderr: `slimproxy > app.log` should capture the logs.
		// The default of stderr silently produced a near-empty file.
		setLogOutput(os.Stdout)
		return "", nopCloser{}, nil
	}

	dir, err := c.resolvedLogDir()
	if err != nil {
		return "", nopCloser{}, err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", nopCloser{}, err
	}
	// Application logs are not credential material, but they sit beside
	// stray-stdout.log and logs/requests/, and on Windows the 0700 above only
	// sets an attribute -- the directory would keep its parent's inherited
	// ACEs and pass them to everything created inside.
	if rerr := fsperm.Restrict(dir); rerr != nil {
		log.Warnf("slimproxy: 未能收紧日志目录 %s 的权限: %v", dir, rerr)
	}

	// Bounded by construction. CLIProxyAPI's own rotation keeps everything
	// (MaxBackups 0, MaxAge 0) and relies on a separate size-capped directory
	// cleaner that only starts from a code path slimproxy never reaches -- so
	// inheriting those defaults would mean an unbounded log directory.
	rotator := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "slimproxy.log"),
		MaxSize:    32, // MB per file
		MaxBackups: 5,
		MaxAge:     28, // days
		Compress:   true,
	}
	if set.noStdout {
		// The caller owns the terminal. File only.
		setLogOutput(rotator)
		// gin's writers were pointed at logrus above, so they follow. Its
		// debug print is the exception: DebugPrintFunc routes through logrus,
		// but gin also writes its startup banner directly to os.Stdout unless
		// it is in release mode.
		gin.SetMode(gin.ReleaseMode)
		return dir, rotator, nil
	}
	// Tee to stdout as well: a container that only collects stdout should not
	// go silent because file logging was enabled.
	setLogOutput(io.MultiWriter(os.Stdout, rotator))
	return dir, rotator, nil
}

// StrayStdoutName is the file that receives whatever writes to stdout while the
// dashboard owns the terminal.
const StrayStdoutName = "stray-stdout.log"

// TakeStdout redirects the process's stdout to a file and hands back the real
// one.
//
// Silencing logrus is not enough to free the terminal. CLIProxyAPI writes two
// lines with a bare fmt.Printf, bypassing every logger:
//
//   - sdk/cliproxy/service.go: "API server started successfully on: ..."
//     unconditionally, about 100ms after the listener binds
//   - internal/api/server.go: "server clients and configuration updated: ..."
//     on every watcher reload -- and the watcher covers the auth directory,
//     which the credential auto-refresh rewrites every 15 minutes
//
// Both land in the middle of a rendered frame and stay there, because
// bubbletea's renderer only repaints what it believes changed. A panel left
// running for an hour collects several.
//
// The destination is a separate file rather than the application log: lumberjack
// holds its own handle on that one, and two appenders on a single file are not
// guaranteed to interleave cleanly. Redirected rather than discarded, so a
// third party's output survives somewhere findable.
//
// The returned file is the original stdout, for whoever is drawing on the
// terminal. restore puts it back.
func TakeStdout(logDir string) (real *os.File, restore func() error, err error) {
	if strings.TrimSpace(logDir) == "" {
		return nil, nil, errors.New("未指定日志目录，无法接管 stdout")
	}
	if err = os.MkdirAll(logDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	path := filepath.Join(logDir, StrayStdoutName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 %s 失败: %w", path, err)
	}

	// Third-party stdout can carry anything, including material the writer did
	// not consider secret.
	if rerr := fsperm.Restrict(path); rerr != nil {
		log.Warnf("slimproxy: 未能收紧 %s 的权限: %v", path, rerr)
	}

	real = os.Stdout
	os.Stdout = f
	return real, func() error {
		os.Stdout = real
		return f.Close()
	}, nil
}

// nopCloser stands in when no file was opened, so callers never nil-check.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// AppLogTarget describes, in one line, where application logs actually land.
//
// Reported by -check and the dashboard because the previous answer was "stderr,
// silently, regardless of what log-to-file said". An operator should never have
// to read the source to find their logs.
func (c *Config) AppLogTarget() string {
	if !c.LogToFile {
		return "stdout（log-to-file 关闭）"
	}
	dir, err := c.resolvedLogDir()
	if err != nil {
		return "无法解析: " + err.Error()
	}
	return filepath.Join(dir, "slimproxy.log") + "（32MB × 5 轮转，压缩，同时输出到 stdout）"
}

// RequestLogTarget describes where request/response bodies land.
//
// "关闭" is meant literally, and that takes work to be true. Upstream still
// writes a full error dump on any 4xx or 5xx when request logging is off --
// which is how a bare 401 used to put a caller's whole prompt on disk while
// this function reported the feature disabled. gatedRequestLogger closes that
// path; TestRequestLogOffMeansNothingOnDisk asserts the wording and the empty
// disk together, so whoever loosens the gate finds out here rather than in an
// incident.
func (c *Config) RequestLogTarget() string {
	if !c.RequestLog {
		return "关闭"
	}
	dir, err := c.resolvedRequestLogDir()
	if err != nil {
		return "无法解析: " + err.Error()
	}
	return dir + "（未做总量上限；请求体与响应体逐字写入）"
}

// resolvedRequestLogDir returns where request/response body logs go.
func (c *Config) resolvedRequestLogDir() (string, error) {
	if strings.TrimSpace(c.RequestLogDir) != "" {
		expanded, err := expandHome(c.RequestLogDir)
		if err != nil {
			return "", err
		}
		return filepath.Abs(expanded)
	}
	base, err := c.resolvedLogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "requests"), nil
}

// ResolvedLogDir returns where application logs go.
//
// Exported because the journal lives beneath it and the query command has to
// find the same directory the proxy writes to -- re-deriving it there is how a
// listing ends up reporting "no events" about a directory nothing writes to.
func (c *Config) ResolvedLogDir() (string, error) { return c.resolvedLogDir() }

// resolvedLogDir returns where application logs go.
//
// Deliberately independent of CLIProxyAPI's ResolveLogDirectory, which consults
// WRITABLE_PATH / writable_path and returns that ahead of anything the config
// says. Honouring an ambient variable would mean -check reports one directory
// and logs land in another.
func (c *Config) resolvedLogDir() (string, error) {
	dir := c.LogDir
	if dir == "" {
		dir = "logs"
	}
	expanded, err := expandHome(dir)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}

// JournalDisabled is the journal-days value that turns the event stream off.
//
// A negative number rather than zero, because zero is what an unset field
// looks like and the useful default for an unset field is "on". Someone who
// does not want a journal has to say so.
const JournalDisabled = -1

// resolvedJournalDir returns where the event stream goes, or "" when disabled.
//
// Deliberately independent of LogToFile. The application log is prose for
// reading when something is on fire; the journal is structured history for
// answering questions later. Tying the second to the first meant an ordinary
// `log-to-file: false` silently produced no history -- and an absent record is
// indistinguishable from an uneventful one.
func (c *Config) ResolvedJournalDir() (string, error) { return c.resolvedJournalDir() }

func (c *Config) resolvedJournalDir() (string, error) {
	if c.JournalDays <= JournalDisabled {
		return "", nil
	}
	base, err := c.resolvedLogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, JournalDirName), nil
}
