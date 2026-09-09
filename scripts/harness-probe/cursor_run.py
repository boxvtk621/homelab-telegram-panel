#!/usr/bin/env python3
"""Bounded host runner for the synthetic Cursor V2 probe."""
import argparse
import json
from pathlib import Path
import re
import stat
import subprocess
import sys
import threading
import time
import uuid

LABEL = "ru.h1-cloud.hl240.run"
PHASES = ("create_markers", "tool_steer_cancel", "resume_deny")
IMAGE_PURPOSE_LABEL = "ru.h1-cloud.hl240.purpose"
IMAGE_PURPOSE = "sdk-feasibility-probe"
OUTPUT_LIMIT = 64 * 1024
WATCHDOG_SECONDS = 195
SAFE_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")
CAPABILITIES = {
    "create_markers": ("markerDialogs",),
    "tool_steer_cancel": ("toolLifecycle", "steerWhileLongTool", "cancelTerminal"),
    "resume_deny": ("restartResume", "denyAfterResume"),
}
ASSERTIONS = {
    "markerDialogs": {"distinctAgentIds", "firstTerminalFinished", "secondTerminalFinished", "firstMarkerToolObserved", "firstReplyMatches", "secondReplyLeaks"},
    "toolLifecycle": {"markerTimelineComplete", "longTimelineComplete", "errorTimelineComplete"},
    "steerWhileLongTool": {"longStartObserved", "steerOutcome"},
    "cancelTerminal": {"cancelStartObserved", "terminalStatus"},
    "restartResume": {"resumedAgentMatchesCheckpoint", "retainedMarkerToolObserved", "retainedMarkerReplyMatches"},
    "denyAfterResume": {"systemToolsObserved", "forbiddenBuiltinsAbsent", "shellCanaryAttempted", "shellCanaryAbsent"},
}


class TransportError(Exception):
    pass


def docker(context, *args, **kwargs):
    return subprocess.run(["docker", "--context", context, *args], check=True, text=True, **kwargs)


def capture(context, *args):
    return docker(context, *args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL).stdout.strip()


def validate_image(context, image):
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", image):
        raise ValueError("image_must_be_full_sha256")
    inspect_id = capture(context, "image", "inspect", image, "--format", "{{.Id}}")
    purpose = capture(context, "image", "inspect", image, "--format", "{{index .Config.Labels \"ru.h1-cloud.hl240.purpose\"}}")
    if inspect_id != image or purpose != IMAGE_PURPOSE:
        raise ValueError("untrusted_probe_image")
    return inspect_id


def validate_key_file(key_file):
    info = key_file.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError("credential_requires_private_regular_file")
    key = key_file.read_text()
    if not 16 <= len(key.rstrip("\n")) <= 4096 or "\n" in key.rstrip("\n") or key.count("\n") > 1 or "\r" in key:
        raise ValueError("invalid_credential_input")
    return key.rstrip("\n")


def _reader(stream, limit, result):
    while True:
        chunk = stream.read(4096)
        if not chunk:
            return
        if len(result["data"]) + len(chunk) > limit:
            result["overflow"] = True
            return
        result["data"] += chunk


def bounded_run(command, key, timeout=WATCHDOG_SECONDS, output_limit=OUTPUT_LIMIT, popen=subprocess.Popen):
    process = popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    stdout, stderr = {"data": b"", "overflow": False}, {"data": b"", "overflow": False}
    threads = [threading.Thread(target=_reader, args=(process.stdout, output_limit, stdout), daemon=True),
               threading.Thread(target=_reader, args=(process.stderr, output_limit, stderr), daemon=True)]
    for thread in threads:
        thread.start()
    try:
        process.stdin.write(key.encode())
        process.stdin.close()
        deadline = time.monotonic() + timeout
        while process.poll() is None:
            if stdout["overflow"] or stderr["overflow"]:
                process.kill()
                raise TransportError("output_limit")
            if time.monotonic() >= deadline:
                process.kill()
                raise TransportError("timeout")
            time.sleep(0.01)
    finally:
        if process.poll() is None:
            process.kill()
        for thread in threads:
            thread.join(timeout=1)
        process.wait(timeout=5)
        process.stdout.close()
        process.stderr.close()
    if stdout["overflow"] or stderr["overflow"]:
        raise TransportError("output_limit")
    if key and (key.encode() in stdout["data"] or key.encode() in stderr["data"]):
        raise TransportError("credential_in_output")
    return process.returncode, stdout["data"].decode("utf-8")


def _safe_id(value):
    return isinstance(value, str) and bool(SAFE_ID.fullmatch(value))


