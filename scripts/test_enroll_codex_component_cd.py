import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import component_release as release
import deploy
import enroll_codex_component_cd as enrollment


CURSOR_ID = "40000000-0000-4000-8000-000000000001"
CODEX_ID = "40000000-0000-4000-8000-000000000002"


def manifest(component, version="v0.2.0"):
    adapter = {"panel": None, "cursor": "1.0.31", "codex": "0.153.4"}[component]
    return {
        "schema": 1, "component": component, "version": version,
        "revision": {"panel": "a", "cursor": "b", "codex": "c"}[component] * 40,
        "image": release.COMPONENTS[component]["image"] + "@sha256:" +
                 {"panel": "d", "cursor": "e", "codex": "f"}[component] * 64,
        "platform": "linux/amd64", "adapter_version": adapter,
        "state_compatibility": {"panel": "1", "cursor": "2", "codex": "3"}[component] * 64,
    }


class InjectedCrash(BaseException):
    pass


class FakeRouter:
    def __init__(self):
        self.cursor = {"mode": "eligible", "stateVersion": 14, "generation": 5,
                       "identityEpoch": 1, "adapterKind": "cursor", "adapterVersion": "1.0.31"}
        self.codex = {"mode": "sealed", "stateVersion": 1, "generation": 0,
                      "identityEpoch": 0, "adapterKind": "codex", "adapterVersion": "",
                      "operationId": "bootstrap"}
        self.calls = []

    def status(self):
        return {"schema": 1, "ownerId": "owner", "registryVersion": 1,
                "registrySHA256": "9" * 64,
                "nodes": {CURSOR_ID: copy.deepcopy(self.cursor), CODEX_ID: copy.deepcopy(self.codex)}}

    def transition_many(self, action, current, operation_id, identities=None):
        self.calls.append((action, copy.deepcopy(current), operation_id, copy.deepcopy(identities)))
        if action != "activate" or current != {CODEX_ID: self.codex} or operation_id != "bootstrap":
            raise AssertionError("activation must target only exact Codex")
        identity = identities[CODEX_ID]
        self.codex = enrollment.host.RouterControl.projected(
            "activate", self.codex, operation_id, identity)
        return {CODEX_ID: copy.deepcopy(self.codex)}


class EnrollmentTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name).resolve()
        self.base.chmod(0o700)

    @staticmethod
    def write(path, raw):
        path.write_bytes(raw)
        path.chmod(0o600)

    def fixture(self, name):
        root = self.base / name
        root.mkdir(mode=0o700)
        compose = root / "compose.yaml"
        candidate = root / "compose.codex-candidate.yaml"
        env = root / "images.env"
        self.write(compose, b"name: alpha\nservices:\n  panel: {}\n  harness: {}\n")
        self.write(candidate, compose.read_bytes() +
                   ("  codex:\n    image: " + manifest("codex")["image"] + "\n").encode())
        self.write(env, b"PANEL_IMAGE=exact\nCURSOR_IMAGE=exact\nCODEX_IMAGE=exact\n")
        router = root / "router-state"
        router.mkdir(mode=0o700)
        router_lock = router / "router.lock"
        self.write(router_lock, b"")
        lock_descriptor = os.open(router_lock, os.O_RDWR)
        fcntl.flock(lock_descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.addCleanup(os.close, lock_descriptor)
        self.write(root / "deploy.lock", b"")
        config = {
            "project": "homelab-panel-alpha", "compose": str(compose),
            "env_file": str(env), "router_socket": str(router / "control.sock"),
            "components": {
                "panel": {"service": "panel"},
                "cursor": {"service": "harness", "url": "https://harness:18443",
                           "node_id": CURSOR_ID, "actor_id": "owner"},
            },
        }
        config_raw = enrollment.encode_json(config)
        self.write(root / "config.json", config_raw)
        ledger = {
            "components": {
                "panel": {"current": manifest("panel"), "previous": None},
                "cursor": {"current": manifest("cursor"), "previous": None},
            },
            "pending": None,
            "config_sha256": enrollment.fingerprint(config_raw, compose.read_bytes(), env.read_bytes()),
        }
        self.write(root / "deployment.json", enrollment.encode_json(ledger))
        journals = root / "journals"
        journals.mkdir(mode=0o700)
        return {
            "root": root, "compose": compose, "candidate": candidate, "env": env,
            "router_lock": router_lock, "router_lock_descriptor": lock_descriptor,
            "consumer_pid": root / "component-cd.pid", "journal": journals / "codex",
            "source_config": copy.deepcopy(config), "source_ledger": copy.deepcopy(ledger),
            "source_compose": compose.read_bytes(), "router": FakeRouter(),
        }

    def evidence(self, fixture, allow_activated=False):
        route = copy.deepcopy(fixture["router"].codex)
        if route["mode"] != "sealed" and not allow_activated:
            raise enrollment.EnrollmentError("CODEX_NOT_EXACT_SEALED_BOOTSTRAP")
        return {
            "containers": {"panel": "a" * 64, "cursor": "b" * 64, "codex": "c" * 64},
            "cursorIdentity": {"node": CURSOR_ID, "epoch": 1, "registry": 1,
                               "kind": "cursor", "version": "1.0.31"},
            "codexIdentity": {"node": CODEX_ID, "epoch": 1, "registry": 1,
                              "kind": "codex", "version": "0.153.4"},
            "cursorRoute": copy.deepcopy(fixture["router"].cursor), "codexRoute": route,
            "registryVersion": 1, "registrySHA256": "9" * 64,
        }

    def invoke(self, fixture, action="apply", direction=None):
        def observe(_root, _config, _ledger, _compose, _manifest, allow_activated=False):
            return self.evidence(fixture, allow_activated)

        with patch.object(enrollment, "require_root"), \
             patch.object(enrollment.cd, "download", return_value=manifest("codex")), \
             patch.object(enrollment, "router_control", return_value=fixture["router"]), \
             patch.object(enrollment, "runtime_evidence", side_effect=observe), \
             patch.object(enrollment.host, "mutate", side_effect=AssertionError("container mutation forbidden")):
            return enrollment.enroll(
                action, direction, fixture["root"], fixture["journal"], fixture["router_lock"],
                fixture["consumer_pid"], fixture["candidate"],
                hashlib.sha256(fixture["source_compose"]).hexdigest(),
                hashlib.sha256(fixture["candidate"].read_bytes()).hexdigest(),
                "codex-v0.2.0", CODEX_ID,
            )

    def assert_target(self, fixture):
        config = deploy.read_json(fixture["root"] / "config.json")
        ledger = deploy.read_json(fixture["root"] / "deployment.json")
        self.assertEqual({key: config["components"][key] for key in ("panel", "cursor")},
                         fixture["source_config"]["components"])
        self.assertEqual({key: ledger["components"][key] for key in ("panel", "cursor")},
                         fixture["source_ledger"]["components"])
        self.assertEqual(config["components"]["codex"], {
            "service": "codex", "url": "https://codex:18443",
            "node_id": CODEX_ID, "actor_id": "owner",
        })
        self.assertEqual(ledger["components"]["codex"],
                         {"current": manifest("codex"), "previous": None})
        self.assertIsNone(ledger["pending"])
        self.assertEqual(fixture["compose"].read_bytes(), fixture["candidate"].read_bytes())
        self.assertEqual(ledger["config_sha256"], enrollment.host.Installer(fixture["root"]).fingerprint())

    def test_apply_preserves_panel_cursor_and_activates_only_codex(self):
        fixture = self.fixture("success")
        result = self.invoke(fixture)
        self.assertEqual(result, {"status": "CODEX_COMPONENT_CD_ENROLLED",
                                  "direction": "target", "activated": True})
        self.assert_target(fixture)
        self.assertEqual(len(fixture["router"].calls), 1)
        self.assertEqual(set(fixture["router"].calls[0][1]), {CODEX_ID})
        state = deploy.read_json(fixture["journal"] / "state.json")
        self.assertEqual((state["direction"], state["phase"]), ("target", "completed"))

    def test_crash_after_each_file_replace_recovers_forward_without_apply_retry(self):
        for index, point in enumerate(("compose.after_replace", "config.after_replace",
                                       "ledger.after_replace")):
            with self.subTest(point=point):
                fixture = self.fixture("forward-" + str(index))

                def crash(name):
                    if name == point:
                        raise InjectedCrash(name)

                with patch.object(enrollment.pair, "checkpoint", side_effect=crash), \
                     self.assertRaises(InjectedCrash):
                    self.invoke(fixture)
                with self.assertRaisesRegex(enrollment.EnrollmentError,
                                            "ENROLLMENT_JOURNAL_ALREADY_EXISTS"):
                    self.invoke(fixture)
                result = self.invoke(fixture, "recover", "target")
                self.assertEqual(result["status"], "CODEX_COMPONENT_CD_ENROLLED")
                self.assert_target(fixture)

    def test_explicit_rollback_restores_exact_source_from_partial_state(self):
        fixture = self.fixture("rollback")

        def crash(name):
            if name == "config.after_replace":
                raise InjectedCrash(name)

        with patch.object(enrollment.pair, "checkpoint", side_effect=crash), \
             self.assertRaises(InjectedCrash):
            self.invoke(fixture)
        result = self.invoke(fixture, "recover", "rollback")
        self.assertEqual(result, {"status": "CODEX_COMPONENT_CD_RECOVERED",
                                  "direction": "rollback", "activated": False})
        self.assertEqual(deploy.read_json(fixture["root"] / "config.json"), fixture["source_config"])
        self.assertEqual(deploy.read_json(fixture["root"] / "deployment.json"), fixture["source_ledger"])
        self.assertEqual(fixture["compose"].read_bytes(), fixture["source_compose"])
        self.assertEqual(fixture["router"].calls, [])

    def test_crash_after_activation_is_read_back_without_replay(self):
        fixture = self.fixture("activation-lost-ack")

        def crash(name):
            if name == "activation.after_request":
                raise InjectedCrash(name)

        with patch.object(enrollment, "checkpoint", side_effect=crash), \
             self.assertRaises(InjectedCrash):
            self.invoke(fixture)
        self.assertEqual(len(fixture["router"].calls), 1)
        original_update = enrollment.update_state

        def crash_during_recovery(root, plan_raw, direction, phase):
            original_update(root, plan_raw, direction, phase)
            if phase == "activation-pending":
                raise InjectedCrash(phase)

        with patch.object(enrollment, "update_state", side_effect=crash_during_recovery), \
             self.assertRaises(InjectedCrash):
            self.invoke(fixture, "recover", "target")
        state = deploy.read_json(fixture["journal"] / "state.json")
        self.assertEqual((state["direction"], state["phase"]),
                         ("target", "activation-pending"))
        self.invoke(fixture, "recover", "target")
        self.assertEqual(len(fixture["router"].calls), 1)
        self.assert_target(fixture)
        self.invoke(fixture, "recover", "target")
        self.assertEqual(len(fixture["router"].calls), 1)
        state = deploy.read_json(fixture["journal"] / "state.json")
        self.assertEqual((state["direction"], state["phase"]), ("target", "completed"))
        with self.assertRaisesRegex(enrollment.EnrollmentError,
                                    "CODEX_ALREADY_ACTIVATED_ROLLBACK_FORBIDDEN"):
            self.invoke(fixture, "recover", "rollback")

        fixture["router"].codex = copy.deepcopy(
            deploy.read_json(fixture["journal"] / "plan.json")["runtime"]["codexRoute"])
        with self.assertRaisesRegex(enrollment.EnrollmentError,
                                    "COMPLETED_ACTIVATION_CHANGED"):
            self.invoke(fixture, "recover", "target")

    def test_hash_runtime_and_journal_tamper_fail_before_activation(self):
        fixture = self.fixture("bad-compose-hash")
        fixture["source_compose"] = b"wrong"
        with self.assertRaisesRegex(enrollment.EnrollmentError, "COMPOSE_HASH_MISMATCH"):
            self.invoke(fixture)
        self.assertFalse(fixture["journal"].exists())
        self.assertEqual(fixture["router"].calls, [])

        fixture = self.fixture("mutable-candidate-image")
        self.write(fixture["candidate"], fixture["source_compose"] +
                   b"  codex:\n    image: ghcr.io/example/codex:latest\n")
        with self.assertRaisesRegex(enrollment.EnrollmentError, "DIGEST_NOT_PINNED"):
            self.invoke(fixture)
        self.assertFalse(fixture["journal"].exists())

        fixture = self.fixture("runtime-mismatch")
        with patch.object(enrollment, "runtime_evidence",
                          side_effect=enrollment.EnrollmentError("CODEX_NATIVE_IDENTITY_MISMATCH")), \
             patch.object(enrollment, "require_root"), \
             patch.object(enrollment.cd, "download", return_value=manifest("codex")), \
             patch.object(enrollment, "router_control", return_value=fixture["router"]):
            with self.assertRaisesRegex(enrollment.EnrollmentError, "NATIVE_IDENTITY"):
                enrollment.enroll(
                    "apply", None, fixture["root"], fixture["journal"], fixture["router_lock"],
                    fixture["consumer_pid"], fixture["candidate"],
                    hashlib.sha256(fixture["source_compose"]).hexdigest(),
                    hashlib.sha256(fixture["candidate"].read_bytes()).hexdigest(),
                    "codex-v0.2.0", CODEX_ID,
                )
        self.assertFalse(fixture["journal"].exists())

        fixture = self.fixture("tampered-journal")
        with patch.object(enrollment.pair, "checkpoint",
                          side_effect=lambda name: (_ for _ in ()).throw(InjectedCrash(name))
                          if name == "compose.after_replace" else None), \
             self.assertRaises(InjectedCrash):
            self.invoke(fixture)
        target = fixture["journal"] / "target-config.json"
        target.write_bytes(target.read_bytes() + b" ")
        target.chmod(0o600)
        with self.assertRaisesRegex(enrollment.EnrollmentError, "PLAN_HASH_MISMATCH"):
            self.invoke(fixture, "recover", "target")

    def test_root_consumer_and_lock_boundaries_fail_closed(self):
        with patch.object(enrollment.os, "geteuid", return_value=12345), \
             self.assertRaisesRegex(enrollment.EnrollmentError, "ROOT_REQUIRED"):
            enrollment.require_root()

        fixture = self.fixture("consumer-running")
        self.write(fixture["consumer_pid"], b"123\n")
        with self.assertRaisesRegex(enrollment.EnrollmentError, "CONSUMER_RUNNING"):
            self.invoke(fixture)
        self.assertFalse(fixture["journal"].exists())

        fixture = self.fixture("router-not-running")
        fcntl.flock(fixture["router_lock_descriptor"], fcntl.LOCK_UN)
        with self.assertRaisesRegex(enrollment.EnrollmentError, "ROUTER_NOT_RUNNING"):
            self.invoke(fixture)
        self.assertFalse(fixture["journal"].exists())

        fixture = self.fixture("deploy-locked")
        descriptor = os.open(fixture["root"] / "deploy.lock", os.O_RDWR)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(enrollment.pair.InstallError, "DEPLOY_LOCK_BUSY"):
                self.invoke(fixture)
        finally:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
            os.close(descriptor)

    def test_runtime_probe_requires_exact_digest_isolation_identity_and_sealed_route(self):
        fixture = self.fixture("runtime-probe")
        config = copy.deepcopy(fixture["source_config"])
        config["components"]["codex"] = {
            "service": "codex", "url": "https://codex:18443",
            "node_id": CODEX_ID, "actor_id": "owner",
        }
        ledger = copy.deepcopy(fixture["source_ledger"])
        ledger["components"]["codex"] = {"current": manifest("codex"), "previous": None}

        class View:
            def compose(self, *args):
                self.compose_args = args
                return json.dumps({"services": {"codex": {"image": manifest("codex")["image"]}}})

            def container(self, name):
                return {"panel": "a" * 64, "cursor": "b" * 64, "codex": "c" * 64}[name]

            def inspect(self, name, exact):
                self.inspected = getattr(self, "inspected", []) + [(name, exact)]

            def health(self, name, exact):
                return {"node": CURSOR_ID if name == "cursor" else CODEX_ID,
                        "epoch": 1, "registry": 1, "kind": name,
                        "version": exact["adapter_version"], "quiescent": True}

            @staticmethod
            def identity(health):
                return {key: health[key] for key in ("node", "epoch", "registry", "kind", "version")}

            def routing(self):
                return fixture["router"].status()

        view = View()
        with patch.object(enrollment, "installer_view", return_value=view):
            value = enrollment.runtime_evidence(
                fixture["root"], config, ledger, fixture["candidate"], manifest("codex"))
            self.assertEqual(value["codexRoute"]["mode"], "sealed")
            self.assertEqual(view.compose_args, ("config", "--format", "json"))
            self.assertEqual({name for name, _ in view.inspected}, {"panel", "cursor", "codex"})
            with patch.object(view, "compose", return_value=json.dumps({
                    "services": {"codex": {"image": "ghcr.io/example/codex:latest"}},
            })):
                with self.assertRaisesRegex(enrollment.EnrollmentError, "DIGEST_NOT_PINNED"):
                    enrollment.runtime_evidence(
                        fixture["root"], config, ledger, fixture["candidate"], manifest("codex"))
            fixture["router"].codex = enrollment.host.RouterControl.projected(
                "activate", fixture["router"].codex, "bootstrap", value["codexIdentity"])
            with self.assertRaisesRegex(enrollment.EnrollmentError, "SEALED_BOOTSTRAP"):
                enrollment.runtime_evidence(
                    fixture["root"], config, ledger, fixture["candidate"], manifest("codex"))
            activated = enrollment.runtime_evidence(
                fixture["root"], config, ledger, fixture["candidate"], manifest("codex"),
                allow_activated=True)
            self.assertEqual(activated["codexRoute"]["mode"], "eligible")


if __name__ == "__main__":
    unittest.main()
