<#
.SYNOPSIS
    Split a staged release into uploadable shards and write the update manifest.

.DESCRIPTION
    Run scripts/build-wails.ps1 first: it stages the whole product under
    desktop/build/release. This script packs that tree into the shards the
    client downloads, hashes them, and writes latest.json — the single document
    the update chain reads for both "is there a newer version" and "where do its
    bytes come from".

    Shards exist because the hosting side caps one attachment (Gitee: 100MB per
    file, no range requests). Splitting also means a failed transfer only costs
    one shard: the client re-fetches that shard and reuses the ones that already
    verified.

.PARAMETER Version
    Release version, with or without a leading "v". Must match wails.json's
    productVersion, which is what the shell reports as its current version.

.PARAMETER Repo
    "<owner>/<repo>" of the repository that will host the release assets.

.PARAMETER Source
    Staged release directory. Defaults to desktop/build/release.

.PARAMETER Out
    Output directory for the shards and latest.json. Defaults to
    desktop/build/release-dist.

.PARAMETER Notes
    Release notes shown next to the version in the app.

.PARAMETER MirrorBase
    Optional second download base URL, tried after the primary one. Assets
    carry a list of sources, so a domestic mirror is added by republishing
    latest.json — no new client build.
#>
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$Repo,
    [string]$Source,
    [string]$Out,
    [string]$Notes = '',
    [string]$MirrorBase = '',
    # Which platform's payload this run publishes, as the GOOS-GOARCH key the
    # client looks itself up with. Defaults to the host, so the usual case needs
    # no argument; a macOS payload is published by passing darwin-* and a
    # -Source holding the flat payload the darwin layout expects.
    [string]$Platform
)

$ErrorActionPreference = 'Stop'

# Hashes via .NET rather than Get-FileHash: that cmdlet failed to resolve on a
# host where the other Utility cmdlets worked, and this script's output is a
# checksum the client refuses to install without.
function Get-Sha256Hex {
    param([Parameter(Mandatory = $true)][string]$Path)

    $stream = [IO.File]::OpenRead($Path)
    try {
        $hasher = [Security.Cryptography.SHA256]::Create()
        try {
            return ([BitConverter]::ToString($hasher.ComputeHash($stream)) -replace '-', '').ToLowerInvariant()
        }
        finally { $hasher.Dispose() }
    }
    finally { $stream.Dispose() }
}

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$desktop = Join-Path $root 'desktop'
$node = (Get-Command node -ErrorAction Stop).Source
# Forward slashes throughout: this script also packages the darwin payload
# (through pwsh), and on Unix a backslash is a legal file-name character, so
# 'build\release-dist' would create a literally-named directory the workflow's
# upload path never matches. Windows accepts '/' everywhere .NET or a tool
# reads these paths, so one spelling serves both.
if (-not $Source) { $Source = Join-Path $desktop 'build/release' }
if (-not $Out) { $Out = Join-Path $desktop 'build/release-dist' }
if (-not $Platform) {
    # $IsMacOS exists in PowerShell 7+ only; on 5.1 this is the Windows path,
    # which is the only place this script published from before.
    if ($IsMacOS) {
        $arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } else { 'amd64' }
        $Platform = "darwin-$arch"
    } else {
        $Platform = 'windows-amd64'
    }
}


# One attachment must fit the host's per-file cap. Keeping the number here
# makes an oversized shard a build-time failure instead of a failed upload.
$maxShardBytes = 100MB

$tag = if ($Version.StartsWith('v')) { $Version } else { "v$Version" }
$bare = $tag.TrimStart('v')
$repo = $Repo.Trim('/')