def validate_phase_report(value, phase, previous_after):
    if not isinstance(value, dict) or value.get("stage") != "V2" or value.get("engine") != "cursor-sdk" or value.get("phase") != phase:
        raise ValueError("report_identity_invalid")
    if set(value) - {"stage", "engine", "phase", "status", "sdkSendCountBefore", "sdkSendCountAfter", "capabilities", "events", "usage", "code"}:
        raise ValueError("report_extra_fields")
    status = value.get("status")
    if status not in {"completed", "unknown", "failed", "blocked"}:
        raise ValueError("report_status_invalid")
    if "code" in value and value["code"] not in {"stdin_key_invalid", "stdin_key_too_large", "checkpoint_invalid", "checkpoint_missing_agent", "tool_input_invalid", "phase_deadline_exceeded", "sdk_send_budget_exhausted", "phase_invalid", "arguments_invalid", "sdk_error"}:
        raise ValueError("report_code_invalid")
    before, after = value.get("sdkSendCountBefore"), value.get("sdkSendCountAfter")
    if type(before) is not int or type(after) is not int or before != previous_after or not 0 <= before <= after <= 8:
        raise ValueError("report_budget_invalid")
    expected_sends = {"create_markers": 2, "tool_steer_cancel": 2, "resume_deny": 1}
    if status == "completed" and after - before != expected_sends[phase]:
        raise ValueError("report_budget_invalid")
    capabilities = value.get("capabilities")
    if not isinstance(capabilities, dict) or set(capabilities) != set(ASSERTIONS):
        raise ValueError("report_capabilities_invalid")
    for name in ASSERTIONS:
        capability = capabilities[name]
        if not isinstance(capability, dict) or set(capability) != {"status", "assertions", "evidence"}:
            raise ValueError("capability_shape_invalid")
        observed = capability["status"] == "observed"
        if capability["status"] not in {"observed", "unknown", "failed", "unsupported", "not_run"}:
            raise ValueError("capability_status_invalid")
        if status == "completed" and name in CAPABILITIES[phase] and not observed:
            raise ValueError("required_capability_not_observed")
        assertions = capability["assertions"]
        if not isinstance(assertions, dict) or (set(assertions) != ASSERTIONS[name] and (observed or assertions)):
            raise ValueError("capability_assertions_invalid")
        for key, assertion in assertions.items():
            if key == "steerOutcome":
                if assertion not in {"complete_delivered", "revert_to_followup", "unsupported", "unknown"} or (observed and assertion != "complete_delivered"):
                    raise ValueError("capability_assertions_invalid")
            elif key == "terminalStatus":
                if assertion not in {"cancelled", "finished", "error", "unknown"} or (observed and assertion != "cancelled"):
                    raise ValueError("capability_assertions_invalid")
            elif type(assertion) is not bool or (observed and assertion is not (key != "secondReplyLeaks")):
                raise ValueError("capability_assertions_invalid")
        evidence = capability.get("evidence", {})
        if not isinstance(evidence, dict) or set(evidence) - {"runId", "eventSeqs", "callIds"}:
            raise ValueError("capability_evidence_invalid")
        if evidence and (not _safe_id(evidence.get("runId")) or not isinstance(evidence.get("eventSeqs"), list) or len(evidence["eventSeqs"]) > 64 or any(type(seq) is not int or seq < 1 for seq in evidence["eventSeqs"])):
            raise ValueError("capability_evidence_invalid")
        if observed and (not evidence or not evidence["eventSeqs"]):
            raise ValueError("capability_evidence_invalid")
        if "callIds" in evidence and (not isinstance(evidence["callIds"], list) or len(evidence["callIds"]) > 64 or not all(_safe_id(call_id) for call_id in evidence["callIds"])):
            raise ValueError("tool_timeline_evidence_invalid")
        if name == "toolLifecycle" and observed and len(set(evidence.get("callIds", []))) < 3:
            raise ValueError("tool_timeline_evidence_invalid")
    events = value.get("events", [])
    if not isinstance(events, list) or len(events) > 64:
        raise ValueError("events_invalid")
    last_seq = 0
    for event in events:
        if not isinstance(event, dict) or set(event) - {"seq", "type", "name", "status", "callId", "correlationId", "scopeId", "runId", "advertisedTools", "markerMatches"}:
            raise ValueError("event_shape_invalid")
        seq = event.get("seq")
        if type(seq) is not int or seq <= last_seq:
            raise ValueError("event_sequence_invalid")
        last_seq = seq
        for field in {"type", "name", "status", "callId", "correlationId", "scopeId", "runId"} & set(event):
            if event[field] != "" and not _safe_id(event[field]):
                raise ValueError("event_value_invalid")
        if "markerMatches" in event and type(event["markerMatches"]) is not bool:
            raise ValueError("event_value_invalid")
        advertised = event.get("advertisedTools", [])
        if not isinstance(advertised, list) or len(advertised) > 16 or any(tool not in {"mcp", "shell", "read", "edit", "grep", "glob", "ls", "task", "webSearch", "webFetch", "delete"} for tool in advertised):
            raise ValueError("event_value_invalid")
    usage = value.get("usage", [])
    if not isinstance(usage, list) or len(usage) > 64:
        raise ValueError("usage_invalid")
    for item in usage:
        if not isinstance(item, dict) or set(item) - {"inputTokens", "outputTokens", "totalTokens", "cachedTokens"} or any(type(count) is not int or not 0 <= count <= 10**12 for count in item.values()):
            raise ValueError("usage_invalid")
    return after


