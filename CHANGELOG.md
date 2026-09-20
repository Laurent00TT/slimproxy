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
- A tunnel that fails to start now names the reason instead of dumping log.
  `tunnel up` printed one templated sentence and twelve raw lines of
  cloudflared's output, each truncated to panel width — which on 2026-09-20 cut
  away the `ip=198.18.0.12` field that was the entire answer. The startup path
  now classifies the region of the log this launch wrote: an edge address inside
  198.18.0.0/15 (a local proxy answering `*.argotunnel.com` DNS in fake-ip
  mode), a failing cloudflared pre-check named by component, a collapsed edge
  address pool, or a child still retrying rather than dead. When nothing
  matches, nothing is claimed and the log tail remains the fallback — a
  confident wrong diagnosis costs more than a raw dump. `doctor` gains
  `tunnel-edge-dns`, which reports the same condition before anything tries to
  start; the existing `upstream-dns` check only ever looked at
  `api.anthropic.com` and stayed silent through the whole incident.
- Upstream policy refusals surfaced instead of silently translated into empty
  completions (per-dialect; see README's protocol support matrix).
- Structured event journal with bounded retention (`journal-days`).
- Windows ACL hardening for credential and state files (`fsperm/`).
- Model catalog refresh (upstream's `models.json`, at start and every three
  hours) now runs in the served process — upstream's own binary starts it,
  its SDK never did — and dials through the same `proxy-url` (or the
  fallback relay) as the requests; upstream's fetcher ignored the proxy
  entirely. A model released after the build no longer waits for a rebuild.
