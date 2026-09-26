#!/usr/bin/env bash
# macOS release build — the darwin counterpart of scripts/build-wails.ps1.
#
# Wails emits WxTap.app; the shell expects every runtime asset inside the
# bundle's Contents/Resources (see resourceProbeRootsFor / resolveCoreScript
# in desktop/). Produces:
#
#   desktop/build/release/
#     WxTap.app/Contents/MacOS/WxTap
#     WxTap.app/Contents/Resources/core/{package.json,dist,hooks,node_modules}
#     WxTap.app/Contents/Resources/resources/{frida,skills,...}
#     WxTap.app/Contents/Resources/migrations/
#
# No Node runtime is bundled: the shell resolves the user's own Node from PATH
# (resolveNodeCommand in desktop/app.go). The bundle still targets the build
# host architecture, because the staged node_modules and the frida native
# binding are architecture-specific. Build separately for Intel and Apple
# Silicon.
#
# Requires: macOS, Go 1.25+, Node.js 22+, Xcode command line tools.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
desktop="$root/desktop"
release="$desktop/build/release"
app_name="WxTap.app"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "build-wails.sh: Wails darwin bundles must be built on macOS (found $(uname -s))." >&2
  exit 1
fi

node_version="$(node --version)"
node_major="${node_version#v}"
node_major="${node_major%%.*}"
if (( node_major < 22 )); then
  echo "build-wails.sh: Node.js 22+ is required (found $node_version)." >&2
  exit 1
fi

echo "==> Building Core"
(cd "$root/core" && npm ci && npm run build)

echo "==> Building frontend"
(cd "$desktop/frontend" && npm ci && npm run build -- --outDir "$desktop/frontend/dist" --emptyOutDir)

go_host_arch="$(go env GOHOSTARCH)"
case "$go_host_arch" in
  arm64) wails_platform="darwin/arm64" ;;
  amd64) wails_platform="darwin/amd64" ;;
  *)
    echo "build-wails.sh: unsupported Go host architecture: $go_host_arch" >&2
    exit 1
    ;;
esac

echo "==> Building Wails bundle ($wails_platform)"
(cd "$desktop" && go run github.com/wailsapp/wails/v2/cmd/wails@v2.16.0 build -clean -s -platform "$wails_platform")

