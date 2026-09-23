# slimproxy

**English** | [中文](README.zh-CN.md)

**Use one LLM subscription from every tool that speaks a different API.**

Point an OpenAI-compatible editor plugin, a Gemini-format script, and Claude Code
at the same local port, and they all reach the same upstream account. slimproxy
runs on `127.0.0.1`, translates between the dialects, and shows you what is
actually happening while it does.

```
┌─ slimproxy 0.1.0 ──────────────────────────────────────────────────┐
│ listen 127.0.0.1:8317  up 4h12m  rpm 17  ttft 842ms  quota 21% ↗   │
├────────────────────────────────────────────────────────────────────┤
│ tunnel  proxy.example.com         ● 2 connections                  │
│ creds   claude · user@example.com ● 7h24m until expiry             │
│ doctor  PASS 4 · WARN 1           ▲ 3m12s ago                      │
├─ active routes ────────────────────────────────────────────────────┤
│ openai → claude                              41    812ms      68%  │
├─ recent requests · 1 in flight ────────────────────────────────────┤
│ 12:41:12 ▸    /v1/messages                        8.4s          —  │
│ 12:41:07 ok   openai → claude   opus-5           812ms       1.9k  │
└────────────────────────────────────────────────────────────────────┘
   tunnel down    stop the cloudflared this process started
     2 connections now · after stopping, proxy.example.com becomes unreachable
 › /tunnel d
```

The protocol emulation is [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)'s,
used exactly as it ships. What slimproxy adds is everything around it: a
17-key config instead of ~200, fail-closed auth, a terminal dashboard, tunnel
lifecycle management, and diagnostics that refuse to call an unanswered
question healthy.

## Before you deploy this

> **Know what you are doing.** The typical setup routes API-shaped traffic
> through a consumer subscription's OAuth credential — most providers' terms of
> service restrict their subscriptions to their own official clients, and
> exposing yours through any proxy risks account suspension. This project does
> not endorse that use; it assumes you have read your provider's terms and made
> your own call. Never share a deployment with anyone you would not hand the
> underlying account to.
>
> **Not affiliated** with Anthropic, OpenAI, Google, Cloudflare, or the
> CLIProxyAPI project.
>
> **Platform status**: developed and operated on Windows. Linux and macOS
> compile and pass the test suite, but have not been run in production by the
> author.

## Quick start

Requires Go (version in [go.mod](go.mod)). No other setup — dependencies come
from the public module proxy.

```bash
go build -o slimproxy ./cmd/slimproxy
./slimproxy init
./slimproxy auth add claude
./slimproxy
```

`init` writes `slimproxy.yaml` with a generated API key, creates the auth
directory, and prints what to do next. `auth add` runs the provider's OAuth flow
and stores the credential where the proxy will load it from. Running with no
arguments serves — and, on a terminal, opens the dashboard above.

Point your client at `http://127.0.0.1:8317/v1` with the generated key as the
bearer token, and you are done.

The interface speaks Chinese and English: it follows the system locale by
default, and a `lang: zh` / `lang: en` line in the config pins it.

## Commands

```bash
./slimproxy check      # validate config, print what would run, exit
./slimproxy status     # report observable state, always exit 0 (for scripts)
./slimproxy doctor     # diagnose deployment problems, exit non-zero if any
./slimproxy auth list  # the credential pool and each entry's state
./slimproxy tunnel status
./slimproxy routes     # which client<->provider dialect routes this build serves
./slimproxy log        # query the structured event journal
./slimproxy test       # send one real request end to end (spends upstream quota)
./slimproxy version    # version and build provenance
```

The original flag spellings (`-check`, `-init`, `-routes`) still work and reach
the same command functions.

## Protocol support

What has actually been exercised end to end against a real Claude backend
(2026-07), versus what merely exists in the route table. "Works" means a real
request went through and the response shape was checked — nothing more.

| Client dialect | Endpoint | Non-stream | Stream | Tools | Notes |
|---|---|---|---|---|---|
| Anthropic | `/v1/messages`, `/v1/messages/count_tokens` | ✅ | ✅ | ✅ | Primary path (Claude Code). |
| OpenAI chat | `/v1/chat/completions` | ✅ | ✅ | ✅ incl. tool-result round-trip | Response `id` keeps the upstream `msg_…` form instead of `chatcmpl-…`. |
| OpenAI Responses | `/v1/responses` | ✅ structured `input` | ✅ | untested | **String-form `input` fails** (upstream translator bug): translated to empty `messages`, error returned in Anthropic shape. Codex CLI uses the structured form and works. |
| Gemini | `:generateContent`, `:streamGenerateContent` | ✅ | ✅ | ✅ | **`:countTokens` fails** (upstream translator bug): builds a count request with `max_tokens`, which Anthropic rejects. |
| interactions | — | untested | untested | untested | No client on hand speaks it. |

