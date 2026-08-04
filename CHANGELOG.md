# Changelog

## 0.1.0 — unreleased

First public release.

- Reverse proxy over CLIProxyAPI's SDK: one config file (18 keys),
  fail-closed inbound auth, management surface pinned off.
- Streaming responses that go silent are severed after 90s and the caller gets
  an explicit timeout to retry against, instead of hanging until its own stall
  detector fires — measured at five to nine minutes per occurrence. On by
  default; `stream-idle-timeout` tunes or disables it.
- Streaming requests that produce nothing for 30s get their `200,
  text/event-stream` committed early with SSE keep-alive heartbeats, so a
  Cloudflare edge in front of the tunnel no longer severs queue-bound requests
  as 524s at its fixed ~100s header deadline. Failures after the preamble
  travel as in-stream SSE error events; a four-minute silence watchdog bounds
  the heartbeat. On by default; `stream-early-flush` tunes or disables it.
- CLI: `serve`, `check`, `init`, `status`, `doctor`, `auth`, `tunnel`,
  `routes`, `log`, `test`, `version`; full-screen terminal dashboard when run
  on a terminal.
- Cloudflare Tunnel lifecycle management (`tunnel up/down/status`) with
  identity-checked process control and connection-count verification.
- Upstream policy refusals surfaced instead of silently translated into empty
  completions (per-dialect; see README's protocol support matrix).
- Structured event journal with bounded retention (`journal-days`).
- Windows ACL hardening for credential and state files (`fsperm/`).
