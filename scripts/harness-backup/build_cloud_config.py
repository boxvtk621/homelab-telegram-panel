#!/usr/bin/env python3
"""Render reviewed guest artifacts as cloud-init JSON (a YAML subset). No secrets."""
import argparse
import json
from pathlib import Path


def render(source):
    files = []
    for name in ('guest-bootstrap.sh', 'validate-guest.sh', 'synthetic-roundtrip.sh'):
        files.append({'path': '/root/' + name, 'owner': 'root:root',
                      'permissions': '0750', 'content': (source / name).read_text()})
    return '#cloud-config\n' + json.dumps({
        'hostname': 'harness-backup', 'manage_etc_hosts': True,
        'ssh_pwauth': False, 'disable_root': True,
        'write_files': files,
        'runcmd': [['/root/guest-bootstrap.sh', '--data-device', '/dev/sdb',
                    '--expected-serial', 'HL248-BACKUP-DATA', '--network-interface', 'eth0']],
    }, indent=2) + '\n'


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, default=Path(__file__).parent)
    args = parser.parse_args()
    print(render(args.source), end='')
