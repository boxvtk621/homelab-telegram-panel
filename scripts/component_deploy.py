#!/usr/bin/env python3
"""Trusted host installer; release assets never supply executable host code."""
import hashlib
import http.client
import json
import os
from pathlib import Path
import socket
import stat
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
ready=get('/health/ready')
snapshot=get('/v1/nodes/'+node+'/snapshot')
assert identity['nodeId']==node and identity['adapter']=={'kind':kind,'version':version}
assert ready['identity']==identity and ready['readiness']=='ready' and ready['blockedReasons']==[]
assert snapshot['nodeId']==node and snapshot['epoch']==identity['identityEpoch'] and snapshot['completeness']=='complete'
state=snapshot['node']
quiescent=(state['transportAvailability']=='online' and state['engineReadiness']=='ready' and
 state['occupancy']=='idle' and state['activeAttemptId'] is None and state['pendingCount']==0 and
 snapshot['activeAttempt'] is None and snapshot['pendingQueue']==[])
print(json.dumps({'node':node,'epoch':identity['identityEpoch'],'registry':identity['registryVersion'],
 'kind':kind,'version':version,'quiescent':quiescent},sort_keys=True))
'''


def run(*args):
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=180)
    except (OSError, subprocess.TimeoutExpired):
        raise deploy.DeployError('CONTAINER_OPERATION_FAILED') from None
    deploy.require(result.returncode == 0, 'CONTAINER_OPERATION_FAILED')
    return result.stdout.strip()


class ContainerMutationUnknown(deploy.DeployError):
    pass


def mutate(*args):
    # A Docker daemon may accept a mutation before the CLI times out or exits
    # nonzero. Callers must read back exact runtime state and never infer that
    # it is safe to issue an inverse mutation.
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=180)
    except (OSError, subprocess.TimeoutExpired):
        raise ContainerMutationUnknown('CONTAINER_MUTATION_OUTCOME_UNKNOWN') from None
    if result.returncode != 0:
        raise ContainerMutationUnknown('CONTAINER_MUTATION_OUTCOME_UNKNOWN')
    return result.stdout.strip()


def configuration(root):
    deploy.private(root, directory=True)
    file = root / 'config.json'
    deploy.private(file)
    config = deploy.read_json(file)
    deploy.require(set(config) == {'project', 'compose', 'env_file', 'router_socket', 'components'}, 'INVALID_HOST_CONFIG')
    deploy.require(config['project'] in ('homelab-agents', 'homelab-panel-alpha'), 'WRONG_COMPOSE_PROJECT')
    deploy.require(set(config['components']) <= set(release.COMPONENTS) and 'panel' in config['components'], 'INVALID_HOST_COMPONENTS')
    for key in ('compose', 'env_file'):
        path = Path(config[key])
        deploy.require(path.is_absolute(), 'ABSOLUTE_CONFIG_PATH_REQUIRED')
        deploy.private(path)
    router_socket = Path(config['router_socket'])
    deploy.require(router_socket.is_absolute() and router_socket == Path(os.path.normpath(router_socket)) and
                   router_socket.parent not in (Path(router_socket.anchor), Path.home()), 'INVALID_ROUTER_SOCKET_PATH')
    for name, component in config['components'].items():
        fields = {'service'} if name == 'panel' else {'service', 'url', 'node_id', 'actor_id'}
        deploy.require(set(component) == fields, 'INVALID_COMPONENT_CONFIG')
        deploy.require(component['service'] in {'panel': ('panel',), 'cursor': ('cursor', 'harness'), 'codex': ('codex',)}[name], 'SERVICE_MISMATCH')
        if name != 'panel':
            deploy.require(component['url'] == 'https://' + component['service'] + ':18443', 'PRIVATE_NODE_URL_REQUIRED')
            deploy.require(deploy.re.fullmatch('[0-9a-f-]{36}', component['node_id']) and
                           isinstance(component['actor_id'], str) and component['actor_id'] and
                           not any(c in component['actor_id'] for c in '\r\n\0'), 'INVALID_NODE_IDENTITY')
    harnesses = [value for name, value in config['components'].items() if name != 'panel']
    deploy.require(harnesses and len({value['node_id'] for value in harnesses}) == len(harnesses) and
                   len({value['actor_id'] for value in harnesses}) == 1, 'INVALID_ROUTER_COMPONENTS')
    return config


def compatible(current, target):
    release.validate(target)
    if current:
        release.validate(current, target['component'])
        deploy.require(current['state_compatibility'] == target['state_compatibility'], 'STATE_CHANGE_REQUIRES_OPERATOR_MIGRATION')


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('router', timeout=35)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class RouterControl:
    NODE_FIELDS = {'mode', 'stateVersion', 'generation', 'identityEpoch', 'adapterKind', 'adapterVersion'}

    def __init__(self, path):
        self.path = path

    def request(self, method, path, payload=None):
        try:
            directory = os.lstat(self.path.parent)
            info = os.lstat(self.path)
            deploy.require(stat.S_ISDIR(directory.st_mode) and not directory.st_mode & 0o077 and directory.st_uid == 10001 and
                           stat.S_ISSOCK(info.st_mode) and not info.st_mode & 0o077 and info.st_uid == 10001,
                           'UNSAFE_ROUTER_CONTROL_SOCKET')
            body = None if payload is None else json.dumps(payload, separators=(',', ':'), sort_keys=True)
            connection = UnixHTTPConnection(self.path)
            connection.request(method, path, body=body, headers={'Content-Type': 'application/json'} if body else {})
            response = connection.getresponse()
            raw = response.read(262145)
            status = response.status
            connection.close()
            deploy.require(len(raw) <= 262144, 'ROUTER_CONTROL_RESPONSE_TOO_LARGE')
            value = json.loads(raw, object_pairs_hook=deploy.pairs)
        except deploy.DeployError:
            raise
        except Exception:
            raise deploy.DeployError('ROUTER_CONTROL_UNAVAILABLE') from None
        if status != 200:
            code = value.get('error') if type(value) is dict and set(value) == {'error'} else ''
            allowed = {'invalid', 'not_found', 'stale', 'identity_changed', 'version_exhausted', 'state_unavailable', 'node_not_ready'}
            deploy.require(code in allowed, 'ROUTER_CONTROL_INVALID_RESPONSE')
            raise deploy.DeployError('ROUTER_' + code.upper())
        return value

    @classmethod
    def node(cls, value):
        deploy.require(type(value) is dict and set(value) in (cls.NODE_FIELDS, cls.NODE_FIELDS | {'operationId'}) and
                       value['mode'] in ('eligible', 'draining', 'sealed') and
                       type(value['stateVersion']) is int and value['stateVersion'] > 0 and
                       type(value['generation']) is int and value['generation'] >= 0 and
                       type(value['identityEpoch']) is int and value['identityEpoch'] >= 0 and
                       value['adapterKind'] in ('cursor', 'codex') and isinstance(value['adapterVersion'], str),
                       'INVALID_ROUTER_STATE')
        if value['mode'] == 'eligible':
            deploy.require('operationId' not in value, 'INVALID_ROUTER_STATE')
        else:
            deploy.require(isinstance(value.get('operationId'), str) and value['operationId'], 'INVALID_ROUTER_STATE')
        return value

    def status(self):
        value = self.request('GET', '/v1/state')
        deploy.require(type(value) is dict and set(value) == {'schema', 'ownerId', 'registryVersion', 'registrySHA256', 'nodes'} and
                       value['schema'] == 1 and isinstance(value['ownerId'], str) and value['ownerId'] and
                       type(value['registryVersion']) is int and value['registryVersion'] > 0 and
                       deploy.re.fullmatch('[0-9a-f]{64}', value['registrySHA256']) and
                       type(value['nodes']) is dict, 'INVALID_ROUTER_STATE')
        for node in value['nodes'].values():
            self.node(node)
        return value

    @classmethod
    def projected(cls, action, state, operation_id, identity=None):
        cls.node(state)
        desired = dict(state)
        desired['stateVersion'] += 1
        if action == 'drain':
            desired['mode'], desired['operationId'] = 'draining', operation_id
        elif action == 'seal':
            desired['mode'] = 'sealed'
        else:
            deploy.require(type(identity) is dict, 'MISSING_ROUTER_IDENTITY')
            desired.update(mode='eligible', generation=state['generation'] + 1,
                           identityEpoch=identity['epoch'], adapterVersion=identity['version'])
            desired.pop('operationId', None)
        return desired

    def transition_many(self, action, current, operation_id, identities=None):
        deploy.require(action in ('drain', 'seal', 'activate', 'abort') and
                       type(current) is dict and current, 'INVALID_ROUTER_TRANSITION')
        identities = identities or {}
        payload = {'operationId': operation_id, 'nodes': {}}
        expected = {}
        for node_id, state in current.items():
            self.node(state)
            request = {'expected': {'mode': state['mode'], 'stateVersion': state['stateVersion'],
                                    'generation': state['generation']}}
            identity = identities.get(node_id)
            desired = self.projected(action, state, operation_id, identity)
            if action in ('activate', 'abort'):
                deploy.require(type(identity) is dict, 'MISSING_ROUTER_IDENTITY')
                request.update(identityEpoch=identity['epoch'], adapterVersion=identity['version'])
            payload['nodes'][node_id] = request
            expected[node_id] = desired
        try:
            response = self.request('POST', '/v1/nodes/' + action, payload)
            deploy.require(type(response) is dict and set(response) == {'nodes'} and
                           type(response['nodes']) is dict and set(response['nodes']) == set(current),
                           'ROUTER_CONTROL_INVALID_RESPONSE')
            result = {node_id: self.node(value) for node_id, value in response['nodes'].items()}
        except deploy.DeployError:
            # A lost Unix-socket response is not permission to repeat a mutation.
            # Read back the exact CAS result before deciding whether it committed.
            observed = self.status().get('nodes', {})
            if all(observed.get(node_id) == value for node_id, value in expected.items()):
                return expected
            raise
        deploy.require(result == expected, 'ROUTER_TRANSITION_READBACK_MISMATCH')
        return result

    def transition(self, node_id, action, current, operation_id, identity=None):
        identities = {} if identity is None else {node_id: identity}
        return self.transition_many(action, {node_id: current}, operation_id, identities)[node_id]


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
        self.router = RouterControl(Path(self.config['router_socket']))

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

    def compose_mutation(self, *args, override=None):
        command = ['docker', 'compose', '--project-name', self.config['project'],
                   '--env-file', self.config['env_file'], '-f', self.config['compose']]
        if override:
            command += ['-f', str(override)]
        return mutate(*command, *args)

    def container(self, name):
        ids = self.compose('ps', '-q', self.config['components'][name]['service']).splitlines()
        deploy.require(len(ids) == 1 and deploy.re.fullmatch('[0-9a-f]{12,64}', ids[0]), 'COMPONENT_CONTAINER_MISSING')
        return ids[0]

    def inspect(self, name, manifest, container=None):
        data = json.loads(run('docker', 'inspect', container or self.container(name)))[0]
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
        return data, image

    def health(self, name, manifest):
        if name == 'panel':
            import cd
            cd.public_health()
            return None
        config = self.config['components'][name]
        value = json.loads(run('docker', 'exec', self.container('panel'), '/usr/local/bin/python3', '-c', PROBE,
                               config['url'], config['node_id'], config['actor_id'], name, manifest['adapter_version']),
                           object_pairs_hook=deploy.pairs)
        deploy.require(type(value) is dict and set(value) == {'node', 'epoch', 'registry', 'kind', 'version', 'quiescent'} and
                       value['node'] == config['node_id'] and type(value['epoch']) is int and value['epoch'] > 0 and
                       type(value['registry']) is int and value['registry'] > 0 and value['kind'] == name and
                       value['version'] == manifest['adapter_version'] and type(value['quiescent']) is bool,
                       'INVALID_NODE_HEALTH')
        return value

    def pin_override(self, name, manifest):
        service = self.config['components'][name]['service']
        override = self.root / (name + '.override.json')
        deploy.atomic_json(override, {'services': {service: {'image': manifest['image']}}})
        return override

    def replace(self, name, manifest, alternate=None, override=None):
        service = self.config['components'][name]['service']
        override = override or self.pin_override(name, manifest)
        try:
            self.compose_mutation('up', '-d', '--no-deps', '--pull', 'never', service, override=override)
        except ContainerMutationUnknown:
            outcome = self.wait_runtime_identity(name, manifest, alternate)
            # Seeing the alternate does not prove the accepted daemon request
            # has finished; it may still replace that container later. Only the
            # mutation's own desired identity is safe to continue automatically.
            if outcome != 'desired':
                raise
            return 'desired_after_unknown'
        return 'desired'

    def wait_runtime_identity(self, name, desired, alternate, timeout=45):
        candidates = [('desired', desired)]
        if alternate is not None:
            candidates.append(('alternate', alternate))
        deadline = time.monotonic() + timeout
        stable, count = None, 0
        while True:
            observed = None
            for label, candidate in candidates:
                try:
                    self.inspect(name, candidate)
                    observed = label
                    break
                except Exception:
                    pass
            if observed == stable:
                count += 1
            else:
                stable, count = observed, 1
            if observed is not None and count >= 3:
                return observed
            if time.monotonic() >= deadline:
                return None
            time.sleep(2)

    @staticmethod
    def identity(health):
        return {key: health[key] for key in ('node', 'epoch', 'registry', 'kind', 'version')}

    def harnesses(self):
        return [name for name in self.config['components'] if name != 'panel']

    def affected(self, name):
        return self.harnesses() if name == 'panel' else [name]

    def routing(self):
        state = self.router.status()
        harnesses = self.harnesses()
        enrolled = {self.config['components'][name]['node_id'] for name in harnesses}
        extra = set(state['nodes']) - enrolled
        deploy.require(state['ownerId'] == self.config['components'][harnesses[0]]['actor_id'] and
                       enrolled <= set(state['nodes']) and
                       all(state['nodes'][node_id]['mode'] == 'sealed' for node_id in extra),
                       'ROUTER_REGISTRY_COMPONENT_MISMATCH')
        for name in harnesses:
            node = state['nodes'][self.config['components'][name]['node_id']]
            deploy.require(node['adapterKind'] == name, 'ROUTER_ADAPTER_MISMATCH')
        return state

    def require_routes(self, names, mode, operation_id=''):
        state = self.routing()
        for name in names:
            node = state['nodes'][self.config['components'][name]['node_id']]
            deploy.require(node['mode'] == mode and
                           (node.get('operationId', '') == operation_id if mode != 'eligible' else 'operationId' not in node),
                           'ROUTER_FENCE_CHANGED')
        return state

    def wait_quiescent(self, name, manifest, expected):
        deadline = time.monotonic() + 45
        while True:
            try:
                health = self.health(name, manifest)
                deploy.require(self.identity(health) == expected and health['quiescent'], 'NODE_NOT_QUIESCENT')
                return health
            except Exception:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(2)

    def wait_healthy(self, name, manifest, expected=None, fenced=(), operation_id=''):
        deadline = time.monotonic() + 45
        while True:
            try:
                self.inspect(name, manifest)
                health = self.health(name, manifest)
                if expected is not None:
                    deploy.require(self.identity(health) == expected, 'NODE_IDENTITY_CHANGED')
                if fenced:
                    self.require_routes(fenced, 'sealed', operation_id)
                return health
            except Exception:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(2)

    def route_subset(self, names, state):
        return {self.config['components'][name]['node_id']:
                state['nodes'][self.config['components'][name]['node_id']] for name in names}

    def identity_subset(self, names, identities):
        return {self.config['components'][name]['node_id']: identities[name] for name in names}

    def verify_candidate(self, name, manifest, affected, operation_id, before_health, before_ids, sealed=True):
        selected_expected = None if name == 'panel' else dict(before_health[name], version=manifest['adapter_version'])
        self.wait_healthy(name, manifest, selected_expected, affected if sealed else (), operation_id)
        deploy.require(all(self.container(other) == value for other, value in before_ids.items()), 'OTHER_COMPONENT_RESTARTED')
        observed = {}
        for component in affected:
            component_manifest = manifest if component == name else self.ledger['components'][component]['current']
            expected = dict(before_health[component], version=component_manifest['adapter_version'])
            observed[component] = self.wait_quiescent(component, component_manifest, expected)
        return observed

    def verify_transition(self, expected):
        observed = self.routing()['nodes']
        deploy.require(all(observed.get(node_id) == state for node_id, state in expected.items()),
                       'ROUTER_TRANSITION_READBACK_MISMATCH')

    def abort_to_prior(self, name, prior, affected, operation_id, before_health, before_ids,
                       expected_container=None):
        # This path issues no Docker mutation. It only reopens the exact prior
        # runtime after all affected nodes are again proven quiescent.
        self.inspect(name, prior)
        if expected_container is not None:
            deploy.require(self.container(name) == expected_container, 'SELECTED_COMPONENT_RESTARTED')
        self.health(name, prior)
        deploy.require(all(self.container(other) == value for other, value in before_ids.items()), 'OTHER_COMPONENT_RESTARTED')
        for component in affected:
            component_manifest = prior if component == name else self.ledger['components'][component]['current']
            self.wait_quiescent(component, component_manifest, before_health[component])
        routes = self.routing()
        current = self.route_subset(affected, routes)
        if all(state['mode'] == 'eligible' for state in current.values()):
            for component in affected:
                state = current[self.config['components'][component]['node_id']]
                identity = before_health[component]
                deploy.require(state['identityEpoch'] == identity['epoch'] and
                               state['adapterVersion'] == identity['version'], 'ROUTER_FENCE_CHANGED')
            return
        deploy.require(all(state['mode'] in ('draining', 'sealed') and
                           state.get('operationId') == operation_id for state in current.values()),
                       'ROUTER_FENCE_CHANGED')
        reopened = self.router.transition_many('abort', current, operation_id,
                                               self.identity_subset(affected, before_health))
        self.verify_transition(reopened)

    def validate_pending(self):
        pending = self.ledger['pending']
        deploy.require(type(pending) is dict, 'INVALID_PENDING_DEPLOYMENT')
        base = {'component', 'prior', 'target', 'operation_id', 'phase', 'rollback',
                'affected', 'before_identities', 'other_containers', 'prior_container', 'prior_slot'}
        activation = {'sealed_routes', 'target_identities'}
        deploy.require(set(pending) in (base, base | activation), 'INVALID_PENDING_DEPLOYMENT')
        name = pending['component']
        deploy.require(name in self.config['components'] and type(pending['rollback']) is bool and
                       deploy.re.fullmatch(r'deploy-[1-9][0-9]{0,19}', pending['operation_id']),
                       'INVALID_PENDING_DEPLOYMENT')
        release.validate(pending['prior'], name)
        release.validate(pending['target'], name)
        compatible(pending['prior'], pending['target'])
        deploy.require(pending['affected'] == self.affected(name) and
                       pending['phase'] in ('prepared', 'sealed', 'replace_started', 'replace_unknown',
                                            'target_verified', 'restore_started', 'prior_verified',
                                            'activation_pending'), 'INVALID_PENDING_DEPLOYMENT')
        deploy.require(type(pending['prior_slot']) is dict and
                       set(pending['prior_slot']) == {'current', 'previous'} and
                       pending['prior_slot']['current'] == pending['prior'], 'INVALID_PENDING_DEPLOYMENT')
        if pending['prior_slot']['previous'] is not None:
            release.validate(pending['prior_slot']['previous'], name)
        expected_others = set(self.config['components']) - {name}
        deploy.require(isinstance(pending['prior_container'], str) and
                       deploy.re.fullmatch('[0-9a-f]{12,64}', pending['prior_container']) and
                       type(pending['other_containers']) is dict and
                       set(pending['other_containers']) == expected_others and
                       all(isinstance(value, str) and deploy.re.fullmatch('[0-9a-f]{12,64}', value)
                           for value in pending['other_containers'].values()), 'INVALID_PENDING_DEPLOYMENT')
        identities = pending['before_identities']
        deploy.require(type(identities) is dict and set(identities) == set(pending['affected']),
                       'INVALID_PENDING_DEPLOYMENT')
        for component, identity in identities.items():
            expected_version = (pending['prior'] if component == name else
                                self.ledger['components'][component]['current'])['adapter_version']
            deploy.require(type(identity) is dict and set(identity) == {'node', 'epoch', 'registry', 'kind', 'version'} and
                           identity['node'] == self.config['components'][component]['node_id'] and
                           type(identity['epoch']) is int and identity['epoch'] > 0 and
                           type(identity['registry']) is int and identity['registry'] > 0 and
                           identity['kind'] == component and identity['version'] == expected_version,
                           'INVALID_PENDING_DEPLOYMENT')
        if pending['phase'] == 'activation_pending':
            deploy.require(set(pending) == base | activation and
                           self.ledger['components'][name] ==
                           {'current': pending['target'], 'previous': pending['prior']},
                           'INVALID_PENDING_DEPLOYMENT')
            node_ids = {self.config['components'][component]['node_id'] for component in pending['affected']}
            deploy.require(type(pending['sealed_routes']) is dict and
                           set(pending['sealed_routes']) == node_ids and
                           type(pending['target_identities']) is dict and
                           set(pending['target_identities']) == node_ids, 'INVALID_PENDING_DEPLOYMENT')
            for node_id, state in pending['sealed_routes'].items():
                RouterControl.node(state)
                deploy.require(state['mode'] == 'sealed' and
                               state.get('operationId') == pending['operation_id'],
                               'INVALID_PENDING_DEPLOYMENT')
            for component in pending['affected']:
                node_id = self.config['components'][component]['node_id']
                identity = pending['target_identities'][node_id]
                expected_version = (pending['target'] if component == name else
                                    self.ledger['components'][component]['current'])['adapter_version']
                deploy.require(type(identity) is dict and set(identity) ==
                               {'node', 'epoch', 'registry', 'kind', 'version'} and
                               identity['node'] == node_id and identity['kind'] == component and
                               identity['version'] == expected_version and
                               type(identity['epoch']) is int and identity['epoch'] > 0 and
                               type(identity['registry']) is int and identity['registry'] > 0,
                               'INVALID_PENDING_DEPLOYMENT')
        else:
            deploy.require(set(pending) == base and
                           self.ledger['components'][name] == pending['prior_slot'],
                           'INVALID_PENDING_DEPLOYMENT')
        return pending

    def reconcile(self):
        if self.ledger['pending'] is None:
            return None
        pending = self.validate_pending()
        name, prior, target = pending['component'], pending['prior'], pending['target']
        affected, operation_id = pending['affected'], pending['operation_id']
        before_health, before_ids = pending['before_identities'], pending['other_containers']
        outcome = self.wait_runtime_identity(name, target, prior)
        deploy.require(outcome is not None, 'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')

        phase = pending['phase']
        no_target_mutation = phase in ('prepared', 'sealed')
        restoring_prior = phase in ('restore_started', 'prior_verified')
        if no_target_mutation or restoring_prior:
            # A target seen while restoring may still be replaced by an
            # in-flight prior mutation. Conversely, a prior seen after a target
            # mutation started may still be replaced by that in-flight target.
            # Only the identity matching the journalled mutation direction is
            # automatically recoverable.
            deploy.require(outcome == 'alternate', 'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
            self.pin_override(name, prior)
            self.ledger['components'][name] = pending['prior_slot']
            expected_container = pending['prior_container'] if no_target_mutation else None
            self.abort_to_prior(name, prior, affected, operation_id, before_health, before_ids,
                                expected_container)
            self.ledger['pending'] = None
            deploy.atomic_json(self.file, self.ledger)
            return 'PRIOR_RETAINED_AFTER_RESTART'

        deploy.require(phase in ('replace_started', 'replace_unknown', 'target_verified', 'activation_pending') and
                       outcome == 'desired', 'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
        self.pin_override(name, target)
        routes = self.routing()
        current = self.route_subset(affected, routes)
        eligible = all(state['mode'] == 'eligible' for state in current.values())
        if phase == 'activation_pending':
            sealed = pending['sealed_routes']
            identities = pending['target_identities']
            expected = {node_id: RouterControl.projected('activate', state, operation_id,
                        identities[node_id]) for node_id, state in sealed.items()}
            deploy.require((eligible and current == expected) or current == sealed,
                           'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
            desired_health = self.verify_candidate(name, target, affected, operation_id,
                                                   before_health, before_ids, sealed=not eligible)
            deploy.require(self.identity_subset(affected, {component: self.identity(value)
                           for component, value in desired_health.items()}) == identities,
                           'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
            if not eligible:
                activated = self.router.transition_many('activate', sealed, operation_id, identities)
                self.verify_transition(activated)
        else:
            deploy.require(all(state['mode'] == 'sealed' and
                           state.get('operationId') == operation_id for state in current.values()),
                           'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
            desired_health = self.verify_candidate(name, target, affected, operation_id,
                                                   before_health, before_ids)
            identities = self.identity_subset(affected, {component: self.identity(value)
                                                         for component, value in desired_health.items()})
            self.ledger['components'][name] = {'current': target, 'previous': prior}
            self.ledger['pending'].update(phase='activation_pending', sealed_routes=current,
                                          target_identities=identities)
            deploy.atomic_json(self.file, self.ledger)
            activated = self.router.transition_many('activate', current, operation_id, identities)
            self.verify_transition(activated)
        self.ledger['pending'] = None
        deploy.atomic_json(self.file, self.ledger)
        return 'ROLLED_BACK_AFTER_RESTART' if pending['rollback'] else 'DEPLOYED_AFTER_RESTART'

    def apply(self, name, target, rollback=False, operation_id=''):
        deploy.require(name in self.ledger['components'], 'COMPONENT_NOT_ENROLLED')
        deploy.require(deploy.re.fullmatch(r'deploy-[1-9][0-9]{0,19}', operation_id), 'INVALID_DEPLOYMENT_OPERATION_ID')
        slot = self.ledger['components'][name]
        prior = slot['current']
        compatible(prior, target)
        deploy.require(self.ledger['pending'] is None, 'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
        if rollback:
            deploy.require(target == slot['previous'], 'ROLLBACK_TARGET_CHANGED')
        prior_container = self.container(name)
        before_ids = {other: self.container(other) for other in self.config['components'] if other != name}
        self.inspect(name, prior)
        self.health(name, prior)
        affected = self.affected(name)
        before_health = {}
        routes = self.require_routes(affected, 'eligible')
        for component in affected:
            health = self.health(component, self.ledger['components'][component]['current'])
            identity = self.identity(health)
            route = routes['nodes'][self.config['components'][component]['node_id']]
            deploy.require(route['identityEpoch'] == identity['epoch'] and route['adapterVersion'] == identity['version'] and
                           routes['registryVersion'] == identity['registry'], 'ROUTER_NODE_IDENTITY_MISMATCH')
            before_health[component] = identity
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
        self.inspect(name, prior)
        deploy.require(self.container(name) == prior_container, 'SELECTED_COMPONENT_RESTARTED')
        for component in affected:
            observed = self.health(component, self.ledger['components'][component]['current'])
            deploy.require(self.identity(observed) == before_health[component], 'NODE_CHANGED_DURING_DEPLOY')
        self.ledger['pending'] = {'component': name, 'prior': prior, 'target': target,
                                  'operation_id': operation_id, 'phase': 'prepared',
                                  'rollback': rollback, 'affected': affected,
                                  'before_identities': before_health, 'other_containers': before_ids,
                                  'prior_container': prior_container,
                                  'prior_slot': dict(slot)}
        deploy.atomic_json(self.file, self.ledger)
        replacement_started = False
        activation_started = False
        unknown_mutation = False
        try:
            routes = self.require_routes(affected, 'eligible')
            drained = self.router.transition_many('drain', self.route_subset(affected, routes), operation_id)
            for component in affected:
                self.wait_quiescent(component, self.ledger['components'][component]['current'], before_health[component])
            sealed = self.router.transition_many('seal', drained, operation_id)
            self.ledger['pending']['phase'] = 'sealed'
            deploy.atomic_json(self.file, self.ledger)
            override = self.pin_override(name, target)
            self.ledger['pending']['phase'] = 'replace_started'
            deploy.atomic_json(self.file, self.ledger)
            replacement_started = True
            try:
                outcome = self.replace(name, target, prior, override)
            except ContainerMutationUnknown:
                unknown_mutation = True
                self.ledger['pending']['phase'] = 'replace_unknown'
                deploy.atomic_json(self.file, self.ledger)
                raise
            if outcome == 'desired_after_unknown':
                # Keep the provenance durable even though target is visible.
                # Any later validation failure must not launch an inverse while
                # the original daemon request may still be completing.
                unknown_mutation = True
                self.ledger['pending']['phase'] = 'replace_unknown'
                deploy.atomic_json(self.file, self.ledger)
            else:
                deploy.require(outcome == 'desired', 'CONTAINER_MUTATION_OUTCOME_UNKNOWN')
            desired_health = self.verify_candidate(name, target, affected, operation_id,
                                                   before_health, before_ids)
            self.ledger['pending']['phase'] = 'target_verified'
            deploy.atomic_json(self.file, self.ledger)
            routes = self.require_routes(affected, 'sealed', operation_id)
            sealed = self.route_subset(affected, routes)
            identities = self.identity_subset(affected, {component: self.identity(value)
                                                         for component, value in desired_health.items()})
            # Once this write starts, automatic inverse mutation is forbidden:
            # disk may already name target/current even if directory fsync fails.
            activation_started = True
            self.ledger['components'][name] = {'current': target, 'previous': prior}
            self.ledger['pending'].update(phase='activation_pending', sealed_routes=sealed,
                                          target_identities=identities)
            deploy.atomic_json(self.file, self.ledger)
            activated = self.router.transition_many('activate', sealed, operation_id, identities)
            self.verify_transition(activated)
        except Exception as error:
            if not replacement_started:
                try:
                    self.pin_override(name, prior)
                    self.abort_to_prior(name, prior, affected, operation_id, before_health, before_ids,
                                        prior_container)
                    self.ledger['pending'] = None
                    deploy.atomic_json(self.file, self.ledger)
                except Exception:
                    raise deploy.DeployError('DEPLOYMENT_FAILED_FENCE_REMAINS') from None
                raise deploy.DeployError('DEPLOYMENT_FAILED_BEFORE_REPLACE_ABORTED') from error
            if activation_started or unknown_mutation:
                raise
            try:
                # Compatibility was checked before replacement. Keep the Router
                # sealed until exact prior image, identity and quiescence return.
                self.ledger['pending']['phase'] = 'restore_started'
                deploy.atomic_json(self.file, self.ledger)
                override = self.pin_override(name, prior)
                try:
                    outcome = self.replace(name, prior, target, override)
                except ContainerMutationUnknown:
                    outcome = None
                deploy.require(outcome in ('desired', 'desired_after_unknown'),
                               'RECOVERY_CONTAINER_OUTCOME_UNKNOWN')
                self.verify_candidate(name, prior, affected, operation_id, before_health, before_ids)
                self.ledger['pending']['phase'] = 'prior_verified'
                deploy.atomic_json(self.file, self.ledger)
                self.abort_to_prior(name, prior, affected, operation_id, before_health, before_ids)
                self.ledger['pending'] = None
                deploy.atomic_json(self.file, self.ledger)
            except Exception:
                raise deploy.DeployError('DEPLOYMENT_FAILED_RECOVERY_INCOMPLETE') from None
            raise deploy.DeployError('DEPLOYMENT_FAILED_PRIOR_RESTORED') from None
        self.ledger['pending'] = None
        deploy.atomic_json(self.file, self.ledger)
        return 'ROLLED_BACK' if rollback else 'DEPLOYED'
