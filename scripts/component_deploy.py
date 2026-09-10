#!/usr/bin/env python3
"""Trusted host installer; release assets never supply executable host code."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time

import deploy
import component_release as release

ROOT = Path('/opt/homelab-agents-cd')
PUBLIC = Path('/var/lib/homelab-panel-public/components.json')

# Runs inside the existing Panel container with its own client certificate.
# No provider credential or Harness state is mounted in this probe.
PROBE = '''import json,ssl,sys,urllib.request
base,node,actor,kind,version=sys.argv[1:]
ctx=ssl.create_default_context(cafile='/config/ca.pem')
ctx.load_cert_chain('/config/gateway.pem','/config/gateway.key')
def get(path):
 req=urllib.request.Request(base+path,headers={'X-Harness-Actor-ID':actor})
 with urllib.request.urlopen(req,context=ctx,timeout=5) as r:
  return json.loads(r.read(1048576))
identity=get('/v1/nodes/'+node+'/identity')
snapshot=get('/v1/nodes/'+node+'/snapshot')
assert identity['nodeId']==node and identity['adapter']=={'kind':kind,'version':version}
assert snapshot['nodeId']==node
state=snapshot['node']
assert state['queuePaused'] is True and state['occupancy']=='idle' and state['activeAttemptId'] is None
print(json.dumps({'node':node,'epoch':identity['identityEpoch'],'kind':kind, 'paused':True}))
'''


def run(*args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=180)
    deploy.require(result.returncode == 0, 'CONTAINER_OPERATION_FAILED')
    return result.stdout.strip()


def configuration(root):
    deploy.private(root, directory=True)
    file = root / 'config.json'
    deploy.private(file)
    config = deploy.read_json(file)
    deploy.require(set(config) == {'project', 'compose', 'env_file', 'components'}, 'INVALID_HOST_CONFIG')
    deploy.require(config['project'] in ('homelab-agents', 'homelab-panel-alpha'), 'WRONG_COMPOSE_PROJECT')
    deploy.require(set(config['components']) <= set(release.COMPONENTS) and 'panel' in config['components'], 'INVALID_HOST_COMPONENTS')
    for key in ('compose', 'env_file'):
        path = Path(config[key])
        deploy.require(path.is_absolute(), 'ABSOLUTE_CONFIG_PATH_REQUIRED')
        deploy.private(path)
    for name, component in config['components'].items():
        fields = {'service'} if name == 'panel' else {'service', 'url', 'node_id', 'actor_id'}
        deploy.require(set(component) == fields, 'INVALID_COMPONENT_CONFIG')
        deploy.require(component['service'] in {'panel': ('panel',), 'cursor': ('cursor', 'harness'), 'codex': ('codex',)}[name], 'SERVICE_MISMATCH')
        if name != 'panel':
            deploy.require(component['url'] == 'https://' + component['service'] + ':18443', 'PRIVATE_NODE_URL_REQUIRED')
            deploy.require(deploy.re.fullmatch('[0-9a-f-]{36}', component['node_id']) and
                           isinstance(component['actor_id'], str) and component['actor_id'] and
                           not any(c in component['actor_id'] for c in '\r\n\0'), 'INVALID_NODE_IDENTITY')
    return config


def compatible(current, target):
    release.validate(target)
    if current:
        release.validate(current, target['component'])
        deploy.require(current['state_compatibility'] == target['state_compatibility'], 'STATE_CHANGE_REQUIRES_OPERATOR_MIGRATION')


class Installer:
    def __init__(self, root=ROOT):
        self.root = root
        self.config = configuration(root)
        self.file = root / 'deployment.json'
        deploy.private(self.file)
        self.ledger = deploy.read_json(self.file)
        deploy.require(set(self.ledger) == {'components', 'pending', 'config_sha256'}, 'INVALID_COMPONENT_LEDGER')
        deploy.require(self.ledger['config_sha256'] == self.fingerprint(), 'CONFIG_DRIFT_REQUIRES_OPERATOR')
        deploy.require(set(self.ledger['components']) == set(self.config['components']), 'LEDGER_COMPONENT_MISMATCH')
        for name, value in self.ledger['components'].items():
            deploy.require(set(value) == {'current', 'previous'}, 'INVALID_COMPONENT_LEDGER')
            release.validate(value['current'], name)
            if value['previous'] is not None:
                release.validate(value['previous'], name)

    def fingerprint(self):
        digest = hashlib.sha256()
        for file in (self.root / 'config.json', Path(self.config['compose']), Path(self.config['env_file'])):
            digest.update(file.read_bytes() + b'\0')
        return digest.hexdigest()

    def compose(self, *args, override=None):
        command = ['docker', 'compose', '--project-name', self.config['project'],
                   '--env-file', self.config['env_file'], '-f', self.config['compose']]
        if override:
            command += ['-f', str(override)]
        return run(*command, *args)

    def container(self, name):
        ids = self.compose('ps', '-q', self.config['components'][name]['service']).splitlines()
        deploy.require(len(ids) == 1 and deploy.re.fullmatch('[0-9a-f]{12,64}', ids[0]), 'COMPONENT_CONTAINER_MISSING')
        return ids[0]

    def inspect(self, name, manifest):
        data = json.loads(run('docker', 'inspect', self.container(name)))[0]
        image = json.loads(run('docker', 'image', 'inspect', manifest['image']))[0]
        labels = image['Config'].get('Labels', {})
        deploy.require(data['State']['Running'] and data['Image'] == image['Id'] and
                       manifest['image'] in image.get('RepoDigests', []) and image['Os'] == 'linux' and
                       image['Architecture'] == 'amd64' and image['Config']['User'] == '10001:10001' and
                       labels.get('org.opencontainers.image.version') == manifest['version'] and
                       labels.get('org.opencontainers.image.revision') == manifest['revision'], 'RUNTIME_IMAGE_MISMATCH')
        deploy.require(data['Config']['User'] == '10001:10001' and data['HostConfig']['ReadonlyRootfs'] and
                       not data['HostConfig']['Privileged'] and 'ALL' in data['HostConfig']['CapDrop'] and
                       'no-new-privileges:true' in data['HostConfig']['SecurityOpt'], 'RUNTIME_ISOLATION_MISMATCH')

    def health(self, name, manifest):
        if name == 'panel':
            import cd
            cd.public_health()
            return None
        config = self.config['components'][name]
        return json.loads(run('docker', 'exec', self.container('panel'), '/usr/local/bin/python3', '-c', PROBE,
                              config['url'], config['node_id'], config['actor_id'], name, manifest['adapter_version']))

    def replace(self, name, manifest):
        service = self.config['components'][name]['service']
        override = self.root / (name + '.override.json')
        deploy.atomic_json(override, {'services': {service: {'image': manifest['image']}}})
        self.compose('up', '-d', '--no-deps', '--pull', 'never', service, override=override)

    def wait_healthy(self, name, manifest, expected):
        deadline = time.monotonic() + 45
        while True:
            try:
                self.inspect(name, manifest)
                deploy.require(self.health(name, manifest) == expected, 'NODE_IDENTITY_OR_PAUSE_CHANGED')
                return
            except Exception:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(2)

    def apply(self, name, target, rollback=False):
        deploy.require(name in self.ledger['components'], 'COMPONENT_NOT_ENROLLED')
        slot = self.ledger['components'][name]
        prior = slot['current']
        compatible(prior, target)
        deploy.require(self.ledger['pending'] is None, 'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
        if rollback:
            deploy.require(target == slot['previous'], 'ROLLBACK_TARGET_CHANGED')
        before_ids = {other: self.container(other) for other in self.config['components'] if other != name}
        self.inspect(name, prior)
        before_health = self.health(name, prior)
        if target == prior:
            return 'ALREADY_CURRENT'
        run('docker', 'pull', target['image'])
        image = json.loads(run('docker', 'image', 'inspect', target['image']))[0]
        labels = image['Config'].get('Labels', {})
        deploy.require(image['Os'] == 'linux' and image['Architecture'] == 'amd64' and
                       image['Config']['User'] == '10001:10001' and target['image'] in image.get('RepoDigests', []) and
                       labels.get('org.opencontainers.image.version') == target['version'] and
                       labels.get('org.opencontainers.image.revision') == target['revision'], 'CANDIDATE_IMAGE_MISMATCH')
        # Re-check after the potentially long download, before any replacement.
        deploy.require(self.fingerprint() == self.ledger['config_sha256'], 'CONFIG_CHANGED_DURING_DEPLOY')
        deploy.require(self.health(name, prior) == before_health, 'NODE_CHANGED_DURING_DEPLOY')
        self.ledger['pending'] = {'component': name, 'prior': prior, 'target': target}
        deploy.atomic_json(self.file, self.ledger)
        try:
            self.replace(name, target)
            self.wait_healthy(name, target, before_health)
            deploy.require(all(self.container(other) == value for other, value in before_ids.items()), 'OTHER_COMPONENT_RESTARTED')
        except Exception:
            # Schema/native-store compatibility was checked before replacement.
            # Keep pending durable if restoration or its readback fails.
            self.replace(name, prior)
            self.wait_healthy(name, prior, before_health)
            self.ledger['pending'] = None
            deploy.atomic_json(self.file, self.ledger)
            raise deploy.DeployError('DEPLOYMENT_FAILED_PRIOR_RESTORED') from None
        self.ledger['components'][name] = {'current': target, 'previous': prior}
        self.ledger['pending'] = None
        deploy.atomic_json(self.file, self.ledger)
        return 'ROLLED_BACK' if rollback else 'DEPLOYED'
