#!/usr/bin/env python3
"""Disposable packaging/startup smoke. No native model calls or real secrets."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import uuid

import component_release as release


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE).strip()


def cursor_smoke(image):
    prefix = 'hl242-smoke-' + uuid.uuid4().hex
    volume, container = prefix + '-data', prefix + '-node'
    openssl = shutil.which('openssl')
    if Path('/opt/homebrew/opt/openssl@3/bin/openssl').exists():
        openssl = '/opt/homebrew/opt/openssl@3/bin/openssl'
    with tempfile.TemporaryDirectory(prefix='hl242-container-') as directory:
        root = Path(directory) / 'fixture'
        run('python3', str(release.ROOT / 'scripts/harness-alpha/setup.py'), '--directory', str(root),
            '--owner-id', 'fixture-owner', '--cursor-key-file', '/fixture/test-key', '--openssl', openssl)
        config = json.loads((root / 'node.json').read_text())
        for key, value in config.items():
            if isinstance(value, str) and value.startswith(str(root)):
                config[key] = value.replace(str(root), '/fixture', 1)
        config['cursor'].update(nodeExecutable='/usr/local/bin/node', workerEntrypoint='/opt/worker/worker.mjs',
                                stateDir='/fixture/cursor-state', apiKeyFile='/fixture/test-key')
        (root / 'node.json').write_text(json.dumps(config))
        (root / 'test-key').write_text('synthetic-container-smoke-only')
        (root / 'test-key').chmod(0o600)
        run('docker', 'volume', 'create', volume)
        try:
            run('docker', 'run', '--rm', '--network', 'none', '--user', '0:0',
                '-v', str(root) + ':/source:ro', '-v', volume + ':/fixture', '--entrypoint', '/bin/sh', image,
                '-c', 'cp -R /source/. /fixture/ && chown -R 10001:10001 /fixture && chmod 700 /fixture')
            run('docker', 'run', '-d', '--name', container, '--network', 'none', '--read-only',
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true', '--cpus', '1', '--memory', '1g',
                '--pids-limit', '128', '-v', volume + ':/fixture', image, '--config', '/fixture/node.json')
            probe = """const https=require('node:https'),fs=require('node:fs');
