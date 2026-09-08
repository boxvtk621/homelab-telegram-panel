#!/usr/bin/env python3
"""CI-only release metadata/bundle builder. Never uses application credentials."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

import deploy

ROOT = Path(__file__).resolve().parent.parent
GITHUB_REPO = "boxvtk621/homelab-telegram-panel"


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def identity(version, revision):
    deploy.require(deploy.VERSION.fullmatch(version), "INVALID_RELEASE_TAG")
    deploy.require(deploy.re.fullmatch(r"[0-9a-f]{40}", revision), "INVALID_REVISION")


def check(version, revision):
    identity(version, revision)
    deploy.require(command("git", "rev-parse", "HEAD") == revision, "CHECKOUT_MISMATCH")
    deploy.require(command("git", "rev-parse", "refs/tags/" + version + "^{commit}") == revision, "TAG_MISMATCH")
    if "-rc." not in version:
        subprocess.run(["git", "merge-base", "--is-ancestor", revision, "origin/main"], check=True)


def guard(version):
    # 404 alone means absent. Auth/permissions/network errors must stop release.
    request = urllib.request.Request("https://api.github.com/repos/" + GITHUB_REPO + "/releases/tags/" + version,
                                     headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"], "Accept": "application/vnd.github+json"})
    try:
        with urllib.request.urlopen(request, timeout=20):
            raise deploy.DeployError("RELEASE_ALREADY_EXISTS_USE_NEW_VERSION")
    except urllib.error.HTTPError as exc:
        deploy.require(exc.code == 404, "RELEASE_LOOKUP_FAILED")


def push(version, revision, arch):
    """Retry only a registry missing-blob response, never rebuild the tested image."""
    identity(version, revision)
    deploy.require(arch in ("amd64", "arm64"), "UNSUPPORTED_ARCHITECTURE")
    tag = deploy.REPOSITORY + ":" + version + "-" + arch

    def local_id():
        result = subprocess.run(["docker", "image", "inspect", "--format", "{{.Id}}", tag],
                                capture_output=True, text=True, timeout=30)
        value = result.stdout.strip()
        deploy.require(result.returncode == 0 and deploy.DIGEST.fullmatch(value), "PUSH_LOCAL_IMAGE_UNAVAILABLE")
        return value

    tested_id = local_id()
    for attempt in range(1, 4):
        deploy.require(local_id() == tested_id, "PUSH_TESTED_IMAGE_CHANGED")
        print("REGISTRY_PUSH_ATTEMPT", attempt, "OF", 3, flush=True)
        try:
            result = subprocess.run(["docker", "push", tag], capture_output=True, text=True, timeout=300)
        except subprocess.TimeoutExpired:
            raise deploy.DeployError("REGISTRY_PUSH_TIMEOUT") from None
        if result.returncode == 0:
            print("REGISTRY_PUSH_CONFIRMED", flush=True)
            return
        # Never echo registry/CLI output: it can contain credentials or raw exceptions.
        diagnostic = (result.stdout + "\n" + result.stderr).lower()
        auth = r"unauthorized|forbidden|denied|authentication required|insufficient_scope|\b(?:401|403)\b"
        deploy.require(not deploy.re.search(auth, diagnostic), "REGISTRY_PUSH_AUTH_OR_PERMISSION_FAILED")
        deploy.require(deploy.re.search(r"\bunknown blob\b|\b(?:manifest_)?blob_unknown\b", diagnostic),
                       "REGISTRY_PUSH_FAILED")
        deploy.require(attempt < 3, "REGISTRY_PUSH_UNKNOWN_BLOB_RETRIES_EXHAUSTED")
        print("REGISTRY_PUSH_UNKNOWN_BLOB_RETRY", flush=True)
        time.sleep((5, 15)[attempt - 1])


def record(version, revision, arch, output):
    identity(version, revision)
    deploy.require(arch in ("amd64", "arm64"), "UNSUPPORTED_ARCHITECTURE")
    tag = deploy.REPOSITORY + ":" + version + "-" + arch
    manifest = json.loads(command("docker", "buildx", "imagetools", "inspect", tag, "--format", "{{json .Manifest}}"))
    deploy.require(deploy.DIGEST.fullmatch(manifest["digest"]), "INVALID_REGISTRY_DIGEST")
    output.write_text(json.dumps({"version": version, "revision": revision, "arch": arch, "image": deploy.REPOSITORY + "@" + manifest["digest"]}, sort_keys=True) + "\n")


def package(version, revision, image, output):
    identity(version, revision)
    output.mkdir(parents=True, exist_ok=False)
    shutil.copyfile(ROOT / "compose.yaml", output / "compose.yaml")
    shutil.copyfile(ROOT / "scripts/deploy.py", output / "deploy.py")
    shutil.copyfile(ROOT / "deploy/panel.env.example", output / ".env.example")
    data = {"schema": 1, "version": version, "revision": revision, "image": image,
            "platforms": ["linux/amd64", "linux/arm64"],
            "files": {name: deploy.checksum(output / name) for name in sorted(deploy.FILES)}}
    (output / "release.json").write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
    (output / "SHA256SUMS").write_text("".join(deploy.checksum(output / name) + "  " + name + "\n" for name in sorted(deploy.FILES | {"release.json"})))
    deploy.bundle(output)
    return data


def publish(version, revision, records, output):
    check(version, revision)
    guard(version)
    refs = []
    for arch in ("amd64", "arm64"):
        data = deploy.read_json(records / (arch + ".json"))
        deploy.require(set(data) == {"version", "revision", "arch", "image"} and data["version"] == version and data["revision"] == revision and data["arch"] == arch, "BUILD_RECORD_MISMATCH")
        deploy.require(data["image"].startswith(deploy.REPOSITORY + "@") and deploy.DIGEST.fullmatch(data["image"].split("@")[-1]), "INVALID_BUILD_DIGEST")
        refs.append(data["image"])
    tag = deploy.REPOSITORY + ":" + version
    subprocess.run(["docker", "buildx", "imagetools", "create", "--tag", tag, *refs], check=True)
    manifest = json.loads(command("docker", "buildx", "imagetools", "inspect", tag, "--format", "{{json .Manifest}}"))
    deploy.require(deploy.DIGEST.fullmatch(manifest["digest"]), "INVALID_REGISTRY_DIGEST")
    platforms = {item["platform"]["os"] + "/" + item["platform"]["architecture"] for item in manifest["manifests"] if item["platform"]["os"] != "unknown"}
    deploy.require(platforms == {"linux/amd64", "linux/arm64"}, "REGISTRY_PLATFORM_MISMATCH")
    image = deploy.REPOSITORY + "@" + manifest["digest"]
    package(version, revision, image, output)
    notes = output.parent / "release-notes.md"
    notes.write_text("Docker release `" + version + "` from `" + revision + "`.\n\n"
                     "Image (linux/amd64 + linux/arm64): `" + image + "`\n\n"
                     "Download all five assets into an empty directory, then run `python3 deploy.py verify`. "
                     "Copy `.env.example` to an operator-owned mode-0600 config outside this directory. "
                     "Use the README deployment instructions. No automatic server rollout. "
                     "Replacing a container interrupts active agents and clears in-memory sessions/history. "
                     "Image/bridge smoke is not real Cursor model acceptance.\n")
    subprocess.run(["gh", "release", "create", version, "--repo", GITHUB_REPO, "--verify-tag", "--title", version, "--notes-file", str(notes), "--latest=false", *(["--prerelease"] if "-rc." in version else []), *[str(output / name) for name in sorted(deploy.FILES | {"release.json", "SHA256SUMS"})]], check=True)
    # Read back both published metadata and asset names; never infer from exit0.
    published = json.loads(command("gh", "release", "view", version, "--repo", GITHUB_REPO, "--json", "tagName,isDraft,isPrerelease,assets"))
    deploy.require(published["tagName"] == version and not published["isDraft"] and published["isPrerelease"] == ("-rc." in version), "RELEASE_READBACK_MISMATCH")
    deploy.require({asset["name"] for asset in published["assets"]} == deploy.FILES | {"release.json", "SHA256SUMS"}, "RELEASE_ASSET_MISMATCH")
    print("RELEASE_PUBLISHED", version, image)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("action", choices=["check", "push", "record", "publish"])
    p.add_argument("--version", required=True)
    p.add_argument("--revision", required=True)
    p.add_argument("--arch", choices=["amd64", "arm64"])
    p.add_argument("--records", type=Path, default=Path("records"))
    p.add_argument("--output", type=Path, default=Path("release-output/bundle"))
    args = p.parse_args()
    try:
        if args.action == "check":
            check(args.version, args.revision)
            guard(args.version)
        elif args.action == "push":
            push(args.version, args.revision, args.arch)
        elif args.action == "record":
            record(args.version, args.revision, args.arch, args.output)
        else:
            publish(args.version, args.revision, args.records, args.output)
        return 0
    except Exception as exc:
        print(str(exc) if isinstance(exc, deploy.DeployError) else "RELEASE_FAILED_CHECK_TAG_REGISTRY_AND_PERMISSIONS", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
