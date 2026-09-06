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

cloudflared prints a CONNECTIVITY PRE-CHECKS table at startup. Read it before
theorising — it usually contains the whole answer:

```
UDP Connectivity  region1.v2.argotunnel.com  FAIL  QUIC connection failed
TCP Connectivity  region1.v2.argotunnel.com  PASS  HTTP/2 connection successful
```

UDP failing while TCP passes means the QUIC transport cannot reach the edge, and
cloudflared will retry it forever with backoff rather than falling back. On a
desktop the usual cause is a local proxy or VPN in fake-ip mode: check whether
the `ip=` field in the log lines is a `198.18.x.x` address. That range is RFC
2544 benchmark space, so a real Cloudflare edge is never in it — seeing it means
DNS was answered by the proxy, which then forwards TCP but not UDP.

**Pinning `-Protocol http2` is not the fix, and it hides the real problem.**
Measured on one such setup: HTTP/2 through the proxy connected immediately and
then logged 14 connection registrations against 15 drops — roughly one drop
every few minutes, each one a window where the hostname returns 502. Health
checks pass between drops, so a single curl reports success and the tunnel looks
fine. Use it only to confirm the diagnosis, never as the end state.

The fix is to keep the tunnel's control connection off the proxy entirely. Two
ways, depending on what the proxy client exposes:

1. **If it accepts custom rules** (most Clash/mihomo-based clients do): route
   `DOMAIN-SUFFIX,argotunnel.com` to DIRECT, and add `+.argotunnel.com` to
   `fake-ip-filter` so DNS returns the real address. Both are needed — a direct
   rule alone still receives a `198.18.x.x` answer and never matches.

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

After either fix the pre-check should report UDP PASS, the log's `ip=` fields
should hold real addresses rather than `198.18.x.x`, and `location=` usually
changes to the genuine anycast landing point. Then drop `-Protocol` and let it
use QUIC again. Same setup after the fix: 2 registrations, **0 drops**.

The same fake-ip behaviour also makes `nslookup` useless for verifying DNS
records — A lookups return the proxy's synthetic address regardless of what
Cloudflare actually holds. Query DoH directly instead:

```
curl -s -H "accept: application/dns-json" "https://cloudflare-dns.com/dns-query?name=proxy.example.com&type=A"
```

`"Status":3` is NXDOMAIN (no record), `"Status":0` with an `Answer` array
containing `104.x` / `172.67.x` addresses is a working proxied record.

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