# Refuse to clean anything outside desktop/build, mirroring the PowerShell
# script's guard: release paths are derived, never trusted.
release_full="$(cd "$(dirname "$release")" && pwd)/$(basename "$release")"
desktop_full="$(cd "$desktop" && pwd)"
case "$release_full" in
  "$desktop_full"/build/*) ;;
  *)
    echo "build-wails.sh: refusing to clean release path outside desktop/build: $release_full" >&2
    exit 1
    ;;
esac

app_source="$desktop/build/bin/$app_name"
if [[ ! -d "$app_source" ]]; then
  echo "build-wails.sh: expected Wails output at $app_source" >&2
  exit 1
fi

rm -rf "$release_full"
mkdir -p "$release_full"
cp -R "$app_source" "$release_full/"

bundle_resources="$release_full/$app_name/Contents/Resources"

# No runtime/ is staged: the release resolves the user's own Node from PATH
# (see resolveNodeCommand in desktop/app.go). The release directory is rebuilt
# from scratch above, so no runtime/ can survive from an earlier build either.
# The released Core needs only its runtime dependencies (frida, js-beautify,
# protobufjs, ws). Everything else in node_modules is build-time tooling —
# typescript, esbuild, eslint and friends, ~100MB — that would ship for nothing,
# and it carries native binaries built for whatever host ran this script.
# Install those into an isolated stage rather than pruning the working tree,
# which would break `npm test` and `npm run lint`. build-wails.ps1 stages the
# same way, so both platforms ship the same dependency set.
echo "==> Staging Core runtime dependencies"
core_stage="$desktop/build/core-stage"
core_stage_full="$(cd "$(dirname "$core_stage")" && pwd)/$(basename "$core_stage")"
case "$core_stage_full" in
  "$desktop_full"/build/*) ;;
  *)
    echo "build-wails.sh: refusing to clean a Core stage outside desktop/build: $core_stage_full" >&2
    exit 1
    ;;
esac
rm -rf "$core_stage_full"
mkdir -p "$core_stage_full"
cp "$root/core/package.json" "$root/core/package-lock.json" "$core_stage_full/"
(cd "$core_stage_full" && npm ci --omit=dev)

echo "==> Bundling Core"
mkdir -p "$bundle_resources/core"
cp -R "$root/core/dist" "$bundle_resources/core/dist"
cp -R "$root/core/hooks" "$bundle_resources/core/hooks"
cp -R "$core_stage_full/node_modules" "$bundle_resources/core/node_modules"
# Node reads core/package.json for "type": "module"; without it, dist/cli.js is
# first parsed as CommonJS and re-parsed as ESM, warning at every startup.
cp "$root/core/package.json" "$bundle_resources/core/package.json"

echo "==> Bundling runtime assets"
cp -R "$root/resources" "$bundle_resources/resources"
cp -R "$desktop/migrations" "$bundle_resources/migrations"

bundle_macos="$release_full/$app_name/Contents/MacOS"

# The update payload is a flat tree, not the bundle. desktop/internal/update
# maps WxTap -> Contents/MacOS/WxTap and core/resources/migrations ->
# Contents/Resources/… when it swaps a staged version in, and the payload has to
# be assembled here because scripts/package-release.ps1 shards this directory —
# without it a macOS release cannot be published at all.
echo "==> Assembling the update payload"
payload_full="$desktop/build/release-payload"
rm -rf "$payload_full"
mkdir -p "$payload_full"
cp "$bundle_macos/WxTap" "$payload_full/WxTap"
cp -R "$bundle_resources/core" "$payload_full/core"
cp -R "$bundle_resources/resources" "$payload_full/resources"
cp -R "$bundle_resources/migrations" "$payload_full/migrations"

# The .dmg is macOS's "one file to download": it opens to the app beside an
# Applications symlink, so installing is one drag. hdiutil ships with macOS, so
# this costs nothing. An unsigned, un-notarised app still trips Gatekeeper on
# first open (right-click -> Open); removing that requires a paid Developer ID.
#
# hdiutil reports "Resource busy" when a volume of the same name is still
# mounted — a previous run's image leaves one behind — and it has been observed
# to report exactly that on a loaded CI runner with no such volume present. The
# two cases are indistinguishable from here, so both are handled: detach a stale
# volume, drop a partial image, and retry. Failing the release on this would
# mean a flaky volume attach can cost a whole version.
echo "==> Building the disk image"
dmg_full="$desktop/build/release-dist/WxTap.dmg"
dmg_stage="$desktop/build/dmg-staging"
mkdir -p "$(dirname "$dmg_full")"
rm -rf "$dmg_stage"
mkdir -p "$dmg_stage"
cp -R "$release_full/$app_name" "$dmg_stage/"
ln -s /Applications "$dmg_stage/Applications"

dmg_attempts=3
for attempt in $(seq 1 "$dmg_attempts"); do
  # `cmd || status=$?` rather than an if-condition: after `if cmd; then break; fi`
  # an untaken branch leaves $? at 0, so the real hdiutil exit status would be
  # lost from the diagnostic that matters most when it finally fails.
  status=0
  hdiutil create -volname "WxTap" -srcfolder "$dmg_stage" -ov -format UDZO "$dmg_full" || status=$?
  if (( status == 0 )); then
    break
  fi
  if (( attempt == dmg_attempts )); then
    echo "build-wails.sh: hdiutil create failed $dmg_attempts times (last exit $status)" >&2
    echo "--- hdiutil info ---" >&2
    hdiutil info >&2 || true
    exit 1
  fi
  echo "hdiutil create failed (attempt $attempt of $dmg_attempts, exit $status); detaching any stale volume and retrying" >&2
  hdiutil detach "/Volumes/WxTap" >/dev/null 2>&1 || true
  rm -f "$dmg_full"
  sleep $(( attempt * 5 ))
done
rm -rf "$dmg_stage"

echo "Release staged at $release_full/$app_name"
echo "Update payload at $payload_full"
echo "Disk image at $dmg_full"
