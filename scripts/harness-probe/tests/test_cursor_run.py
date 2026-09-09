import importlib.util
import io
import copy
import json
from pathlib import Path
import subprocess
import sys
from unittest.mock import patch
import unittest

MODULE = Path(__file__).parents[1] / "cursor_run.py"
SPEC = importlib.util.spec_from_file_location("cursor_run", MODULE)
cursor_run = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cursor_run)


def completed(phase, before=0, after=None):
    if after is None:
        after = before + {"create_markers": 2, "tool_steer_cancel": 2, "resume_deny": 1}[phase]
    capabilities = {name: {"status": "not_run", "assertions": {}, "evidence": {}} for name in cursor_run.ASSERTIONS}
    for name in cursor_run.CAPABILITIES[phase]:
        assertions = {key: True for key in cursor_run.ASSERTIONS[name]}
        if name == "markerDialogs":
            assertions["secondReplyLeaks"] = False
        if name == "steerWhileLongTool":
            assertions["steerOutcome"] = "complete_delivered"
        if name == "cancelTerminal":
            assertions["terminalStatus"] = "cancelled"
        capabilities[name] = {"status": "observed", "assertions": assertions, "evidence": {"runId": "run-1", "eventSeqs": [1], **({"callIds": ["call-1", "call-2", "call-3"]} if name == "toolLifecycle" else {})}}
    return {"stage": "V2", "engine": "cursor-sdk", "phase": phase, "status": "completed", "sdkSendCountBefore": before, "sdkSendCountAfter": after, "capabilities": capabilities}


class CursorRunTest(unittest.TestCase):
    def test_rejects_untrusted_image_before_key(self):
        with self.assertRaisesRegex(ValueError, "image_must_be_full_sha256"):
            cursor_run.validate_image("x", "latest")
        with patch.object(cursor_run, "capture", side_effect=["sha256:" + "a" * 64, "untrusted"]):
            with self.assertRaisesRegex(ValueError, "untrusted_probe_image"):
                cursor_run.validate_image("x", "sha256:" + "a" * 64)

    def test_rejects_forged_completed_and_budget_discontinuity(self):
        forged = completed("create_markers")
        forged["capabilities"]["markerDialogs"]["assertions"]["firstReplyMatches"] = "true"
        with self.assertRaisesRegex(ValueError, "capability_assertions_invalid"):
            cursor_run.validate_phase_report(forged, "create_markers", 0)
        discontinuity = completed("create_markers", before=2)
        with self.assertRaisesRegex(ValueError, "report_budget_invalid"):
            cursor_run.validate_phase_report(discontinuity, "create_markers", 0)

    def test_rejects_count_overflow_and_false_deny(self):
        overflow = completed("create_markers", after=9)
        with self.assertRaisesRegex(ValueError, "report_budget_invalid"):
            cursor_run.validate_phase_report(overflow, "create_markers", 0)
        deny = completed("resume_deny")
        deny["capabilities"]["denyAfterResume"]["assertions"]["shellCanaryAbsent"] = False
        with self.assertRaisesRegex(ValueError, "capability_assertions_invalid"):
            cursor_run.validate_phase_report(deny, "resume_deny", 0)

    def test_timeout_and_oversized_output_are_unknown(self):
        outcome, count, failure = cursor_run.run_phase([], "key", "create_markers", 0, runner=lambda *_: (_ for _ in ()).throw(cursor_run.TransportError("timeout")))
        self.assertEqual(("unknown", 0, "unknown"), (outcome["status"], count, failure))
        outcome, count, failure = cursor_run.run_phase([], "key", "create_markers", 0, runner=lambda *_: (0, "x" * (cursor_run.OUTPUT_LIMIT + 1)))
        self.assertEqual(("unknown", 0, "unknown"), (outcome["status"], count, failure))

    def test_false_evidence_cannot_pass(self):
        for phase in cursor_run.PHASES:
            valid = completed(phase)
            cursor_run.validate_phase_report(valid, phase, 0)
            for name in cursor_run.CAPABILITIES[phase]:
                for assertion, expected in valid["capabilities"][name]["assertions"].items():
                    if type(expected) is not bool:
                        continue
                    forged = copy.deepcopy(valid)
                    forged["capabilities"][name]["assertions"][assertion] = not expected
                    with self.subTest(name=name, assertion=assertion), self.assertRaises(ValueError):
                        cursor_run.validate_phase_report(forged, phase, 0)

    def test_raw_extra_fields_and_unsafe_values_rejected(self):
        for mutate in (
            lambda report: report.update(raw="provider response"),
            lambda report: report.update(events=[{"seq": 1, "raw": "response"}]),
            lambda report: report.update(usage=[{"raw": "secret"}]),
            lambda report: report["capabilities"]["restartResume"].update(raw="response"),
            lambda report: report["capabilities"]["markerDialogs"]["evidence"].update(eventSeqs=["raw"]),
            lambda report: report.update(sdkSendCountBefore=False),
        ):
            forged = completed("create_markers")
            mutate(forged)
            with self.assertRaises(ValueError):
                cursor_run.validate_phase_report(forged, "create_markers", 0)

    def test_known_failure_remains_reviewable(self):
        value = completed("create_markers")
        value["status"] = "unknown"
        value["capabilities"]["markerDialogs"]["status"] = "unknown"
        value["capabilities"]["markerDialogs"]["assertions"]["firstReplyMatches"] = False
        outcome, count, failure = cursor_run.run_phase([], "key", "create_markers", 0, runner=lambda *_: (1, json.dumps(value)))
        self.assertEqual((outcome, count, failure), (value, 2, "unknown"))

    def test_phase_container_is_labelled_for_watchdog_cleanup(self):
        command = cursor_run.phase_command("test", "sha256:123", "owned", "owned-state", "create_markers")
        self.assertEqual(command[command.index("--label") + 1], cursor_run.LABEL + "=owned")
        self.assertEqual(command[command.index("--name") + 1], "owned-create_markers")
        self.assertIn("--tmpfs", command)
        self.assertFalse(any("type=bind" in arg or "API_KEY" in arg or "docker.sock" in arg for arg in command))

    def test_actual_transport_is_bounded_and_times_out(self):
        for script, timeout, limit in (
            ("import time; time.sleep(5)", 0.05, 1024),
            ("import sys; sys.stdout.write('x'*100000); sys.stdout.flush()", 5, 1024),
        ):
            with self.assertRaises(cursor_run.TransportError):
                cursor_run.bounded_run([sys.executable, "-c", script], "synthetic-key", timeout=timeout, output_limit=limit)


if __name__ == "__main__":
    unittest.main()