Both failures above are translation bugs in upstream CLIProxyAPI, observable
through any deployment of it, not just this one.

Run `slimproxy routes` for the full matrix your build serves: 30 client→provider
routes are reachable with request + streaming response transforms; token counts
are expressible on only 9 of them.

### Known behaviours to plan around

- **`/v1/models` is a static registry, not a capability probe.** It advertises
  models the upstream registry knows about; your credential may still refuse
  some of them (a subscription credential 404s on models outside its plan).
  The registry also gates serving: a model absent from it is refused even if
  your credential could serve it.
- **The upstream injects the Claude Code system prompt** (~1.4k tokens) into
  every request as part of client emulation. Non-Claude-Code clients will see
  the assistant identify as "Claude Code" and count those tokens in usage.
- **Refusal marking varies by dialect.** Anthropic clients receive the native
  `stop_reason:"refusal"` untouched; OpenAI chat clients get
  `finish_reason:"content_filter"`; every other dialect currently receives an
  empty completion plus a WARN in the log (see [proxy/refusal.go](proxy/refusal.go)
  for why).

## Security posture

| Request | Result |
|---|---|
| `GET /healthz` | 200 |
| `GET /v1/models` no key | 401 |
| `GET /v1/models` wrong key | 401 |
| `GET /v1/models` correct key | 200 |
| `GET /v0/management/config` | 404 |
| `GET /management.html` | 404 |
| config with empty `api-keys` | refuses to start |
| config with a typo'd key | refuses to start, names the key |

Inbound auth is fail-*closed*: CLIProxyAPI's own middleware accepts every
request when no key is configured, so slimproxy refuses to start rather than
inherit that. `MANAGEMENT_PASSWORD` in the environment also makes it refuse —
that variable alone enables CLIProxyAPI's full management API
(`internal/api/server.go:400-404`) and cannot be suppressed from outside the
module, so refusing is the only alternative to lying about the posture.

Found a security issue? See [SECURITY.md](SECURITY.md).

## Documentation

| | |
|---|---|
| [docs/GUIDE.md](docs/GUIDE.md) | User guide (Chinese). From zero: when to use each command, how to read the output, what to check when something breaks. |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Architecture deep-dive (Chinese). The constraints behind each design decision, and what breaks if you change it. |
| [docs/MIGRATION.md](docs/MIGRATION.md) | Moving to another machine (Chinese). Clone, build, re-authorise; what to carry over and what must not be. |
| [deploy/TUNNEL.md](deploy/TUNNEL.md) | One-time Cloudflare Tunnel setup (English). Exposes the local port to the internet. |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Build, test, and what a change needs to carry. |

The CLI itself is bilingual — see `lang` above. The long-form guides are
Chinese-first because the tool's first audience was the author's circle;
contributions translating either direction are welcome.

## Package map

| | |
|---|---|
| `proxy/` | Minimal reverse proxy over `sdk/cliproxy.Builder`. Everything not needed is pinned off. |
| `tui/` | Full-screen terminal dashboard (bubbletea + lipgloss). Presentation only. |
| `diag/` | Deployment diagnostics. Never reports an unanswered question as healthy. |
| `tunnel/` | Operates the cloudflared child process: three-state status, start, stop. |
| `credentials/` | Inspects and manages the upstream OAuth credential pool. |
| `metrics/` | Request telemetry — completed and in-flight — read in-process by the dashboard. |
| `journal/` | Structured event stream: one JSON object per request and per state change. |
| `i18n/` | Interface language selection. Inline bilingual strings, guarded by an AST lint test. |
| `translate/` | Typed, fail-loud facade over `sdk/translator`. Usable on its own — 14 indirect dependencies. **Not on the serving path**: CLIProxyAPI's executors call `sdktranslator` directly, so the guard for that path lives in `proxy/untranslated.go`. |
| `fsperm/` | Restricts sensitive paths to the current user. On Windows the permission bits are not access control. |
| `cmd/slimproxy` | CLI: `serve`, `check`, `init`, `status`, `doctor`, `auth`, `tunnel`, `routes`, `log`, `test`, `version`, `help`. |

