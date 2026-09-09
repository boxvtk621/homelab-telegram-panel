#!/usr/bin/env python3
import json
import pathlib
import subprocess
import unittest


ROOT = pathlib.Path(__file__).resolve().parent


class HarnessBackupArtifactsTest(unittest.TestCase):
    def test_shell_syntax_and_help(self) -> None:
        for name in ("guest-bootstrap.sh", "validate-guest.sh", "synthetic-roundtrip.sh"):
            script = ROOT / name
            subprocess.run(["sh", "-n", str(script)], check=True)
            result = subprocess.run(
                ["sh", str(script), "--help"],
                check=False,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            self.assertEqual(result.returncode, 0, (name, result.stderr))
            self.assertIn("Usage:", result.stdout)

    def test_provisioning_contract_is_exact_and_secret_free(self) -> None:
        contract = json.loads((ROOT / "provisioning-contract.json").read_text())
        self.assertEqual(contract["issue"], "HL-248")
        self.assertEqual(contract["guest"]["vmid"], 116)
        self.assertEqual(contract["guest"]["network"]["addressMode"], "dhcp")
        self.assertEqual(contract["guest"]["network"]["client"], "dhcpcd 10.3.2")
        self.assertEqual(contract["guest"]["network"]["gateway"], "10.202.2.4")
        self.assertEqual(contract["guest"]["network"]["dns"], ["10.202.2.4"])
        self.assertFalse(contract["guest"]["network"]["acceptDhcpRoutes"])
        self.assertFalse(contract["guest"]["network"]["ipv6"])
        self.assertFalse(contract["guest"]["passwordAuthentication"])
        self.assertEqual(contract["systemDisk"], {"storage": "local-lvm", "sizeGiB": 8})
        data = contract["dataDisk"]
        self.assertEqual(data["storage"], "harness-backup-vm")
        self.assertEqual(data["sizeGiB"], 128)
        self.assertEqual(data["format"], "qcow2")
        self.assertEqual(data["serial"], "HL248-BACKUP-DATA")
        self.assertEqual(data["expectedMountUuid"], "d2c8edad-0e90-4987-9063-f82f42e44c45")
        serialized = json.dumps(contract).lower()
        for forbidden in ("private key", "authorization:", "password\": \""):
            self.assertNotIn(forbidden, serialized)

    def test_format_is_reachable_only_after_identity_and_full_zero_scan(self) -> None:
        bootstrap = (ROOT / "guest-bootstrap.sh").read_text()
        self.assertEqual(bootstrap.count("mkfs.ext4"), 1)
        mkfs_position = bootstrap.index("\nmkfs.ext4 ")
        required_before = (
            'EXPECTED_SIZE_BYTES=137438953472',
            '[ "$actual_serial" = "$EXPECTED_SERIAL" ]',
            'root_source=$(findmnt',
            'data device has child partitions or mappings',
            'blkid -p "$DATA_DEVICE"',
            'wipefs -n "$DATA_DEVICE"',
            'cmp -n "$actual_size" "$DATA_DEVICE" /dev/zero',
        )
        for guard in required_before:
            self.assertIn(guard, bootstrap[:mkfs_position], guard)
        for forbidden in ("wipefs -a", "wipefs --all", "dd if=", "sgdisk", "parted"):
            self.assertNotIn(forbidden, bootstrap)

    def test_guest_boundary_has_no_scheduler_or_provider_mutation(self) -> None:
        combined = "\n".join(
            path.read_text()
            for path in ROOT.iterdir()
            if path.is_file() and path.suffix in {".sh", ".json"}
        ).lower()
        for forbidden in (
            "qm create",
            "qm set",
            "pvesm add",
            "crontab",
            "retention",
            "docker",
            "curl ",
        ):
            self.assertNotIn(forbidden, combined)
        self.assertIn("authenticationmethods publickey", combined)
        self.assertIn("forcecommand internal-sftp", combined)
        self.assertIn("$backup_user:np:", combined)
        self.assertIn("backup account key already exists; refusing ambiguous bootstrap", combined)

        bootstrap = (ROOT / "guest-bootstrap.sh").read_text()
        ssh_config = bootstrap.split("cat > /etc/ssh/sshd_config.d/90-harness-backup.conf <<'EOF'", 1)[1].split("\nEOF", 1)[0]
        global_config, match_config = ssh_config.split("\nMatch User harness-backup\n", 1)
        self.assertNotIn("AuthorizedKeysFile", global_config)
        self.assertIn("AuthorizedKeysFile /etc/ssh/authorized_keys/%u", match_config)

    def test_dhcp_override_and_roundtrip_are_concrete(self) -> None:
        bootstrap = (ROOT / "guest-bootstrap.sh").read_text()
        self.assertIn("/etc/dhcpcd.conf", bootstrap)
        self.assertIn("static routers=$EXPECTED_GATEWAY", bootstrap)
        self.assertIn("static domain_name_servers=$EXPECTED_GATEWAY", bootstrap)
        self.assertIn("nooption classless_static_routes", bootstrap)
        self.assertIn("nooption ms_classless_static_routes", bootstrap)
        self.assertIn("noipv6", bootstrap)
        self.assertIn('dhcpcd -n "$NETWORK_INTERFACE"', bootstrap)
        self.assertLess(bootstrap.index("/etc/dhcpcd.conf"), bootstrap.index("apk update"))
        self.assertLess(bootstrap.index("rc-service qemu-guest-agent start"), bootstrap.index("cmp -n"))
        roundtrip = (ROOT / "synthetic-roundtrip.sh").read_text()
        self.assertIn('chmod 0640 "$source_dir/empty"', roundtrip)
        for evidence in ("payload.tar.gz", "expected.manifest", "tar --numeric-owner", "diff -u"):
            self.assertIn(evidence, roundtrip)


if __name__ == "__main__":
    unittest.main()
