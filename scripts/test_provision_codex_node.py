import base64
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

import provision_codex_node as provision
import registry_transition as transition


class CodexProvisionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        homebrew = Path("/opt/homebrew/opt/openssl@3/bin/openssl")
        self.openssl = str(homebrew) if homebrew.is_file() else shutil.which("openssl")
        if not self.openssl or not self.command("version").startswith(b"OpenSSL 3."):
            self.skipTest("OpenSSL 3 required")
        self.ca_key = self.root / "ca.key"
        self.ca = self.root / "ca.pem"
        self.command("req", "-x509", "-newkey", "ed25519", "-noenc", "-keyout", self.ca_key,
                     "-out", self.ca, "-subj", "/CN=fixture-ca", "-days", "30",
                     "-addext", "basicConstraints=critical,CA:TRUE",
                     "-addext", "keyUsage=critical,keyCertSign,cRLSign")
        self.gateway_key = self.root / "gateway.key"
        gateway_request = self.root / "gateway.csr"
        gateway_extension = self.root / "gateway.ext"
        gateway_extension.write_text("basicConstraints=critical,CA:FALSE\n"
                                     "keyUsage=critical,digitalSignature\n"
                                     "extendedKeyUsage=clientAuth\n")
        self.gateway = self.root / "gateway.pem"
        self.command("req", "-new", "-newkey", "ed25519", "-noenc", "-keyout", self.gateway_key,
                     "-out", gateway_request, "-subj", "/CN=gateway")
        self.command("x509", "-req", "-in", gateway_request, "-CA", self.ca, "-CAkey", self.ca_key,
                     "-set_serial", "2", "-days", "29", "-out", self.gateway,
                     "-extfile", gateway_extension)
        self.signer_key = self.root / "registry-signing.key"
        self.signer_public = self.root / "registry-signing.pem"
        self.command("genpkey", "-algorithm", "ed25519", "-out", self.signer_key)
        self.command("pkey", "-in", self.signer_key, "-pubout", "-out", self.signer_public)
        for path in (self.ca_key, self.gateway_key, self.signer_key):
            path.chmod(0o600)
        self.cursor_id = "2fd6caa6-0b0e-440d-9503-701fedcc2568"
        self.codex_id = "8d96e23f-6dd5-4f4f-8ec7-220e1be07e0a"
        self.owner = "2-1"
        self.registry = self.root / "registry.json"
        manifest = {"registryVersion": 1, "ownerId": self.owner, "mode": "live", "nodes": [{
            "nodeId": self.cursor_id, "name": "Cursor alpha", "adapter": "cursor",
            "url": "https://harness:18443", "certificateSHA256": "a" * 64,
        }]}
        payload = transition.canonical_manifest(manifest)
        message = self.root / "manifest.json"
        message.write_bytes(payload)
        signature = self.command("pkeyutl", "-sign", "-rawin", "-inkey", self.signer_key,
                                 "-in", message)
        self.registry.write_text(json.dumps({"manifest": manifest,
                                            "signature": base64.b64encode(signature).decode()}))
        gateway_der = self.command("x509", "-in", self.gateway, "-outform", "DER")
        self.gateway_pin = hashlib.sha256(gateway_der).hexdigest()
        self.cursor = self.root / "cursor-node.json"
        self.cursor.write_text(json.dumps({"nodeId": self.cursor_id, "ownerId": self.owner,
                                          "registryVersion": 1,
                                          "gatewayCertificateSHA256": self.gateway_pin}))
        self.cursor.chmod(0o600)

    def command(self, *arguments):
        values = [str(value) for value in arguments]
        return subprocess.run([self.openssl, *values], check=True, capture_output=True,
                              timeout=10).stdout

    def prepare(self, output=None, **changes):
        arguments = dict(
            registry_path=self.registry,
            signer_public_key=self.signer_public,
            ca_certificate=self.ca,
            ca_private_key=self.ca_key,
            cursor_node_config=self.cursor,
            gateway_certificate=self.gateway,
            output=output or self.root / "codex-node",
            model="gpt-5.6-luna",
            effort="low",
            valid_days=14,
            node_id=self.codex_id,
            runtime_uid=os.geteuid(),
            runtime_gid=os.getegid(),
            openssl=self.openssl,
        )
        arguments.update(changes)
        return provision.prepare(**arguments)

    def test_prepares_isolated_empty_codex_node_from_existing_trust(self):
        before = {path: path.read_bytes() for path in
                  (self.registry, self.signer_public, self.ca, self.ca_key, self.cursor, self.gateway)}
        result = self.prepare()
        output = self.root / "codex-node"
        self.assertEqual(result["nodeId"], self.codex_id)
        config = json.loads((output / "codex-config/node.json").read_text())
        self.assertEqual(config["adapter"], "codex")
        self.assertNotIn("cursor", config)
        self.assertEqual((config["nodeId"], config["ownerId"], config["registryVersion"]),
                         (self.codex_id, self.owner, 1))
        self.assertEqual(config["gatewayCertificateSHA256"], self.gateway_pin)
        self.assertEqual(config["codex"], {
            "codexHome": "/auth/codex", "effort": "low",
            "executable": "/opt/codex/node_modules/.bin/codex",
            "homeDir": "/state/codex/home", "model": "gpt-5.6-luna",
            "stateDir": "/state/codex", "workingDir": "/workspace",
        })
        self.assertEqual((output / "codex-config/tools.json").read_text(), "[]\n")
        self.assertEqual((output / "codex-config/policy.txt").read_text(), provision.POLICY)
        self.assertEqual(list((output / "codex-auth/codex").iterdir()), [])
        for directory in ("codex-config", "codex-state", "codex-state/node", "codex-state/codex",
                          "codex-state/codex/home", "codex-auth", "codex-auth/codex",
                          "codex-workspace"):
            info = (output / directory).stat()
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o700)
            self.assertEqual((info.st_uid, info.st_gid), (os.geteuid(), os.getegid()))
        for name in ("ca.pem", "node.pem", "node.key", "policy.txt", "tools.json", "node.json"):
            info = (output / "codex-config" / name).stat()
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
            self.assertEqual((info.st_uid, info.st_gid), (os.geteuid(), os.getegid()))
        self.command("verify", "-CAfile", output / "codex-config/ca.pem", "-purpose", "sslserver",
                     "-verify_hostname", "codex", output / "codex-config/node.pem")
        client_check = subprocess.run(
            [self.openssl, "verify", "-CAfile", str(output / "codex-config/ca.pem"),
             "-purpose", "sslclient", str(output / "codex-config/node.pem")],
            capture_output=True, timeout=10,
        )
        self.assertNotEqual(client_check.returncode, 0)
        san = self.command("x509", "-in", output / "codex-config/node.pem", "-noout",
                           "-ext", "subjectAltName").decode()
        eku = self.command("x509", "-in", output / "codex-config/node.pem", "-noout",
                           "-ext", "extendedKeyUsage").decode()
        self.assertEqual([line.strip() for line in san.splitlines() if "DNS:" in line],
                         ["DNS:codex"])
        self.assertIn("TLS Web Server Authentication", eku)
        self.assertNotIn("TLS Web Client Authentication", eku)
        metadata = json.loads((output / "provision.json").read_text())
        self.assertEqual(metadata["authState"], "empty")
        self.assertEqual(metadata["certificateSHA256"], result["certificateSHA256"])
        self.assertEqual(before, {path: path.read_bytes() for path in before})

    def test_rejects_wrong_ca_key_before_output(self):
        wrong = self.root / "wrong-ca.key"
        self.command("genpkey", "-algorithm", "ed25519", "-out", wrong)
        wrong.chmod(0o600)
        with self.assertRaisesRegex(provision.ProvisionError, "CA_KEY_MISMATCH"):
            self.prepare(ca_private_key=wrong)
        self.assertFalse((self.root / "codex-node").exists())

    def test_rejects_gateway_pin_mismatch(self):
        value = json.loads(self.cursor.read_text())
        value["gatewayCertificateSHA256"] = "b" * 64
        self.cursor.write_text(json.dumps(value))
        with self.assertRaisesRegex(provision.ProvisionError, "GATEWAY_PIN_MISMATCH"):
            self.prepare()

    def test_rejects_tampered_registry_signature(self):
        value = json.loads(self.registry.read_text())
        value["manifest"]["ownerId"] = "other"
        self.registry.write_text(json.dumps(value))
        with self.assertRaisesRegex(provision.ProvisionError, "OPENSSL_REJECTED_INPUT"):
            self.prepare()

    def test_rejects_ca_without_leaf_safety_horizon(self):
        short_key = self.root / "short-ca.key"
        short_ca = self.root / "short-ca.pem"
        self.command("req", "-x509", "-newkey", "ed25519", "-noenc", "-keyout", short_key,
                     "-out", short_ca, "-subj", "/CN=short-ca", "-days", "1",
                     "-addext", "basicConstraints=critical,CA:TRUE",
                     "-addext", "keyUsage=critical,keyCertSign,cRLSign")
        short_gateway_key = self.root / "short-gateway.key"
        short_gateway_request = self.root / "short-gateway.csr"
        short_gateway = self.root / "short-gateway.pem"
        self.command("req", "-new", "-newkey", "ed25519", "-noenc", "-keyout", short_gateway_key,
                     "-out", short_gateway_request, "-subj", "/CN=gateway")
        self.command("x509", "-req", "-in", short_gateway_request, "-CA", short_ca,
                     "-CAkey", short_key, "-set_serial", "4", "-days", "1",
                     "-out", short_gateway, "-extfile", self.root / "gateway.ext")
        for path in (short_key, short_gateway_key):
            path.chmod(0o600)
        short_pin = hashlib.sha256(self.command("x509", "-in", short_gateway,
                                                "-outform", "DER")).hexdigest()
        value = json.loads(self.cursor.read_text())
        value["gatewayCertificateSHA256"] = short_pin
        self.cursor.write_text(json.dumps(value))
        with self.assertRaisesRegex(provision.ProvisionError, "CA_VALIDITY_TOO_SHORT"):
            self.prepare(ca_certificate=short_ca, ca_private_key=short_key,
                         gateway_certificate=short_gateway)

    def test_rejects_duplicate_node_and_invalid_lifetime(self):
        with self.assertRaisesRegex(provision.ProvisionError, "INVALID_CODEX_NODE_ID"):
            self.prepare(node_id=self.cursor_id)
        with self.assertRaisesRegex(provision.ProvisionError, "INVALID_CERTIFICATE_LIFETIME"):
            self.prepare(valid_days=22)

    def test_never_overwrites_existing_output(self):
        output = self.root / "existing"
        output.mkdir(mode=0o700)
        marker = output / "marker"
        marker.write_text("preserve")
        with self.assertRaisesRegex(provision.ProvisionError, "NEW_ABSOLUTE_OUTPUT_REQUIRED"):
            self.prepare(output=output)
        self.assertEqual(marker.read_text(), "preserve")

    def test_source_mutation_is_fenced_before_publication(self):
        original = transition.read_regular
        reads = 0

        def mutate_on_recheck(path, maximum, private=False):
            nonlocal reads
            if Path(path) == self.registry:
                reads += 1
                if reads == 2:
                    self.registry.write_bytes(self.registry.read_bytes() + b"\n")
            return original(path, maximum, private)

        with patch.object(transition, "read_regular", side_effect=mutate_on_recheck), \
             self.assertRaisesRegex(provision.ProvisionError,
                                    "SOURCE_CHANGED_DURING_PREPARATION"):
            self.prepare()
        self.assertFalse((self.root / "codex-node").exists())

    def test_rejects_untrusted_or_symlinked_output_parent(self):
        shared = self.root / "shared"
        shared.mkdir(mode=0o755)
        with self.assertRaisesRegex(provision.ProvisionError, "INVALID_OUTPUT_PARENT"):
            self.prepare(output=shared / "codex-node")
        private = self.root / "private"
        private.mkdir(mode=0o700)
        alias = self.root / "alias"
        alias.symlink_to(private, target_is_directory=True)
        with self.assertRaisesRegex(provision.ProvisionError, "INVALID_OUTPUT_PARENT"):
            self.prepare(output=alias / "codex-node")

    def test_parent_fsync_failure_preserves_ambiguous_publication(self):
        output = self.root / "ambiguous"
        original = os.fsync

        def fail_published_parent(descriptor):
            info = os.fstat(descriptor)
            if output.exists() and stat.S_ISDIR(info.st_mode) and \
                    info.st_ino == self.root.stat().st_ino:
                raise OSError("fixture")
            return original(descriptor)

        with patch.object(provision.os, "fsync", side_effect=fail_published_parent), \
             self.assertRaisesRegex(provision.ProvisionError,
                                    "PUBLICATION_DURABILITY_UNKNOWN"):
            self.prepare(output=output)
        self.assertTrue((output / "provision.json").is_file())

    def test_post_publish_close_failure_does_not_mask_success(self):
        output = self.root / "published"
        original = os.close
        injected = False

        def fail_first_close_after_publish(descriptor):
            nonlocal injected
            if not injected and output.exists():
                injected = True
                original(descriptor)
                raise OSError("fixture")
            return original(descriptor)

        with patch.object(provision.os, "close", side_effect=fail_first_close_after_publish):
            metadata = self.prepare(output=output)
        self.assertTrue(injected)
        self.assertEqual(metadata["nodeId"], self.codex_id)
        self.assertTrue((output / "provision.json").is_file())

    def test_write_owned_syncs_inode_after_chown(self):
        path = self.root / "owned"
        events = []
        real_chown = os.chown
        real_open = os.open
        real_fsync = os.fsync
        real_close = os.close

        def write_private(target, content):
            Path(target).write_bytes(content)
            Path(target).chmod(0o600)
            events.append("write")

        def chown(target, uid, gid):
            real_chown(target, uid, gid)
            events.append("chown")

        def open_file(target, flags):
            events.append("open")
            return real_open(target, flags)

        def sync_file(descriptor):
            events.append("fsync")
            return real_fsync(descriptor)

        def close_file(descriptor):
            events.append("close")
            return real_close(descriptor)

        with patch.object(transition, "write_private", side_effect=write_private), \
             patch.object(provision.os, "chown", side_effect=chown), \
             patch.object(provision.os, "open", side_effect=open_file), \
             patch.object(provision.os, "fsync", side_effect=sync_file), \
             patch.object(provision.os, "close", side_effect=close_file):
            provision.write_owned(path, b"secret", os.geteuid(), os.getegid())
        self.assertEqual(events, ["write", "chown", "open", "fsync", "close"])


if __name__ == "__main__":
    unittest.main()
