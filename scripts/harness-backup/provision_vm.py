#!/usr/bin/env python3
"""Run on PVE: provision only the new HL-248 backup VM after guarded preflight.

Default mode prints the reviewed plan. --apply creates the exact new resources.
Failures retain owned resources for inspection; existing workloads are untouched.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess

VMID = 116
NODE = 'pve'
STORAGE = 'harness-backup-vm'
MOUNT = Path('/mnt/pve/nas-storage')
MOUNT_UUID = 'd2c8edad-0e90-4987-9063-f82f42e44c45'
ROOT = MOUNT / STORAGE
IMAGE_NAME = 'generic_alpine-3.24.1-x86_64-bios-cloudinit-r0.qcow2'
IMAGE = Path('/var/lib/vz/template/cache') / IMAGE_NAME
CHECKSUM_URL = 'https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/cloud/' + IMAGE_NAME + '.sha512'
PUBLIC_KEY = Path('/tmp/docker-host-admin.pub')
PUBLIC_KEY_FINGERPRINT = 'SHA256:jESkcOmQ0c8IMsHSd/kC9RZy4cEtCzZ4EYfuiWullZ8'


def run(*args):
    return subprocess.check_output(list(args), text=True).strip()


def api(path):
    return json.loads(run('pvesh', 'get', path, '--output-format', 'json'))


def require(value, code):
    if not value:
        raise ValueError(code)


def owner_key():
    require(PUBLIC_KEY.is_file() and not PUBLIC_KEY.is_symlink(), 'public_key_missing')
    lines = PUBLIC_KEY.read_text().splitlines()
    require(len(lines) == 1 and lines[0].startswith('ssh-ed25519 '), 'single_owner_key_required')
    fingerprints = run('ssh-keygen', '-lf', str(PUBLIC_KEY)).splitlines()
    require(len(fingerprints) == 1, 'single_key_fingerprint_required')
    fields = fingerprints[0].split()
    require(len(fields) > 1 and fields[1] == PUBLIC_KEY_FINGERPRINT, 'owner_public_key_mismatch')
    return lines[0]


def disk_config(value):
    parts = value.split(',')
    return parts[0], dict(part.split('=', 1) for part in parts[1:])


def verify_final(config, system_volume, snippet):
    system, system_options = disk_config(config.get('scsi0', ''))
    data, data_options = disk_config(config.get('scsi1', ''))
    cloud, cloud_options = disk_config(config.get('ide2', ''))
    require(system == system_volume and system_options.get('size') == '8G'
            and system_options.get('serial') == 'HL248-BACKUP-SYS', 'system_disk_identity')
    require(re.fullmatch(STORAGE + r':116/vm-116-disk-[0-9]+\.qcow2', data)
            and data_options.get('size') == '128G'
            and data_options.get('serial') == 'HL248-BACKUP-DATA', 'data_disk_identity')
    require(cloud == 'local-lvm:vm-116-cloudinit' and cloud_options.get('media') == 'cdrom', 'cloudinit_disk_identity')
    require(config.get('cicustom') == 'user=' + STORAGE + ':snippets/' + snippet, 'cloudinit_source')
    require(config.get('name') == 'harness-backup' and int(config.get('memory', 0)) == 1024
            and int(config.get('cores', 0)) == 1 and int(config.get('balloon', -1)) == 0, 'vm_resources')
    require(config.get('boot') == 'order=scsi0' and config.get('scsihw') == 'virtio-scsi-pci'
            and config.get('ipconfig0') == 'ip=dhcp' and config.get('nameserver') == '10.202.2.4', 'boot_network_config')
    net = dict(part.split('=', 1) for part in config.get('net0', '').split(','))
    require(re.fullmatch(r'(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}', net.get('virtio', ''))
            and net.get('bridge') == 'vmbr0' and net.get('firewall') == '1', 'network_interface')
    data_path = Path(run('pvesm', 'path', data))
    require(not data_path.is_symlink() and data_path.resolve().parent == ROOT / 'images' / str(VMID), 'data_path_identity')
    info = json.loads(run('qemu-img', 'info', '--output=json', str(data_path)))
    require(info.get('format') == 'qcow2' and info.get('virtual-size') == 128 * 1024**3
            and not info.get('backing-filename'), 'new_data_image_identity')
    return {'systemVolume': system, 'dataVolume': data, 'cloudinitVolume': cloud}


def journal(**values):
    print(json.dumps({'createdResources': values}), flush=True)


def render_cloud_config(path):
    raw = path.read_text()
    require(raw.startswith('#cloud-config\n'), 'cloud_config_header')
    parsed = json.loads(raw.split('\n', 1)[1])
    require(parsed.get('ssh_pwauth') is False and parsed.get('disable_root') is True, 'cloud_config_auth')
    require(parsed.get('hostname') == 'harness-backup', 'cloud_config_hostname')
    # Custom user-data replaces PVE-generated user-data, including its SSH keys.
    # Use the owner key only after preflight has verified its fingerprint.
    parsed['users'] = ['default']
    parsed['ssh_authorized_keys'] = [owner_key()]
    return '#cloud-config\n' + json.dumps(parsed, indent=2) + '\n'


def preflight(cloud_config):
    require(run('hostname') == NODE, 'wrong_pve_node')
    resources = api('/cluster/resources')
    require(not any(item.get('vmid') == VMID for item in resources), 'vmid_in_use')
    for kind in ('qemu-server', 'lxc'):
        require(not (Path('/etc/pve') / kind / f'{VMID}.conf').exists(), 'vmid_config_exists')
    require(not any(item.get('storage') == STORAGE for item in api('/storage')), 'storage_exists')
    require(not ROOT.exists(), 'storage_path_exists')
    for storage in api('/nodes/pve/storage'):
        if storage.get('active') == 1 and 'images' in storage.get('content', '').split(','):
            volumes = json.loads(run('pvesh', 'get', f"/nodes/pve/storage/{storage['storage']}/content",
                                     '--vmid', str(VMID), '--output-format', 'json'))
            require(not volumes, 'orphan_volumes_exist:' + storage['storage'])
    mount = json.loads(run('findmnt', '--json', '--target', str(MOUNT), '--output', 'TARGET,UUID,FSTYPE'))['filesystems']
    require(len(mount) == 1 and mount[0].get('target') == str(MOUNT)
            and mount[0].get('uuid') == MOUNT_UUID and mount[0].get('fstype') == 'ext4', 'backup_mount_identity')
    require(shutil.disk_usage(MOUNT).free > 256 * 1024**3, 'backup_capacity')
    system_store = api('/nodes/pve/storage/local-lvm/status')
    require(system_store.get('active') == 1 and system_store.get('avail', 0) > 12 * 1024**3, 'system_capacity')
    require(api('/nodes/pve/status')['memory']['available'] > 2 * 1024**3, 'memory_capacity')
    owner_key()
    require(IMAGE.is_file() and not IMAGE.is_symlink(), 'clean_image_missing')
    require(cloud_config.is_file() and 0 < cloud_config.stat().st_size <= 128 * 1024, 'cloud_config_invalid')
    raw = render_cloud_config(cloud_config)
    expected = run('curl', '--fail', '--silent', '--show-error', '--proto', '=https', '--tlsv1.2', CHECKSUM_URL).split()[0]
    require(re.fullmatch(r'[a-fA-F0-9]{128}', expected) is not None, 'official_checksum_invalid')
    digest = hashlib.sha512()
    with IMAGE.open('rb') as image:
        for chunk in iter(lambda: image.read(1024 * 1024), b''):
            digest.update(chunk)
    require(digest.hexdigest().lower() == expected.lower(), 'clean_image_checksum_mismatch')
    info = json.loads(run('qemu-img', 'info', '--output=json', str(IMAGE)))
    require(info.get('format') == 'qcow2' and not info.get('backing-filename'), 'clean_image_backing')
    require(0 < info.get('virtual-size', 0) <= 8 * 1024**3, 'clean_image_size')
    return {'issue': 'HL-248', 'node': NODE, 'vmid': VMID, 'cpu': 1, 'memoryMiB': 1024,
            'systemDiskGiB': 8, 'dataDiskGiB': 128, 'storage': STORAGE, 'storagePath': str(ROOT),
            'mountUuid': MOUNT_UUID, 'dataSerial': 'HL248-BACKUP-DATA', 'address': 'dhcp',
            'gateway': '10.202.2.4', 'dns': '10.202.2.4', 'imageSha512': digest.hexdigest(),
            'cloudConfigSha256': hashlib.sha256(raw.encode()).hexdigest()}


def provision(cloud_config):
    run('pvesm', 'add', 'dir', STORAGE, '--path', str(ROOT), '--content', 'images,snippets',
        '--nodes', NODE, '--is_mountpoint', str(MOUNT), '--preallocation', 'off')
    journal(storage=STORAGE, path=str(ROOT))
    snippets = ROOT / 'snippets'
    snippets.mkdir(exist_ok=True)
    destination = snippets / 'hl248-backup-116.yaml'
    rendered = render_cloud_config(cloud_config)
    with destination.open('x') as output:
        destination.chmod(0o600)
        output.write(rendered)
    require(destination.read_text() == rendered, 'cloud_config_readback')
    journal(snippet=str(destination))
    run('qm', 'create', str(VMID), '--name', 'harness-backup', '--memory', '1024', '--balloon', '0',
        '--cores', '1', '--net0', 'virtio,bridge=vmbr0,firewall=1', '--scsihw', 'virtio-scsi-pci',
        '--ostype', 'l26', '--agent', 'enabled=1,fstrim_cloned_disks=1', '--serial0', 'socket',
        '--vga', 'serial0', '--onboot', '1', '--description', 'HL-248: dedicated Harness backup VM; DHCP, OpenWRT gateway/DNS; no public ingress.')
    journal(vmid=VMID)
    run('qm', 'importdisk', str(VMID), str(IMAGE), 'local-lvm', '--format', 'raw')
    config = api(f'/nodes/{NODE}/qemu/{VMID}/config')
    volumes = [value for key, value in config.items() if re.fullmatch(r'unused[0-9]+', key)]
    require(len(volumes) == 1 and re.fullmatch(r'local-lvm:vm-116-disk-[0-9]+', volumes[0]), 'imported_volume_identity')
    journal(importedVolume=volumes[0])
    run('qm', 'set', str(VMID), '--scsi0', volumes[0] + ',discard=on,ssd=1,serial=HL248-BACKUP-SYS',
        '--scsi1', STORAGE + ':128,format=qcow2,discard=on,serial=HL248-BACKUP-DATA',
        '--ide2', 'local-lvm:cloudinit', '--boot', 'order=scsi0', '--ciuser', 'alpine',
        '--sshkeys', str(PUBLIC_KEY), '--ipconfig0', 'ip=dhcp', '--nameserver', '10.202.2.4',
        '--cicustom', 'user=' + STORAGE + ':snippets/' + destination.name)
    run('qm', 'resize', str(VMID), 'scsi0', '8G')
    final = api(f'/nodes/{NODE}/qemu/{VMID}/config')
    journal(**verify_final(final, volumes[0], destination.name))
    run('qm', 'start', str(VMID))
    return {'vmid': VMID, 'started': True, 'runtimeAcceptance': 'pending_guest_verification'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cloud-config', required=True, type=Path)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    plan = preflight(args.cloud_config)
    print(json.dumps({'plan': plan, 'apply': args.apply}), flush=True)
    if args.apply:
        print(json.dumps(provision(args.cloud_config)), flush=True)


if __name__ == '__main__':
    main()
