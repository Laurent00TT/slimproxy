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
		return cliproxylogging.NewFileRequestLoggerWithOptions(enabled, dir, filepath.Dir(configPath), maxFiles)
	}
}

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
