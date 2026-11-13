#!/usr/bin/env node
// Launcher for the apipod binary.
//
// The binary ships in a platform-specific package (@apipod/cli-<os>-<cpu>)
// that npm installs through optionalDependencies. This package has no install
// scripts and downloads nothing: what npm verified at install time is exactly
// what runs.
"use strict";

const { spawnSync } = require("node:child_process");

const PACKAGES = {
  "darwin arm64": "@apipod/cli-darwin-arm64",
  "darwin x64": "@apipod/cli-darwin-x64",
  "linux arm64": "@apipod/cli-linux-arm64",
  "linux x64": "@apipod/cli-linux-x64",
  "win32 arm64": "@apipod/cli-win32-arm64",
  "win32 x64": "@apipod/cli-win32-x64",
};

function binaryPath() {
  const pkg = PACKAGES[`${process.platform} ${process.arch}`];
  if (!pkg) {
    throw new Error(
      `apipod has no prebuilt binary for ${process.platform}/${process.arch}. ` +
        "Download one from https://github.com/geniuspath/apipod-cli/releases",
    );
  }
  const file = process.platform === "win32" ? "apipod.exe" : "apipod";
  try {
    return require.resolve(`${pkg}/bin/${file}`);
  } catch {
    throw new Error(
      `The package ${pkg} is not installed. It is an optional dependency of ` +
        "apipod: reinstall without --omit=optional (or --no-optional).",
    );
  }
}

function main() {
  let bin;
  try {
    bin = binaryPath();
  } catch (err) {
    console.error(`apipod: ${err.message}`);
    process.exit(1);
  }
  const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
  if (result.error) {
    console.error(`apipod: cannot run ${bin}: ${result.error.message}`);
    process.exit(1);
  }
  if (result.signal) {
    // Re-raise the signal so the caller sees how the process ended.
    process.kill(process.pid, result.signal);
    return;
  }
  process.exit(result.status === null ? 1 : result.status);
}

main();
