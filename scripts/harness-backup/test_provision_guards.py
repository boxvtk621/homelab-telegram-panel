#!/usr/bin/env python3
"""Exercise fail-closed boundaries without a PVE connection or disk writes."""
import copy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import provision_vm as p


class ProvisionGuards(unittest.TestCase):
    def test_occupied_vmid_stops_before_storage(self):
        with patch.object(p, 'run', return_value='pve'), \
                patch.object(p, 'api', return_value=[{'vmid': 116}]) as api:
            with self.assertRaisesRegex(ValueError, 'vmid_in_use'):
                p.preflight(Path('/never-read'))
            api.assert_called_once_with('/cluster/resources')

    def test_extra_key_cannot_enter_cloud_init(self):
        with tempfile.TemporaryDirectory() as tmp:
            key = Path(tmp) / 'owner.pub'
            key.write_text('ssh-ed25519 AAAA owner\nssh-ed25519 BBBB other\n')
            with patch.object(p, 'PUBLIC_KEY', key), patch.object(p, 'run') as run:
                with self.assertRaisesRegex(ValueError, 'single_owner_key_required'):
                    p.owner_key()
                run.assert_not_called()

    def test_wrong_fingerprint_cannot_enter_cloud_init(self):
        with tempfile.TemporaryDirectory() as tmp:
            key = Path(tmp) / 'owner.pub'
            key.write_text('ssh-ed25519 AAAA owner\n')
            with patch.object(p, 'PUBLIC_KEY', key), patch.object(p, 'run', return_value='256 SHA256:foreign owner'):
                with self.assertRaisesRegex(ValueError, 'owner_public_key_mismatch'):
                    p.owner_key()

    def test_substituted_disks_fail_before_image_access(self):
        config = {
            'scsi0': 'local-lvm:vm-116-disk-0,size=8G,serial=HL248-BACKUP-SYS',
            'scsi1': 'harness-backup-vm:116/vm-116-disk-0.qcow2,size=128G,serial=HL248-BACKUP-DATA',
            'ide2': 'local-lvm:vm-116-cloudinit,media=cdrom',
            'cicustom': 'user=harness-backup-vm:snippets/hl248-backup-116.yaml',
            'name': 'harness-backup', 'memory': 1024, 'cores': 1, 'balloon': 0,
            'boot': 'order=scsi0', 'scsihw': 'virtio-scsi-pci', 'ipconfig0': 'ip=dhcp',
            'nameserver': '10.202.2.4', 'net0': 'virtio=BC:24:11:12:34:56,bridge=vmbr0,firewall=1',
        }
        for key, value in (
            ('scsi0', 'local-lvm:vm-115-disk-0,size=8G,serial=HL248-BACKUP-SYS'),
            ('scsi1', 'harness-backup-vm:115/vm-115-disk-0.qcow2,size=128G,serial=HL248-BACKUP-DATA'),
            ('scsi1', config['scsi1'].replace('128G', '64G')),
            ('ide2', 'local-lvm:vm-115-cloudinit,media=cdrom'),
            ('cicustom', 'user=local:snippets/other.yaml'),
            ('net0', config['net0'].replace('vmbr0', 'vmbr1')),
        ):
            with self.subTest(key=key, value=value), patch.object(p, 'run') as run:
                altered = copy.deepcopy(config)
                altered[key] = value
                with self.assertRaises(ValueError):
                    p.verify_final(altered, 'local-lvm:vm-116-disk-0', 'hl248-backup-116.yaml')
                run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
