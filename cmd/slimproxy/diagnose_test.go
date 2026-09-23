package main

import (
	"errors"
	"testing"

	"github.com/Laurent00TT/slimproxy/diag"
	"github.com/Laurent00TT/slimproxy/proxy"
)

// TestTargetCarriesProxyURL: upstream-dns tells a proxied upstream from a
// direct one only by the proxy-url it is handed. Dropped here -- in the one
// function both `doctor` and the dashboard's /doctor build their target with --
// every proxied deployment on a fake-ip machine is back to a WARN.
func TestTargetCarriesProxyURL(t *testing.T) {
	// targetFor also reads ~/.cloudflared; an empty home keeps this test off
	// the developer's own tunnel config.
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	const raw = "http://127.0.0.1:7897"
	if got := targetFor(&proxy.Config{ProxyURL: raw}, "slimproxy.yaml", nil).ProxyURL; got != raw {
		t.Errorf("target.ProxyURL = %q, want %q", got, raw)
	}
}

// TestExitForReport pins doctor's machine-readable contract. CI gates on this
// exit code, so each mapping is stated explicitly rather than inferred.
func TestExitForReport(t *testing.T) {
	cases := []struct {
		name     string
		levels   []diag.Level
		wantExit bool
	}{
		{"all pass", []diag.Level{diag.Pass, diag.Pass}, false},
		{"a warning is not a failure", []diag.Level{diag.Pass, diag.Warn}, false},
		{"a failure", []diag.Level{diag.Pass, diag.Fail}, true},
		// The rule that makes this diagnostic trustworthy: something we could
		// not determine is not something we may call healthy.
		{"an unknown", []diag.Level{diag.Pass, diag.Unknown}, true},
		{"unknown outranks warn", []diag.Level{diag.Warn, diag.Unknown}, true},
	}

	for _, c := range cases {
		results := make([]diag.Result, 0, len(c.levels))
		for _, l := range c.levels {
			results = append(results, diag.Result{Level: l})
		}
		err := exitForReport(diag.Report{Results: results})

		if c.wantExit && err == nil {
			t.Errorf("%s: exited 0, want non-zero", c.name)
			continue
		}
		if !c.wantExit && err != nil {
			t.Errorf("%s: exited non-zero (%v), want 0", c.name, err)
			continue
		}
		if !c.wantExit {
			continue
		}
		// main distinguishes this from an ordinary error via errors.As; if that
		// stops working the report is followed by a blank error line.
		var silent *silentError
		if !errors.As(err, &silent) {
			t.Errorf("%s: error is not a silentError, main would print an empty line", c.name)
		} else if silent.code != 1 {
			t.Errorf("%s: exit code %d, want 1", c.name, silent.code)
		}
	}
}
