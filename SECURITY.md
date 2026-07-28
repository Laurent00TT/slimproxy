# Security Policy

slimproxy is a proxy that holds live OAuth subscription credentials and,
optionally, exposes itself to the internet through a tunnel. That makes the
following classes of bug security-relevant, not just incorrect:

- Anything that lets a request through without a valid inbound API key
  (the inbound auth is designed to fail closed — a fail-open regression is
  the worst bug this project can have).
- Anything that writes credential material, request bodies, or tokens to a
  location the documentation does not name as secret.
- Anything that widens what the tunnel exposes beyond the API surface.
- Permission regressions on `auths/`, the effective config, or the PID record
  (on Windows, mode bits alone are not access control — see `fsperm/`).

## Reporting

Please use GitHub's private vulnerability reporting on this repository
("Security" tab → "Report a vulnerability") rather than a public issue.
You should get a response within a week. There is no bounty program — this is
a personal project — but reports are taken seriously and fixes are prioritized
over features.

If the issue is in the protocol translation or client emulation layer, it
likely belongs upstream in
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI); reporting it
there helps every deployment, not just this one.

## Supported versions

Only the latest release is supported. There is no backporting.
