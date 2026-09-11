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

import registry_transition as transition


CURSOR_ID = "20000000-0000-4000-8000-000000000001"
CODEX_ID = "20000000-0000-4000-8000-000000000002"


class RegistryTransitionTests(unittest.TestCase):
    def setUp(self):
        homebrew = Path("/opt/homebrew/opt/openssl@3/bin/openssl")
        self.openssl = str(homebrew) if homebrew.is_file() else shutil.which("openssl")
        if self.openssl is None:
            self.skipTest("OpenSSL is required")
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        self.state_dir = self.root / "router"
        self.state_dir.mkdir(mode=0o700)
        self.lock = self.state_dir / "router.lock"
        self.lock.write_bytes(b"")
        self.lock.chmod(0o600)
        self.openssl_run("req", "-x509", "-newkey", "ed25519", "-noenc", "-keyout", "ca.key",
                 "-out", "ca.pem", "-subj", "/CN=fixture", "-days", "2",
                 "-addext", "basicConstraints=critical,CA:TRUE",
                 "-addext", "keyUsage=critical,keyCertSign,cRLSign")
        self.cert("cursor", 1)
        self.cert("codex", 2)
        self.openssl_run("genpkey", "-algorithm", "ed25519", "-out", "registry.key")
        self.openssl_run("pkey", "-in", "registry.key", "-pubout", "-out", "registry.pem")
        (self.root / "registry.key").chmod(0o600)
        cursor_pin = self.pin("cursor.pem")
        manifest = {"registryVersion": 1, "ownerId": "2-1", "mode": "live", "nodes": [{
            "nodeId": CURSOR_ID, "name": "Cursor alpha", "adapter": "cursor",
            "url": "https://cursor:18443", "certificateSHA256": cursor_pin,
        }]}
        payload = transition.canonical_manifest(manifest)
        message = self.root / "manifest.json"
        message.write_bytes(payload)
        signature = subprocess.check_output([self.openssl, "pkeyutl", "-sign", "-rawin", "-inkey",
                                             str(self.root / "registry.key"), "-in", str(message)])
        self.registry = self.root / "registry.json"
        self.registry.write_text(json.dumps({"manifest": manifest,
                                             "signature": base64.b64encode(signature).decode()}, separators=(",", ":")) + "\n")
        state = {"schema": 1, "ownerId": "2-1", "registryVersion": 1,
                 "registrySHA256": hashlib.sha256(payload).hexdigest(), "nodes": {CURSOR_ID: {
                     "mode": "eligible", "stateVersion": 14, "generation": 5,
                     "identityEpoch": 1, "adapterKind": "cursor", "adapterVersion": "1.0.31",
                 }}}
        self.state = self.state_dir / "state.json"
        self.state.write_text(json.dumps(state, separators=(",", ":")) + "\n")
        self.state.chmod(0o600)
        self.config = self.root / "codex-node.json"
        self.config.write_text(json.dumps({"nodeId": CODEX_ID, "ownerId": "2-1", "registryVersion": 1,
                                           "adapter": "codex", "codex": {"model": "gpt-5"}}) + "\n")
        self.config.chmod(0o600)

    def openssl_run(self, *args):
        subprocess.run([self.openssl, *args], cwd=self.root, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=15, check=True)

    def cert(self, name, serial, hostname=None):
        self.openssl_run("req", "-new", "-newkey", "ed25519", "-noenc", "-keyout", name + ".key",
                 "-out", name + ".csr", "-subj", "/CN=" + name)
        (self.root / (name + ".ext")).write_text(
            "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n"
            "extendedKeyUsage=serverAuth\nsubjectAltName=DNS:" + (hostname or name) + "\n")
        self.openssl_run("x509", "-req", "-in", name + ".csr", "-CA", "ca.pem", "-CAkey", "ca.key",
                 "-set_serial", str(serial), "-days", "2", "-out", name + ".pem",
                 "-extfile", name + ".ext")

    def pin(self, name):
        der = subprocess.check_output([self.openssl, "x509", "-in", str(self.root / name),
                                       "-outform", "DER"])
        return hashlib.sha256(der).hexdigest()

    def set_cursor_url(self, url):
        registry = json.loads(self.registry.read_text())
        registry["manifest"]["nodes"][0]["url"] = url
        payload = transition.canonical_manifest(registry["manifest"])
        message = self.root / "manifest.json"
        message.write_bytes(payload)
        signature = subprocess.check_output([
            self.openssl, "pkeyutl", "-sign", "-rawin", "-inkey",
            str(self.root / "registry.key"), "-in", str(message),
        ])
        registry["signature"] = base64.b64encode(signature).decode()
        self.registry.write_text(json.dumps(registry, separators=(",", ":")) + "\n")
        state = json.loads(self.state.read_text())
        state["registrySHA256"] = hashlib.sha256(payload).hexdigest()
        self.state.write_text(json.dumps(state, separators=(",", ":")) + "\n")
        self.state.chmod(0o600)

    def prepare(self, output=None, certificate="codex.pem", url="https://codex:18443"):
        output = output or self.root / "transition"
        return transition.prepare(self.registry, self.state, self.root / "registry.pem",
                                  self.root / "registry.key", self.root / "ca.pem", self.config,
                                  self.root / certificate, "Codex alpha", url, output, self.openssl)

    def test_prepares_exact_additive_pair_and_rollback(self):
        old_registry, old_state = self.registry.read_bytes(), self.state.read_bytes()
        metadata = self.prepare()
        bundle = self.root / "transition"
        self.assertEqual((bundle / "rollback" / "registry.json").read_bytes(), old_registry)
        self.assertEqual((bundle / "rollback" / "state.json").read_bytes(), old_state)
        registry = json.loads((bundle / "next" / "registry.json").read_text())
        state = json.loads((bundle / "next" / "state.json").read_text())
        self.assertEqual(registry["manifest"]["registryVersion"], 1)
        self.assertEqual(registry["manifest"]["ownerId"], "2-1")
        self.assertEqual(registry["manifest"]["nodes"][0], json.loads(old_registry)["manifest"]["nodes"][0])
        self.assertEqual([node["adapter"] for node in registry["manifest"]["nodes"]], ["cursor", "codex"])
        old_cursor_state = json.loads(old_state)["nodes"][CURSOR_ID]
        self.assertEqual(state["nodes"][CURSOR_ID], old_cursor_state)
        self.assertEqual(state["nodes"][CODEX_ID], {
            "mode": "sealed", "stateVersion": 1, "generation": 0, "operationId": "bootstrap",
            "identityEpoch": 0, "adapterKind": "codex", "adapterVersion": "",
        })
        manifest_raw = transition.canonical_manifest(registry["manifest"])
        self.assertEqual(state["registrySHA256"], hashlib.sha256(manifest_raw).hexdigest())
        transition.validate_registry(registry, self.openssl, self.root / "registry.pem")
        self.assertEqual(metadata["next"]["manifestSHA256"], state["registrySHA256"])
        self.assertEqual(self.registry.read_bytes(), old_registry)
        self.assertEqual(self.state.read_bytes(), old_state)
        self.assertEqual((bundle.stat().st_mode & 0o777), 0o700)
        for path in bundle.rglob("*.json"):
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_requires_offline_router_lock(self):
        descriptor = os.open(self.lock, os.O_RDWR)
        self.addCleanup(os.close, descriptor)
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        with self.assertRaisesRegex(transition.TransitionError, "ROUTER_MUST_BE_OFFLINE"):
            self.prepare()
        self.assertFalse((self.root / "transition").exists())

    def test_does_not_overwrite_existing_bundle(self):
        output = self.root / "transition"
        output.mkdir()
        marker = output / "operator-data"
        marker.write_text("preserve")
        with self.assertRaisesRegex(transition.TransitionError, "NEW_ABSOLUTE_OUTPUT_REQUIRED"):
            self.prepare(output=output)
        self.assertEqual(marker.read_text(), "preserve")

    def test_rejects_registry_state_drift_without_output(self):
        state = json.loads(self.state.read_text())
        state["registrySHA256"] = "f" * 64
        self.state.write_text(json.dumps(state) + "\n")
        self.state.chmod(0o600)
        with self.assertRaisesRegex(transition.TransitionError, "REGISTRY_STATE_DRIFT"):
            self.prepare()
        self.assertFalse((self.root / "transition").exists())

    def test_rejects_duplicate_node_identity_and_certificate(self):
        config = json.loads(self.config.read_text())
        config["nodeId"] = CURSOR_ID
        self.config.write_text(json.dumps(config) + "\n")
        self.config.chmod(0o600)
        with self.assertRaisesRegex(transition.TransitionError, "DUPLICATE_NODE_ID"):
            self.prepare()
        config["nodeId"] = CODEX_ID
        self.config.write_text(json.dumps(config) + "\n")
        self.config.chmod(0o600)
        with self.assertRaisesRegex(transition.TransitionError, "DUPLICATE_NODE_CERTIFICATE"):
            self.prepare(certificate="cursor.pem", url="https://cursor:18443")

    def test_rejects_duplicate_node_endpoint(self):
        self.cert("codex-at-cursor", 3, "cursor")
        with self.assertRaisesRegex(transition.TransitionError, "DUPLICATE_NODE_ENDPOINT"):
            self.prepare(certificate="codex-at-cursor.pem", url="https://CURSOR:18443")
        self.set_cursor_url("https://cursor:443")
        with self.assertRaisesRegex(transition.TransitionError, "DUPLICATE_NODE_ENDPOINT"):
            self.prepare(certificate="codex-at-cursor.pem", url="https://CURSOR")

    def test_rejects_certificate_hostname_and_registry_version(self):
        with self.assertRaisesRegex(transition.TransitionError, "OPENSSL_REJECTED_INPUT"):
            self.prepare(url="https://other:18443")
        config = json.loads(self.config.read_text())
        config["registryVersion"] = 2
        self.config.write_text(json.dumps(config) + "\n")
        self.config.chmod(0o600)
        with self.assertRaisesRegex(transition.TransitionError, "CODEX_IDENTITY_MISMATCH"):
            self.prepare()

    def test_failed_bundle_write_is_not_published(self):
        original = transition.write_private
        calls = 0

        def fail_second(path, content):
            nonlocal calls
            calls += 1
            if calls == 6:
                raise OSError("fixture")
            return original(path, content)

        with patch.object(transition, "write_private", side_effect=fail_second), \
             self.assertRaisesRegex(transition.TransitionError, "REGISTRY_TRANSITION_FAILED"):
            self.prepare()
        self.assertFalse((self.root / "transition").exists())

    def test_post_rename_fsync_reports_ambiguous_publication(self):
        original = os.fsync

        def fail_parent(descriptor):
            info = os.fstat(descriptor)
            if stat.S_ISDIR(info.st_mode) and info.st_ino == self.root.stat().st_ino:
                raise OSError("fixture")
            return original(descriptor)

        with patch.object(transition.os, "fsync", side_effect=fail_parent), \
             self.assertRaisesRegex(transition.TransitionError,
                                    "PUBLICATION_DURABILITY_UNKNOWN"):
            self.prepare()
        self.assertTrue((self.root / "transition" / "transition.json").is_file())

    def test_rejects_untrusted_or_symlinked_output_parent(self):
        shared = self.root / "shared"
        shared.mkdir(mode=0o755)
        with self.assertRaisesRegex(transition.TransitionError, "INVALID_OUTPUT_PARENT"):
            self.prepare(output=shared / "transition")
        private = self.root / "private"
        private.mkdir(mode=0o700)
        alias = self.root / "alias"
        alias.symlink_to(private, target_is_directory=True)
        with self.assertRaisesRegex(transition.TransitionError, "INVALID_OUTPUT_PARENT"):
            self.prepare(output=alias / "transition")

    def test_publication_race_does_not_replace_destination(self):
        original = transition.rename_no_replace

        def race(parent_fd, source_name, destination_name):
            destination = self.root / destination_name
            destination.mkdir(mode=0o700)
            marker = destination / "operator-data"
            marker.write_text("preserve")
            original(parent_fd, source_name, destination_name)

        with patch.object(transition, "rename_no_replace", side_effect=race), \
             self.assertRaisesRegex(transition.TransitionError, "OUTPUT_ALREADY_EXISTS"):
            self.prepare()
        self.assertEqual((self.root / "transition" / "operator-data").read_text(), "preserve")

    def test_openssl_only_receives_private_snapshot_paths(self):
        original = transition.openssl_run
        arguments = []

        def record(executable, *items, **kwargs):
            arguments.extend(str(item) for item in items)
            return original(executable, *items, **kwargs)

        with patch.object(transition, "openssl_run", side_effect=record):
            self.prepare()
        command_text = "\n".join(arguments)
        for source in (self.root / "registry.pem", self.root / "registry.key",
                       self.root / "ca.pem", self.root / "codex.pem"):
            self.assertNotIn(str(source), command_text)
        self.assertIn("registry-transition-inputs-", command_text)


if __name__ == "__main__":
    unittest.main()
