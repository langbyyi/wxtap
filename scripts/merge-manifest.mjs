#!/usr/bin/env node
// Merge one platform's payload into the published release manifest.
//
// The manifest is compiled into every shipped client, so its shape is a
// contract: both platforms publish their entry through
// scripts/package-release.ps1 (Windows calls it directly; the macOS leg of the
// release workflow calls the same script after build-wails.sh has produced the
// flat payload), so the two legs cannot disagree about the document they
// produce — and the merge stays testable on either platform, unlike the shell
// around it.
//
// Usage:
//   node merge-manifest.mjs <manifest.json> <platform-key> <assets.json> <version> [notes]
//
// <assets.json> is an array of {name, size, sha256, urls}. An existing manifest
// keeps its other platforms' entries; this platform's entry is replaced.

import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

const [manifestPath, platform, assetsPath, version, notes = ""] = process.argv.slice(2);

function fail(message) {
  console.error(`merge-manifest: ${message}`);
  process.exit(1);
}

if (!manifestPath || !platform || !assetsPath || !version) {
  fail("usage: merge-manifest.mjs <manifest.json> <platform-key> <assets.json> <version> [notes]");
}
// The client looks its own platform up by this key, so a typo here produces a
// manifest nobody can install from.
if (!/^[a-z0-9]+-[a-z0-9]+$/.test(platform)) {
  fail(`platform key ${JSON.stringify(platform)} should look like goos-goarch`);
}

let assets;
try {
  assets = JSON.parse(readFileSync(assetsPath, "utf8"));
} catch (error) {
  fail(`cannot read ${assetsPath}: ${error.message}`);
}
if (!Array.isArray(assets) || assets.length === 0) {
  fail(`${assetsPath} should hold a non-empty array of assets`);
}
for (const asset of assets) {
  for (const field of ["name", "size", "sha256", "urls"]) {
    if (asset[field] === undefined) fail(`asset ${JSON.stringify(asset.name)} is missing ${field}`);
  }
}

// A manifest written before platforms existed carries a bare `assets` list. It
// describes one platform only, so it is dropped rather than carried forward —
// this platform's entry replaces it, and the other platform's build adds its own.
const previous = readIfJson(manifestPath);
const platforms = previous?.platforms && typeof previous.platforms === "object" ? previous.platforms : {};
platforms[platform] = { assets };
const manifest = { version, notes, platforms };

// Renamed into place rather than written in place: a reader must never see a
// half-written manifest, and the temporary name is platform-qualified so two
// builds running side by side cannot race on it.
const temporary = `${manifestPath}.${platform}.tmp`;
mkdirSync(dirname(manifestPath), { recursive: true });
writeFileSync(temporary, `${JSON.stringify(manifest, null, 2)}\n`);
renameSync(temporary, manifestPath);

const keys = Object.keys(platforms).sort();
const total = Object.values(platforms)
  .flatMap((entry) => entry.assets)
  .reduce((sum, asset) => sum + asset.size, 0);
console.log(
  `manifest ${manifestPath}: ${version}, ${keys.join(" + ")} (${(total / 1024 / 1024).toFixed(1)} MB in total)`,
);

function readIfJson(path) {
  if (!existsSync(path)) return undefined;
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (error) {
    fail(`existing manifest ${path} is not valid JSON: ${error.message}`);
  }
}
