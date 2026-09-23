# Exposing slimproxy on a domain via Cloudflare Tunnel

No inbound port is opened, no public IP is needed (CGNAT is fine), and TLS is
handled at the edge. slimproxy keeps listening on plain HTTP on `127.0.0.1` and
never becomes directly reachable from the internet.

Prerequisites: a domain whose nameservers point at Cloudflare, and a Cloudflare
account. Steps 0, 2 and 3 authenticate against that account in a browser and
have to be run by you.

## 0. Point the domain's nameservers at Cloudflare

Skip this if `cloudflared tunnel login` already lists your domain as a zone.

A Cloudflare Tunnel needs Cloudflare to be authoritative for the domain, because
step 4 creates a CNAME that only resolves through Cloudflare's edge. Registering
the domain elsewhere is fine — only the nameservers have to move. DNS records
you already have keep working; Cloudflare copies them during onboarding.

1. Cloudflare dashboard → **Add a site** → enter the apex domain (`example.com`,
   not `proxy.example.com`) → pick the Free plan.
2. Cloudflare scans your existing records. **Check that list before continuing** —
   anything it misses (rare, but it happens with unusual record types) stops
   resolving once the nameservers move. Add what's missing by hand.

   "Records we found: 0" on a freshly registered domain means there is nothing
   to preserve and the switchover is risk-free. Cloudflare will still suggest
   adding MX / `www` / apex records; ignore all three. The only record this
   setup needs is created for you in step 4, and adding one by hand now just
   collides with it.
3. Cloudflare shows two nameservers, e.g. `ana.ns.cloudflare.com` /
   `rick.ns.cloudflare.com`. They are specific to your account — don't copy
   these.
4. At your **registrar** (where you bought the domain, not Cloudflare), replace
   the existing nameservers with those two. Remove the old ones entirely;
   leaving them alongside produces intermittent resolution.
5. Wait for Cloudflare to mark the zone **Active**. Usually minutes; the TTL on
   the registrar's NS delegation sets the ceiling and can be up to 48h.

Check from here rather than trusting the dashboard alone:

```
nslookup -type=NS example.com 1.1.1.1
```

Both answers should be `*.ns.cloudflare.com`. Until they are, step 2 will not
offer the zone.

Two things this changes that are easy to miss:

- **Cloudflare becomes your DNS provider.** Records are edited in their
  dashboard from now on, not the registrar's.
- **Email is unaffected only if the MX records came across.** Verify them in
  the Cloudflare DNS list before the switchover, not after.

## 1. Install cloudflared

```
winget install --id Cloudflare.cloudflared
```

Open a new terminal afterwards so `cloudflared` is on PATH.

## 2. Authenticate

```
cloudflared tunnel login
```

Opens a browser. Pick the zone (domain) you want to use. Writes
`%USERPROFILE%\.cloudflared\cert.pem`.

## 3. Create the tunnel

```
cloudflared tunnel create slimproxy
```

Prints a tunnel UUID and writes `%USERPROFILE%\.cloudflared\<UUID>.json`.
Keep that file: it is the tunnel's credential.

## 4. Point a hostname at it

```
cloudflared tunnel route dns slimproxy proxy.example.com
```

Creates the CNAME in Cloudflare DNS. Replace `proxy.example.com` with the
subdomain you want.

## 5. Install the config

```
.\deploy\install-tunnel-config.ps1 -TunnelId <UUID from step 3> -Hostname <hostname from step 4>
```

That substitutes the four placeholders, rewrites `credentials-file` to this
user's actual `.cloudflared` directory, and refuses to write anything if the
UUID is not a UUID, the hostname is not fully qualified, the credentials file is
missing, or a placeholder survived. It backs up an existing config and will not
replace one without `-Force`.

Doing it by hand works too — copy `cloudflared-config.yml` to
`%USERPROFILE%\.cloudflared\config.yml` and replace `REPLACE_WITH_TUNNEL_ID`
(twice) and `REPLACE_WITH_HOSTNAME` (once). The reason for the script is that
every one of those mistakes fails the same way: cloudflared starts, connects to
the edge, reports itself healthy, and then serves 404s.