$sourceFull = [IO.Path]::GetFullPath($Source)
if (-not (Test-Path -LiteralPath $sourceFull)) {
    throw "Staged release not found: $sourceFull — run scripts/build-wails.ps1 first."
}
$desktopFull = [IO.Path]::GetFullPath($desktop)
$outFull = [IO.Path]::GetFullPath($Out)
if (-not $outFull.StartsWith($desktopFull + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to write release output outside desktop/build: $outFull"
}

# The shell reports its own version from wails.json, so a mismatch here is the
# update chain offering a build that calls itself something else.
$wails = Get-Content -LiteralPath (Join-Path $desktop 'wails.json') -Raw | ConvertFrom-Json
$productVersion = [string]$wails.info.productVersion
if ($productVersion -and $productVersion -ne $bare) {
    Write-Warning "wails.json productVersion is '$productVersion' but packaging '$bare'; update wails.json (and the appVersion constant in desktop/ipc_bridge.go) or the app will report the wrong current version."
}

# tar ships with Windows, but Git for Windows puts its own first on PATH.
# SystemRoot is unset away from Windows (the darwin leg runs this same script
# through pwsh), and Join-Path rejects a null parent as a terminating error.
$tar = 'tar'
if ($env:SystemRoot) {
    $systemTar = Join-Path $env:SystemRoot 'System32\tar.exe'
    if (Test-Path -LiteralPath $systemTar) { $tar = $systemTar }
}

# Entries are relative to the staging root: the client unpacks each shard into
# one version directory, so a leading "./" or an absolute path would scatter
# the tree.
# What a payload calls its items differs by platform: Windows ships a flat
# directory beside WxTap.exe, macOS ships the same items with the shell named
# WxTap (desktop/internal/update/apply.go maps them into the .app bundle). The
# shard split follows: Windows needs two because `core` alone is far past what
# one attachment may carry; the macOS payload is one shard.
if ($Platform -like 'darwin-*') {
    $shards = @(
        @{ Name = "WxTap-$bare-mac.tar.gz"; Entries = @('WxTap', 'resources', 'migrations', 'core') }
    )
} else {
    $shards = @(
        @{ Name = "WxTap-$bare-a.tar.gz"; Entries = @('WxTap.exe', 'resources', 'migrations') },
        @{ Name = "WxTap-$bare-b.tar.gz"; Entries = @('core') }
    )
}

if (-not (Test-Path -LiteralPath $outFull)) { New-Item -ItemType Directory -Path $outFull | Out-Null }
Get-ChildItem -LiteralPath $outFull -Filter '*.tar.gz' -File | Remove-Item -Force

$baseUrl = "https://github.com/$repo/releases/download/$tag"
$assets = @()
foreach ($shard in $shards) {
    $missing = @($shard.Entries | Where-Object { -not (Test-Path -LiteralPath (Join-Path $sourceFull $_)) })
    if ($missing.Count -gt 0) {
        throw "Staged release is missing $($missing -join ', '); re-run scripts/build-wails.ps1."
    }
    $archive = Join-Path $outFull $shard.Name
    Push-Location $sourceFull
    try {
        & $tar -czf $archive @($shard.Entries)
        if ($LASTEXITCODE -ne 0) { throw "tar failed with exit code $LASTEXITCODE." }
    }
    finally {
        Pop-Location
    }
    $file = Get-Item -LiteralPath $archive
    if ($file.Length -gt $maxShardBytes) {
        throw "$($shard.Name) is $([math]::Round($file.Length / 1MB, 1))MB, over the $([math]::Round($maxShardBytes / 1MB, 0))MB per-file cap; move an entry to another shard."
    }
    $digest = Get-Sha256Hex -Path $archive
    $urls = @("$baseUrl/$($shard.Name)")
    if ($MirrorBase) { $urls += "$($MirrorBase.TrimEnd('/'))/$($shard.Name)" }
    $assets += [ordered]@{
        name   = $shard.Name
        size   = $file.Length
        sha256 = $digest
        urls   = $urls
    }
    Write-Host ("packed {0}  {1} MB  {2}" -f $shard.Name, [math]::Round($file.Length / 1MB, 1), $digest)
}

$manifestPath = Join-Path $outFull 'latest.json'
$assetsPath = Join-Path $outFull "assets-$Platform.json"
# The manifest is written by scripts/merge-manifest.mjs rather than here: the
# other platform's build publishes into the same document, and one
# implementation of the merge is what keeps the two build scripts from
# disagreeing about its shape. BOM-free is its business too — encoding/json
# rejects one.
#
# -InputObject with an explicit [object[]] cast, NOT a pipeline: piping unrolls
# the collection, so a list of one serializes as a bare JSON object instead of a
# one-element array. That is why the Windows fragment (two shards) passed while
# the macOS one (a single shard) was rejected by merge-manifest's non-empty-array
# check — the shape of the document depended on how many shards happened to be
# in it.
[IO.File]::WriteAllText($assetsPath, (ConvertTo-Json -InputObject ([object[]]$assets) -Depth 5), (New-Object Text.UTF8Encoding $false))
& $node (Join-Path $root 'scripts/merge-manifest.mjs') $manifestPath $Platform $assetsPath $tag $Notes
if ($LASTEXITCODE -ne 0) { throw "merge-manifest.mjs failed with exit code $LASTEXITCODE." }
# The fragment is kept, not deleted: it is what the release workflow's publish
# job merges the other platform's entry from, and it documents exactly what this
# run published. Name-qualified by platform, so two runs cannot collide.

$total = [math]::Round((Get-ChildItem -LiteralPath $outFull -Filter '*.tar.gz' | Measure-Object -Property Length -Sum).Sum / 1MB, 1)
Write-Host ''
Write-Host "Release packaged at $outFull ($total MB across $($shards.Count) shards for $Platform)"
Write-Host ''
Write-Host 'Upload steps:'
Write-Host "  1. Create a release on $repo tagged $tag and attach these $($shards.Count) files:"
foreach ($shard in $shards) { Write-Host "       $outFull\$($shard.Name)" }
Write-Host "  2. Commit $manifestPath to the repository's default branch (the client reads only that fixed address)."
Write-Host "     It carries one entry per platform: running this script for the other platform merges into the same file."
Write-Host '  3. Confirm the client can reach the manifest:'
Write-Host "       curl -s https://raw.githubusercontent.com/$repo/main/latest.json"
Write-Host ''
Write-Host 'The release assets must be anonymously downloadable: a private repository needs a token, and a token shipped inside the client is readable by every user.'
