import fcntl
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import component_deploy
import deploy
import panel_registry_supervisor as supervisor


PANEL_ID = "a" * 64
CURSOR_ID = "b" * 64


def manifest(name):
    return {"component": name, "version": "v1.2.3", "revision": "d" * 40,
            "image": "example.invalid/" + name + "@sha256:" + "c" * 64}


class FakeInstaller:
    def __init__(self, root, runtime):
        self.root = root
        self.runtime = runtime
        self.config = {
            "components": {"panel": {"service": "panel"}, "cursor": {"service": "cursor"}}
        }
        self.ledger = {"components": {
            "panel": {"current": manifest("panel"), "previous": None},
            "cursor": {"current": manifest("cursor"), "previous": None},
        }, "pending": None, "config_sha256": "fixture"}
        self.ids = {"panel": PANEL_ID, "cursor": CURSOR_ID}
        self.states = {PANEL_ID: {"Running": True, "Pid": 1234},
                       CURSOR_ID: {"Running": True, "Pid": 2345}}
        self.mutations = []
        self.inspections = []
        self.health_checks = []
        self.change_other = False
        self.unknown = False

    def compose(self, *args, override=None):
        self.assertEqualOverride(override)
        service = args[-1]
        name = "panel" if service == "panel" else "cursor"
        return self.ids[name]

    def assertEqualOverride(self, override):
        if override is not None:
            assert override == self.root / "panel.override.json"

    def compose_mutation(self, *args, override=None):
        self.mutations.append((args, override))
        if self.unknown:
            raise component_deploy.ContainerMutationUnknown("CONTAINER_MUTATION_OUTCOME_UNKNOWN")
        if args[0] == "stop":
            self.states[PANEL_ID] = {"Running": False, "Pid": 0}
        else:
            self.states[PANEL_ID] = {"Running": True, "Pid": 4321}
        if self.change_other:
            self.ids["cursor"] = "d" * 64

    def inspect(self, name, target, container=None):
        self.inspections.append((name, target, container))
        if not self.states[container]["Running"]:
            raise deploy.DeployError("RUNTIME_IMAGE_MISMATCH")

    def health(self, name, target):
        self.health_checks.append((name, target))


class PanelRegistrySupervisorTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name).resolve()
        self.root = self.base / "cd"
        self.runtime = self.base / "run"
        self.root.mkdir(mode=0o700)
        self.runtime.mkdir(mode=0o700)
        self.lock = self.root / "deploy.lock"
        self.lock.write_bytes(b"")
        self.lock.chmod(0o600)
        self.fake = FakeInstaller(self.root, self.runtime)
        self.write_override()

    def write_override(self):
        path = self.root / "panel.override.json"
        path.write_text(json.dumps({"services": {"panel": {
            "image": self.fake.ledger["components"]["panel"]["current"]["image"]}}}) + "\n")
        path.chmod(0o600)

    def write_proof(self, state, pid=None):
        value = {"schema": 1, "service": "panel", "state": state, "pid": pid}
        status = self.runtime / supervisor.STATUS_NAME
        status.write_text(json.dumps(value, separators=(",", ":")) + "\n")
        status.chmod(0o600)
        if pid is not None:
            pid_file = self.runtime / supervisor.PID_NAME
            pid_file.write_text(str(pid) + "\n")
            pid_file.chmod(0o600)

    def invoke(self, action):
        def docker_run(*args):
            return json.dumps([{"State": self.fake.states[args[-1]]}])

        with patch.object(supervisor, "require_root"), \
             patch.object(component_deploy, "Installer", return_value=self.fake), \
             patch.object(component_deploy, "run", side_effect=docker_run):
            return supervisor.supervise(action, self.root, self.runtime)

    def test_normal_stop_and_start_write_exact_private_proof(self):
        self.runtime.rmdir()
        stopped = self.invoke("stop")
        self.assertEqual(stopped, {"status": "PANEL_STOPPED", "pid": None,
                                   "idempotent": False})
        self.assertFalse((self.runtime / supervisor.PID_NAME).exists())
        self.assertEqual(json.loads((self.runtime / supervisor.STATUS_NAME).read_text()),
                         {"schema": 1, "service": "panel", "state": "stopped", "pid": None})
        running = self.invoke("start")
        self.assertEqual(running, {"status": "PANEL_RUNNING", "pid": 4321,
                                   "idempotent": False})
        self.assertEqual((self.runtime / supervisor.PID_NAME).read_text(), "4321\n")
        self.assertEqual(json.loads((self.runtime / supervisor.STATUS_NAME).read_text()),
                         {"schema": 1, "service": "panel", "state": "running", "pid": 4321})
        for path in (self.runtime, self.runtime / supervisor.PID_NAME,
                     self.runtime / supervisor.STATUS_NAME):
            expected = 0o700 if path == self.runtime else 0o600
            self.assertEqual(path.stat().st_mode & 0o777, expected)
            self.assertEqual((path.stat().st_uid, path.stat().st_gid),
                             (os.geteuid(), os.getegid()))
        self.assertEqual(self.fake.mutations[0][0], ("stop", "panel"))
        self.assertEqual(self.fake.mutations[1][0],
                         ("up", "-d", "--no-deps", "--pull", "never", "panel"))
        self.assertIn(("panel", self.fake.ledger["components"]["panel"]["current"]),
                      self.fake.health_checks)

    def test_lock_contention_fails_before_runtime_read(self):
        descriptor = os.open(self.lock, os.O_RDWR)
        self.addCleanup(os.close, descriptor)
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        with self.assertRaisesRegex(deploy.DeployError, "DEPLOY_LOCK_BUSY"):
            self.invoke("stop")
        self.assertEqual(self.fake.mutations, [])

    def test_other_container_change_fails_closed(self):
        self.fake.change_other = True
        with self.assertRaisesRegex(deploy.DeployError, "OTHER_COMPONENT_CONTAINER_CHANGED"):
            self.invoke("stop")
        self.assertFalse((self.runtime / supervisor.STATUS_NAME).exists())

    def test_mutation_unknown_never_claims_desired_state(self):
        self.fake.unknown = True
        with self.assertRaisesRegex(component_deploy.ContainerMutationUnknown,
                                    "CONTAINER_MUTATION_OUTCOME_UNKNOWN"):
            self.invoke("stop")
        self.assertFalse((self.runtime / supervisor.STATUS_NAME).exists())

        self.fake.states[PANEL_ID] = {"Running": False, "Pid": 0}
        self.write_proof("stopped")
        with self.assertRaisesRegex(component_deploy.ContainerMutationUnknown,
                                    "CONTAINER_MUTATION_OUTCOME_UNKNOWN"):
            self.invoke("start")
        self.assertFalse((self.runtime / supervisor.STATUS_NAME).exists())
        self.assertFalse((self.runtime / supervisor.PID_NAME).exists())

    def test_stale_or_mismatched_proof_rejects_without_mutation(self):
        self.write_proof("stopped")
        with self.assertRaisesRegex(deploy.DeployError, "SUPERVISOR_PROOF_RUNTIME_DRIFT"):
            self.invoke("stop")
        self.assertEqual(self.fake.mutations, [])

        (self.runtime / supervisor.STATUS_NAME).unlink()
        self.fake.states[PANEL_ID] = {"Running": False, "Pid": 0}
        self.write_proof("running", 9999)
        with self.assertRaisesRegex(deploy.DeployError, "SUPERVISOR_PROOF_RUNTIME_DRIFT"):
            self.invoke("start")
        self.assertEqual(self.fake.mutations, [])

    def test_idempotent_readback_requires_matching_runtime_and_proof(self):
        self.write_proof("running", 1234)
        running = self.invoke("start")
        self.assertTrue(running["idempotent"])
        self.assertEqual(self.fake.mutations, [])

        (self.runtime / supervisor.STATUS_NAME).unlink()
        (self.runtime / supervisor.PID_NAME).unlink()
        self.fake.states[PANEL_ID] = {"Running": False, "Pid": 0}
        self.write_proof("stopped")
        stopped = self.invoke("stop")
        self.assertTrue(stopped["idempotent"])
        self.assertEqual(self.fake.mutations, [])

    def test_pending_ledger_is_rejected(self):
        self.fake.ledger["pending"] = {"phase": "replace_started"}
        with self.assertRaisesRegex(deploy.DeployError,
                                    "INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR"):
            self.invoke("stop")
        self.assertEqual(self.fake.mutations, [])

    def test_authoritative_stopped_readback_checks_exact_runtime_identity(self):
        self.fake.states[PANEL_ID] = {"Running": False, "Pid": 0}
        current = self.fake.ledger["components"]["panel"]["current"]
        data = {"State": self.fake.states[PANEL_ID], "Image": "sha256:image",
                "Config": {"User": "10001:10001"},
                "HostConfig": {"ReadonlyRootfs": True, "Privileged": False,
                               "CapDrop": ["ALL"],
                               "SecurityOpt": ["no-new-privileges:true"]}}
        image = {"Id": "sha256:image", "RepoDigests": [current["image"]],
                 "Os": "linux", "Architecture": "amd64",
                 "Config": {"User": "10001:10001", "Labels": {
                     "org.opencontainers.image.version": current["version"],
                     "org.opencontainers.image.revision": current["revision"]}}}

        def docker_run(*args):
            return json.dumps([image if args[1:3] == ("image", "inspect") else data])

        with patch.object(component_deploy, "Installer", return_value=self.fake), \
             patch.object(component_deploy, "run", side_effect=docker_run):
            self.assertEqual(supervisor.verify_panel_stopped(self.root, PANEL_ID), PANEL_ID)
            self.fake.states[PANEL_ID] = {"Running": True, "Pid": 1234}
            data["State"] = self.fake.states[PANEL_ID]
            with self.assertRaisesRegex(deploy.DeployError,
                                        "PANEL_STOPPED_RUNTIME_MISMATCH"):
                supervisor.verify_panel_stopped(self.root, PANEL_ID)


if __name__ == "__main__":
    unittest.main()