## 6. Nothing to configure on the slimproxy side

There is no slimproxy setting for "I am behind a tunnel". Earlier versions
shipped a Web dashboard and a `ui-behind-proxy` switch to protect it; both were
removed when the project became a terminal-only tool, and with them went the
whole class of "the process mistakes internet traffic for local traffic"
problems. The TUI runs in your terminal, not over HTTP — a tunnel exposes only
the API, which is exactly the part protected by `api-keys`.

If you find `ui-behind-proxy` (or any `ui-*` key) in an old config, delete it:
slimproxy rejects unknown configuration keys at startup, by design.

## 7. Run it

Foreground, to watch it connect:

```
cloudflared tunnel run slimproxy
```

As a Windows service, once it works:

```
cloudflared service install
```

### If it never connects

Run `slimproxy doctor` first. Its `tunnel-edge-dns` check resolves
`region1/region2.v2.argotunnel.com` through the same resolver cloudflared will
use, reports a `198.18.x.x` answer as a finding, and prints both halves of the
fix as its remedy — all before anything tries to start the tunnel. Reading it
out of the log by hand works too, and the section below is how: it is the
reasoning behind that one finding, and the path to follow when cloudflared is
being run on its own.

The question the check asks, and the one to ask by hand otherwise, separates "a
local proxy is on the path" from every other cause, and it holds whatever the
failure looks like afterwards: **is the `ip=` field in cloudflared's log a
`198.18.x.x` address?**

```
INF Tunnel connection curve preferences: [...] connIndex=0 event=0 ip=198.18.0.11
ERR Failed to dial a quic connection error="..." connIndex=0 event=0 ip=198.18.0.12
```

`198.18.0.0/15` is RFC 2544 benchmark space, reserved for router throughput
tests and never routed to a real destination, so a Cloudflare edge address is
never in it. Seeing it means the DNS answer for `region1.v2.argotunnel.com`
came from a local proxy or VPN in fake-ip mode: the address is a handle the TUN
stack hands back to itself, and whatever cloudflared sends to it is relayed
through whichever outbound node the proxy's rules select. If the field holds a
real address, the proxy is not on the path and nothing else in this section
applies.

cloudflared also prints a CONNECTIVITY PRE-CHECKS table at startup. Read it,
but read it as a hint and not as a verdict — it has been wrong here in both
directions. Two shapes of this failure occur, and they do not call for the same
thing.

**Shape A — UDP fails, TCP works.**

```
UDP Connectivity  region1.v2.argotunnel.com  FAIL  QUIC connection failed
TCP Connectivity  region1.v2.argotunnel.com  PASS  HTTP/2 connection successful
```

The proxy forwards TCP but not UDP. QUIC then times out forever — cloudflared
retries it with backoff rather than falling back on its own — while HTTP/2 to
the same host genuinely works.

**Shape B — neither transport works.** Measured 2026-09-20:

```
INF precheck component="UDP Connectivity" details="QUIC connection successful" status=pass target=region1.v2.argotunnel.com
INF precheck component="TCP Connectivity" details="HTTP/2 connection is blocked or unreachable" status=fail target=region1.v2.argotunnel.com
ERR Failed to dial a quic connection error="failed to dial to edge with quic: timeout: no recent network activity" connIndex=0 event=0 ip=198.18.0.12
```

No rule matched `argotunnel.com`, so the traffic fell through to `MATCH` and
left through whichever node happened to be selected — here a VLESS relay whose
UDP forwarding carried QUIC's first stateless round-trip but not the
multi-packet handshake. That is why the UDP probe passed while every real dial
still timed out. TCP 7844 was no better, and the way it failed is worth
spelling out: the connection completed a TLS handshake and the peer presented
the genuine `*.cftunnel.com` certificate, but ALPN never negotiated `h2`, and
writing a single HTTP/2 preface frame failed 6 times out of 6. Both legs reach
*something*; neither reaches Cloudflare. cloudflared does not exit on that — it
retries indefinitely — so the only symptom that surfaces is a supervisor giving
up: slimproxy kills it once the 15s startup grace expires and reports that the
tunnel failed to start.

