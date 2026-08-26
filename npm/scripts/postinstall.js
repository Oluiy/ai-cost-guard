#!/usr/bin/env node
"use strict";
// Downloads the fitguard binary matching this machine from GitHub
// Releases, verifies it against the published checksums (same check
// install.sh does), and extracts it into npm/dist/. No dependencies:
// https (built-in) for the download, and the system `tar` for
// extraction — present by default on macOS, Linux, and Windows 10+
// (bsdtar, which also opens .zip), so there's nothing extra to install
// just to install this.

const fs = require("fs");
const path = require("path");
const http = require("http");
const https = require("https");
const crypto = require("crypto");
const { execFileSync } = require("child_process");

const { resolvePlatform, archiveName, binDir, binPath } = require("./platform");
const pkg = require("../package.json");

const REPO = "Oluiy/ai-cost-guard";
// The npm package version and the git tag are kept in lockstep (1.2.3 <-> v1.2.3),
// so there's one version number to bump per release rather than two to keep in sync.
const VERSION = process.env.FITGUARD_VERSION || `v${pkg.version}`;
const BASE_URL =
  process.env.FITGUARD_BASE_URL || `https://github.com/${REPO}/releases/download`;

function get(url, redirects) {
  redirects = redirects || 0;
  return new Promise((resolve, reject) => {
    if (redirects > 5) return reject(new Error(`too many redirects fetching ${url}`));
    // GitHub always serves https, but FITGUARD_BASE_URL is also the hook
    // this postinstall step is tested against locally, so the transport
    // follows the URL rather than being hardcoded to https.
    const transport = url.startsWith("http://") ? http : https;
    transport
      .get(url, { headers: { "User-Agent": "fitguard-npm-installer" } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          res.resume();
          return resolve(get(res.headers.location, redirects + 1));
        }
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error(`GET ${url} -> HTTP ${res.statusCode}`));
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(Buffer.concat(chunks)));
        res.on("error", reject);
      })
      .on("error", reject);
  });
}

function sha256(buf) {
  return crypto.createHash("sha256").update(buf).digest("hex");
}

async function main() {
  const { goos, goarch } = resolvePlatform();
  const archive = archiveName(goos, goarch);
  const archiveUrl = `${BASE_URL}/${VERSION}/${archive}`;
  const checksumsUrl = `${BASE_URL}/${VERSION}/checksums.txt`;

  console.log(`fitguard: downloading ${archive} (${VERSION})...`);
  const [archiveBuf, checksumsBuf] = await Promise.all([
    get(archiveUrl),
    get(checksumsUrl),
  ]);

  const line = checksumsBuf
    .toString("utf8")
    .split("\n")
    .find((l) => l.trim().endsWith(archive));
  if (!line) {
    throw new Error(`no checksum entry for ${archive} in checksums.txt`);
  }
  const expected = line.trim().split(/\s+/)[0];
  const actual = sha256(archiveBuf);
  if (expected !== actual) {
    throw new Error(
      `checksum mismatch for ${archive}: expected ${expected}, got ${actual}`
    );
  }

  const dist = binDir();
  fs.mkdirSync(dist, { recursive: true });
  const archivePath = path.join(dist, archive);
  fs.writeFileSync(archivePath, archiveBuf);

  console.log("fitguard: extracting...");
  // bsdtar (macOS/BSD tar, and Windows 10 1803+'s tar.exe) opens .zip
  // through the same `tar -xf`, so this one call covers every platform
  // this package targets without a separate zip codepath.
  execFileSync("tar", ["-xf", archivePath, "-C", dist, "fitguard" + (goos === "windows" ? ".exe" : "")], {
    stdio: "inherit",
  });
  fs.unlinkSync(archivePath);

  if (goos !== "windows") {
    fs.chmodSync(binPath(), 0o755);
  }

  console.log(`fitguard: installed to ${binPath()}`);
}

main().catch((err) => {
  console.error(`fitguard: install failed: ${err.message}`);
  console.error(
    "You can install manually instead: https://github.com/Oluiy/ai-cost-guard#installation"
  );
  process.exit(1);
});
