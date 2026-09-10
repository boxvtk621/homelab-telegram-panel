#!/usr/bin/env python3
"""Build an allowlisted linux/amd64 alpha bundle without credentials or state."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("directory", type=Path)
parser.add_argument("--version", required=True)
args = parser.parse_args()
if not args.version or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-" for c in args.version):
    parser.error("invalid version")
root = Path(__file__).resolve().parents[2]
output = args.directory.resolve()
output.mkdir(mode=0o700)
env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64")
for package, name in (("./cmd/fixik-next-mobile-gateway", "panel"), ("./harness/cmd/harness-node", "harness-node")):
    subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
        "-buildid= -s -w -X github.com/boxvtk621/homelab-telegram-panel/internal/buildinfo.Version=" + args.version,
        "-o", str(output / name), package], cwd=root, env=env, check=True)
for source, destination in (("deploy/alpha/Dockerfile", "Dockerfile"), ("deploy/alpha/compose.yaml", "compose.yaml"),
    ("harness/adapters/cursor/worker/package.json", "package.json"),
    ("harness/adapters/cursor/worker/package-lock.json", "package-lock.json"),
    ("harness/adapters/cursor/worker/worker.mjs", "worker.mjs")):
    shutil.copyfile(root / source, output / destination)
manifest = {"version": args.version, "base": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip(),
    "files": {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(output.iterdir())}}
(output / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
print(json.dumps({"bundle": str(output), "version": args.version, "files": len(manifest["files"])}))
