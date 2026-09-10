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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('component', choices=release.COMPONENTS)
    parser.add_argument('image')
    args = parser.parse_args()
    release.buildable(args.component)
    assert run('docker', 'image', 'inspect', '--format', '{{.Config.User}}', args.image) == '10001:10001'
    if args.component == 'panel':
        subprocess.run(['bash', str(release.ROOT / 'scripts/test-container.sh'), args.image], check=True)
    elif args.component == 'cursor':
        missing = subprocess.run(['docker', 'run', '--rm', '--network', 'none', '--read-only', args.image], capture_output=True)
        assert missing.returncode == 1, 'missing mounted config must fail closed'
        cursor_smoke(args.image)
    else:
        raise RuntimeError('CODEX_NATIVE_CONTAINER_SMOKE_REQUIRED_HL258')


if __name__ == '__main__':
    main()