---

The rest of this file is reference material: what "slim" is and is not, how the
dashboard behaves, the translator facade's contract, and configuration notes
worth knowing before something surprises you.

## What "slim" does and does not mean

**Does**: the configuration and runtime surface. One config file with 17 keys instead of
~200. No management API, no control panel, no plugin host, no pprof, no Redis usage queue.
Unknown config keys are errors, not silently ignored. Inbound auth is fail-*closed*.

**Does not**: the dependency tree or the binary size. The provider executors — which carry
the upstream client emulation — live in CLIProxyAPI's `internal/` tree. Go's
internal-package rule makes `sdk/cliproxy.Builder` the only way for another module to reach
them, and building a `Service` links gin, pion/webrtc, redis and lumberjack whether or not
those subsystems run. Genuinely shrinking that requires forking CLIProxyAPI, which trades
the dependency tree for permanent merge burden on the emulation layer.

**The emulation layer is used exactly as CLIProxyAPI ships it.** Nothing in this module
modifies, extends, or hardens it.

## Dashboard

`slimproxy` with no arguments serves *and* draws the panel, in one process.
There is no HTTP surface behind it: the panel reads the collector in memory and
calls the same functions the subcommands do.

The header carries the two numbers that decide anything on a subscription: the
five-hour quota window (with a direction arrow) and the prompt-cache hit rate —
the one metric that is invisible when it breaks, because replies stay normal
while quota burns at full price.

Press `/` for commands: `monitor`, `tunnel status|up|down`, `auth list|rm`,
`doctor`, `routes`, `quit`. The panel above the prompt never reflows — the
completion list grows downward from outside the frame, so the state you are
reading stays put while you type.

**Destructive commands state the cost before you press Enter.** `tunnel down`
does not say "stop the tunnel", it says how many edge connections are live and
which hostnames stop answering. Then it asks y/n.

### What it deliberately does not do

- **`auth add` is not implemented in the panel.** The OAuth flows open a browser
  and wait for a pasted code; driving that from inside an alternate screen means
  every failure mode — no browser, wrong account signed in, expired code —
  surfaces as a frozen dashboard. The command exists and points at
  `slimproxy auth add <provider>`.
- **`auth rm` never passes `-force`.** The guard against deleting the last
  loadable credential cannot be bypassed from a panel with no way to type a
  flag. The CLI command is where an operator has to write it by hand.
- **No config editing.** `api-keys` is live-reloadable, and emptying it
  unregisters the only inbound auth provider — after which `AuthMiddleware`
  calls `c.Next()` for every request, turning a process holding live OAuth
  subscriptions into an open relay. `init` covers the actual need.
- **The doctor row is the last `doctor` verdict, not a live upstream probe.** A
  real probe costs a DNS lookup and a TLS handshake per refresh. Showing a
  timestamped real answer beats inventing a fresh-looking one.

### Other things to know

- **In-flight requests are rows, not just a count.** A completed request's
  telemetry comes from the SDK's usage record, which does not exist until the
  upstream answers — so a middleware tracks arrivals separately, and a running
  request shows as a `▸` row whose elapsed time counts up. When the upstream
  answers, the row becomes a normal one in place.
- **Redirected output falls back to line logging.** `slimproxy > log 2>&1` under
  a service manager gets the original behaviour; the panel only starts when both
  stdin and stdout are terminals. `-no-tui` forces it off on a terminal.
- **The panel takes stdout, so logs move to a file.** An alternate screen is a
  single writer surface and a logrus line lands in the middle of a frame. Panel
  mode turns on file logging if it was off, rather than discarding the lines —
  `slimproxy check` reports where they go.

## Generated state

On start, the resolved CLIProxyAPI configuration is written to
`<state-dir>/slimproxy.<port>.effective.yaml`. It contains secrets, and is restricted to
the current user — see `fsperm/` for why the mode argument alone does not achieve that on
Windows. The name carries the port because the state directory defaults to the working
directory: with a fixed name, a second instance started in the same folder overwrites the
file the first is serving from, and that file is hot-reloaded. This file is not
optional bookkeeping: `Builder.Build()` requires a config path that exists, the file watcher
re-reads that path on every change, and credential loading happens through the watcher's
first pass. The file is regenerated on every start — edit `slimproxy.yaml` instead.

## The `translate` package

