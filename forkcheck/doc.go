// Package forkcheck wires the third_party SDK fork's regression tests into
// this module's ordinary `go test ./...`.
//
// It exists because of a property of nested Go modules that review measured
// the hard way: third_party/CLIProxyAPI carries its own go.mod, so every
// package pattern rooted in this module -- `go test ./...`, CI, and even the
// upgrade recipe's original `go test ./third_party/...` -- prunes the fork
// entirely. The command prints a warning, runs zero fork tests, and exits 0.
// The regression test that exists specifically to catch an upstream rebase
// losing the EnsurePublished patches was therefore reachable by no default
// test entry point: the guard tripped only if someone remembered to cd into
// the fork, which is the failure mode guards exist to remove.
//
// The tests here shell out to `go test` inside the fork (scoped to the patch
// regressions and Codex compatibility baseline -- the full upstream suite has timing-flaky cases on
// Windows), and pin the go.mod require version against the baseline named in
// SLIMPROXY_PATCHES.md, which nothing else enforces.
package forkcheck