const node=JSON.parse(fs.readFileSync('/fixture/node.json')).nodeId;
https.get({hostname:'127.0.0.1',port:18443,path:'/v1/nodes/'+node+'/identity',
ca:fs.readFileSync('/fixture/ca.pem'),cert:fs.readFileSync('/fixture/gateway.pem'),key:fs.readFileSync('/fixture/gateway.key'),
headers:{'X-Harness-Actor-ID':'fixture-owner'}},r=>{let d='';r.on('data',c=>d+=c);r.on('end',()=>{
const v=JSON.parse(d); if(r.statusCode!==200||v.nodeId!==node||v.adapter.kind!=='cursor')process.exit(1);
console.log(JSON.stringify({node:v.nodeId,epoch:v.identityEpoch,adapter:v.adapter}));});}).on('error',()=>process.exit(1));"""
            deadline = time.monotonic() + 30
            while True:
                try:
                    before = json.loads(run('docker', 'exec', container, 'node', '-e', probe))
                    break
                except subprocess.CalledProcessError:
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(1)
            run('docker', 'restart', container)
            deadline = time.monotonic() + 30
            while True:
                try:
                    after = json.loads(run('docker', 'exec', container, 'node', '-e', probe))
                    assert before == after, 'Harness identity changed on restart'
                    break
                except subprocess.CalledProcessError:
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(1)
            info = json.loads(run('docker', 'inspect', container))[0]
            assert info['Config']['User'] == '10001:10001' and info['HostConfig']['ReadonlyRootfs']
            assert info['HostConfig']['NetworkMode'] == 'none'
            print('CURSOR_CONTAINER_MTLS_IDENTITY_RESTART_PASS; no model calls', flush=True)
        finally:
            subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'volume', 'rm', volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def panel_router_smoke(image):
    prefix = 'hl260-panel-smoke-' + uuid.uuid4().hex
    config_volume, state_volume, container = prefix + '-config', prefix + '-state', prefix + '-panel'
    openssl = shutil.which('openssl')
    if Path('/opt/homebrew/opt/openssl@3/bin/openssl').exists():
        openssl = '/opt/homebrew/opt/openssl@3/bin/openssl'
    with tempfile.TemporaryDirectory(prefix='hl260-panel-') as directory:
        root = Path(directory) / 'fixture'
        run('python3', str(release.ROOT / 'scripts/harness-alpha/setup.py'), '--directory', str(root),
            '--owner-id', 'fixture-owner', '--cursor-key-file', '/fixture/test-key', '--openssl', openssl, '--container')
        run('docker', 'volume', 'create', config_volume)
        run('docker', 'volume', 'create', state_volume)
        environment = [
            '-e', 'PANEL_LISTEN=0.0.0.0:18080', '-e', 'PANEL_PUBLIC_ORIGIN=https://panel.example.invalid',
            '-e', 'PANEL_HARNESS_COMMANDS_ENABLED=true',
            '-e', 'PANEL_HARNESS_REGISTRY=/config/registry.json',
            '-e', 'PANEL_HARNESS_SIGNER_PUBLIC_KEY=/config/registry-signing.pem',
            '-e', 'PANEL_HARNESS_CA=/config/ca.pem', '-e', 'PANEL_HARNESS_CLIENT_CERT=/config/gateway.pem',
            '-e', 'PANEL_HARNESS_CLIENT_KEY=/config/gateway.key',
            '-e', 'PANEL_HARNESS_ROUTER_STATE=/router/state.json',
            '-e', 'PANEL_HARNESS_ROUTER_SOCKET=/router/control.sock',
        ]
        mounts = ['-v', config_volume + ':/config', '-v', state_volume + ':/router']
        try:
            run('docker', 'run', '--rm', '--user', '0:0', '-v', str(root / 'panel-config') + ':/source:ro',
                *mounts, '--entrypoint', '/bin/sh', image, '-c',
                'cp -R /source/. /config/ && chown -R 10001:10001 /config /router && chmod 700 /config /router')
            assert run('docker', 'run', '--rm', '--network', 'none', '--read-only', '--cap-drop', 'ALL',
                       '--security-opt', 'no-new-privileges:true', *environment, *mounts, image,
                       'router-bootstrap') == 'ROUTER_BOOTSTRAPPED'
            duplicate = subprocess.run(['docker', 'run', '--rm', '--network', 'none', '--read-only', '--cap-drop', 'ALL',
                                        '--security-opt', 'no-new-privileges:true', *environment, *mounts, image,
                                        'router-bootstrap'], capture_output=True, text=True, timeout=15)
            assert duplicate.returncode == 1 and 'ROUTER_BOOTSTRAP_FAILED' in duplicate.stdout
            run('docker', 'run', '-d', '--name', container, '--read-only', '--cap-drop', 'ALL',
                '--security-opt', 'no-new-privileges:true', '-p', '127.0.0.1::18080', *environment, *mounts,
                image, 'serve')
            address = run('docker', 'port', container, '18080/tcp')
            deadline = time.monotonic() + 15
            while True:
                try:
                    health = run('curl', '--fail', '--silent', '--max-time', '1', '-H', 'Host: panel.example.invalid',
                                 'http://' + address + '/api/v2/healthz')
                    assert json.loads(health) == {'panel': 'up'}
                    break
                except (subprocess.CalledProcessError, AssertionError):
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(.5)
            state = json.loads(run('docker', 'exec', container, '/usr/local/bin/python3', '-c',
                                   "import os,stat; p='/router/control.sock'; s=os.stat(p); "
                                   "assert stat.S_ISSOCK(s.st_mode) and s.st_mode & 0o777 == 0o600; "
                                   "print(open('/router/state.json').read())"))
            assert list(state['nodes'].values())[0]['mode'] == 'sealed'
            competing = subprocess.run(['docker', 'run', '--rm', '--read-only', '--cap-drop', 'ALL',
                                        '--security-opt', 'no-new-privileges:true', *environment, *mounts, image,
                                        'serve'], capture_output=True, text=True, timeout=15)
            assert competing.returncode == 2 and 'CONFIG_INVALID' in competing.stdout
            run('docker', 'restart', container)
            deadline = time.monotonic() + 15
            while True:
                try:
                    state = json.loads(run('docker', 'exec', container, '/usr/local/bin/python3', '-c',
                                           "import json; print(open('/router/state.json').read())"))
                    assert list(state['nodes'].values())[0]['mode'] == 'sealed'
                    break
                except (subprocess.CalledProcessError, AssertionError):
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(.5)
            print('PANEL_ROUTER_SEALED_RESTART_PASS; no node or provider calls', flush=True)
        finally:
            subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'volume', 'rm', config_volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'volume', 'rm', state_volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('component', choices=release.COMPONENTS)
    parser.add_argument('image')
    args = parser.parse_args()
    release.buildable(args.component)
    assert run('docker', 'image', 'inspect', '--format', '{{.Config.User}}', args.image) == '10001:10001'
    if args.component == 'panel':
        subprocess.run(['bash', str(release.ROOT / 'scripts/test-container.sh'), args.image], check=True)
        panel_router_smoke(args.image)
    elif args.component == 'cursor':
        missing = subprocess.run(['docker', 'run', '--rm', '--network', 'none', '--read-only', args.image], capture_output=True)
        assert missing.returncode == 1, 'missing mounted config must fail closed'
        cursor_smoke(args.image)
    else:
        raise RuntimeError('CODEX_NATIVE_CONTAINER_SMOKE_REQUIRED_HL258')


if __name__ == '__main__':
    main()
