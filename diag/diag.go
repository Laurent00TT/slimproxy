// Package diag inspects a slimproxy deployment from the outside.
//
// Everything here observes: it opens sockets, reads files, and runs external
// commands, but never mutates state. That constraint is what lets `status` and
// `doctor` share it, and what makes it safe to call on a repeating timer from a
// TUI.
//
// A deliberate limitation: this runs in a different process from the proxy it
// inspects, so in-process telemetry (requests/min, time-to-first-token) is not
// reachable. Reporting those would require inventing numbers. What is reported
// is only what a separate process can actually establish.
package diag

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// Level classifies one check's outcome.
type Level int

const (
	// Pass means the check ran and found nothing wrong.
	Pass Level = iota
	// Warn means the check found something that works but will bite later.
	Warn
	// Fail means the check found something broken.
	Fail
	// Unknown means the check could not run to a conclusion -- a timeout, a
	// missing external tool, no permission.
	//
	// This exists as a distinct level rather than collapsing into Pass because
	// a diagnostic that reports untested things as healthy is worse than no
	// diagnostic: it actively directs the operator away from the problem.
	Unknown
)

func (l Level) String() string {
	switch l {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return "UNKNOWN"
	}
}

// Result is one check's finding.
type Result struct {
	// Name identifies the check, stable across runs.
	Name string
	// Level is the outcome.
	Level Level
	// Detail states what was observed, in one line.
	Detail string
	// Remedy tells the operator what to do. Required for Warn and Fail;
	// for Unknown it should say how to make the check possible.
	Remedy string
	// Err is the underlying error, if the check could not complete.
	Err error
	// Took is how long the check ran, useful when one dominates the wall clock.
	Took time.Duration
}

func (r Result) String() string {
	s := fmt.Sprintf("%-8s %-24s %s", r.Level, r.Name, r.Detail)
	if r.Remedy != "" {
		s += "\n" + fmt.Sprintf("%-8s %-24s → %s", "", "", r.Remedy)
	}
	return s
}

// Check is one diagnostic.
//
// A Check must respect ctx and must return Unknown rather than Pass when it
// cannot reach a conclusion. It must not panic; run recovers, but a panicking
// check has already lost the information it was supposed to report.
type Check struct {
	Name string
	// Timeout bounds this check. Zero means DefaultCheckTimeout.
	Timeout time.Duration
	Run     func(ctx context.Context) Result
}

// DefaultCheckTimeout bounds a check that does not set its own.
//
// Chosen to be shorter than a human's patience for a diagnostic command but
// long enough for a TLS handshake over a slow link.
const DefaultCheckTimeout = 8 * time.Second

// run executes one check with its timeout applied, converting a panic or an
// overrun into Unknown rather than losing the result.
func (c Check) run(ctx context.Context) (res Result) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	started := time.Now()

	defer func() {
		if r := recover(); r != nil {
			res = Result{
				Name:   c.Name,
				Level:  Unknown,
				Detail: fmt.Sprintf(i18n.T("检查过程 panic: %v", "a check panicked: %v"), r),
				Remedy: i18n.T("这是 slimproxy 的缺陷，请报告", "this is a slimproxy defect; please report it"),
			}
		}
		res.Name = c.Name
		res.Took = time.Since(started)
	}()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan Result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- Result{
					Level:  Unknown,
					Detail: fmt.Sprintf(i18n.T("检查过程 panic: %v", "a check panicked: %v"), r),
					Remedy: i18n.T("这是 slimproxy 的缺陷，请报告", "this is a slimproxy defect; please report it"),
				}
			}
		}()
		done <- c.Run(cctx)
	}()

	select {
	case r := <-done:
		return r
	case <-cctx.Done():
		// The goroutine may still be blocked; it writes to a buffered channel
		// so it will not leak on eventual completion.
		return Result{
			Level:  Unknown,
			Detail: fmt.Sprintf(i18n.T("超时（%s）未得出结论", "timed out (%s) without a conclusion"), timeout),
			Remedy: i18n.T("重试；若持续超时说明被检查的对象本身无响应", "retry; persistent timeouts mean the thing being checked is itself unresponsive"),
			Err:    cctx.Err(),
		}
	}
}

// ShowRemedy reports whether this result's remedy is worth printing.
//
// A remedy under a passing check is noise -- there is nothing to fix. Exported
// because both the CLI and the dashboard render reports, with different
// layouts but necessarily the same judgement: the dashboard once printed
// remedies for passing checks because it reimplemented the loop without this.
func (r Result) ShowRemedy() bool {
	return r.Remedy != "" && r.Level != Pass
}

// ExtraErr returns the error text worth printing beside Detail, empty when
// there is none or when Detail already carries it.
//
// Some checks put the cause into Detail and some cannot, so the error is shown
// only when it adds something. Printing it unconditionally repeats the same
// sentence twice on the checks that did.
func (r Result) ExtraErr() string {
	if r.Err == nil {
		return ""
	}
	if msg := r.Err.Error(); !strings.Contains(r.Detail, msg) {
		return msg
	}
	return ""
}

// LevelsBySeverity lists the levels most severe first.
//
// The single source of the ordering, shared with Worst. Every consumer that
// tallies a report needs it, and each one that wrote out its own drifted:
// the CLI summary listed Unknown last, behind Fail, which reads as "least
// important" for the level that exists precisely because an unanswered
// question about a proxy holding live credentials is not benign.
func LevelsBySeverity() []Level { return []Level{Fail, Unknown, Warn, Pass} }

// Report is the outcome of a full run.
type Report struct {
	Results []Result
	Took    time.Duration
}

// Worst returns the most severe level present, treating Unknown as more severe
// than Warn: an unanswered question about a proxy holding live credentials
// deserves more attention than a known-benign warning.
func (r Report) Worst() Level {
	// Rank derived from LevelsBySeverity rather than written out again, so the
	// severity order has exactly one definition.
	rank := map[Level]int{}
	levels := LevelsBySeverity()
	for i, l := range levels {
		rank[l] = len(levels) - i
	}
	worst := Pass
	for _, res := range r.Results {
		if rank[res.Level] > rank[worst] {
			worst = res.Level
		}
	}
	return worst
}

// Counts tallies results by level.
func (r Report) Counts() map[Level]int {
	out := map[Level]int{}
	for _, res := range r.Results {
		out[res.Level]++
	}
	return out
}

// Run executes checks sequentially and collects the report.
//
// Sequential rather than concurrent: several checks contend for the same
// external resources (DNS, the cloudflared binary, the upstream endpoint), and
// a diagnostic that reports timeouts caused by its own parallelism would be
// diagnosing itself.
func Run(ctx context.Context, checks []Check) Report {
	started := time.Now()
	out := Report{Results: make([]Result, 0, len(checks))}
	for _, c := range checks {
		out.Results = append(out.Results, c.run(ctx))
		if ctx.Err() != nil {
			// The caller gave up. Report what ran rather than pretending the
			// rest passed.
			for _, remaining := range checks[len(out.Results):] {
				out.Results = append(out.Results, Result{
					Name:   remaining.Name,
					Level:  Unknown,
					Detail: i18n.T("已取消，未执行", "canceled before running"),
					Remedy: i18n.T("重新运行 doctor", "run doctor again"),
				})
			}
			break
		}
	}
	out.Took = time.Since(started)
	return out
}
