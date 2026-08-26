#!/usr/bin/env node
"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");
const { binPath } = require("../scripts/platform");

const bin = binPath();

if (!fs.existsSync(bin)) {
  console.error(
    `fitguard binary not found at ${bin}.\n` +
      "The postinstall download may have failed. Try: npm install fitguard --force"
  );
  process.exit(1);
}

// stdio: "inherit" hands the real terminal (including the masked
// password prompts `fitguard init` uses) straight to the Go binary,
// rather than this wrapper trying to proxy stdin/stdout itself.
const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });

if (result.error) {
  console.error(`fitguard: ${result.error.message}`);
  process.exit(1);
}

// status is null when the child was killed by a signal (e.g. Ctrl+C)
// rather than exiting normally; process.exit() requires a number, so
// that case falls back to a generic failure code.
process.exit(result.status === null ? 1 : result.status);
