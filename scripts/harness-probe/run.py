#!/usr/bin/env python3
"""Build/run only the HL-240 offline fixture image; never accepts credentials."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parent
LABEL = "ru.h1-cloud.hl240.probe-run"


def docker(context, *args, **kwargs):
    return subprocess.run(["docker", "--context", context, *args], check=True, **kwargs)


def capture(context, *args):
    return docker(context, *args, capture_output=True, text=True).stdout.strip()


def run_args(image_id, run_id, volume):
    return [
        "run", "--rm", "--name", run_id, "--label", f"{LABEL}={run_id}",
        "--network", "none", "--read-only", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges", "--pids-limit", "128",
        "--memory", "512m", "--memory-swap", "512m", "--cpus", "1",
        "--user", "10001:10001", "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777",
        "--mount", f"type=volume,source={volume},target=/state",
        "--env", "HOME=/state/home", "--env", "CODEX_HOME=/state/home/codex",
        "--env", "XDG_CONFIG_HOME=/state/home/config", image_id,
    ]


def validate_report(report):
    if (report.get("stage") != "V1" or report.get("evidenceKind") != "offline_fixture"
            or report.get("providerCalls") != 0 or report.get("providerValidation") != "not_run"):
        raise ValueError("invalid_offline_evidence")
    expected = {"two_marker_dialogs", "tool_start_and_result", "steer_during_tool",
                "cancel_terminal", "restart_resume", "deny_after_resume"}
    matrix = report.get("mandatoryCapabilities", {})
    if set(matrix) != {"cursor", "codex"}:
        raise ValueError("missing_engine_matrix")
    for values in matrix.values():
        if set(values) != expected or any(v != "not_run" for v in values.values()):
            raise ValueError("offline_cannot_verify_provider")
    if report.get("status") != "passed":
        raise ValueError("offline_probe_failed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("build", "smoke"))
    parser.add_argument("--context", required=True, help="Exact Docker context, e.g. desktop-linux")
    parser.add_argument("--image", default="hl240-harness-probe:local")
    parser.add_argument("--output", type=Path, help="New evidence file; never overwrites an existing file")
    args = parser.parse_args()
    if args.action == "build":
        docker(args.context, "build", "--tag", args.image, str(ROOT))
        return 0
    if args.output and args.output.exists():
        parser.error("output already exists")
    image_id = capture(args.context, "image", "inspect", args.image, "--format", "{{.Id}}")
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", image_id):
        raise ValueError("invalid_image_id")
    image_label = capture(args.context, "image", "inspect", image_id, "--format",
                          '{{index .Config.Labels "ru.h1-cloud.hl240.purpose"}}')
    if image_label != "sdk-feasibility-probe":
        raise ValueError("not_a_probe_image")
    run_id = "hl240-probe-" + uuid.uuid4().hex
    volume = run_id + "-state"
    capture(args.context, "volume", "create", "--label", f"{LABEL}={run_id}", volume)
    try:
        # Captured output is bounded by fixture implementation. SDK/model runs are absent.
        result = docker(args.context, *run_args(image_id, run_id, volume),
                        capture_output=True, text=True, timeout=120)
        report = json.loads(result.stdout)
        validate_report(report)
        report["docker"] = {"context": args.context, "imageId": image_id, "runId": run_id,
                            "network": "none", "memoryBytes": 536870912, "cpus": 1}
        if report.get("source", {}).get("lockSha256") != hashlib.sha256((ROOT / "package-lock.json").read_bytes()).hexdigest():
            raise ValueError("image_lockfile_mismatch")
    finally:
        # Exact resources created by this invocation only; never prune or touch another run.
        existing = capture(args.context, "ps", "-aq", "--filter", f"label={LABEL}={run_id}")
        if existing:
            docker(args.context, "rm", "--force", run_id, stdout=subprocess.DEVNULL)
        owner = capture(args.context, "volume", "inspect", volume, "--format", f'{{{{index .Labels "{LABEL}"}}}}')
        if owner != run_id:
            raise ValueError("volume_owner_mismatch")
        docker(args.context, "volume", "rm", volume, stdout=subprocess.DEVNULL)
    # Cleanup is part of V1 acceptance. Never persist PASS before it succeeds.
    report["cleanupVerified"] = True
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        with args.output.open("x") as target:
            target.write(encoded)
    print(encoded, end="")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError):
        print("probe_failed: inspect the isolated fixture directly; no provider calls were authorized", file=sys.stderr)
        sys.exit(1)