`sdk/translator` is powerful but has three sharp edges. This facade removes them.

**1. Probe and translate take opposite argument orders.** Registration stores
`responses[client][provider]`. The `Has*ResponseTransformer` probes read
`responses[from][to]`, so they take `(client, provider)`. But `TranslateStream`,
`TranslateNonStream` and `TranslateTokenCount` read `responses[to][from]`, so they take
`(provider, client)`.

This is worse than a lookup miss. Because most routes have a registered reverse —
`openai→claude` and `claude→openai` are both real — the wrong order does not fall back to
passthrough. It resolves to the *reverse route's transform* and silently applies it.
`internal/pluginhost/adapters.go:1453,1473` compensates by hand-flipping at the call site.

This package takes `(Client, Provider)` everywhere and flips internally.
`TestProbeAndTranslateTakeOppositeOrders` pins the asymmetry, so if upstream ever
normalizes it the test fails rather than the behaviour silently changing.

**2. Streaming state is an unchecked `*any`.** Transforms do `(*param).(*Params)` without
comma-ok, so reusing one `*any` across two pairs panics inside the transform. A `Stream` is
bound to one `Translator` at construction and owns its state privately.

**3. A missing registration is not an error.** The registry echoes the raw body, so a
half-wired provider forwards untranslated bytes upstream with nothing logged. `New` returns
`ErrNoTransformer` unless passthrough is opted into explicitly — which is legitimate for
identity routes like `claude→claude`, which the executors short-circuit rather than
translate.

```go
tr, err := translate.New(translate.Pair{
    Client:   translate.OpenAI,
    Provider: translate.Claude,
}, translate.Options{})

upstreamBody, err := tr.Request(model, clientBody, true)

stream, err := tr.NewStream(model, clientBody, upstreamBody)
defer stream.Finish()
err = stream.ReadFrom(ctx, resp.Body, func(frame []byte) error {
    _, err := fmt.Fprintf(w, "data: %s\n\n", frame)
    return err
})
```

Two things the facade documents but cannot fix, because they are the transforms' own
contract:

- `Line` takes one **raw line**, not one SSE event. Transforms recover the event name from
  the payload's `"type"` field and drop lines lacking a `data: ` prefix.
- `Finish` does **not** emit a terminal sentinel. Claude upstreams never synthesize
  `[DONE]` while Gemini/Antigravity/Kimi do, and what the client expects varies by dialect
  (`data: [DONE]`, a bare newline, or nothing). Emit it in the layer that owns the HTTP
  response.

## Configuration notes worth knowing

- **`request-retry` is not a failure-retry count — and with one credential it retries
  nothing.** The outer loop only re-runs a request while some credential is cooling down
  with a recovery deadline inside `max-retry-interval` (or a 429 carries a compatible
  `retry-after`). 529 and dial failures never enter cooldown, and the transient-5xx
  cooldown default (60s) exceeds the suggested 30s interval, so a single-credential
  deployment attempts every failure class exactly once regardless of the value —
  measured against v7.2.103, pinned by the `TestUpstreamRetry_*` characterization
  tests. At 0 the loop is skipped entirely; CLIProxyAPI ships no default for it.
  The one retry that does happen sits a layer below that loop: a failed connection
  *setup* to Anthropic or chatgpt.com — dial, proxy CONNECT, TLS handshake, before a
  byte of the request is sent — is retried inside the engine, each attempt bounded at
  15s, three attempts in all with 1s and 3s pauses, abandoned as soon as the client
  goes away. A resend there cannot run or bill the request twice; once the request is
  on the wire, a failure is still attempted once. The retry helps with a transient
  failure, or one Clash routes around between attempts; a node that stays dead, and that
  Clash keeps choosing, fails all three — the EOF shape then after ~19s rather than ~5s,
  a hang after ~49s rather than ~60s. Each failed setup is logged as
  `utls: connection setup to … failed in <dial|handshake|h2> phase on attempt n/3` —
  the journal keeps only the cause, so that line is where a stall's location shows.
