"use strict";
// Shared between postinstall.js (downloads the binary) and bin/cli.js
// (runs it), so the two can never disagree on where it lives or what
// GoReleaser named it.

const path = require("path");

// Node's process.platform/arch values vs. GoOS/GoArch, which is what
// .goreleaser.yaml's archive name_template uses (fitguard_<os>_<arch>).
const OS_MAP = { darwin: "darwin", linux: "linux", win32: "windows" };
const ARCH_MAP = { x64: "amd64", arm64: "arm64" };

// Must match .goreleaser.yaml's `ignore:` list: every GOOS/GOARCH pair
// that isn't built has no release asset to download.
const UNSUPPORTED = new Set(["windows/arm64"]);

function resolvePlatform() {
  const goos = OS_MAP[process.platform];
  const goarch = ARCH_MAP[process.arch];
  if (!goos || !goarch) {
    throw new Error(
      `fitguard has no prebuilt binary for ${process.platform}/${process.arch}. ` +
        "Build from source instead: https://github.com/Oluiy/ai-cost-guard#installation"
    );
  }
  if (UNSUPPORTED.has(`${goos}/${goarch}`)) {
    throw new Error(
      `fitguard does not build for ${goos}/${goarch}. ` +
        "Build from source instead: https://github.com/Oluiy/ai-cost-guard#installation"
    );
  }
  return { goos, goarch };
}

function archiveName(goos, goarch) {
  const ext = goos === "windows" ? "zip" : "tar.gz";
  return `fitguard_${goos}_${goarch}.${ext}`;
}

function binDir() {
  return path.join(__dirname, "..", "dist");
}

function binPath() {
  const ext = process.platform === "win32" ? ".exe" : "";
  return path.join(binDir(), `fitguard${ext}`);
}

module.exports = { resolvePlatform, archiveName, binDir, binPath };
