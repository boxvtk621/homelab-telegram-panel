"""Offline release/deployment regression tests. No daemon, registry or secrets."""
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import deploy
import release

REV = "a" * 40
IMAGE = deploy.REPOSITORY + "@sha256:" + "b" * 64


class FixtureInstaller(deploy.Installer):
    def __init__(self, state):
        super().__init__(state, {})
        self.running = None
        self.starts = []
        self.failures = set()
        self.fail_preflight = False

    def containers(self):
        return [self.running["container"]] if self.running else []

    def preflight(self, record):
        deploy.require(not self.fail_preflight, "SYNTHETIC_PREFLIGHT_FAILURE")

    def validate_recovery(self, current, attempted):
        allowed = [x["manifest"]["version"] for x in (current, attempted) if x]
        deploy.require(not self.running or self.running.get("manifest", {}).get("version") in allowed, "UNMANAGED_RECOVERY_CONTAINER")

    def start(self, record):
        version = record["manifest"]["version"]
        self.starts.append(version)
        self.running = dict(record, container=version)
        deploy.require(version not in self.failures, "SYNTHETIC_STARTUP_FAILURE")
        return self.running

    def healthy(self, record):
        deploy.require(self.running == record, "SYNTHETIC_IDENTITY_FAILURE")
        return record

    def compose(self, record, *args):
        if args == ("rm", "--stop", "--force", "panel"):
            self.running = None
            return ""
        raise AssertionError(args)


class ReleaseTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state = self.root / "installation"
        self.state.mkdir(mode=0o700)
        self.a = self.root / "release-a"
        self.b = self.root / "release-b"
        release.package("v0.1.0-rc.1", REV, IMAGE, self.a)
        release.package("v0.1.0-rc.2", REV, IMAGE, self.b)
        self.installer = FixtureInstaller(self.state)

    def ledger(self):
        return deploy.read_json(self.state / "deployment.json")

    def test_bundle_integrity_version_image_and_duplicate_keys(self):
        self.assertEqual(deploy.bundle(self.a)["image"], IMAGE)
        for value in ["latest", "v1.0", "v01.0.0", "v1.0.0;echo", "v1.0.0-rc.0"]:
            with self.assertRaises(deploy.DeployError):
                release.identity(value, REV)
        for image in ["ghcr.io/other/panel@sha256:" + "b" * 64, deploy.REPOSITORY + ":latest"]:
            with self.assertRaises(deploy.DeployError):
                release.package("v0.1.0", REV, image, self.root / str(len(list(self.root.iterdir()))))
        (self.a / "compose.yaml").write_text("services: {unsafe: {privileged: true}}")
        with self.assertRaisesRegex(deploy.DeployError, "CHECKSUM"):
            deploy.bundle(self.a)
        with self.assertRaisesRegex(deploy.DeployError, "DUPLICATE"):
            json.loads('{"schema":1,"schema":2}', object_pairs_hook=deploy.pairs)

    def test_install_update_same_release_and_rollback(self):
        i = self.installer
        self.assertEqual(i.perform(self.a, allow_interrupt=True), "DEPLOYED")
        self.assertEqual(i.perform(self.a, allow_interrupt=True), "ALREADY_CURRENT")
        self.assertEqual(len(i.starts), 1)
        self.assertEqual(i.perform(self.b, allow_interrupt=True), "DEPLOYED")
        self.assertEqual(self.ledger()["previous"]["manifest"]["version"], "v0.1.0-rc.1")
        self.assertEqual(i.perform(None, rollback=True, allow_interrupt=True), "ROLLED_BACK")
        self.assertEqual(i.running["manifest"]["version"], "v0.1.0-rc.1")
        self.assertIsNone(self.ledger()["pending"])

    def test_preflight_does_not_stop_existing_container(self):
        i = self.installer
        i.perform(self.a, allow_interrupt=True)
        i.fail_preflight = True
        with self.assertRaises(deploy.DeployError):
            i.perform(self.b, allow_interrupt=True)
        self.assertEqual(i.starts, ["v0.1.0-rc.1"])
        self.assertIsNone(self.ledger()["pending"])

    def test_failed_update_restores_previous_and_failed_first_install_cleans_own_service(self):
        i = self.installer
        i.failures.add("v0.1.0-rc.1")
        with self.assertRaisesRegex(deploy.DeployError, "ROLLBACK_COMPLETED"):
            i.perform(self.a, allow_interrupt=True)
        self.assertIsNone(i.running)
        self.assertIsNone(self.ledger()["current"])
        i.failures.clear()
        i.perform(self.a, allow_interrupt=True)
        i.failures.add("v0.1.0-rc.2")
        with self.assertRaisesRegex(deploy.DeployError, "ROLLBACK_COMPLETED"):
            i.perform(self.b, allow_interrupt=True)
        self.assertEqual(i.running["manifest"]["version"], "v0.1.0-rc.1")
        self.assertEqual(self.ledger()["current"], i.running)

    def test_failed_rollback_keeps_recovery_journal_and_blocks_new_apply(self):
        i = self.installer
        i.perform(self.a, allow_interrupt=True)
        i.failures.update(["v0.1.0-rc.1", "v0.1.0-rc.2"])
        with self.assertRaisesRegex(deploy.DeployError, "STATE_PENDING"):
            i.perform(self.b, allow_interrupt=True)
        self.assertIsNotNone(self.ledger()["pending"])
        with self.assertRaisesRegex(deploy.DeployError, "INCOMPLETE_DEPLOYMENT"):
            i.perform(self.a, allow_interrupt=True)
        i.failures.clear()
        self.assertEqual(i.perform(None, rollback=True, allow_interrupt=True), "RECOVERED")
        self.assertEqual(self.ledger()["current"], i.running)

    def test_foreign_container_interruption_ack_and_lock(self):
        i = self.installer
        with self.assertRaisesRegex(deploy.DeployError, "ACKNOWLEDGE"):
            i.perform(self.a)
        self.assertEqual(i.starts, [])
        i.running = {"container": "foreign"}
        with self.assertRaisesRegex(deploy.DeployError, "UNMANAGED"):
            i.perform(self.a, allow_interrupt=True)
        with deploy.locked(self.state):
            with self.assertRaisesRegex(deploy.DeployError, "LOCKED"):
                with deploy.locked(self.state):
                    self.fail("duplicate lock")

    def test_configuration_is_literal_private_and_not_copied_to_state(self):
        config = self.root / "panel.env"
        value = (release.ROOT / "deploy/panel.env.example").read_text().replace("PANEL_CURSOR_API_KEY=\n", "PANEL_CURSOR_API_KEY=synthetic-$not-expanded\n")
        config.write_text(value)
        config.chmod(0o600)
        self.assertEqual(deploy.configuration(config)["PANEL_CURSOR_API_KEY"], "synthetic-$not-expanded")
        config.chmod(0o644)
        with self.assertRaisesRegex(deploy.DeployError, "OWNER_ONLY"):
            deploy.configuration(config)
        config.chmod(0o600)
        config.write_text(value + "COMPOSE_FILE=foreign.yaml\n")
        with self.assertRaisesRegex(deploy.DeployError, "INVALID_CONFIG"):
            deploy.configuration(config)
        self.installer.perform(self.a, allow_interrupt=True)
        self.assertNotIn("synthetic-", (self.state / "deployment.json").read_text())

    def test_docker_boundary_does_not_inherit_context_or_log_keys(self):
        i = deploy.Installer(self.state, {"PANEL_CURSOR_API_KEY": "synthetic-secret"})
        with patch.dict(os.environ, {"DOCKER_HOST": "tcp://foreign", "COMPOSE_FILE": "foreign.yaml"}):
            with patch("subprocess.run", return_value=subprocess.CompletedProcess([], 1, "synthetic-secret", "synthetic-secret")) as run:
                with self.assertRaisesRegex(deploy.DeployError, "^DOCKER_COMMAND_FAILED$"):
                    i.command(["version"])
                args, kwargs = run.call_args
                self.assertEqual(args[0][:3], ["docker", "--context", "default"])
                self.assertNotIn("DOCKER_HOST", kwargs["env"])
                self.assertNotIn("COMPOSE_FILE", kwargs["env"])
                self.assertNotIn("synthetic-secret", str(args))

    def test_release_ref_mismatch_and_stable_main_guard(self):
        with patch("release.command", side_effect=[REV, "c" * 40]):
            with self.assertRaisesRegex(deploy.DeployError, "TAG_MISMATCH"):
                release.check("v0.1.0-rc.1", REV)
        with patch("release.command", return_value=REV), patch("subprocess.run") as run:
            release.check("v0.1.0", REV)
            run.assert_called_once_with(["git", "merge-base", "--is-ancestor", REV, "origin/main"], check=True)

    def test_cli_error_redacts_raw_exception(self):
        output = io.StringIO()
        with patch.object(os, "environ", {}), patch.object(deploy.sys, "argv", ["deploy.py", "verify"]), patch("deploy.bundle", side_effect=ValueError("synthetic-secret")), contextlib.redirect_stderr(output):
            self.assertEqual(deploy.main(), 1)
        self.assertNotIn("synthetic-secret", output.getvalue())

    def test_copy_interruption_is_retryable_and_config_change_reapplies(self):
        i = self.installer
        with patch("shutil.copyfile", side_effect=OSError("synthetic interrupted copy")):
            with self.assertRaises(OSError):
                i.perform(self.a, allow_interrupt=True)
        self.assertEqual(i.perform(self.a, allow_interrupt=True), "DEPLOYED")
        i.config["PANEL_CURSOR_API_KEY"] = "synthetic-new-key"
        self.assertEqual(i.perform(self.a, allow_interrupt=True), "DEPLOYED")
        self.assertEqual(len(i.starts), 2)
        self.assertNotIn("synthetic-new-key", (self.state / "deployment.json").read_text())

    def test_config_rotation_preserves_previous_release_for_rollback(self):
        i = self.installer
        i.perform(self.a, allow_interrupt=True)
        i.perform(self.b, allow_interrupt=True)
        i.config["PANEL_CURSOR_API_KEY"] = "synthetic-rotated-key"
        i.perform(self.b, allow_interrupt=True)
        self.assertEqual(self.ledger()["previous"]["manifest"]["version"], "v0.1.0-rc.1")
        self.assertEqual(i.perform(None, rollback=True, allow_interrupt=True), "ROLLED_BACK")
        self.assertEqual(i.running["manifest"]["version"], "v0.1.0-rc.1")

    def test_pending_recovery_rejects_foreign_container_and_corrupt_bundle(self):
        i = self.installer
        i.perform(self.a, allow_interrupt=True)
        target = i.remember(self.b)
        deploy.atomic_json(self.state / "deployment.json", dict(self.ledger(), pending=target))
        current = i.running
        i.running = {"container": "foreign"}
        with self.assertRaisesRegex(deploy.DeployError, "UNMANAGED"):
            i.perform(None, rollback=True, allow_interrupt=True)
        self.assertIsNotNone(self.ledger()["pending"])
        i.running = current
        (i.location(target) / "compose.yaml").write_text("corrupt")
        with self.assertRaisesRegex(deploy.DeployError, "CHECKSUM"):
            i.perform(None, rollback=True, allow_interrupt=True)
        self.assertEqual(i.starts, ["v0.1.0-rc.1"])

    def test_probe_timeout_cleans_only_its_unique_container(self):
        i = deploy.Installer(self.state, {"PANEL_CURSOR_API_KEY": "synthetic-secret"})
        record = {"manifest": deploy.bundle(self.a)}
        calls = []
        def command(args, **kwargs):
            calls.append(args)
            if args[0] == "run":
                raise deploy.DeployError("DOCKER_UNAVAILABLE_OR_TIMEOUT")
            return ""
        with patch.object(i, "command", side_effect=command):
            with self.assertRaisesRegex(deploy.DeployError, "TIMEOUT"):
                i.probe(record, "validate")
        name = calls[0][calls[0].index("--name") + 1]
        self.assertEqual(calls[1], ["rm", "--force", name])
        self.assertIn(name, calls[2][-1])
        self.assertNotIn("synthetic-secret", str(calls))

    def test_publish_checks_both_records_and_release_asset_readback(self):
        records = self.root / "records"
        records.mkdir()
        for arch in ("amd64", "arm64"):
            (records / (arch + ".json")).write_text(json.dumps({"version": "v0.1.0-rc.1", "revision": REV, "arch": arch, "image": IMAGE}))
        registry = {"digest": "sha256:" + "c" * 64, "manifests": [{"platform": {"os": "linux", "architecture": arch}} for arch in ("amd64", "arm64")]}
        published = {"tagName": "v0.1.0-rc.1", "isDraft": False, "isPrerelease": True, "assets": [{"name": name} for name in deploy.FILES | {"release.json", "SHA256SUMS"}]}
        with patch("release.check"), patch("release.guard"), patch("release.command", side_effect=[json.dumps(registry), json.dumps(published)]), patch("subprocess.run") as run, contextlib.redirect_stdout(io.StringIO()):
            release.publish("v0.1.0-rc.1", REV, records, self.root / "publish-bundle")
            self.assertEqual(run.call_args_list[0].args[0][:4], ["docker", "buildx", "imagetools", "create"])
            self.assertEqual(run.call_args_list[1].args[0][:3], ["gh", "release", "create"])
        (records / "arm64.json").write_text(json.dumps({"version": "v9.9.9", "revision": REV, "arch": "arm64", "image": IMAGE}))
        with patch("release.check"), patch("release.guard"), patch("subprocess.run") as run:
            with self.assertRaisesRegex(deploy.DeployError, "BUILD_RECORD_MISMATCH"):
                release.publish("v0.1.0-rc.1", REV, records, self.root / "bad-bundle")
            run.assert_not_called()