def run_phase(command, key, phase, previous_after, runner=bounded_run):
    try:
        returncode, raw = runner(command, key)
        if len(raw.encode()) > OUTPUT_LIMIT:
            raise TransportError("output_limit")
        value = json.loads(raw)
        after = validate_phase_report(value, phase, previous_after)
        if returncode and value["status"] == "completed":
            raise ValueError("process_status_mismatch")
        return value, after, None if value["status"] == "completed" else value["status"]
    except (TransportError, UnicodeDecodeError, json.JSONDecodeError, ValueError, OSError, subprocess.SubprocessError):
        return {"phase": phase, "status": "unknown", "code": "probe_transport_or_output_unknown", "sdkSendCountAfter": "unknown"}, previous_after, "unknown"


def phase_command(context, image_id, run_id, volume, phase, prior_sdk_sends=0):
    return ["docker", "--context", context, "run", "--rm", "-i", "--name", run_id + "-" + phase,
            "--label", LABEL + "=" + run_id, "--read-only", "--network", "bridge",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
            "--memory", "512m", "--memory-swap", "512m", "--cpus", "1", "--user", "10001:10001",
            "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777",
            "--mount", "type=volume,source=" + volume + ",target=/state", "--env", "HOME=/state/home",
            "--env", "XDG_CONFIG_HOME=/state/home/config", "--entrypoint", "node", image_id,
            "/opt/probe/cursor_probe.mjs", "--phase", phase, "--state", "/state/cursor-probe/checkpoint.json",
            "--prior-sdk-sends", str(prior_sdk_sends)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--key-file", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--prior-sdk-sends", required=True, type=int, choices=range(9), help="Already consumed sends from earlier evidence; never reset the approved budget")
    args = parser.parse_args()
    if args.output.exists():
        parser.error("output_exists")
    image_id = validate_image(args.context, args.image)
    key = validate_key_file(args.key_file)
    run_id = "hl240-cursor-" + uuid.uuid4().hex
    volume = run_id + "-state"
    report = {"reportVersion": 1, "stage": "V2", "evidenceKind": "live_provider_probe", "engine": "cursor-sdk", "model": "composer-2.5", "imageId": image_id, "context": args.context, "runId": run_id, "phases": [], "status": "failed", "cleanupVerified": False,
              "budget": {"maxSdkSends": 8, "priorSdkSends": args.prior_sdk_sends, "automaticRetries": False, "phaseDeadlineSeconds": 180}, "codexValidation": "not_run"}
    try:
        capture(args.context, "volume", "create", "--label", LABEL + "=" + run_id, volume)
        previous_after = args.prior_sdk_sends
        for phase in PHASES:
            command = phase_command(args.context, image_id, run_id, volume, phase, args.prior_sdk_sends)
            outcome, previous_after, failed = run_phase(command, key, phase, previous_after)
            report["phases"].append(outcome)
            if failed:
                report["status"] = failed
                break
        if len(report["phases"]) == len(PHASES) and all(phase["status"] == "completed" for phase in report["phases"]):
            report["status"] = "completed"
    finally:
        key = ""
        for container in capture(args.context, "ps", "-aq", "--filter", "label=" + LABEL + "=" + run_id).split():
            docker(args.context, "rm", "--force", container, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if capture(args.context, "volume", "inspect", volume, "--format", "{{index .Labels \"" + LABEL + "\"}}") != run_id:
            raise ValueError("volume_owner_mismatch")
        docker(args.context, "volume", "rm", volume, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        report["cleanupVerified"] = True
    with args.output.open("x") as handle:
        handle.write(json.dumps(report, ensure_ascii=False) + "\n")
    print(json.dumps(report, ensure_ascii=False))
    return 0 if report["status"] == "completed" else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, subprocess.SubprocessError):
        print("cursor_probe_failed: safe runner error", file=sys.stderr)
        raise SystemExit(1)
