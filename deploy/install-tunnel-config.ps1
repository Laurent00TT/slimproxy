<#
.SYNOPSIS
    Generates the cloudflared config.yml for slimproxy from the template.

.DESCRIPTION
    The template has four placeholders across two settings, and a typo in any of
    them fails in a way that is annoying to read: cloudflared starts, connects to
    the edge, and then every request 404s or the tunnel serves nothing at all.
    This substitutes them, then verifies the things that are checkable locally
    before you find out from a failed curl.

.PARAMETER TunnelId
    The UUID printed by `cloudflared tunnel create`.

.PARAMETER Hostname
    The hostname routed in `cloudflared tunnel route dns`, e.g. proxy.example.com.

.PARAMETER Protocol
    Edge transport. Omit to let cloudflared choose (QUIC, which is preferable).
    Pass http2 when UDP 7844 to the edge is blocked -- typically a local proxy or
    VPN in fake-ip mode that forwards TCP but not UDP. cloudflared's startup
    pre-check reports this as UDP FAIL / TCP PASS.

.PARAMETER Force
    Overwrite an existing config.yml. Without it, an existing file is kept and
    the script stops -- silently replacing a working tunnel config is worse than
    making you look at it.

.EXAMPLE
    .\install-tunnel-config.ps1 -TunnelId 8f14e45f-... -Hostname proxy.example.com

.EXAMPLE
    .\install-tunnel-config.ps1 -TunnelId 8f14e45f-... -Hostname proxy.example.com -Protocol http2
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$TunnelId,
    [Parameter(Mandatory = $true)][string]$Hostname,
    [ValidateSet('quic', 'http2')][string]$Protocol,
    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$templatePath = Join-Path $PSScriptRoot 'cloudflared-config.yml'
$cfDir        = Join-Path $env:USERPROFILE '.cloudflared'
$targetPath   = Join-Path $cfDir 'config.yml'
$credPath     = Join-Path $cfDir "$TunnelId.json"

# ── Validate inputs before touching anything ────────────────────────────────

if ($TunnelId -notmatch '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$') {
    throw "TunnelId '$TunnelId' is not a UUID. Copy it from the output of 'cloudflared tunnel create', not the tunnel name."
}

# A bare label would produce a config that cloudflared accepts and that then
# never matches a request, which looks identical to a broken origin.
if ($Hostname -notmatch '^[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?)+$') {
    throw "Hostname '$Hostname' does not look like a fully qualified hostname (expected e.g. proxy.example.com)."
}
if ($Hostname -match '^https?://') {
    throw "Hostname '$Hostname' must not include a scheme. Use just the hostname."
}

if (-not (Test-Path $templatePath)) {
    throw "Template not found at $templatePath"
}

if (-not (Test-Path $cfDir)) {
    New-Item -ItemType Directory -Path $cfDir | Out-Null
    Write-Host "created $cfDir"
}

# The credentials file is written by `cloudflared tunnel create`. Its absence
# means the tunnel was created under a different account, a different home
# directory, or not at all -- all of which are easier to diagnose now than as a
# connection failure later.
if (-not (Test-Path $credPath)) {
    throw @"
Credentials file not found: $credPath

That file is written by 'cloudflared tunnel create'. Check that:
  - the tunnel was actually created (cloudflared tunnel list)
  - the UUID matches the one printed at creation
  - it was created by this Windows user
"@
}

if ((Test-Path $targetPath) -and -not $Force) {
    throw @"
$targetPath already exists.

Refusing to overwrite it. Inspect it first; pass -Force if replacing it is what
you want. A backup is taken either way.
"@
}

# ── Substitute ──────────────────────────────────────────────────────────────

$content = Get-Content $templatePath -Raw

$idHits   = ([regex]::Matches($content, 'REPLACE_WITH_TUNNEL_ID')).Count
$hostHits = ([regex]::Matches($content, 'REPLACE_WITH_HOSTNAME')).Count

# Pin the template's shape. If someone edits the template and drops a
# placeholder, a silently under-substituted config is the failure this catches.
if ($idHits -ne 2)   { throw "Template should contain REPLACE_WITH_TUNNEL_ID exactly twice, found $idHits. Template edited?" }
if ($hostHits -ne 2) { throw "Template should contain REPLACE_WITH_HOSTNAME exactly twice, found $hostHits. Template edited?" }

$content = $content -replace 'REPLACE_WITH_TUNNEL_ID', $TunnelId
$content = $content -replace 'REPLACE_WITH_HOSTNAME',  $Hostname

# The template hardcodes a credentials path under one user's profile. Rewrite it
# to this user's actual .cloudflared directory so the script works anywhere.
$content = $content -replace '(?m)^credentials-file:.*$', "credentials-file: $credPath"

# The template ships `# protocol: http2` commented out. Uncommenting it here
# keeps the explanation above it intact, which is the part worth reading when
# someone later wonders why this tunnel is not on QUIC.
if ($PSBoundParameters.ContainsKey('Protocol')) {
    $before = $content
    $content = $content -replace '(?m)^#\s*protocol:.*$', "protocol: $Protocol"
    if ($content -eq $before) {
        throw "Could not find the '# protocol:' line in the template to enable. Template edited?"
    }
    Write-Host "protocol pinned to $Protocol"
}

if ($content -match 'REPLACE_WITH_') {
    throw "A placeholder survived substitution. Not writing the file."
}

if (Test-Path $targetPath) {
    $backup = "$targetPath.bak-" + (Get-Item $targetPath).LastWriteTime.ToString('yyyyMMdd-HHmmss')
    Copy-Item $targetPath $backup
    Write-Host "backed up existing config to $backup"
}

Set-Content -Path $targetPath -Value $content -Encoding utf8
Write-Host "wrote $targetPath"

# ── Show what matters, so it is checked rather than assumed ─────────────────

Write-Host ""
Write-Host "--- key lines ---"
Get-Content $targetPath | Select-String -Pattern '^tunnel:|^credentials-file:|hostname:|service:|path:' | ForEach-Object {
    Write-Host ("  " + $_.Line.Trim())
}

Write-Host ""
Write-Host "Next:"
Write-Host "  cloudflared tunnel run slimproxy      # foreground, watch it connect"
Write-Host "  curl https://$Hostname/healthz        # expect 200"
Write-Host "  curl -o NUL -w '%{http_code}' https://$Hostname/v1/models   # expect 401 -- anonymous callers must be refused"
