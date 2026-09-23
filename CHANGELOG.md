# Changelog

## 0.1.0 — unreleased

First public release.

- Reverse proxy over CLIProxyAPI's SDK: one config file (19 keys),
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
- A preamble sent while the request body is still uploading (the 80s
  desperation case) no longer kills that upload. Go's HTTP server took the
  first response byte to mean the body was no longer wanted: with under 256KB
  left it threw the rest away and the request ended as an in-stream 502, after
  80–124s of uploading, and the client sent the whole 0.5–2MB body again — 20
  requests on 2026-09-23 ended this way. The server now keeps reading while the
  response streams (full duplex), and a body that does stop arriving after the
  preamble is recorded as an interrupted upload, not as an upstream failure.
- `claude-code-cache-ttl: "1h"` rewrites every prompt-cache breakpoint Claude
  Code sends to the one-hour lifetime, inside the engine's executor. The
  default 5 minutes are timed from the start of the request that last touched
  the prefix, generation included, so agentic turns longer than that came back
  to an evicted conversation and wrote all of it again at full price — 18 full
  rewrites of a ~300k-token prefix in one morning, measured 2026-09-16. Off by
  default (breakpoints go as written); `slimproxy init` generates `"1h"`.
- Claude Opus 5.5 (`claude-opus-5-5`) is served before the upstream model
  catalog lists it. Routing only admits catalog models, and the catalog is
  replaced wholesale by the remote copy at every start, so a released model the
  remote has not yet listed was a local 502 `unknown provider` — all thirteen
  Claude Code requests for it on 2026-09-23 before this change, none sent
  upstream. The engine now fills catalog gaps with pinned entries when the
  catalog is read; the remote entry takes over once published (it was, later
  the same day), and the pinned one still covers a start whose catalog fetch
  fails. The pinned thinking block is levels-only, so no positive budget
  reaches Opus 5.5 as `budget_tokens`, which it rejects.
- A connection setup that fails once no longer costs the request. Connection
  setup to Anthropic and chatgpt.com (dial, proxy CONNECT, TLS handshake) ran
  with no deadline and no retry, and with one credential nothing above it
  retries. On the Clash path a dead node surfaced as an EOF inside the handshake
  ~5s in, or the handshake hung ~60s — 82 and 47 times over 2026-09-21..23,
  every one before a response byte, each making Claude Code resend a 0.5–2MB
  body. Each setup attempt is now bounded at 15s and a failed one retried
  (three attempts, 1s and 3s apart), abandoned as soon as the client goes away.
  That rescues a transient failure, or one Clash routes around between
  attempts. It does not rescue a node that stays dead: if Clash keeps choosing
  it for every attempt (a `select` group pinned to it, a `url-test` or
  `fallback` group that has not re-probed yet), all three fail, and the EOF
  shape then fails the request after ~19s instead of ~5s (the hang after ~49s
  instead of ~60s). Nothing is retried once the request has been sent, so
  nothing can run or be billed twice. Each failed attempt logs its phase (dial
  / handshake / h2) and duration, the only record of where a stall sits.
  HTTP/2 connections to those hosts are also PING-checked after 30s without a
  frame, so one that dies mid-response fails within ~45s instead of hanging,
  and closed once they have carried no request for 90s, so the PINGs do not
  keep alive the connection every request leaves behind.
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
