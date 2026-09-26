# Cut a release from this machine: bump the version, run the local gates,
# commit, tag (annotated — the CI publishes the tag's message as latest.json's
# notes, which is what the in-app settings page shows), and push. The tag push
# starts release.yml; everything after that is CI's job (docs/RELEASE.md §二).
#
# Usage:
#   pwsh -File scripts/new-release.ps1 -Version 1.1.0 -Notes "Fixed ...; added ..."
#
# The gates mirror ci.yml (core lint+test, frontend test, go vet+test, and
# golangci-lint when it is on PATH). -SkipTests exists for re-running the
# pipeline on an already-tested tree; the CI runs the full matrix again, so
# skipping the local gates costs a red run, not a broken release.
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$Notes,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

$Version = $Version.TrimStart('v')
if ($Version -notmatch '^\d+\.\d+\.\d+$') {
    throw "Version '$Version' is not in x.y.z form."
}

function Invoke-Gate {
    param([string]$Name, [string]$WorkingDir, [string]$Command, [string[]]$Arguments)
    Write-Host "==> $Name"
    Push-Location $WorkingDir
    try {
        & $Command @Arguments
        if ($LASTEXITCODE -ne 0) { throw "$Name failed (exit $LASTEXITCODE)." }
    }
    finally {
        Pop-Location
    }
}

Push-Location $root
try {
    if ((git branch --show-current) -ne 'main') { throw "Releases are cut from main (see git branch --show-current)." }
    if (git status --porcelain) { throw "The working tree is not clean — commit or stash first (git status)." }
    if (git tag -l "v$Version") { throw "Tag v$Version already exists. To re-cut a version, delete the release and tag first (docs/RELEASE.md §二)." }

    # desktop/wails.json is the single source of the release version (the
    # frontend reads it at build time, the shell embeds it at compile time);
    # the two package.json fields are npm metadata only, bumped here so they
    # cannot drift. Each pattern replaces the first field occurrence only —
    # never the dependency tables.
    foreach ($file in @('desktop\wails.json', 'core\package.json', 'desktop\frontend\package.json')) {
        $path = Join-Path $root $file
        $pattern = if ($file -like '*wails.json') { '("productVersion"\s*:\s*")[^"]*' } else { '("version"\s*:\s*")[^"]*' }
        $text = Get-Content -LiteralPath $path -Raw
        $updated = [regex]::Replace($text, $pattern, ('${1}' + $Version), [System.Text.RegularExpressions.RegexOptions]::Singleline)
        if ($updated -eq $text) { throw "No version field matched in $file — the packaging format changed, update this script." }
        [IO.File]::WriteAllText($path, $updated, (New-Object Text.UTF8Encoding $false))
        Write-Host "bumped $file -> $Version"
    }

    if (-not $SkipTests) {
        $npm = (Get-Command npm -ErrorAction Stop).Source
        Invoke-Gate 'core: lint'        (Join-Path $root 'core')               $npm @('run', 'lint')
        Invoke-Gate 'core: test'        (Join-Path $root 'core')               $npm @('test')
        Invoke-Gate 'frontend: test'    (Join-Path $root 'desktop\frontend')   $npm @('test')
        Invoke-Gate 'desktop: vet'      (Join-Path $root 'desktop')            'go'  @('vet', './...')
        Invoke-Gate 'desktop: test'     (Join-Path $root 'desktop')            'go'  @('test', './...')
        if (Get-Command golangci-lint -ErrorAction SilentlyContinue) {
            Invoke-Gate 'desktop: golangci-lint' (Join-Path $root 'desktop') 'golangci-lint' @('run', './...')
        }
    }

    git add desktop/wails.json core/package.json desktop/frontend/package.json
    git commit -m "release: v$Version"
    git tag -a "v$Version" -m $Notes
    git push origin main "v$Version"

    Write-Host ""
    Write-Host "v$Version pushed. release.yml now builds and publishes it; watch with:"
    Write-Host "  gh run watch (`gh run list --repo langbyyi/wxtap --workflow release`)"
}
finally {
    Pop-Location
}
