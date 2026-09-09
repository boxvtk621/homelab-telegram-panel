"""V1 offline evidence inside an isolated Linux container. No model calls."""
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent
SDK_CHECKS = (
    "two_marker_dialogs", "tool_start_and_result", "steer_during_tool",
    "cancel_terminal", "restart_resume", "deny_after_resume",
)


def checked(argv, timeout=30):
    result = subprocess.run(argv, cwd=ROOT, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError("offline_check_failed")
    return result.stdout


def required_fields(schema, name):
    # Generated stable schemas are version-specific; never infer a PASS from prose.
    if schema.get("title") == name:
        return set(schema.get("required", []))
    for definitions in ("definitions", "$defs"):
        if name in schema.get(definitions, {}):
            return set(schema[definitions][name].get("required", []))
    return None


def check_codex_schemas(folder):
    required = {
        "TurnSteerParams": {"threadId", "expectedTurnId", "input"},
        "TurnInterruptParams": {"threadId", "turnId"},
        "ThreadResumeParams": {"threadId"},
    }
    found = {}
    files = sorted(folder.rglob("*.json"))
    digest = hashlib.sha256()
    for path in files:
        data = path.read_bytes()
        digest.update(str(path.relative_to(folder)).encode() + b"\0" + data)
        schema = json.loads(data)
        for name, expected in required.items():
            fields = required_fields(schema, name)
            if fields is not None:
                if not expected.issubset(fields):
                    raise RuntimeError("codex_schema_mismatch")
                found[name] = sorted(fields)
    if set(found) != set(required):
        raise RuntimeError("codex_schema_missing")
    return {"status": "passed", "requiredFields": found, "schemaSha256": digest.hexdigest()}


def main():
    started = time.monotonic()
    report = {
        "reportVersion": 1, "stage": "V1", "evidenceKind": "offline_fixture",
        "providerCalls": 0, "providerValidation": "not_run",
        "mandatoryCapabilities": {
            engine: {name: "not_run" for name in SDK_CHECKS}
            for engine in ("cursor", "codex")
        },
        "runtime": {"system": platform.system(), "machine": platform.machine(),
                    "python": platform.python_version(), "uid": os.getuid()},
    }
    try:
        if platform.system() != "Linux" or os.getuid() == 0:
            raise RuntimeError("isolated_linux_user_required")
        if any(os.environ.get(k) for k in ("OPENAI_API_KEY", "CURSOR_API_KEY", "YOUTRACK_TOKEN")):
            raise RuntimeError("credentials_forbidden_in_offline_probe")
        report["packages"] = json.loads(checked(["node", str(ROOT / "preflight.mjs")]))
        version = checked(["codex", "--version"]).strip()
        if version != "codex-cli 0.153.4":
            raise RuntimeError("codex_version_mismatch")
        report["codexVersion"] = version
        with tempfile.TemporaryDirectory(prefix="hl240-schema-") as folder:
            checked(["codex", "app-server", "generate-json-schema", "--out", folder])
            report["codexStableSchema"] = check_codex_schemas(Path(folder))
        if not (ROOT / "tools/fixture_server.py").is_file() or not (ROOT / "tests/test_tools.py").is_file():
            raise RuntimeError("fixture_files_missing")
        tests = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", str(ROOT / "tests"), "-v"],
                               cwd=ROOT, capture_output=True, text=True, timeout=60)
        count = re.search(r"Ran (\d+) tests?", tests.stderr)
        if tests.returncode or not count or int(count[1]) < 3 or "skipped=" in tests.stderr:
            raise RuntimeError("fixture_tests_failed")
        report["fixtureTests"] = {"status": "passed", "count": int(count[1])}
        report["source"] = {"lockSha256": hashlib.sha256((ROOT / "package-lock.json").read_bytes()).hexdigest()}
        report["status"] = "passed"
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as exc:
        report["status"] = "failed"
        # Never emit argv, SDK exception details or environment values.
        allowed_codes = {"isolated_linux_user_required", "credentials_forbidden_in_offline_probe",
                         "codex_version_mismatch", "codex_schema_mismatch", "codex_schema_missing",
                         "offline_check_failed", "fixture_files_missing", "fixture_tests_failed"}
        report["errorCode"] = str(exc) if str(exc) in allowed_codes else "offline_probe_error"
    report["elapsedSeconds"] = round(time.monotonic() - started, 3)
    print(json.dumps(report, sort_keys=True))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
