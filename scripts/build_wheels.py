#!/usr/bin/env python3
"""Build platform wheels that carry the apipod binary.

    python3 scripts/build_wheels.py [--dist dist] [--out python/dist] [--version X.Y.Z]

Reads GoReleaser's dist/artifacts.json and writes one wheel per platform.
A wheel holds the binary under <name>-<version>.data/scripts/, which pip
installs into the environment's bin directory, plus the dist-info metadata.
Only the standard library is used.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import re
import sys
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

DIST_NAME = "apipod-cli"          # project name on PyPI
WHEEL_NAME = "apipod_cli"         # normalized form used in file names
SUMMARY = "Generate images and video with APIPod from the terminal"

# (goos, goarch) -> wheel platform tags. The binaries are static (CGO disabled)
# so one Linux build serves both glibc (manylinux) and musl (musllinux).
PLATFORM_TAGS = {
    ("darwin", "arm64"): ["macosx_11_0_arm64"],
    ("darwin", "amd64"): ["macosx_10_13_x86_64"],
    ("linux", "arm64"): ["manylinux2014_aarch64", "musllinux_1_1_aarch64"],
    ("linux", "amd64"): ["manylinux2014_x86_64", "musllinux_1_1_x86_64"],
    ("windows", "arm64"): ["win_arm64"],
    ("windows", "amd64"): ["win_amd64"],
}

# Fixed timestamp for every archive member, for reproducible wheels.
EPOCH = (1980, 1, 1, 0, 0, 0)


def pep440(version: str) -> str:
    """Convert a GoReleaser version to a PEP 440 version.

    1.2.3                  -> 1.2.3
    1.2.3-rc.1             -> 1.2.3rc1
    0.1.1-snapshot+abc1234 -> 0.1.1.dev0+abc1234
    """
    version = version.removeprefix("v")
    public, _, local = version.partition("+")
    release, _, pre = public.partition("-")
    if not re.fullmatch(r"\d+(\.\d+)*", release):
        raise ValueError(f"cannot convert version {version!r} to PEP 440")
    out = release
    if pre:
        match = re.fullmatch(r"(alpha|beta|rc|a|b)\.?(\d+)?", pre, re.IGNORECASE)
        if match:
            kind = {"alpha": "a", "beta": "b"}.get(match.group(1).lower(), match.group(1).lower())
            out += f"{kind}{match.group(2) or 0}"
        else:
            out += ".dev0"
    if local:
        out += "+" + re.sub(r"[^A-Za-z0-9.]", ".", local)
    return out


def record_hash(data: bytes) -> str:
    digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=")
    return "sha256=" + digest.decode("ascii")


def metadata(version: str, readme: str) -> str:
    return (
        "Metadata-Version: 2.4\n"
        f"Name: {DIST_NAME}\n"
        f"Version: {version}\n"
        f"Summary: {SUMMARY}\n"
        "License-Expression: MIT\n"
        "License-File: LICENSE\n"
        "Keywords: apipod,cli,ai,image-generation,video-generation\n"
        "Project-URL: Homepage, https://www.apipod.ai\n"
        "Project-URL: Repository, https://github.com/geniuspath/apipod-cli\n"
        "Project-URL: Issues, https://github.com/geniuspath/apipod-cli/issues\n"
        "Classifier: Environment :: Console\n"
        "Classifier: Intended Audience :: Developers\n"
        "Classifier: Programming Language :: Go\n"
        "Classifier: Topic :: Multimedia :: Graphics\n"
        "Classifier: Topic :: Multimedia :: Video\n"
        "Requires-Python: >=3.8\n"
        "Description-Content-Type: text/markdown\n"
        "\n"
        f"{readme}"
    )


def wheel_file(tags: list[str]) -> str:
    lines = [
        "Wheel-Version: 1.0",
        "Generator: apipod-cli build_wheels.py",
        # The binary is platform-specific, so the wheel is not pure.
        "Root-Is-Purelib: false",
    ]
    lines += [f"Tag: py3-none-{tag}" for tag in tags]
    return "\n".join(lines) + "\n"


def build_wheel(out_dir: Path, version: str, binary: Path, exe_name: str, tags: list[str], readme: str, license_text: str) -> Path:
    dist_info = f"{WHEEL_NAME}-{version}.dist-info"
    scripts = f"{WHEEL_NAME}-{version}.data/scripts"

    # (archive name, content, unix mode), written in this fixed order.
    members = [
        (f"{scripts}/{exe_name}", binary.read_bytes(), 0o755),
        (f"{dist_info}/METADATA", metadata(version, readme).encode(), 0o644),
        (f"{dist_info}/WHEEL", wheel_file(tags).encode(), 0o644),
        (f"{dist_info}/licenses/LICENSE", license_text.encode(), 0o644),
    ]
    record_name = f"{dist_info}/RECORD"
    record = "".join(f"{name},{record_hash(data)},{len(data)}\n" for name, data, _ in members)
    record += f"{record_name},,\n"
    members.append((record_name, record.encode(), 0o644))

    path = out_dir / f"{WHEEL_NAME}-{version}-py3-none-{'.'.join(tags)}.whl"
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as archive:
        for name, data, mode in members:
            info = zipfile.ZipInfo(name, date_time=EPOCH)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3  # unix, so the mode bits below are honoured
            info.external_attr = (0o100000 | mode) << 16
            archive.writestr(info, data)
    return path


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--dist", default="dist", help="GoReleaser output directory")
    parser.add_argument("--out", default="python/dist", help="directory for the wheels")
    parser.add_argument("--version", help="override the version from dist/metadata.json")
    args = parser.parse_args()

    dist = (ROOT / args.dist).resolve()
    out_dir = (ROOT / args.out).resolve()
    artifacts_path = dist / "artifacts.json"
    if not artifacts_path.exists():
        print(f"build_wheels: {artifacts_path} not found. Run goreleaser first.", file=sys.stderr)
        return 1

    raw_version = args.version or json.loads((dist / "metadata.json").read_text())["version"]
    version = pep440(raw_version)
    binaries = [a for a in json.loads(artifacts_path.read_text()) if a.get("type") == "Binary"]
    readme = (ROOT / "README.md").read_text(encoding="utf-8")
    license_text = (ROOT / "LICENSE").read_text(encoding="utf-8")

    out_dir.mkdir(parents=True, exist_ok=True)
    for old in out_dir.glob("*.whl"):
        old.unlink()

    built = []
    for (goos, goarch), tags in PLATFORM_TAGS.items():
        artifact = next((a for a in binaries if a.get("goos") == goos and a.get("goarch") == goarch), None)
        if artifact is None:
            print(f"build_wheels: no binary for {goos}/{goarch} in {dist}", file=sys.stderr)
            return 1
        exe_name = "apipod.exe" if goos == "windows" else "apipod"
        built.append(build_wheel(out_dir, version, ROOT / artifact["path"], exe_name, tags, readme, license_text))

    for path in built:
        print(f"build_wheels: {path.relative_to(ROOT)}")
    print(f"build_wheels: wrote {len(built)} wheels (version {version})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
