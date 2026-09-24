# Contributing

## Build and test

```bash
go build ./cmd/slimproxy
go vet ./...
go test ./...
```

The test suite is hermetic: no network, no cloudflared binary, no credentials
required. If your change makes a test need any of those, that is a design
smell — the packages take function seams (`tunnel.Manager`'s `verify` /
`readConfig` / `connections`, for example) precisely so decisions are testable
without the world.

## What a change needs

- **A test that pins the behaviour**, written so it fails on the bug it
  guards against. This project's history includes assertions that stayed green
  under real bugs; since then the norm is mutation-checking new tests — inject
  the bug by hand, watch the assertion go red, revert.
- **Comments that state constraints, not narration.** The codebase's comment
  style explains *why the code must be this way* (what breaks otherwise),
  not what the next line does. Match it.
- **Honest failure reporting.** Unknown is never reported as healthy; a
  capability that does not work is not listed as if it did. If your change
  bounds or samples something, it says so where the operator can see it.

## Language

CLI output and most docs are Chinese; README, TUNNEL.md, and code comments are
English. Keep new user-facing strings consistent with the surface they appear
on. Translation contributions in either direction are welcome.

## Scope

The emulation layer is slimproxy's fork of CLIProxyAPI, under
`third_party/CLIProxyAPI`. A fix or enhancement that belongs there is made
there — not worked around from outside — and carries what keeps the next
upstream upgrade tractable:

- **An entry in `SLIMPROXY_PATCHES.md`**: what changed, why, with the
  measurement that motivated it, and when it can be dropped.
- **New code in a new `slimproxy_*.go` file** wherever it can be; edits to
  upstream files kept to the call site, each marked with a `slimproxy patch`
  comment so `grep` finds them all.
- **Guard tests wired into the root `go test ./...` through `forkcheck/`.**
  The fork is a nested module that the root's package pattern never enters;
  a guard `forkcheck` does not run is a guard nobody runs.

A change upstream would plausibly accept is still worth proposing there — every
patch it takes is one fewer to carry — but it is not a prerequisite.