**The pre-check is not a verdict.** In the run above, at `02:46:13Z` the table
reported UDP PASS with `details="QUIC connection successful"` and
`suggested_protocol=quic`, and in that same second cloudflared logged a failed
dial to `198.18.0.12`. A probe that completes one round-trip against a fake IP
shows that the TUN stack answered, nothing more. When the table and the
`Failed to dial` lines disagree, the dial lines are the ones telling the truth.

**Pinning `-Protocol http2` is a confirmation tool for shape A, not a fix and
not a general fallback.** In shape A it connects immediately, which confirms
the reading — and it is then measurably worse than working QUIC. Measured on
one such setup: 14 registration events against 15 drops, roughly one drop every
few minutes, each one a window where the hostname returns 502. Health checks
pass between drops, so a single curl reports success and the tunnel looks fine.
In shape B it buys nothing at all, because TCP 7844 is the leg that is already
dead — switching to it moves the tunnel onto the transport you have just
watched fail 6 times out of 6. Either way it is a probe, never the end state.

The fix, for both shapes, is to keep the tunnel's control connection off the
proxy entirely. Two ways, depending on what the proxy client exposes:

1. **If it accepts custom rules** (most Clash/mihomo-based clients do): add
   `+.argotunnel.com` to `fake-ip-filter` so DNS returns the real address, and
   give `DOMAIN-SUFFIX,argotunnel.com` (plus the two edge ranges as `IP-CIDR`
   rules) an explicit target. Both are needed — a rule alone still receives a
   `198.18.x.x` answer and never matches. DIRECT is the simplest target and is
   what first fixed this setup; from mainland China it is also slow, so read
   "If it connects but uploads crawl" below before settling on it.

   Clash Verge Rev adds a trap of its own. An extend file takes effect only if
   the active profile lists it under `option:` in `profiles.yaml`; a "rules"
   extend file that is not listed there is written, accepted, and silently
   ignored, which leaves you reading a rule on disk that the running config has
   never seen. Put the two lines in whichever merge or script extend is already
   bound to the profile. That placement also survives the subscription refresh,
   which rewrites `profiles/<uid>.yaml` wholesale and discards anything edited
   into it directly.

2. **If it does not** (locked-down or vendor-customised clients): bypass at the
   OS level instead. Put the real edge addresses in `hosts`:

   ```
   198.41.192.27   region1.v2.argotunnel.com
   198.41.200.23   region2.v2.argotunnel.com
   ```

   then add static routes so those addresses leave through the physical gateway.
   A TUN client typically claims the public address space with wide prefixes
   (`196.0.0.0/6` and similar), and a `/20` beats them on longest-prefix match:

   ```
   route -p add 198.41.192.0 mask 255.255.240.0 <gateway> metric 1
   route -p add 198.41.200.0 mask 255.255.240.0 <gateway> metric 1
   ```

   Confirm with `Find-NetRoute -RemoteIPAddress 198.41.192.27` — the chosen
   interface must be the physical adapter, not the tunnel.

Two obvious ways of checking your work will lie to you while fake-ip is in
play. Both were confirmed on this setup:

- **`Test-NetConnection` succeeds against an address that goes nowhere.** The
  TUN client's gVisor stack completes the local TCP handshake itself and only
  then tries to reach upstream, so
  `Test-NetConnection region1.v2.argotunnel.com -Port 7844` reports
  `TcpTestSucceeded : True` for any address in the fake-ip pool. A completed
  three-way handshake is not evidence of a path.
