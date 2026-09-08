#!/usr/bin/env python3
"""First UI deployment on the exact approved VM115. No app credentials."""
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import urllib.request

ROOT = Path('/opt/homelab-panel')
CONFIG = Path('/etc/homelab-panel.env')
VERSION = 'v0.1.0-rc.5'
REVISION = '3a855a7350516c717aec7767f955566efee74540'
REPO = 'boxvtk621/homelab-telegram-panel'
FILES = {'compose.yaml', 'deploy.py', 'panel.env.example', 'release.json', 'SHA256SUMS'}


def fetch(url, maximum):
    with urllib.request.urlopen(url, timeout=30) as response:
        data = response.read(maximum + 1)
    if len(data) > maximum:
        raise ValueError('OVERSIZED_ASSET')
    return data


def main():
    if os.geteuid() != 0 or socket.gethostname() != 'alpine-docker' or CONFIG.exists():
        raise ValueError('FIRST_DEPLOY_PRECONDITION_FAILED')
    os.umask(0o077)
    metadata = json.loads(fetch('https://api.github.com/repos/' + REPO + '/releases/tags/' + VERSION, 131072))
    if metadata['draft'] or metadata['tag_name'] != VERSION or metadata['author']['login'] != 'github-actions[bot]' or set(a['name'] for a in metadata['assets']) != FILES:
        raise ValueError('RELEASE_IDENTITY_MISMATCH')
    target = ROOT / 'releases' / VERSION
    target.mkdir(mode=0o700)
    for asset in metadata['assets']:
        data = fetch('https://github.com/' + REPO + '/releases/download/' + VERSION + '/' + asset['name'], 1048576)
        if 'sha256:' + hashlib.sha256(data).hexdigest() != asset['digest']:
            raise ValueError('GITHUB_ASSET_DIGEST_MISMATCH')
        (target / asset['name']).write_bytes(data)
    manifest = json.loads((target / 'release.json').read_text())
    if manifest['version'] != VERSION or manifest['revision'] != REVISION:
        raise ValueError('RELEASE_REVISION_MISMATCH')
    subprocess.run(['python3', str(target / 'deploy.py'), 'verify'], check=True)
    with CONFIG.open('x') as config:
        config.write('PANEL_HOST_PORT=18080\nPANEL_PUBLIC_ORIGIN=https://h1-cloud.ru\nPANEL_BASE_PATH=/panel\nPANEL_YOUTRACK_URL=https://youtrack.h1-cloud.ru\nPANEL_PROJECT_ID=0-1\nPANEL_PROJECT_KEY=HL\nPANEL_OWNER_LOGIN=kondor\nPANEL_CURSOR_MODEL=composer-2.5\n')
    subprocess.run(['python3', str(target / 'deploy.py'), 'apply', '--config', str(CONFIG), '--state', str(ROOT / 'state'), '--allow-interrupt'], check=True)
    print('PANEL_RC5_INITIAL_DEPLOYMENT_COMPLETE')


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('PANEL_INITIAL_DEPLOYMENT_FAILED_INSPECT_EXACT_STATE', file=sys.stderr)
        raise SystemExit(1) from None
