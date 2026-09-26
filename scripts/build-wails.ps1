$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$desktop = Join-Path $root 'desktop'
$release = Join-Path $desktop 'build\release'
$frontendStage = Join-Path $desktop 'build\frontend-stage'

function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)][string]$Command,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$WorkingDirectory
    )

    Push-Location $WorkingDirectory
    try {
        & $Command @Arguments
        if ($LASTEXITCODE -ne 0) {
            throw "$Command failed with exit code $LASTEXITCODE."
        }
    }
    finally {
        Pop-Location
    }
}

# Wails -clean wipes build\bin, and that wipe fails halfway when the directory
# still holds an open file: the running binary itself, but equally its log or its
# WebView profile. `wails dev` runs WxTap-dev.exe out of this same directory, so
# probing WxTap.exe alone misses the common case. A second open asking for
# FileShare None fails whenever anyone else holds the file, which is what makes
# this a usable probe.
function Test-InUse {
    param([Parameter(Mandatory = $true)][string[]]$Path)

    foreach ($candidate in $Path) {
        if (-not (Test-Path -LiteralPath $candidate)) { continue }
        foreach ($file in Get-ChildItem -LiteralPath $candidate -Recurse -File -Force -ErrorAction SilentlyContinue) {
            try {
                $probe = [IO.File]::Open($file.FullName, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
                $probe.Dispose()
            }
            catch { return $true }
        }
    }
    return $false
}

# NSIS builds the installer a new user downloads. Wails only does
# exec.LookPath("makensis") — no environment variable, no bundled copy, and its
# own fallback is a warning that leaves no installer behind. So resolve it here:
# PATH, then the usual install locations, then the portable archive fetched once
# into build\tools (it needs no installation and no admin).
$nsisZipUrl = 'https://downloads.sourceforge.net/project/nsis/NSIS%203/3.11/nsis-3.11.zip'
function Resolve-Makensis {
    $onPath = Get-Command makensis -ErrorAction SilentlyContinue
    if ($onPath) { return $onPath.Source }

    foreach ($root in @(${env:ProgramFiles(x86)}, $env:ProgramFiles, $env:LOCALAPPDATA)) {
        if (-not $root) { continue }
        foreach ($relative in @('NSIS\makensis.exe', 'Programs\NSIS\makensis.exe')) {
            $candidate = Join-Path $root $relative
            if (Test-Path -LiteralPath $candidate) { return $candidate }
        }
    }

    # The portable archive holds two copies of makensis.exe (root and Bin/).
    # Only the root one has Stubs/ and Include/ beside it, which is how makensis
    # locates its own data files; the Bin/ copy needs NSISDIR pointing at the
    # parent. So the shallowest match is the one to run — for the cache probe and
    # a fresh extract alike.
    $tools = Join-Path $desktop 'build\tools\nsis'
    $cached = Get-ChildItem -LiteralPath $tools -Recurse -Filter 'makensis.exe' -File -ErrorAction SilentlyContinue |
        Sort-Object { $_.FullName.Length } | Select-Object -First 1
    if ($cached) { return $cached.FullName }

    $toolsRoot = Split-Path -Parent $tools
    $zip = Join-Path $toolsRoot 'nsis.zip'
    New-Item -ItemType Directory -Path $toolsRoot -Force | Out-Null
    Write-Host "Fetching portable NSIS into $tools ..."
    # curl.exe, not Invoke-WebRequest: SourceForge answers IWR with an HTML
    # "your download is starting" page, which Expand-Archive then rejects with a
    # bare FileFormatException. The OS ships curl, like the tar.exe that
    # package-release.ps1 already relies on.
    $curl = Join-Path $env:SystemRoot 'System32\curl.exe'
    if (-not (Test-Path -LiteralPath $curl)) { $curl = 'curl.exe' }
    & $curl -sS -L --fail -o $zip $nsisZipUrl
    if ($LASTEXITCODE -ne 0) { throw "curl failed to download NSIS (exit $LASTEXITCODE): $nsisZipUrl" }
    # Check the magic bytes so an interstitial page fails as itself.
    $stream = [IO.File]::OpenRead($zip)
    try { $head = New-Object byte[] 2; $null = $stream.Read($head, 0, 2) } finally { $stream.Dispose() }
    if ($head[0] -ne 0x50 -or $head[1] -ne 0x4B) {
        throw "The NSIS download is not a zip (first bytes $($head[0]),$($head[1])); the mirror may have served an HTML page."
    }
    Expand-Archive -LiteralPath $zip -DestinationPath $tools -Force
    Remove-Item -LiteralPath $zip -Force
    $cached = Get-ChildItem -LiteralPath $tools -Recurse -Filter 'makensis.exe' -File -ErrorAction SilentlyContinue |
        Sort-Object { $_.FullName.Length } | Select-Object -First 1
    if (-not $cached) { throw "The NSIS archive did not contain makensis.exe." }
    return $cached.FullName
}

$npm = (Get-Command npm -ErrorAction Stop).Source
$go = (Get-Command go -ErrorAction Stop).Source
$node = (Get-Command node -ErrorAction Stop).Source
$nodeVersion = (& $node --version).Trim()
$nodeMajor = [int](($nodeVersion -replace '^v', '').Split('.')[0])
if ($nodeMajor -lt 22) {
    throw "Node.js 22+ is required to build Core and the frontend; found $nodeVersion."
}

# Fail here rather than after three minutes of building: the installer is the
# artifact users download, so a build that cannot produce it is not a success.
$makensis = Resolve-Makensis
Write-Host "Using makensis: $makensis"
$env:PATH = (Split-Path -Parent $makensis) + [IO.Path]::PathSeparator + $env:PATH

# `npm ci` replaces core/node_modules, and Core's frida native binding
# (frida_binding.node) is held open by the Core process while the engine is
# running. Windows then refuses the replace with EPERM and npm buries the reason
# in a stack trace, so the lock is probed first and named here instead — the same
# FileShare None trick Test-InUse uses for the bin directory.
$fridaBinding = Join-Path $root 'core\node_modules\frida\build\frida_binding.node'
if (Test-Path -LiteralPath $fridaBinding) {
    try {
        $probe = [IO.File]::Open($fridaBinding, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
        $probe.Dispose()
    }
    catch {
        throw "Core 正在运行：$fridaBinding 被占用（WxTap.exe 或引擎还开着）。请先退出应用，再重新构建。"
    }
}

Invoke-Native $npm @('ci') (Join-Path $root 'core')
Invoke-Native $npm @('run', 'build') (Join-Path $root 'core')

# Build the frontend from an isolated copy. A running Vite/Codex preview keeps
# esbuild.exe open on Windows, so `npm ci` in the live source tree can fail
# with EPERM while trying to replace it.
$frontendSource = Join-Path $desktop 'frontend'
$frontendStageFull = [IO.Path]::GetFullPath($frontendStage)
$desktopFull = [IO.Path]::GetFullPath($desktop)
if (-not $frontendStageFull.StartsWith($desktopFull + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to clean a frontend stage outside desktop/build: $frontendStageFull"
}
if (Test-Path -LiteralPath $frontendStageFull) {
    Remove-Item -LiteralPath $frontendStageFull -Recurse -Force
}
New-Item -ItemType Directory -Path $frontendStageFull | Out-Null
foreach ($file in @('package.json', 'package-lock.json', 'tsconfig.json', 'vite.config.ts', 'index.html')) {
    Copy-Item (Join-Path $frontendSource $file) $frontendStageFull
}
Copy-Item (Join-Path $frontendSource 'src') (Join-Path $frontendStageFull 'src') -Recurse
Copy-Item (Join-Path $frontendSource 'scripts') (Join-Path $frontendStageFull 'scripts') -Recurse
$publicSource = Join-Path $frontendSource 'public'
if (Test-Path -LiteralPath $publicSource) {
    Copy-Item $publicSource (Join-Path $frontendStageFull 'public') -Recurse
}
Invoke-Native $npm @('ci') $frontendStageFull
$frontendDist = Join-Path $desktop 'frontend\dist'
Invoke-Native $npm @('run', 'build', '--', '--outDir', $frontendDist, '--emptyOutDir') $frontendStageFull

# The released Core needs only its runtime dependencies (frida, js-beautify,
# protobufjs, ws). Everything else in node_modules is build-time tooling —
# typescript, esbuild, eslint and friends, ~100MB — that would ship for nothing.
# Install those into an isolated stage instead of pruning the working tree:
# pruning in place would break `npm test` and `npm run lint`, which need them.
$coreStage = Join-Path $desktop 'build\core-stage'
$coreStageFull = [IO.Path]::GetFullPath($coreStage)
if (-not $coreStageFull.StartsWith($desktopFull + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to clean a Core stage outside desktop/build: $coreStageFull"
}
if (Test-Path -LiteralPath $coreStageFull) {
    Remove-Item -LiteralPath $coreStageFull -Recurse -Force
}
New-Item -ItemType Directory -Path $coreStageFull | Out-Null
$coreSource = Join-Path $root 'core'
foreach ($file in @('package.json', 'package-lock.json')) {
    Copy-Item (Join-Path $coreSource $file) $coreStageFull
}
Invoke-Native $npm @('ci', '--omit=dev') $coreStageFull

$wailsBase = @(
    'run',
    'github.com/wailsapp/wails/v2/cmd/wails@v2.16.0',
    'build'
)
# A running instance keeps its own files open, so -clean has to be skipped or the
# wipe dies on whatever it still holds. Without -clean Wails compiles a
# replacement anyway, and the staging step below writes a timestamp-independent
# WxTap-latest.exe beside the old file.
$binLocked = Test-InUse -Path @(
    (Join-Path $desktop 'build\bin\WxTap.exe'),
    (Join-Path $desktop 'build\bin\WxTap-dev.exe'),
    (Join-Path $desktop 'build\bin\logs')
)
if ($binLocked) { Write-Host 'build\bin is in use (an app or `wails dev` is running): building without -clean.' }
$wailsArgs = $wailsBase
if (-not $binLocked) { $wailsArgs += '-clean' }
$wailsArgs += @(
    '-s',
    '-platform', 'windows/amd64',
    # A Windows installer is what a new user downloads: one file to run, which
    # lays down the directory the app needs. WxTap.exe alone cannot start — it
    # reads core/, resources/ and migrations/ from beside itself.
    '-nsis',
    # Per-user: the install has to land somewhere writable without elevation,
    # because the database, logs and WebView2 profile live next to the exe and
    # the update chain swaps files in that same directory. The default
    # (machine) would need admin and land in Program Files.
    '-installscope', 'user',
    # download is the default, spelled out because it is load-bearing: a machine
    # without WebView2 (LTSC, stripped images) gets it installed rather than a
    # window that never paints.
    '-webview2', 'download'
)
Invoke-Native $go $wailsArgs $desktop

$releaseFull = [IO.Path]::GetFullPath($release)
if (-not $releaseFull.StartsWith($desktopFull + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to clean a release path outside desktop/build: $releaseFull"
}
if (-not (Test-Path -LiteralPath $releaseFull)) { New-Item -ItemType Directory -Path $releaseFull | Out-Null }

$releaseExe = Join-Path $releaseFull 'WxTap.exe'
$releaseExeLocked = $false
if (Test-Path -LiteralPath $releaseExe) {
    try {
        $probe = [IO.File]::Open($releaseExe, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
        $probe.Dispose()
    } catch { $releaseExeLocked = $true }
}
if ($releaseExeLocked) {
    $releaseExe = Join-Path $releaseFull 'WxTap-latest.exe'
} else {
    if (Test-Path -LiteralPath $releaseExe) { Remove-Item -LiteralPath $releaseExe -Force }
    $staleLatest = Join-Path $releaseFull 'WxTap-latest.exe'
    if (Test-Path -LiteralPath $staleLatest) { Remove-Item -LiteralPath $staleLatest -Force }
}
Copy-Item (Join-Path $desktop 'build\bin\WxTap.exe') $releaseExe -Force

# No runtime/ is staged: the release resolves the user's own Node from PATH,
# from the path saved in Settings, or from a well-known install location (see
# resolveNodeRuntime in desktop/node_runtime.go). That keeps ~33MB out of every
# download. A runtime/ left over from an earlier build still has to go — the
# shell ignores it now, and package-release.ps1 would otherwise pack 89MB of
# a runtime nothing reads.
$runtimeRelease = Join-Path $releaseFull 'runtime'
if (Test-Path -LiteralPath $runtimeRelease) { Remove-Item -LiteralPath $runtimeRelease -Recurse -Force }

$coreRelease = Join-Path $releaseFull 'core'
if (Test-Path -LiteralPath $coreRelease) { Remove-Item -LiteralPath $coreRelease -Recurse -Force }
New-Item -ItemType Directory -Path $coreRelease -Force | Out-Null
Copy-Item (Join-Path $root 'core\dist') (Join-Path $coreRelease 'dist') -Recurse
Copy-Item (Join-Path $root 'core\hooks') (Join-Path $coreRelease 'hooks') -Recurse
Copy-Item (Join-Path $coreStageFull 'node_modules') (Join-Path $coreRelease 'node_modules') -Recurse
# Node reads core/package.json for "type": "module"; without it, dist/cli.js is
# first parsed as CommonJS and re-parsed as ESM, warning at every startup.
Copy-Item (Join-Path $root 'core\package.json') $coreRelease

# Runtime assets live under resources/ (frida, skills,
# devtools_electron.js, icons). Keep the whole tree next
# to WxTap.exe so resolve* helpers and Core WXTAP_RESOURCE_ROOT defaults work.
$releaseResources = Join-Path $releaseFull 'resources'
$releaseMigrations = Join-Path $releaseFull 'migrations'
if (Test-Path -LiteralPath $releaseResources) { Remove-Item -LiteralPath $releaseResources -Recurse -Force }
if (Test-Path -LiteralPath $releaseMigrations) { Remove-Item -LiteralPath $releaseMigrations -Recurse -Force }
Copy-Item (Join-Path $root 'resources') $releaseResources -Recurse
Copy-Item (Join-Path $desktop 'migrations') $releaseMigrations -Recurse

Write-Host "Release staged at $releaseFull"

# Wails' own NSIS project packs only build/bin\<exe>, so the installer it just
# wrote holds an executable with nothing beside it — which cannot start, because
# the app reads core/, resources/ and migrations/ from its own directory. Add the
# payload to the install section and rebuild the installer with makensis, reusing
# Wails' project so the install scope, shortcuts, uninstall entry and WebView2
# bootstrap all stay Wails'. This runs after the release staging above, so the
# installer is built from exactly the tree that was staged.
$nsiProject = Join-Path $desktop 'build\windows\installer\project.nsi'
$nsiText = Get-Content -LiteralPath $nsiProject -Raw

# Solid LZMA: the payload is mostly the 118MB frida native binding, and NSIS
# defaults to zlib, which would nearly double the download a user faces.
$nsiHeader = @'
; Added by scripts/build-wails.ps1. Solid LZMA over the payload below; the NSIS
; default (zlib) would land near the uncompressed shard total.
SetCompressor /SOLID lzma

'@
$nsiText = $nsiHeader + $nsiText

$filesAnchor = '    !insertmacro wails.files'
if (-not $nsiText.Contains($filesAnchor)) {
    throw "build\windows\installer\project.nsi has no '$filesAnchor' line; the Wails template changed, so the payload patch must be updated."
}
$payloadBlock = @'
    !insertmacro wails.files

    ; The payload the executable reads from beside itself. Without these the
    ; installer produces an app that installs cleanly and then cannot start.
    SetOutPath "$INSTDIR\core"
    File /r "${ARG_WXTAP_PAYLOAD}\core\*"
    ; The skill documents moved from resources\mcp_skills to resources\skills. NSIS
    ; only adds files, so an upgraded install would otherwise keep the old directory
    ; forever; nothing reads it once the move is done, so drop it here instead of
    ; leaving a dead tree beside a live one of the same content. RMDir /r is a no-op
    ; when the directory is absent.
    RMDir /r "$INSTDIR\resources\mcp_skills"
    SetOutPath "$INSTDIR\resources"
    File /r "${ARG_WXTAP_PAYLOAD}\resources\*"
    SetOutPath "$INSTDIR\migrations"
    File /r "${ARG_WXTAP_PAYLOAD}\migrations\*"
    SetOutPath "$INSTDIR"
'@
$nsiText = $nsiText.Replace($filesAnchor, $payloadBlock)

# Uninstall must not take the user's history with it. The Wails template removes
# $INSTDIR wholesale, but on Windows that directory is also where the app keeps
# its data — traffic database, operation logs, config.json, decompiled output
# and hook scripts all live beside the executable (see userBaseDir in
# desktop/ipc_bridge.go). Delete what the installer put there and leave the rest:
# `RMDir` without /r removes the directory only when it is empty, so a user with
# data keeps it and a machine with none loses nothing. wails.deleteUninstaller
# already removes uninstall.exe, so it is not listed here.
$uninstallAnchor = '    RMDir /r $INSTDIR'
if (-not $nsiText.Contains($uninstallAnchor)) {
    throw "build\windows\installer\project.nsi has no '$uninstallAnchor' line; the Wails template changed, so the uninstall patch must be updated."
}
$uninstallBlock = @'
    ; Replaced by scripts/build-wails.ps1. The template's `RMDir /r $INSTDIR`
    ; would delete traffic.db, logs/, config.json, output/ and hook_scripts/ —
    ; user data, not program files.
    Delete "$INSTDIR\${PRODUCT_EXECUTABLE}"
    RMDir /r "$INSTDIR\core"
    RMDir /r "$INSTDIR\resources"
    RMDir /r "$INSTDIR\migrations"
    RMDir "$INSTDIR"
'@
$nsiText = $nsiText.Replace($uninstallAnchor, $uninstallBlock)
# Patch a copy rather than project.nsi itself: Wails owns that file and does not
# necessarily regenerate it, so rewriting it would double-insert the payload on
# the next build. The copy must sit in the same directory — project.nsi resolves
# wails_tools.nsh and its OutFile path relative to its own location.
$payloadNsi = Join-Path (Split-Path -Parent $nsiProject) 'wxtap-payload.nsi'
[IO.File]::WriteAllText($payloadNsi, $nsiText, (New-Object Text.UTF8Encoding $false))

$installerDir = Split-Path -Parent $nsiProject
Write-Host "Rebuilding the installer with the payload ..."
# The same defines Wails passes, plus the payload root. Both paths travel as
# command-line defines rather than sitting in the .nsi: makensis reads the
# script as ANSI, so a checkout under a non-ASCII path (this one is under 桌面)
# would otherwise be looked up under the wrong bytes and report no files found.
#
# One element per line, no commas: inside @(), a comma binds tighter than "+",
# so `"a=" + $x, "b=" + $y` collapses into a single concatenated argument and
# makensis answers with its usage text instead of a script name.
$nsisArgs = @(
    ('-DARG_WAILS_AMD64_BINARY=' + (Join-Path $desktop 'build\bin\WxTap.exe'))
    ('-DARG_WXTAP_PAYLOAD=' + [IO.Path]::GetFullPath($release))
    '-DWAILS_INSTALL_SCOPE=user'
    '-DREQUEST_EXECUTION_LEVEL=user'
    'wxtap-payload.nsi'
)
Push-Location $installerDir
try {
    & $makensis @nsisArgs
    if ($LASTEXITCODE -ne 0) { throw "makensis failed with exit code $LASTEXITCODE." }
}
finally {
    Pop-Location
}

# The installer is the artifact a new user downloads: one file to run. It sits
# beside the shards because both are "things you publish" — the shards exist so
# the update chain can fetch a new version without a reinstall.
$installers = @(Get-ChildItem -LiteralPath (Join-Path $desktop 'build\bin') -Filter '*-installer.exe' -File -ErrorAction SilentlyContinue)
if ($installers.Count -ne 1) {
    throw "Expected exactly one installer in build\bin, found $($installers.Count). Wails names it <product>-<arch>-installer.exe."
}
$distDir = Join-Path $desktop 'build\release-dist'
if (-not (Test-Path -LiteralPath $distDir)) { New-Item -ItemType Directory -Path $distDir | Out-Null }
$stagedSetup = Join-Path $distDir 'WxTap-setup.exe'
Copy-Item -LiteralPath $installers[0].FullName $stagedSetup -Force
Write-Host ("Installer staged at {0} ({1} MB)" -f $stagedSetup, [math]::Round($installers[0].Length / 1MB, 1))
