#!/usr/bin/env python3
"""Enroll existing exact component releases; never replaces app containers."""
import argparse
import hashlib
from pathlib import Path
import socket
import subprocess

import component_cd as cd
import component_deploy as host
import deploy

INGRESS = Path('/etc/homelab-panel/nginx.conf')
SERVICE = Path('/etc/init.d/homelab-components-cd')
SCRIPTS = ('cd.py', 'deploy.py', 'component_release.py', 'component_deploy.py', 'component_cd.py')
LOCATION = '''    location = /panel/components.json {
      alias /var/lib/homelab-panel-public/components.json;
      default_type application/json;
      add_header Cache-Control "no-store" always;
      add_header X-Content-Type-Options "nosniff" always;
      add_header Content-Security-Policy "default-src 'none'; frame-ancestors 'none'" always;
      limit_except GET { deny all; }
    }
'''
INIT = '''#!/sbin/openrc-run
description="Independent Panel / Cursor / Codex release consumer"
command="/usr/bin/python3"
command_args="/opt/homelab-agents-cd/executor/component_cd.py serve"
command_background="yes"
pidfile="/run/homelab-components-cd.pid"
output_log="/var/log/homelab-components-cd.log"
error_log="/var/log/homelab-components-cd.log"
depend() { need net docker; after homelab-panel-ingress; }
'''


def run(*args):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tag', action='append', required=True, help='One current published tag per configured component')
    parser.add_argument('--expected-ingress-sha256', required=True)
    args = parser.parse_args()
    deploy.require(deploy.os.geteuid() == 0 and socket.gethostname() == 'alpine-docker', 'WRONG_TARGET')
    deploy.require(not SERVICE.exists() and not (host.ROOT / 'deployment.json').exists(), 'ALREADY_ENROLLED')
    config = host.configuration(host.ROOT)
    executor = host.ROOT / 'executor'
    deploy.private(executor, directory=True)
    for name in SCRIPTS:
        deploy.private(executor / name)
    initial = {}
    for tag in args.tag:
        manifest = cd.download(tag)
        name = manifest['component']
        deploy.require(name not in initial, 'DUPLICATE_COMPONENT')
        initial[name] = {'current': manifest, 'previous': None}
    deploy.require(set(initial) == set(config['components']), 'ENROLLMENT_COMPONENT_MISMATCH')
    before = INGRESS.read_bytes()
    deploy.require(hashlib.sha256(before).hexdigest() == args.expected_ingress_sha256 and
                   b'/panel/components.json' not in before and before.count(b'    location ^~ /panel/') == 1, 'INGRESS_CHANGED')
    # Validate existing runtime before persisting enrollment or changing ingress.
    installer = object.__new__(host.Installer)
    installer.root, installer.config = host.ROOT, config
    installer.router = host.RouterControl(Path(config['router_socket']))
    identities = {}
    for name, slot in initial.items():
        installer.inspect(name, slot['current'])
        health = installer.health(name, slot['current'])
        if name != 'panel':
            identities[name] = installer.identity(health)
    # Persist every exact baseline override while Router admission is still
    # sealed. A partial write remains fail-closed and a retry rewrites the same
    # immutable manifests; no service mutation is issued here.
    for name, slot in initial.items():
        override = installer.pin_override(name, slot['current'])
        service = config['components'][name]['service']
        deploy.require(deploy.read_json(override) ==
                       {'services': {service: {'image': slot['current']['image']}}},
                       'BASELINE_OVERRIDE_MISMATCH')
    routes = installer.routing()
    sealed = {}
    activation_identities = {}
    for name, identity in identities.items():
        node_id = config['components'][name]['node_id']
        route = routes['nodes'][node_id]
        if route['mode'] == 'sealed' and route.get('operationId') == 'bootstrap':
            sealed[node_id] = route
            activation_identities[node_id] = identity
        else:
            deploy.require(route['mode'] == 'eligible' and route['identityEpoch'] == identity['epoch'] and
                           route['adapterVersion'] == identity['version'], 'ROUTER_NOT_BOOTSTRAPPED')
    if sealed:
        deploy.require(len(sealed) == len(identities), 'ROUTER_BOOTSTRAP_PARTIAL_STATE')
        installer.router.transition_many('activate', sealed, 'bootstrap', activation_identities)
    installer.require_routes(list(identities), 'eligible')
    ledger = {'components': initial, 'pending': None, 'config_sha256': installer.fingerprint()}
    deploy.atomic_json(host.ROOT / 'deployment.json', ledger)
    installer = host.Installer()
    host.PUBLIC.parent.mkdir(mode=0o755, exist_ok=True)
    # Ignore historical requests on first boot; enrollment does not authorize replay.
    pending = cd.cd.github('/deployments?environment=' + cd.ENVIRONMENT + '&per_page=1')
    record = {'id': pending[0]['id'] if pending else 0, 'status': 'idle', 'result': 'BOOTSTRAP_COMPLETE'}
    deploy.atomic_json(host.ROOT / 'request.json', record)
    cd.publish(record, installer)
    backup = host.ROOT / 'nginx.before-components.conf'
    with backup.open('xb') as out:
        out.write(before)
    candidate = before.replace(b'    location ^~ /panel/', LOCATION.encode() + b'    location ^~ /panel/')
    written = False
    try:
        deploy.require(INGRESS.read_bytes() == before, 'INGRESS_CHANGED')
        INGRESS.write_bytes(candidate)
        written = True
        run('nginx', '-t', '-c', str(INGRESS))
        run('nginx', '-s', 'reload', '-c', str(INGRESS))
    except Exception:
        # Never overwrite an operator's concurrent edit, including a failed CAS.
        if not written or INGRESS.read_bytes() != candidate:
            raise
        INGRESS.write_bytes(before)
        run('nginx', '-t', '-c', str(INGRESS))
        run('nginx', '-s', 'reload', '-c', str(INGRESS))
        raise
    SERVICE.write_text(INIT)
    SERVICE.chmod(0o755)
    run('rc-service', 'homelab-components-cd', 'start')
    run('rc-update', 'add', 'homelab-components-cd', 'default')
    run('rc-service', 'homelab-components-cd', 'status')
    print('COMPONENT_CD_ENROLLED_NO_APP_RESTART')


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, deploy.DeployError) else 'COMPONENT_BOOTSTRAP_FAILED_INSPECT_STATE')
        raise SystemExit(1) from None
