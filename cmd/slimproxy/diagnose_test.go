package main

import (
	"errors"
	"testing"

	"github.com/Laurent00TT/slimproxy/diag"
)

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