- **`stream-idle-timeout` cuts dead streams loose.** A streaming response that sends nothing
  for the window (90s by default) is severed and the client gets an explicit timeout to retry
  against. It exists because a stream can die silently mid-flight — no FIN, no RST, no error —
  and neither upstream nor this proxy had a read deadline, so the client waited out its own
  stall detector: five to nine minutes per occurrence, measured 2026-08-01. Healthy streams are
  never quiet that long (the upstream emits SSE pings through thinking pauses). `0` selects the
  default; a negative value turns the guard off. Beneath it, the engine PINGs an HTTP/2
  connection to Anthropic or chatgpt.com after 30s without a frame and drops it if no ack
  comes within 30s, so a connection that is actually dead fails within ~60s of its last frame
  even with the guard off. Each time that check drops a connection still carrying a request, it
  logs a warning (`utls: health check closed the connection …`) with the request's ID, so how
  often it cuts requests short can be counted. What only the guard catches is an upstream that
  stays connected but stops sending — with the guard off, that still hangs indefinitely. Those
  PINGs go out on an idle connection too, and every request leaves one behind (each builds its
  own client, so none is reused), so the engine also closes a connection once it has carried no
  request for 90s; without that, the PINGs would keep every one of them open through Clash and
  the node for good.
- **`stream-early-flush` buys streaming requests out of Cloudflare's 100s guillotine.** Behind
  a Cloudflare tunnel, an origin that shows no response headers within ~100 seconds is severed
  as a 524 (the limit is fixed on free/Pro plans) — and the upstream handler writes nothing
  until the first chunk arrives, so requests queue-bound behind tunnel bandwidth were killed
  while their execution would have succeeded (129 crossed 100s on 2026-08-03 alone). Past the
  threshold (30s by default) the proxy commits `200, text/event-stream` early and covers the
  silence with SSE keep-alive comments; a silence past four minutes is ended by a watchdog.
  The trade is confined to slow requests: one that fails after the preamble delivers its error
  as an in-stream SSE event instead of an HTTP status (and loses `Retry-After`). Requests
  answered inside the threshold — nearly all of them — are byte-for-byte untouched. `0`
  selects the default; a negative value turns the preamble off.
- **`claude-code-cache-ttl: "1h"` stops long turns from rewriting the whole conversation.**
  Anthropic's prompt cache lives 5 minutes by default, timed from the *start* of the request
  that last touched the prefix — generation time counts — so an agentic turn that runs past
  four minutes or so comes back to find the whole conversation evicted and writes it again at
  full price (18 full rewrites of a ~300k-token prefix in one morning, measured 2026-09-16).
  With `"1h"` the proxy rewrites every breakpoint Claude Code sends to the one-hour lifetime:
  every one, not only those missing a ttl, because Anthropic rejects a 1h breakpoint placed
  after a 5m one and the engine flattens such a mix back to 5m. The trade is a 1h write at
  2x the base input rate against 1.25x for 5m, so a client that never returns within five
  minutes pays the premium for nothing — hence empty by default (breakpoints go as written)
  and Claude Code only; `slimproxy init` generates `"1h"`. Applied inside the engine's
  executor via a fork patch, see `third_party/CLIProxyAPI/SLIMPROXY_PATCHES.md`.
- **`request-log` writes bodies verbatim.** Redaction is header- and query-name based, and
  the name has to contain `authorization`, `api-key`, `apikey`, `token` or `secret` to
  match — `Cookie` does not, and is written in the clear. Any credential carried in a
  request or response body lands in the log unredacted.
- **`request-log: false` genuinely writes nothing.** Upstream forces a full dump on any 4xx
  or 5xx while request logging is disabled, so an unauthenticated request was enough to put
  a caller's whole prompt on disk. slimproxy closes that path by narrowing the logger's
  method set; see `gatedRequestLogger` in `proxy/requestlog.go`.
- **Shutdown is not graceful after 30s uptime.** CLIProxyAPI establishes its shutdown
  deadline at startup rather than at signal time, so a long-lived process cuts in-flight
  streams instead of draining them.

See [slimproxy.example.yaml](slimproxy.example.yaml) for the full key set with
comments.

## Open decision

`proxy.Config.AllowModel` currently does exact matching on `models:`, which is safe but
brittle — upstream names carry dated suffixes and this proxy also sees names with a thinking
suffix appended (`gpt-5.5(high)`). Prefix matching, suffix-stripping, and globs each trade
convenience against over-exposure. It is a real access-control boundary, so the policy is
left marked `TODO(you)` in `proxy/config.go` rather than guessed.

## Building against upstream

`go.mod` depends on a published CLIProxyAPI release from the public module
proxy — clone and `go build ./cmd/slimproxy`, nothing else required.
`slimproxy version` prints the exact upstream version a binary was built
against; include it in bug reports, since the translation layer's behaviour is
the upstream's.

## License

MIT — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
