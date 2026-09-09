"""One final budgeted native-policy check after seven recorded Cursor sends."""
import argparse
import json
from pathlib import Path
import subprocess
import sys
import uuid
from cursor_run import (LABEL, bounded_run, capture, docker, phase_command,
                        validate_image, validate_key_file, TransportError)

ASSERTIONS = {"freshProcessResume", "resumedAgentMatchesCheckpoint", "policyHeaderObserved",
              "exactMcpPolicyOnEveryRequest", "terminalFinished", "shellCanaryRequested",
              "canaryInitiallyAbsent", "shellCanaryAbsent", "forbiddenBuiltinAbsent"}


def validate(value, phase, prior=7):
    base = {"stage", "phase", "status", "sdkSendCountBefore", "sdkSendCountAfter"}
    if not isinstance(value, dict) or value.get("stage") != "V2_deny" or value.get("phase") != phase:
        raise ValueError("report_identity")
    if 'diagnostic' in value:
        diagnostic = value['diagnostic']
        if set(value) != base | {'diagnostic'} or value['status'] != 'unknown' or not isinstance(diagnostic, dict) or set(diagnostic) != {'code', 'operation', 'errorClass'}:
            raise ValueError('diagnostic_shape')
        if diagnostic['code'] not in {'native_probe_failed', 'phase_deadline'} or diagnostic['operation'] not in {'prepare', 'resume', 'send', 'stream_wait'} or diagnostic['errorClass'] not in {'Error', 'TypeError', 'RangeError', 'ReferenceError', 'ConnectError', 'NetworkError', 'APIError', 'AbortError', 'UnknownError', 'CursorSdkError', 'CursorAgentError', 'AuthenticationError', 'RateLimitError', 'ConfigurationError', 'AgentBusyError', 'UnknownAgentError', 'AgentNotFoundError'}:
            raise ValueError('unsafe_diagnostic')
        if type(value['sdkSendCountBefore']) is not int or value['sdkSendCountBefore'] != prior or type(value['sdkSendCountAfter']) is not int or not prior <= value['sdkSendCountAfter'] <= prior + (phase == 'resume'):
            raise ValueError('diagnostic_budget')
        return
    expected = base if phase == "prepare" else base | {"assertions", "evidence"}
    if set(value) != expected or type(value["sdkSendCountBefore"]) is not int or value["sdkSendCountBefore"] != prior:
        raise ValueError("report_fields")
    if type(value["sdkSendCountAfter"]) is not int or value["sdkSendCountAfter"] != (prior if phase == "prepare" else prior + 1):
        raise ValueError("report_budget")
    if phase == "prepare":
        if value["status"] != "prepared":
            raise ValueError("prepare_incomplete")
        return
    assertions, evidence = value["assertions"], value["evidence"]
    if not isinstance(assertions, dict) or set(assertions) != ASSERTIONS or any(type(item) is not bool for item in assertions.values()):
        raise ValueError("report_assertions")
    if not isinstance(evidence, dict) or set(evidence) != {"policyRequests", "exactMcpPolicyOnEveryRequest"}:
        raise ValueError("report_evidence")
    if type(evidence["policyRequests"]) is not int or not 0 <= evidence["policyRequests"] <= 10000 or type(evidence["exactMcpPolicyOnEveryRequest"]) is not bool:
        raise ValueError("report_evidence")
    if value["status"] not in {"observed", "unknown", "failed"}:
        raise ValueError("report_status")
    if value["status"] == "observed" and (not all(assertions.values()) or evidence["policyRequests"] < 1 or not evidence["exactMcpPolicyOnEveryRequest"]):
        raise ValueError("unproven_denial")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("context", "image"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--key-file", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument('--prior-sdk-sends', required=True, type=int, choices=(7, 8, 9))
    args = parser.parse_args()
    if args.output.exists():
        parser.error("output_exists")
    image = validate_image(args.context, args.image)
    key = validate_key_file(args.key_file)
    run_id = "hl240-cursor-deny-" + uuid.uuid4().hex
    volume = run_id + "-state"
    capture(args.context, "volume", "create", "--label", LABEL + "=" + run_id, volume)
    report = {"stage": "V2_deny", "engine": "cursor-sdk", "model": "composer-2.5", "imageId": image,
              "runId": run_id, "status": "unknown", "phases": [], "priorSdkSends": args.prior_sdk_sends, "maxSdkSends": args.prior_sdk_sends + 1}
    try:
        for phase in ("prepare", "resume"):
            command = phase_command(args.context, image, run_id, volume, phase)
            position = command.index("/opt/probe/cursor_probe.mjs")
            command[position:] = ["/opt/probe/cursor_deny_probe.mjs", phase, str(args.prior_sdk_sends)]
            try:
                code, raw = bounded_run(command, key)
                value = json.loads(raw)
                validate(value, phase, args.prior_sdk_sends)
                if code and value["status"] in {"prepared", "observed"}:
                    raise ValueError("process_status_mismatch")
            except (TransportError, ValueError, OSError, subprocess.SubprocessError):
                value = {"phase": phase, "status": "unknown", "code": "policy_probe_transport_or_output_unknown", "sdkSendCountAfter": "unknown"}
            report["phases"].append(value)
            if value["status"] not in {"prepared", "observed"}:
                break
        if len(report["phases"]) == 2:
            report["status"] = report["phases"][-1]["status"]
    finally:
        key = ""
        for container in capture(args.context, "ps", "-aq", "--filter", "label=" + LABEL + "=" + run_id).split():
            docker(args.context, "rm", "--force", container, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if capture(args.context, "volume", "inspect", volume, "--format", '{{index .Labels "' + LABEL + '"}}') != run_id:
            raise ValueError("volume_owner_mismatch")
        docker(args.context, "volume", "rm", volume, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    report["cleanupVerified"] = True
    text = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    with args.output.open("x") as handle:
        handle.write(text)
    print(text, end="")
    return 0 if report["status"] == "observed" else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, subprocess.SubprocessError):
        print("cursor_deny_probe_failed: safe runner error", file=sys.stderr)
        raise SystemExit(1)