class PushTest(unittest.TestCase):
    def exercise(self, results, expected_error=None, image_ids=None):
        calls = []
        ids = iter(image_ids) if image_ids else None

        def run(args, **kwargs):
            calls.append((args, kwargs))
            if args[1:3] == ["image", "inspect"]:
                return subprocess.CompletedProcess(args, 0, next(ids) if ids else "sha256:" + "a" * 64, "")
            self.assertEqual(args, ["docker", "push", deploy.REPOSITORY + ":v0.1.0-rc.2-amd64"])
            result = results.pop(0)
            if isinstance(result, Exception):
                raise result
            return subprocess.CompletedProcess(args, *result)

        output = io.StringIO()
        with patch("release.subprocess.run", side_effect=run), patch("release.time.sleep") as sleep, contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            if expected_error:
                with self.assertRaisesRegex(deploy.DeployError, expected_error):
                    release.push("v0.1.0-rc.2", REV, "amd64")
            else:
                release.push("v0.1.0-rc.2", REV, "amd64")
            sleeps = [call.args[0] for call in sleep.call_args_list]
        self.assertNotIn("synthetic-secret", output.getvalue())
        self.assertNotIn("raw-exception", output.getvalue())
        for _, kwargs in calls:
            self.assertTrue(kwargs["capture_output"])
            self.assertTrue(kwargs["text"])
            self.assertLessEqual(kwargs["timeout"], 300)
        pushes = [args for args, _ in calls if args[1] == "push"]
        self.assertEqual(len(pushes), output.getvalue().count("REGISTRY_PUSH_ATTEMPT"))
        self.assertEqual("REGISTRY_PUSH_CONFIRMED" in output.getvalue(), expected_error is None)
        return pushes, sleeps

    def test_push_success_without_retry(self):
        pushes, sleeps = self.exercise([(0, "synthetic-secret", "")])
        self.assertEqual(len(pushes), 1)
        self.assertEqual(sleeps, [])

    def test_push_missing_blob_recovers_same_tag_without_rebuild(self):
        pushes, sleeps = self.exercise([(1, "synthetic-secret", "unknown blob"),
                                       (1, "", "MANIFEST_BLOB_UNKNOWN"), (0, "", "")])
        self.assertEqual(len(pushes), 3)
        self.assertTrue(all(args == pushes[0] for args in pushes))
        self.assertEqual(sleeps, [5, 15])

    def test_push_missing_blob_exhaustion_fails(self):
        pushes, sleeps = self.exercise([(1, "", "BLOB_UNKNOWN synthetic-secret")] * 3, "RETRIES_EXHAUSTED")
        self.assertEqual(len(pushes), 3)
        self.assertEqual(sleeps, [5, 15])

    def test_push_auth_and_other_errors_fail_fast(self):
        for diagnostic in ["401", "403", "unauthorized", "forbidden", "denied", "authentication required", "insufficient_scope"]:
            with self.subTest(diagnostic=diagnostic):
                pushes, sleeps = self.exercise([(1, "unknown blob synthetic-secret", diagnostic)], "AUTH_OR_PERMISSION_FAILED")
                self.assertEqual(len(pushes), 1)
                self.assertEqual(sleeps, [])
        pushes, sleeps = self.exercise([(1, "synthetic-secret", "raw-exception connection failed")], "REGISTRY_PUSH_FAILED")
        self.assertEqual(len(pushes), 1)
        self.assertEqual(sleeps, [])

    def test_push_timeout_fails_without_raw_output_or_retry(self):
        pushes, sleeps = self.exercise([subprocess.TimeoutExpired("synthetic-secret", 300, output="raw-exception")], "REGISTRY_PUSH_TIMEOUT")
        self.assertEqual(len(pushes), 1)
        self.assertEqual(sleeps, [])

    def test_push_changed_or_missing_local_image_rejected(self):
        original = "sha256:" + "a" * 64
        changed = "sha256:" + "b" * 64
        pushes, sleeps = self.exercise([], "TESTED_IMAGE_CHANGED", [original, changed])
        self.assertEqual(pushes, [])
        self.assertEqual(sleeps, [])
        pushes, sleeps = self.exercise([(1, "", "unknown blob")], "TESTED_IMAGE_CHANGED", [original, original, changed])
        self.assertEqual(len(pushes), 1)
        self.assertEqual(sleeps, [5])
        pushes, sleeps = self.exercise([], "LOCAL_IMAGE_UNAVAILABLE", ["invalid synthetic-secret"])
        self.assertEqual(pushes, [])
        self.assertEqual(sleeps, [])


if __name__ == "__main__":
    unittest.main()
