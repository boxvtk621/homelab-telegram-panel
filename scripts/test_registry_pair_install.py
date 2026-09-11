import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import registry_pair_install as installer
import registry_transition as transition


CURSOR_ID = "30000000-0000-4000-8000-000000000001"
CODEX_ID = "30000000-0000-4000-8000-000000000002"


class InjectedCrash(BaseException):
    pass


class RegistryPairInstallTests(unittest.TestCase):
    def setUp(self):
        homebrew = Path("/opt/homebrew/opt/openssl@3/bin/openssl")
        self.openssl = str(homebrew) if homebrew.is_file() else shutil.which("openssl")
        if self.openssl is None:
            self.skipTest("OpenSSL is required")
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        self.template = self.root / "template"
        self.template.mkdir(mode=0o700)
        self.make_bundle()

    def openssl_run(self, *args):
        subprocess.run([self.openssl, *args], cwd=self.template, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=15, check=True)

    def make_bundle(self):
        self.openssl_run("req", "-x509", "-newkey", "ed25519", "-noenc", "-keyout", "ca.key",
                         "-out", "ca.pem", "-subj", "/CN=fixture", "-days", "2",
                         "-addext", "basicConstraints=critical,CA:TRUE",
                         "-addext", "keyUsage=critical,keyCertSign,cRLSign")
        for name, serial in (("cursor", 1), ("codex", 2)):
            self.openssl_run("req", "-new", "-newkey", "ed25519", "-noenc",
                             "-keyout", name + ".key", "-out", name + ".csr",
                             "-subj", "/CN=" + name)
            (self.template / (name + ".ext")).write_text(
                "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n"
                "extendedKeyUsage=serverAuth\nsubjectAltName=DNS:" + name + "\n")
            self.openssl_run("x509", "-req", "-in", name + ".csr", "-CA", "ca.pem",
                             "-CAkey", "ca.key", "-set_serial", str(serial), "-days", "2",
                             "-out", name + ".pem", "-extfile", name + ".ext")
        self.openssl_run("genpkey", "-algorithm", "ed25519", "-out", "registry.key")
        self.openssl_run("pkey", "-in", "registry.key", "-pubout", "-out", "registry.pem")
        (self.template / "registry.key").chmod(0o600)
        (self.template / "registry.pem").chmod(0o600)
        der = subprocess.check_output([self.openssl, "x509", "-in",
                                       str(self.template / "cursor.pem"), "-outform", "DER"])
        manifest = {"registryVersion": 1, "ownerId": "3-1", "mode": "live", "nodes": [{
            "nodeId": CURSOR_ID, "name": "Cursor alpha", "adapter": "cursor",
            "url": "https://cursor:18443", "certificateSHA256": hashlib.sha256(der).hexdigest(),
        }]}
        payload = transition.canonical_manifest(manifest)
        message = self.template / "manifest.json"
        message.write_bytes(payload)
        signature = subprocess.check_output([
            self.openssl, "pkeyutl", "-sign", "-rawin", "-inkey",
            str(self.template / "registry.key"), "-in", str(message),
        ])
        registry = self.template / "registry.json"
        registry.write_text(json.dumps({
            "manifest": manifest, "signature": base64.b64encode(signature).decode(),
        }, separators=(",", ":")) + "\n")
        registry.chmod(0o600)
        router = self.template / "router"
        router.mkdir(mode=0o700)
        state = router / "state.json"
        state.write_text(json.dumps({
            "schema": 1, "ownerId": "3-1", "registryVersion": 1,
            "registrySHA256": hashlib.sha256(payload).hexdigest(), "nodes": {CURSOR_ID: {
                "mode": "eligible", "stateVersion": 9, "generation": 4,
                "identityEpoch": 1, "adapterKind": "cursor", "adapterVersion": "1.0.31",
            }},
        }, separators=(",", ":")) + "\n")
        state.chmod(0o600)
        router_lock = router / "router.lock"
        router_lock.write_bytes(b"")
        router_lock.chmod(0o600)
        config = self.template / "codex-node.json"
        config.write_text(json.dumps({
            "nodeId": CODEX_ID, "ownerId": "3-1", "registryVersion": 1,
            "adapter": "codex", "codex": {"model": "gpt-5"},
        }) + "\n")
        config.chmod(0o600)
        transition.prepare(
            registry, state, self.template / "registry.pem", self.template / "registry.key",
            self.template / "ca.pem", config, self.template / "codex.pem", "Codex alpha",
            "https://codex:18443", self.template / "bundle", self.openssl,
        )

    def fixture(self, name):
        root = self.root / name
        root.mkdir(mode=0o700)
        shutil.copytree(self.template / "bundle", root / "bundle")
        config = root / "config"
        config.mkdir(mode=0o700)
        shutil.copy2(self.template / "registry.pem", config / "registry.pem")
        shutil.copy2(self.template / "bundle" / "rollback" / "registry.json",
                     config / "registry.json")
        for path in config.iterdir():
            path.chmod(0o600)
        router = root / "router"
        router.mkdir(mode=0o700)
        shutil.copy2(self.template / "bundle" / "rollback" / "state.json", router / "state.json")
        (router / "state.json").chmod(0o600)
        (router / "router.lock").write_bytes(b"")
        (router / "router.lock").chmod(0o600)
        operator = root / "operator"
        operator.mkdir(mode=0o700)
        (operator / "deploy.lock").write_bytes(b"")
        (operator / "deploy.lock").chmod(0o600)
        (operator / "consumer-status.json").write_text(json.dumps({
            "schema": 1, "service": "panel", "state": "stopped", "pid": None,
        }, separators=(",", ":")) + "\n")
        (operator / "consumer-status.json").chmod(0o600)
        journal = root / "journal"
        journal.mkdir(mode=0o700)
        return {
            "root": root, "bundle": root / "bundle", "registry": config / "registry.json",
            "state": router / "state.json", "public": config / "registry.pem",
            "router_lock": router / "router.lock", "deploy_lock": operator / "deploy.lock",
            "status": operator / "consumer-status.json", "pid": operator / "panel.pid",
            "journal": journal / "install.json",
        }

    def invoke(self, fixture, action="apply", direction=None):
        with patch.object(installer, "require_root"):
            return installer.install(
                action, direction, fixture["bundle"], fixture["registry"], fixture["state"],
                fixture["public"], fixture["router_lock"], fixture["deploy_lock"],
                fixture["status"], fixture["pid"], fixture["journal"], self.openssl,
            )

    def assert_pair(self, fixture, direction):
        directory = "next" if direction == "target" else "rollback"
        self.assertEqual(fixture["registry"].read_bytes(),
                         (fixture["bundle"] / directory / "registry.json").read_bytes())
        self.assertEqual(fixture["state"].read_bytes(),
                         (fixture["bundle"] / directory / "state.json").read_bytes())

    def test_apply_and_idempotent_completed_recovery(self):
        fixture = self.fixture("success")
        registry_owner = (fixture["registry"].stat().st_uid, fixture["registry"].stat().st_gid)
        state_owner = (fixture["state"].stat().st_uid, fixture["state"].stat().st_gid)
        result = self.invoke(fixture)
        self.assertEqual(result, {"status": "REGISTRY_PAIR_INSTALLED", "direction": "target",
                                  "idempotent": False})
        self.assert_pair(fixture, "target")
        self.assertEqual(fixture["registry"].stat().st_mode & 0o777, 0o600)
        self.assertEqual(fixture["state"].stat().st_mode & 0o777, 0o600)
        self.assertEqual((fixture["registry"].stat().st_uid, fixture["registry"].stat().st_gid),
                         registry_owner)
        self.assertEqual((fixture["state"].stat().st_uid, fixture["state"].stat().st_gid),
                         state_owner)
        for _ in range(2):
            result = self.invoke(fixture, "recover", "target")
            self.assertTrue(result["idempotent"])
            self.assert_pair(fixture, "target")
        self.assertEqual(json.loads(fixture["journal"].read_text())["phase"], "completed")
        self.assertEqual(fixture["journal"].stat().st_mode & 0o777, 0o600)
        self.assertEqual(fixture["journal"].stat().st_uid, os.geteuid())

    def test_durable_journal_precedes_first_runtime_replace(self):
        fixture = self.fixture("journal-order")
        events = []
        with patch.object(installer, "checkpoint", side_effect=events.append):
            self.invoke(fixture)
        self.assertLess(events.index("journal.after_directory_fsync"),
                        events.index("registry.before_replace"))

    def test_recover_converges_after_every_file_crash_point(self):
        points = [
            component + suffix
            for component in ("journal", "registry", "state")
            for suffix in (".before_file_fsync", ".after_file_fsync", ".before_replace",
                           ".after_replace", ".before_directory_fsync",
                           ".after_directory_fsync")
        ]
        for index, point in enumerate(points):
            with self.subTest(point=point):
                fixture = self.fixture("crash-" + str(index))

                def crash(name):
                    if name == point:
                        raise InjectedCrash(name)

                with patch.object(installer, "checkpoint", side_effect=crash), \
                     self.assertRaises(InjectedCrash):
                    self.invoke(fixture)
                registry_hash = hashlib.sha256(fixture["registry"].read_bytes()).hexdigest()
                state_hash = hashlib.sha256(fixture["state"].read_bytes()).hexdigest()
                metadata = json.loads((fixture["bundle"] / "transition.json").read_text())
                self.assertIn(registry_hash, (metadata["source"]["registrySHA256"],
                                              metadata["next"]["registrySHA256"]))
                self.assertIn(state_hash, (metadata["source"]["routerStateSHA256"],
                                           metadata["next"]["routerStateSHA256"]))
                if fixture["journal"].exists():
                    self.invoke(fixture, "recover", "target")
                else:
                    self.invoke(fixture)
                self.assert_pair(fixture, "target")

    def test_oserror_after_state_replace_is_unknown_and_never_inverts(self):
        fixture = self.fixture("oserror")
        original = os.fsync
        state_directory = fixture["state"].parent.stat().st_ino
        target_state = (fixture["bundle"] / "next" / "state.json").read_bytes()

        def fail_state_directory_fsync(descriptor):
            info = os.fstat(descriptor)
            if (stat.S_ISDIR(info.st_mode) and info.st_ino == state_directory and
                    fixture["state"].read_bytes() == target_state):
                raise OSError("fixture")
            return original(descriptor)

        with patch.object(installer.os, "fsync", side_effect=fail_state_directory_fsync), \
             self.assertRaisesRegex(installer.InstallError, "INSTALL_STATE_UNKNOWN"):
            self.invoke(fixture)
        self.assert_pair(fixture, "target")
        self.invoke(fixture, "recover", "target")
        self.assert_pair(fixture, "target")

    def test_recover_can_explicitly_rollback_a_partial_pair(self):
        fixture = self.fixture("rollback")

        def crash(name):
            if name == "registry.after_replace":
                raise InjectedCrash(name)

        with patch.object(installer, "checkpoint", side_effect=crash), \
             self.assertRaises(InjectedCrash):
            self.invoke(fixture)
        self.invoke(fixture, "recover", "rollback")
        self.assert_pair(fixture, "rollback")

    def test_apply_refuses_non_source_pair_without_journal(self):
        fixture = self.fixture("not-source")
        fixture["state"].write_bytes((fixture["bundle"] / "next" / "state.json").read_bytes())
        with self.assertRaisesRegex(installer.InstallError, "APPLY_REQUIRES_EXACT_SOURCE_PAIR"):
            self.invoke(fixture)
        self.assertFalse(fixture["journal"].exists())

    def test_tampered_bundle_and_journal_fail_closed(self):
        fixture = self.fixture("bundle-tamper")
        target = fixture["bundle"] / "next" / "state.json"
        target.write_bytes(target.read_bytes() + b" ")
        target.chmod(0o600)
        with self.assertRaisesRegex(installer.InstallError, "TRANSITION_HASH_MISMATCH"):
            self.invoke(fixture)
        self.assertFalse(fixture["journal"].exists())

        fixture = self.fixture("journal-tamper")

        def crash(name):
            if name == "registry.after_replace":
                raise InjectedCrash(name)

        with patch.object(installer, "checkpoint", side_effect=crash), \
             self.assertRaises(InjectedCrash):
            self.invoke(fixture)
        journal = json.loads(fixture["journal"].read_text())
        journal["bundlePath"] += "-other"
        fixture["journal"].write_text(json.dumps(journal) + "\n")
        fixture["journal"].chmod(0o600)
        with self.assertRaisesRegex(installer.InstallError, "JOURNAL_BUNDLE_MISMATCH"):
            self.invoke(fixture, "recover", "target")

    def test_router_and_deploy_locks_are_nonblocking(self):
        for index, key in enumerate(("router_lock", "deploy_lock")):
            fixture = self.fixture("lock-" + str(index))
            descriptor = os.open(fixture[key], os.O_RDWR)
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                code = "ROUTER_LOCK_BUSY" if key == "router_lock" else "DEPLOY_LOCK_BUSY"
                with self.assertRaisesRegex(installer.InstallError, code):
                    self.invoke(fixture)
            finally:
                fcntl.flock(descriptor, fcntl.LOCK_UN)
                os.close(descriptor)
        fixture = self.fixture("lock-mode")
        fixture["router_lock"].chmod(0o644)
        with self.assertRaisesRegex(installer.InstallError, "INVALID_LOCK_FILE"):
            self.invoke(fixture)

    def test_running_consumer_is_rejected(self):
        fixture = self.fixture("running-status")
        fixture["status"].write_text(json.dumps({
            "schema": 1, "service": "panel", "state": "running", "pid": 123,
        }) + "\n")
        fixture["status"].chmod(0o600)
        with self.assertRaisesRegex(installer.InstallError, "CONSUMER_RUNNING"):
            self.invoke(fixture)
        fixture = self.fixture("running-pid")
        fixture["pid"].write_text("123\n")
        fixture["pid"].chmod(0o600)
        with self.assertRaisesRegex(installer.InstallError, "CONSUMER_RUNNING"):
            self.invoke(fixture)

    def test_consumer_restart_between_files_leaves_known_partial_pair(self):
        fixture = self.fixture("consumer-race")

        def restart(name):
            if name == "registry.after_directory_fsync":
                fixture["pid"].write_text("123\n")
                fixture["pid"].chmod(0o600)

        with patch.object(installer, "checkpoint", side_effect=restart), \
             self.assertRaisesRegex(installer.InstallError, "CONSUMER_RUNNING"):
            self.invoke(fixture)
        self.assertEqual(fixture["registry"].read_bytes(),
                         (fixture["bundle"] / "next" / "registry.json").read_bytes())
        self.assertEqual(fixture["state"].read_bytes(),
                         (fixture["bundle"] / "rollback" / "state.json").read_bytes())
        fixture["pid"].unlink()
        self.invoke(fixture, "recover", "target")
        self.assert_pair(fixture, "target")

    def test_private_mode_symlink_owner_and_root_boundaries(self):
        fixture = self.fixture("bad-mode")
        (fixture["bundle"] / "transition.json").chmod(0o644)
        with self.assertRaisesRegex(installer.InstallError, "INVALID_PRIVATE_FILE"):
            self.invoke(fixture)

        fixture = self.fixture("symlink")
        real = fixture["registry"].with_name("registry-real.json")
        fixture["registry"].rename(real)
        fixture["registry"].symlink_to(real)
        with self.assertRaisesRegex(installer.InstallError, "INVALID_PRIVATE_FILE"):
            self.invoke(fixture)

        fixture = self.fixture("owner")
        with self.assertRaisesRegex(installer.InstallError, "INVALID_PRIVATE_FILE"):
            installer.private_file_info(Path("relative-registry.json"))
        with self.assertRaisesRegex(installer.InstallError, "INVALID_PRIVATE_FILE"):
            installer.private_file_info(fixture["registry"], os.geteuid() + 1)
        with patch.object(installer.os, "geteuid", return_value=12345), \
             self.assertRaisesRegex(installer.InstallError, "ROOT_REQUIRED"):
            installer.install(
                "apply", None, fixture["bundle"], fixture["registry"], fixture["state"],
                fixture["public"], fixture["router_lock"], fixture["deploy_lock"],
                fixture["status"], fixture["pid"], fixture["journal"], self.openssl,
            )


if __name__ == "__main__":
    unittest.main()
