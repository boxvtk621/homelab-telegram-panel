"""Bounded Docker runner for a native Codex probe using a fresh auth volume."""
import argparse
import json
from pathlib import Path
import subprocess
import sys
import uuid

from cursor_run import LABEL, bounded_run, capture, docker, validate_image, TransportError
import codex_probe as probe


def validate_report(value, phase, previous, model, effort):
    if not isinstance(value, dict) or set(value) - {"stage", "engine", "phase", "status", "countBefore", "countAfter", "model", "reasoningEffort", "capabilities", "events", "usage", "code"}:
        raise ValueError("report_shape")
    if (value.get("stage"), value.get("engine"), value.get("phase"), value.get("model"), value.get("reasoningEffort")) != ("V3", "codex-app-server", phase, model, effort):
        raise ValueError("report_identity")
    before, after = value.get("countBefore"), value.get("countAfter")
    if type(before) is not int or type(after) is not int or before != previous or not 0 <= before <= after <= 8:
        raise ValueError("report_count")
    if value.get("status") not in {"completed", "unknown", "failed", "blocked"}:
        raise ValueError("report_status")
    capabilities = value.get("capabilities")
    if not isinstance(capabilities, dict) or set(capabilities) != set(probe.CAPABILITY_NAMES):
        raise ValueError("report_capabilities")
    for name, capability in capabilities.items():
        if not isinstance(capability, dict) or set(capability) != {"status", "assertions", "evidence"}:
            raise ValueError("capability_shape")
        if capability["status"] not in {"not_run", "observed", "unknown", "failed", "unsupported"}:
            raise ValueError("capability_status")
        assertions = capability["assertions"]
        if not isinstance(assertions, dict) or set(assertions) - probe.CAPABILITY_ASSERTIONS[name] or any(type(item) is not bool for item in assertions.values()):
            raise ValueError("capability_assertions")
        evidence = capability["evidence"]
        if not isinstance(evidence, dict) or set(evidence) - {"threadIds", "turnIds", "itemIds"}:
            raise ValueError("capability_evidence")
        for ids in evidence.values():
            if not isinstance(ids, list) or len(ids) > 16 or any(not probe.safe_id(identifier) for identifier in ids):
                raise ValueError("capability_ids")
        if capability["status"] == "observed":
            if set(assertions) != probe.CAPABILITY_ASSERTIONS[name] or any(item is not (key not in probe.FALSE_ASSERTIONS) for key, item in assertions.items()):
                raise ValueError("unproven_capability")
            if not evidence.get("threadIds") or not evidence.get("turnIds"):
                raise ValueError("missing_execution_evidence")
    state = {"capabilities": capabilities, "events": value.get("events"), "usage": value.get("usage"), "turnStartCount": after}
    if not isinstance(state["events"], list) or not isinstance(state["usage"], list) or len(state["events"]) > probe.MAX_EVENTS or len(state["usage"]) > probe.MAX_USAGE:
        raise ValueError("report_bounded_arrays")
    normalized = probe.build_report(phase, value["status"], before, state, model, effort, value.get("code"))
    if normalized != value:
        raise ValueError("unsafe_report_fields")
    if value["status"] == "completed":
        if after - before != probe.EXPECTED_TURNS[phase] or any(capabilities[name]["status"] != "observed" for name in probe.PHASE_CAPABILITIES[phase]):
            raise ValueError("incomplete_phase")
    return after


def phase_command(context, image, run_id, state_volume, auth_volume, phase, model, effort, prior=0):
    return ["docker", "--context", context, "run", "--rm", "--name", run_id + "-" + phase,
            "--label", LABEL + "=" + run_id, "--read-only", "--network", "bridge",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
            "--memory", "512m", "--memory-swap", "512m", "--cpus", "1", "--user", "10001:10001",
            "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777",
            "--mount", "type=volume,source=" + state_volume + ",target=/state",
            "--mount", "type=volume,source=" + auth_volume + ",target=/state/auth",
            "--env", "HOME=/state/home", "--env", "CODEX_HOME=/state/auth/home/codex",
            "--entrypoint", "python3", image, "-B", "/opt/probe/codex_probe.py", "--phase", phase,
            "--model", model, "--effort", effort, "--state", "/state/codex-probe/checkpoint.json",
            "--prior-turn-starts", str(prior)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("context", "image", "auth-volume", "auth-run-id", "model", "effort"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument('--prior-turn-starts', required=True, type=int, choices=range(4))
    args = parser.parse_args()
    if args.output.exists():
        parser.error("output_exists")
    image = validate_image(args.context, args.image)
    if not args.auth_run_id.startswith("hl240-codex-auth-") or args.auth_volume != args.auth_run_id + "-state":
        raise ValueError("fresh_auth_volume_required")
    if capture(args.context, "volume", "inspect", args.auth_volume, "--format", '{{index .Labels "' + LABEL + '"}}') != args.auth_run_id:
        raise ValueError("auth_volume_owner")
    if capture(args.context, "ps", "-q", "--filter", "volume=" + args.auth_volume):
        raise ValueError("auth_volume_in_use")
    run_id = "hl240-codex-" + uuid.uuid4().hex
    volume = run_id + "-state"
    capture(args.context, "volume", "create", "--label", LABEL + "=" + run_id, volume)
    report = {"stage": "V3", "engine": "codex-app-server", "evidenceKind": "live_provider_probe",
              "imageId": image, "context": args.context, "runId": run_id, "authMode": "chatgpt",
              "model": args.model, "reasoningEffort": args.effort, "status": "unknown", "phases": [],
              "budget": {"priorTurnStarts": args.prior_turn_starts, "maxTurnStarts": 8, "phaseDeadlineSeconds": 180, "automaticProbeRetries": False},
              "cleanupVerified": False, "authVolumeRetained": True}
    count = args.prior_turn_starts
    try:
        for phase in probe.PHASES:
            try:
                command = phase_command(args.context, image, run_id, volume, args.auth_volume, phase, args.model, args.effort, args.prior_turn_starts)
                returncode, raw = bounded_run(command, "")
                outcome = json.loads(raw)
                count = validate_report(outcome, phase, count, args.model, args.effort)
                if returncode and outcome["status"] == "completed":
                    raise ValueError("process_status_mismatch")
            except (TransportError, ValueError, OSError, subprocess.SubprocessError):
                outcome = {"phase": phase, "status": "unknown", "countAfter": "unknown", "code": "native_probe_transport_or_report_unknown"}
            report["phases"].append(outcome)
            if outcome["status"] != "completed":
                report["status"] = outcome["status"]
                break
        else:
            report["status"] = "completed"
    finally:
        for container in capture(args.context, "ps", "-aq", "--filter", "label=" + LABEL + "=" + run_id).split():
            docker(args.context, "rm", "--force", container, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if capture(args.context, "volume", "inspect", volume, "--format", '{{index .Labels "' + LABEL + '"}}') != run_id:
            raise ValueError("state_volume_owner")
        docker(args.context, "volume", "rm", volume, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        report["cleanupVerified"] = True
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    with args.output.open("x") as handle:
        handle.write(encoded)
    print(encoded, end="")
    return 0 if report["status"] == "completed" else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, subprocess.SubprocessError):
        print("codex_probe_failed: safe runner error", file=sys.stderr)
        raise SystemExit(1)
