# Changelog

## 0.1.0 — unreleased

First public release.

- Reverse proxy over CLIProxyAPI's SDK: one config file (18 keys),
  fail-closed inbound auth, management surface pinned off.
- Streaming responses that go silent are severed after 90s and the caller gets
  an explicit timeout to retry against, instead of hanging until its own stall
  detector fires — measured at five to nine minutes per occurrence. On by
  default; `stream-idle-timeout` tunes or disables it.
- CLI: `serve`, `check`, `init`, `status`, `doctor`, `auth`, `tunnel`,
  `routes`, `log`, `test`, `version`; full-screen terminal dashboard when run
  on a terminal.
- Cloudflare Tunnel lifecycle management (`tunnel up/down/status`) with
  identity-checked process control and connection-count verification.
- Upstream policy refusals surfaced instead of silently translated into empty
  completions (per-dialect; see README's protocol support matrix).
- Structured event journal with bounded retention (`journal-days`).
- Windows ACL hardening for credential and state files (`fsperm/`).
