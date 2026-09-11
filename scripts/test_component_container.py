#!/usr/bin/env python3
"""Disposable packaging/startup smoke. No native model calls or real secrets."""
import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import uuid

import component_release as release


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE).strip()


def codex_denied_features():
    source = (release.ROOT / 'harness/adapters/codex/adapter.go').read_text()
    match = re.search(r'var deniedNativeFeatures = \[\]string\{(.*?)\n\}', source, re.S)
    assert match is not None, 'Codex native feature deny list is missing'
    features = re.findall(r'"([a-z0-9_]+)"', match.group(1))
    assert features and len(features) == len(set(features)), 'Codex native feature deny list is invalid'
    return features


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


def codex_smoke(image):
    prefix = 'hl258-smoke-' + uuid.uuid4().hex
    volumes = {name: prefix + '-' + name for name in ('config', 'state', 'auth', 'workspace', 'native')}
    container = prefix + '-node'
    openssl = shutil.which('openssl')
    if Path('/opt/homebrew/opt/openssl@3/bin/openssl').exists():
        openssl = '/opt/homebrew/opt/openssl@3/bin/openssl'
    with tempfile.TemporaryDirectory(prefix='hl258-container-') as directory:
        root = Path(directory) / 'fixture'
        run('python3', str(release.ROOT / 'scripts/harness-alpha/setup.py'), '--directory', str(root),
            '--owner-id', 'fixture-owner', '--adapter', 'codex',
            '--codex-executable', '/opt/codex/node_modules/.bin/codex', '--codex-home', '/auth/codex',
            '--codex-model', 'fixture-model', '--openssl', openssl, '--container')
        for volume in volumes.values():
            run('docker', 'volume', 'create', volume)
        mounts = [
            '-v', volumes['config'] + ':/config', '-v', volumes['state'] + ':/state',
            '-v', volumes['auth'] + ':/auth', '-v', volumes['workspace'] + ':/workspace:ro',
        ]
        try:
            run('docker', 'run', '--rm', '--network', 'none', '--user', '0:0',
                '-v', str(root / 'node-config') + ':/source:ro',
                '-v', str(root) + ':/fixture:ro',
                '-v', volumes['config'] + ':/config', '-v', volumes['state'] + ':/state',
                '-v', volumes['auth'] + ':/auth', '-v', volumes['workspace'] + ':/workspace',
                '-v', volumes['native'] + ':/native',
                '--entrypoint', '/bin/sh', image, '-c',
                'cp -R /source/. /config/ && cp /fixture/gateway.pem /fixture/gateway.key /config/ '
                '&& mkdir -p /state/codex/home /auth/codex /native/home /native/codex '
                '&& chown -R 10001:10001 /config /state /auth /workspace /native '
                '&& chmod 700 /config /state /state/codex /state/codex/home /auth /auth/codex /workspace /native /native/home /native/codex')
            native_probe = r"""const {spawn}=require('node:child_process');
const denied=__DENIED__,features=Object.fromEntries(denied.map(name=>[name,false]));
const child=spawn('/opt/codex/node_modules/.bin/codex',['app-server','--listen','stdio://'],{env:{
HOME:'/native/home',CODEX_HOME:'/native/codex',PATH:'/opt/codex/node_modules/.bin:/usr/local/bin:/usr/bin:/bin',
SSL_CERT_FILE:'/etc/ssl/certs/ca-certificates.crt'},stdio:['pipe','pipe','ignore']});
let next=0,buffer='';const pending=new Map(),stop=()=>{try{child.kill('SIGTERM')}catch{}};
child.stdout.on('data',chunk=>{buffer+=chunk.toString('utf8');for(;;){const end=buffer.indexOf('\n');if(end<0)break;
const line=buffer.slice(0,end);buffer=buffer.slice(end+1);if(!line)continue;let frame;try{frame=JSON.parse(line)}catch(error){stop();continue}
if(frame.id===undefined||frame.method)continue;const saved=pending.get(String(frame.id));if(!saved)continue;pending.delete(String(frame.id));
frame.error?saved.reject(new Error('rpc '+String(frame.error.code))):saved.resolve(frame.result);}});
const call=(method,params)=>new Promise((resolve,reject)=>{const id=++next,timer=setTimeout(()=>{pending.delete(String(id));reject(new Error('timeout'))},10000);
pending.set(String(id),{resolve:value=>{clearTimeout(timer);resolve(value)},reject:error=>{clearTimeout(timer);reject(error)}});
child.stdin.write(JSON.stringify({id,method,params})+'\n');});
const notify=(method,params)=>child.stdin.write(JSON.stringify({method,params})+'\n');
(async()=>{await call('initialize',{clientInfo:{name:'harness-codex-smoke',title:'Harness Codex smoke',version:'0.153.4'}});notify('initialized',{});
const started=await call('thread/start',{model:'gpt-5.2-codex',cwd:'/workspace',approvalPolicy:'never',sandbox:'read-only',
developerInstructions:'deny-only container smoke',config:{features,mcp_servers:{},web_search:'disabled'}});
if(!started?.thread?.id||started.approvalPolicy!=='never'||started.sandbox?.type!=='readOnly'||started.sandbox?.networkAccess!==false)throw new Error('thread policy mismatch');
let cursor=null,pages=0;const observed=new Map();do{const params={threadId:started.thread.id,limit:100};if(cursor)params.cursor=cursor;
const page=await call('experimentalFeature/list',params);if(!Array.isArray(page?.data))throw new Error('feature list invalid');
for(const feature of page.data)if(denied.includes(feature.name))observed.set(feature.name,feature.enabled);cursor=page.nextCursor;
if(++pages>10)throw new Error('feature pagination overflow');}while(cursor!==null);
for(const name of denied)if(observed.get(name)!==false)throw new Error('native feature enabled: '+name);
const mcp=await call('mcpServerStatus/list',{threadId:started.thread.id,limit:100,detail:'toolsAndAuthOnly'});
if(!Array.isArray(mcp?.data)||mcp.data.length!==0||mcp.nextCursor!==null)throw new Error('native MCP isolation mismatch');
console.log('CODEX_NATIVE_THREAD_POLICY_PASS; no turn/start, no model call');stop();setTimeout(()=>process.exit(0),250);
})().catch(error=>{console.error(error.message);stop();setTimeout(()=>process.exit(1),250)});""".replace('__DENIED__', json.dumps(codex_denied_features()))
            native = subprocess.run([
                'docker', 'run', '--rm', '--platform', 'linux/amd64', '--network', 'none', '--read-only', '--user', '10001:10001',
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                '--tmpfs', '/tmp:rw,noexec,nosuid,size=67108864',
                '-v', volumes['native'] + ':/native', '-v', volumes['workspace'] + ':/workspace:ro',
                '--entrypoint', 'node', image, '-e', native_probe,
            ], capture_output=True, text=True, timeout=20)
            assert native.returncode == 0, 'Codex native policy probe failed: ' + native.stderr.strip()[-256:]
            assert native.stdout.strip() == 'CODEX_NATIVE_THREAD_POLICY_PASS; no turn/start, no model call'
            run('docker', 'run', '-d', '--name', container, '--network', 'none', '--read-only',
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true', '--cpus', '1', '--memory', '1g',
                '--pids-limit', '128', *mounts, image, '--config', '/config/node.json')
            probe = """const https=require('node:https'),fs=require('node:fs');
const node=JSON.parse(fs.readFileSync('/config/node.json')).nodeId;
https.get({hostname:'127.0.0.1',port:18443,path:'/v1/nodes/'+node+'/identity',
ca:fs.readFileSync('/config/ca.pem'),cert:fs.readFileSync('/config/gateway.pem'),key:fs.readFileSync('/config/gateway.key'),
headers:{'X-Harness-Actor-ID':'fixture-owner'}},r=>{let d='';r.on('data',c=>d+=c);r.on('end',()=>{
const v=JSON.parse(d); const capabilities=Object.values(v.capabilities||{});
if(r.statusCode!==200||v.nodeId!==node||v.adapter.kind!=='codex'||v.adapter.version!=='0.153.4'||capabilities.length!==7||capabilities.some(x=>x!=='verified'))process.exit(1);
console.log(JSON.stringify({node:v.nodeId,epoch:v.identityEpoch,adapter:v.adapter,capabilities:v.capabilities}));});}).on('error',()=>process.exit(1));"""
            deadline = time.monotonic() + 30
            while True:
                try:
                    before = json.loads(run('docker', 'exec', container, 'node', '-e', probe))
                    break
                except subprocess.CalledProcessError:
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(1)
            assert run('docker', 'exec', container, '/opt/codex/node_modules/.bin/codex', '--version') == 'codex-cli 0.153.4'
            ledger_probe = """const fs=require('node:fs');
const state=JSON.parse(fs.readFileSync('/state/codex/native-mapping.json'));
if(state.schemaVersion!==2||Object.keys(state.attempts||{}).length!==0||Object.keys(state.dialogs||{}).length!==0)process.exit(1);
console.log('CODEX_EMPTY_DURABLE_DISPATCH_LEDGER_PASS');"""
            assert run('docker', 'exec', container, 'node', '-e', ledger_probe) == \
                'CODEX_EMPTY_DURABLE_DISPATCH_LEDGER_PASS'
            run('docker', 'exec', container, '/opt/codex/node_modules/.bin/codex', 'app-server',
                'generate-json-schema', '--out', '/state/schema-contract')
            schema_probe = """const fs=require('node:fs'),crypto=require('node:crypto');
const root='/state/schema-contract/'; const out={};
for(const name of ['ClientRequest.json','ServerNotification.json','ServerRequest.json'])
out[name]=crypto.createHash('sha256').update(fs.readFileSync(root+name)).digest('hex');
console.log(JSON.stringify(out));"""
            schemas = json.loads(run('docker', 'exec', container, 'node', '-e', schema_probe))
            assert schemas == {
                'ClientRequest.json': '25bc001b5dfe3b35785597b8f9ad9e5aaf7e437331fa9921f041c9e0e03fc9f3',
                'ServerNotification.json': 'b3e76cf11842f3e8b3270c05e000212b56eabafb0152fc38e8f920e2ef902991',
                'ServerRequest.json': '31f580ad468fbd18766eb7adb12744be4e3790e3357b58bd4413c0108c4f65d0',
            }, 'Codex app-server schema pin changed'
            run('docker', 'restart', container)
            deadline = time.monotonic() + 30
            while True:
                try:
                    after = json.loads(run('docker', 'exec', container, 'node', '-e', probe))
                    assert before == after, 'Harness identity changed on restart'
                    break
                except (subprocess.CalledProcessError, AssertionError):
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(1)
            info = json.loads(run('docker', 'inspect', container))[0]
            assert info['Config']['User'] == '10001:10001' and info['HostConfig']['ReadonlyRootfs']
            assert info['HostConfig']['NetworkMode'] == 'none'
            print('CODEX_CONTAINER_NATIVE_POLICY_MTLS_EMPTY_LEDGER_RESTART_PASS; zero turns, no model calls', flush=True)
        finally:
            subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for volume in volumes.values():
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
        missing = subprocess.run(['docker', 'run', '--rm', '--network', 'none', '--read-only', args.image], capture_output=True)
        assert missing.returncode == 1, 'missing mounted config must fail closed'
        codex_smoke(args.image)


if __name__ == '__main__':
    main()