- **`nslookup` returns the synthetic address even when you name a public
  resolver.** The TUN hijacks port 53, so `nslookup region1.v2.argotunnel.com
  1.1.1.1` is answered locally and still says `198.18.x.x`. DoH rides HTTPS and
  is therefore subject to the routing rules rather than to the DNS
  interception, which makes it the one way to see the real answer from this
  machine:

  ```
  curl -s -H "accept: application/dns-json" "https://cloudflare-dns.com/dns-query?name=region1.v2.argotunnel.com&type=A"
  ```

  What makes that answer real is that it is *outside* `198.18.0.0/15`; that is
  the whole test. The addresses seen from here landed in `198.41.192.0/20` and
  `198.41.200.0/20`, which is what the `hosts` and `route` lines above pin, but
  Cloudflare answers from whatever it likes — an address outside those two is
  not by itself wrong.

A fixed tunnel is recognisable without guessing, which matters because a
partial fix resembles a working one closely enough to be mistaken for it. All
four of these hold on a healthy run:

- The `ip=` fields hold addresses outside `198.18.0.0/15` (here
  `198.41.192.x` / `198.41.200.x`) and `location=` names a real anycast colo.
  For *connecting*, read it as "a colo at all"; which colo it is decides how
  fast the tunnel is, which is the next section.
- All four `connIndex` values, 0 through 3, register.
- Zero `Failed to dial` lines.
- No `You requested 4 HA connections but I can give you at most 2` line. It
  reads like an account limit and is not one: it was there on the intercepted
  run above and gone on the healthy one, and the two it offers is the number of
  distinct fake-ip addresses handed back — one per `regionN.v2.argotunnel.com`
  name. Seeing it after a fix means DNS is still being answered locally.

Then drop `-Protocol` and let it use QUIC again. On the shape-A setup measured
above the churn stopped dead once its traffic left the proxy: **0 drops**,
against 15 before. Its registration count over that window was 2, not the four
`connIndex` the list above expects — a different run, and nothing measured here
explains the gap, so take the drop count as the result and the list as what to
check.

One more place the same fake-ip behaviour bites, this time when verifying the
DNS record rather than connectivity: `nslookup` is equally useless for checking
what Cloudflare holds for your hostname, since A lookups return the proxy's
synthetic address regardless. Query DoH directly here too:

```
curl -s -H "accept: application/dns-json" "https://cloudflare-dns.com/dns-query?name=proxy.example.com&type=A"
```

`"Status":3` is NXDOMAIN (no record), `"Status":0` with an `Answer` array
containing `104.x` / `172.67.x` addresses is a working proxied record.

### If it connects but uploads crawl

The tunnel registers, `/healthz` answers, and yet every Claude Code turn waits
a minute before the model even starts. `slimproxy log` shows where: the `传`
(upload) leg of each request row, the time from the request headers reaching
slimproxy to the last byte of the body. Claude Code resends the whole
conversation every turn — ~2 MB at a few hundred thousand tokens — and prompt
caching saves the upstream's compute, not those bytes on the wire.

Measured on this setup, 2026-09-23, same client, same tunnel, DIRECT route:
upload p50 **0.3 s at 03:00, 38 s at 16:00**, with the model itself at ~8 s
throughout. cloudflared's own metrics (`127.0.0.1:20241/metrics`) showed why:
`quic_client_smoothed_rtt` ~200 ms and ~2.5% of sent packets lost
(`quic_client_lost_packets` over `quic_client_sent_bytes` / MTU). At that RTT
and loss one congestion-controlled connection tops out near 50 KB/s, which is
what the journal showed; 179 connection re-registrations in 15 hours came with
it.

The cause is the colo in `location=`. Anycast lands a connection wherever its
*source* address routes to, and from a mainland-China line going DIRECT that
was `lax*` / `sjc*` — the tunnel then crosses the public trans-Pacific path,
which is congested every afternoon. On 2026-09-16..18 the same tunnel left
through a proxy node, registered in `hkg*` / `tpe*`, and uploads stayed at
0.3 s all day. **Choosing the egress is the only way to choose the colo**;
pinning an edge IP does not help, since every colo announces the same
addresses.

