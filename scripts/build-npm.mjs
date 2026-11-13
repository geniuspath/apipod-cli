#!/usr/bin/env node
// Assembles the npm packages from GoReleaser output.
//
//   node scripts/build-npm.mjs [--dist dist] [--out npm/dist] [--version X.Y.Z]
//
// Output: <out>/apipod (the launcher) and <out>/cli-<os>-<cpu> (one binary
// each). The version defaults to the one recorded in dist/metadata.json.
import { chmodSync, cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");

function option(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i === -1 ? fallback : process.argv[i + 1];
}

const dist = resolve(root, option("dist", "dist"));
const out = resolve(root, option("out", "npm/dist"));

// GoReleaser (goos, goarch) -> npm (os, cpu).
const TARGETS = [
  { goos: "darwin", goarch: "arm64", os: "darwin", cpu: "arm64" },
  { goos: "darwin", goarch: "amd64", os: "darwin", cpu: "x64" },
  { goos: "linux", goarch: "arm64", os: "linux", cpu: "arm64" },
  { goos: "linux", goarch: "amd64", os: "linux", cpu: "x64" },
  { goos: "windows", goarch: "arm64", os: "win32", cpu: "arm64" },
  { goos: "windows", goarch: "amd64", os: "win32", cpu: "x64" },
];

function readJSON(path) {
  return JSON.parse(readFileSync(path, "utf8"));
}

// npm versions are semver without build metadata in practice: a snapshot
// version such as 0.1.1-snapshot+abc1234 is published-safe once "+" is gone.
function npmVersion(version) {
  return version.replace(/^v/, "").replace("+", ".");
}

if (!existsSync(join(dist, "artifacts.json"))) {
  console.error(`build-npm: ${join(dist, "artifacts.json")} not found. Run goreleaser first.`);
  process.exit(1);
}

const version = npmVersion(option("version", readJSON(join(dist, "metadata.json")).version));
const binaries = readJSON(join(dist, "artifacts.json")).filter((a) => a.type === "Binary");

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });

const template = readJSON(join(root, "npm/apipod/package.json"));
const optionalDependencies = {};

for (const target of TARGETS) {
  const artifact = binaries.find((a) => a.goos === target.goos && a.goarch === target.goarch);
  if (!artifact) {
    console.error(`build-npm: no binary for ${target.goos}/${target.goarch} in ${dist}`);
    process.exit(1);
  }
  const name = `@apipod/cli-${target.os}-${target.cpu}`;
  const dir = join(out, `cli-${target.os}-${target.cpu}`);
  const file = target.os === "win32" ? "apipod.exe" : "apipod";
  mkdirSync(join(dir, "bin"), { recursive: true });
  cpSync(resolve(root, artifact.path), join(dir, "bin", file));
  chmodSync(join(dir, "bin", file), 0o755);
  cpSync(join(root, "LICENSE"), join(dir, "LICENSE"));
  writeFileSync(
    join(dir, "README.md"),
    `# ${name}\n\nThe \`apipod\` binary for ${target.os}/${target.cpu}. ` +
      "Install the [`apipod`](https://www.npmjs.com/package/apipod) package instead of this one.\n",
  );
  writeFileSync(
    join(dir, "package.json"),
    JSON.stringify(
      {
        name,
        version,
        description: `apipod binary for ${target.os}/${target.cpu}`,
        homepage: template.homepage,
        repository: template.repository,
        license: template.license,
        os: [target.os],
        cpu: [target.cpu],
        files: ["bin"],
        // No "scripts": nothing runs at install time.
      },
      null,
      2,
    ) + "\n",
  );
  optionalDependencies[name] = version;
}

const launcher = join(out, "apipod");
mkdirSync(join(launcher, "bin"), { recursive: true });
cpSync(join(root, "npm/apipod/bin/apipod.js"), join(launcher, "bin/apipod.js"));
chmodSync(join(launcher, "bin/apipod.js"), 0o755);
cpSync(join(root, "README.md"), join(launcher, "README.md"));
cpSync(join(root, "LICENSE"), join(launcher, "LICENSE"));
writeFileSync(
  join(launcher, "package.json"),
  JSON.stringify({ ...template, version, optionalDependencies }, null, 2) + "\n",
);

console.log(`build-npm: wrote ${TARGETS.length + 1} packages (version ${version}) to ${out}`);
