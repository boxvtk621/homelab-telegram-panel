import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("probe_run", ROOT / "run.py")
run = importlib.util.module_from_spec(spec)
spec.loader.exec_module(run)


class RunnerTests(unittest.TestCase):
    def report(self):
        cases = ("two_marker_dialogs", "tool_start_and_result", "steer_during_tool",
                 "cancel_terminal", "restart_resume", "deny_after_resume")
        return {"status": "passed", "stage": "V1", "evidenceKind": "offline_fixture",
                "providerCalls": 0, "providerValidation": "not_run",
                "mandatoryCapabilities": {e: dict.fromkeys(cases, "not_run") for e in ("cursor", "codex")}}

    def test_offline_cannot_claim_provider_pass_or_omit_engine(self):
        run.validate_report(self.report())
        for change in ("pass", "omit_engine", "omit_case", "call", "failed"):
            report = copy.deepcopy(self.report())
            if change == "pass":
                report["mandatoryCapabilities"]["codex"]["cancel_terminal"] = "passed"
            elif change == "omit_engine":
                del report["mandatoryCapabilities"]["cursor"]
            elif change == "omit_case":
                del report["mandatoryCapabilities"]["cursor"]["deny_after_resume"]
            elif change == "call":
                report["providerCalls"] = 1
            else:
                report["status"] = "failed"
            with self.subTest(change=change), self.assertRaises(ValueError):
                run.validate_report(report)

    def test_runtime_has_no_host_mount_key_or_network(self):
        args = run.run_args("sha256:123", "test-run", "test-run-state")
        self.assertEqual(args[args.index("--network") + 1], "none")
        self.assertEqual(args[args.index("--user") + 1], "10001:10001")
        self.assertEqual(args[args.index("--mount") + 1], "type=volume,source=test-run-state,target=/state")
        self.assertIn("--read-only", args)
        self.assertNotIn("--privileged", args)
        self.assertFalse(any("API_KEY" in x or "docker.sock" in x or "type=bind" in x for x in args))

    def test_cleanup_failure_never_persists_pass(self):
        report = self.report()
        report["source"] = {"lockSha256": hashlib.sha256((ROOT / "package-lock.json").read_bytes()).hexdigest()}
        run_id = "hl240-probe-" + "b" * 32

        def capture(context, *args):
            if args[:2] == ("image", "inspect"):
                return "sha256:" + "a" * 64 if args[-1] == "{{.Id}}" else "sdk-feasibility-probe"
            if args[:2] == ("volume", "inspect"):
                return run_id
            return ""

        def docker(context, *args, **kwargs):
            if args[:2] == ("volume", "rm"):
                raise subprocess.CalledProcessError(1, ["docker", "volume", "rm"])
            return SimpleNamespace(stdout=json.dumps(report))

        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / "report.json"
            with patch.object(run, "capture", side_effect=capture), patch.object(run, "docker", side_effect=docker), \
                    patch.object(run.uuid, "uuid4", return_value=SimpleNamespace(hex="b" * 32)), \
                    patch.object(run.sys, "argv", ["run.py", "smoke", "--context", "test", "--output", str(output)]), \
                    patch.object(run.sys, "stdout", new_callable=io.StringIO) as stdout:
                with self.assertRaises(subprocess.CalledProcessError):
                    run.main()
                self.assertFalse(output.exists())
                self.assertEqual("", stdout.getvalue())


if __name__ == "__main__":
    unittest.main()