The fix is to send the edge connections through a nearby node, keeping the
fake-ip exemption from the previous section. With the exemption in place the
node is handed a real `198.41.x.x` address rather than a hostname, and QUIC
through it registers normally; without it you are back in shape B. On this
machine that is a fallback group in the Clash Verge script extend: a preferred
Singapore node, the other Singapore and Hong Kong nodes after it, DIRECT last
(so losing every node degrades to the slow path instead of losing the tunnel),
members picked by name pattern from the current subscription so a renamed node
never leaves the group pointing at nothing. Pick nodes by measured latency to
Cloudflare, not by name — the controller's
`GET /proxies/<node>/delay?url=https://www.cloudflare.com/cdn-cgi/trace` gives
it per node, and the colo a node lands on is whatever `location=` says
afterwards. Result here: `sin*` colos, RTT 110–145 ms, a 2 MB body uploaded end
to end in 1.4 s.

Two things surprise on the way:

- **Re-routing does not move live connections.** Changing the rule while
  cloudflared runs makes QUIC *migrate* the existing connections onto the new
  path — still anchored to the old colo, so they get worse (200 → 283 ms here),
  not better. Only fresh connections land in the new colo, which means a
  cloudflared restart. To do it without a gap, start a second connector for the
  same tunnel first (`cloudflared tunnel --metrics 127.0.0.1:20299 run <id>`),
  confirm its `location=`, then `slimproxy tunnel down` / `up -detach` while
  `cloudflared_tunnel_concurrent_requests_per_tunnel` is 0, and stop the
  second connector afterwards.
- **The startup pre-check reports UDP and TCP 7844 as FAIL through the node**
  (`hard_fail=true`) in the same run where all four connections register. As
  in shape B, the dial and registration lines are the ones to believe.

## 8. Verify

```
curl https://proxy.example.com/healthz
```

Expect `200`. Then check the two properties that matter:

```
curl -o /dev/null -w "%{http_code}\n" https://proxy.example.com/v1/models
curl -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer <api-key>" https://proxy.example.com/v1/models
```

Expected: `401` then `200` — anonymous callers are refused, your key works.

## Sharing it with more than one person

Two gaps matter here, and neither is fixed by anything above. Both are
consequences of slimproxy having been written for a loopback listener.

**No rate limiting.** slimproxy has none at all. A leaked API key is unbounded
use of your upstream subscription until you notice and rotate it. Cloudflare
can add one at the edge (Security → WAF → Rate limiting rules) on the hostname
— worth setting *before* anything else knows the URL, since the cost of being
wrong is your quota, not an error page.

**No attribution.** This is the one that surprises people. Handing each person
their own entry in `api-keys` looks like it buys you per-person revocation and
per-person logs. It buys the first, not the second:

- The access log line carries a peer IP, and behind a tunnel every request's
  peer is cloudflared on `127.0.0.1`. The field is a constant.
- This project never calls `SetTrustedProxies`, and gin's default is to trust
  every proxy and honour `X-Forwarded-For`. Any IP that *does* reach the log
  through that path is client-supplied and therefore unfalsifiable in the wrong
  direction — a caller can write whatever origin it likes.
- Nothing logs which key authenticated a request, not even a fingerprint.

So when quota starts disappearing, the logs will not tell you whose key it was.
You can still rotate all of them, which is the blunt version of a fix.

If per-person identity actually matters, put it at the edge instead of trying
to retrofit it here: **Cloudflare Access** (free up to 50 users) authenticates
in front of the tunnel and logs a real identity per request. For programmatic
callers it issues service tokens (`CF-Access-Client-Id` /
`CF-Access-Client-Secret` headers) rather than requiring a browser login.
Configure it once the tunnel is up and the zone is Active — the ordering
matters, since Access policies attach to the hostname the tunnel creates.

None of that is set up yet. Getting the tunnel working first is the right
order; just don't read a working tunnel as a finished access-control story.

## Turning it off

```
cloudflared service uninstall
cloudflared tunnel delete slimproxy
```

Then remove the DNS record in the Cloudflare dashboard.
