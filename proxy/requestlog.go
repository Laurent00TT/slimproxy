package proxy

import (
	"bytes"
	"encoding/json"
	"path/filepath"

	cliproxyconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	cliproxylogging "github.com/router-for-me/CLIProxyAPI/v7/sdk/logging"
	log "github.com/sirupsen/logrus"
)

// newRequestLoggerFactory builds the request logger CLIProxyAPI will use.
//
// It exists to stop -check from lying about where logs land. CLIProxyAPI's own
// factory calls ResolveLogDirectory, which prefers $WRITABLE_PATH and otherwise
// falls back from "logs" to <auth-dir>/logs depending on what happens to be
// writable at startup. That is three possible answers, none of them the one
// -check printed, so request logs reliably turned up somewhere other than where
// slimproxy said they would. Passing our own resolved directory makes the
// reported path the real one.
//
// findRefusal below is used by the translator hooks in refusal.go, not here: a
// logger sees the bytes too late to repair the response.
func newRequestLoggerFactory(c Config) func(*cliproxyconfig.Config, string) cliproxylogging.RequestLogger {
	return func(cfg *cliproxyconfig.Config, configPath string) cliproxylogging.RequestLogger {
		dir, err := c.resolvedRequestLogDir()
		if err != nil {
			// Refusing to start over a log path would be a poor trade. Fall back
			// to the upstream default and say so, rather than failing the proxy.
			log.Warnf("slimproxy: cannot resolve request-log-dir (%v); falling back to CLIProxyAPI's default location", err)
			dir = "logs"
		}

		maxFiles := 10
		if cfg != nil && cfg.ErrorLogsMaxFiles > 0 {
			maxFiles = cfg.ErrorLogsMaxFiles
		}

		enabled := cfg != nil && cfg.RequestLog
		inner := cliproxylogging.NewFileRequestLoggerWithOptions(enabled, dir, filepath.Dir(configPath), maxFiles)
		return &gatedRequestLogger{RequestLogger: inner, inner: inner}
	}
}

// gatedRequestLogger narrows what CLIProxyAPI can reach on the file logger so
// that "request-log: false" means nothing lands on disk.
//
// The problem it solves: disabling request logging did not disable writing. The
// upstream middleware turns a disabled logger into logOnErrorOnly, and the
// response writer then calls the logger with force=true on any 4xx or 5xx. A
// bare 401 was enough to put a caller's entire prompt on disk -- verbatim, in a
// directory nobody asked for. Two days of upstream trouble wrote 6.8MB of other
// people's conversations that way, while -check reported "关闭".
//
// Implementing RequestLogger ourselves is not an option: LogRequest names
// internal/interfaces.ErrorMessage, and internal/ cannot be imported from here.
// What works is making the force path unreachable. force only arrives through
// three optional interfaces the response writer discovers by type assertion
// (LogRequestWithOptions and its two ...AndSources variants); RequestLogger's
// own LogRequest hardcodes force=false. Embedding the *interface* rather than
// the concrete *FileRequestLogger promotes only the interface's methods, so
// those assertions fail and the writer falls back to LogRequest -- which
// respects enabled. The guarantee is a property of the method set, fixed at
// compile time, so unlike a setter it cannot be undone by a config reload.
//
// This wrapper is applied unconditionally, including when request logging is
// on. Gating only the disabled case would be enough today and would keep the
// spooling described below, but SetEnabled(false) arrives on config reload:
// a logger built while enabled would then be a bare one with the force path
// live again. A security property must not depend on what the config said at
// construction time.
//
// The cost is that NewFileBodySource disappears from the method set too, so
// with request logging on, oversized bodies accumulate in memory instead of
// spooling to a temp file. Upstream degrades cleanly here (it checks for a nil
// source and takes the in-memory path), and for a single-machine proxy the
// bodies are megabytes at worst. Paying that to close the leak is the trade.
//
// The setters are forwarded explicitly because upstream discovers them the same
// way -- by assertion -- and hiding them would break request-log hot reload,
// home-mode toggling, and error-log retention updates.
type gatedRequestLogger struct {
	cliproxylogging.RequestLogger
	inner *cliproxylogging.FileRequestLogger
}

func (g *gatedRequestLogger) SetEnabled(enabled bool)     { g.inner.SetEnabled(enabled) }
func (g *gatedRequestLogger) SetHomeEnabled(enabled bool) { g.inner.SetHomeEnabled(enabled) }
func (g *gatedRequestLogger) SetErrorLogsMaxFiles(n int)  { g.inner.SetErrorLogsMaxFiles(n) }

// refusalDetails is the part of an upstream refusal worth repeating.
type refusalDetails struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

// refusalMarker is cheap to test for and lets the JSON decode be skipped on the
// overwhelming majority of chunks, which carry ordinary content deltas.
var refusalMarker = []byte(`"refusal"`)

// findRefusal reports whether a raw upstream chunk carries a policy refusal.
//
// Chunks arrive as Server-Sent Events, sometimes several per write and
// sometimes with the `data: ` prefix already stripped, so this tolerates both
// rather than assuming a framing.
func findRefusal(chunk []byte) (refusalDetails, bool) {
	if !bytes.Contains(chunk, refusalMarker) {
		return refusalDetails{}, false
	}

	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		var event struct {
			Delta struct {
				StopReason  string         `json:"stop_reason"`
				StopDetails refusalDetails `json:"stop_details"`
			} `json:"delta"`
			// Non-streaming shapes put the same fields at the top level.
			StopReason  string         `json:"stop_reason"`
			StopDetails refusalDetails `json:"stop_details"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		if event.Delta.StopReason == "refusal" {
			return event.Delta.StopDetails, true
		}
		if event.StopReason == "refusal" {
			return event.StopDetails, true
		}
	}
	return refusalDetails{}, false
}
