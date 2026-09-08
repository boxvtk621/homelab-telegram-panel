#!/usr/bin/env python3
"""Install already copied local CD scripts on the exact VM115 target.

Copy cd.py and deploy.py to /opt/homelab-panel/deployer first. They are the
reviewed host implementation; release bundles never replace this executor.
"""
import os
from pathlib import Path
import socket
import subprocess
import sys

ROOT = Path('/opt/homelab-panel/deployer')
SERVICE = Path('/etc/init.d/homelab-panel-cd')
INGRESS = Path('/etc/homelab-panel/nginx.conf')
STATUS_LOCATION = '''    location = /panel/deployment-status.json {
      alias /var/lib/homelab-panel-public/deployment-status.json;
      default_type application/json;
      add_header Cache-Control "no-store" always;
      add_header X-Content-Type-Options "nosniff" always;
      add_header Content-Security-Policy "default-src 'none'; frame-ancestors 'none'" always;
      limit_except GET { deny all; }
    }
'''
INIT = '''#!/sbin/openrc-run
description="Panel outbound GitHub Deployments consumer"
command="/usr/bin/python3"
command_args="/opt/homelab-panel/deployer/cd.py serve"
command_background="yes"
pidfile="/run/homelab-panel-cd.pid"
output_log="/var/log/homelab-panel-cd.log"
error_log="/var/log/homelab-panel-cd.log"
depend() { need net docker; after homelab-panel-ingress; }
'''


def run(*args):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)


def main():
    if os.geteuid() != 0 or socket.gethostname() != 'alpine-docker' or SERVICE.exists():
        raise ValueError('WRONG_TARGET_OR_ALREADY_PROVISIONED')
    if ROOT.is_symlink() or not ROOT.is_dir() or ROOT.stat().st_uid != 0 or ROOT.stat().st_mode & 0o077:
        raise ValueError('EXECUTOR_DIRECTORY_NOT_PRIVATE')
    for script in ('cd.py', 'deploy.py'):
        file = ROOT / script
        if file.is_symlink() or not file.is_file() or file.stat().st_uid != 0 or file.stat().st_mode & 0o077:
            raise ValueError('EXECUTOR_NOT_PROVISIONED_PRIVATELY')
    public = Path('/var/lib/homelab-panel-public')
    public.mkdir(mode=0o755)
    public.chmod(0o755)
    (Path('/opt/homelab-panel') / 'cd').mkdir(mode=0o700)
    before = INGRESS.read_text()
    if '/panel/deployment-status.json' in before or before.count('    location ^~ /panel/') != 1:
        raise ValueError('INGRESS_CHANGED')
    backup = INGRESS.with_name('nginx.before-cd.conf')
    with backup.open('x') as out:
        out.write(before)
    try:
        INGRESS.write_text(before.replace('    location ^~ /panel/', STATUS_LOCATION + '    location ^~ /panel/'))
        run('nginx', '-t', '-c', str(INGRESS))
        # Generate the public idle snapshot before starting periodic consumption.
        run('/usr/bin/python3', '-c', 'import sys; sys.path.insert(0, "/opt/homelab-panel/deployer"); import cd; cd.publish(cd.snapshot(0, "idle", "BOOTSTRAP_COMPLETE"))')
        run('nginx', '-s', 'reload', '-c', str(INGRESS))
    except Exception:
        INGRESS.write_text(before)
        run('nginx', '-t', '-c', str(INGRESS))
        run('nginx', '-s', 'reload', '-c', str(INGRESS))
        raise
    SERVICE.write_text(INIT)
    SERVICE.chmod(0o755)
    run('rc-service', 'homelab-panel-cd', 'start')
    run('rc-update', 'add', 'homelab-panel-cd', 'default')
    run('rc-service', 'homelab-panel-cd', 'status')
    print('PANEL_OUTBOUND_CD_STARTED')


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('CD_BOOTSTRAP_FAILED_INSPECT_OWN_STATE', file=sys.stderr)
        raise SystemExit(1) from None
