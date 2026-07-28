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

The emulation layer belongs to upstream CLIProxyAPI — slimproxy deliberately
uses it as shipped. Fixes to translation behaviour should go upstream;
slimproxy-side workarounds need a comment explaining why upstream is not the
right place.
