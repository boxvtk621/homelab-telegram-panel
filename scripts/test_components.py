import copy
import datetime
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import component_cd as cd
import component_deploy as host
import component_release as release
import deploy


def manifest(component='cursor', version='v0.2.0', digest='a'):
    adapter_version = {'panel': None, 'cursor': '1.0.31', 'codex': '0.153.4'}[component]
    return dict(schema=1, component=component, version=version, revision='b' * 40,
                image=release.COMPONENTS[component]['image'] + '@sha256:' + digest * 64,
                platform='linux/amd64', adapter_version=adapter_version, state_compatibility='d' * 64 if component == 'panel' else 'c' * 64)


def request():
    now = datetime.datetime.now(datetime.timezone.utc)
    return dict(id=3, environment=cd.ENVIRONMENT, creator={'login': 'github-actions[bot]', 'type': 'Bot'},
                created_at=now.isoformat(), task='cursor:apply', ref='cursor-v0.2.0',
                payload=dict(schema=1, component='cursor', operation='apply', tag='cursor-v0.2.0',
                             run_id=7, run_attempt=1, workflow_sha='a' * 40, allow_interrupt=True))


def run_metadata(_):
    return dict(event='workflow_dispatch', path=cd.WORKFLOW, head_branch='main', head_sha='a' * 40,
                head_repository={'full_name': release.REPO}, status='in_progress', run_attempt=1)


class ComponentContractTests(unittest.TestCase):
    def test_request_rejects_cross_component_untrusted_fork_replay_and_commands(self):
        self.assertEqual(cd.authorize(request(), read=run_metadata)['component'], 'cursor')
        variants = []
        item = request(); item['task'] = 'panel:apply'; variants.append(item)
        item = request(); item['payload']['component'] = 'codex'; variants.append(item)
        item = request(); item['payload']['shell'] = 'id'; variants.append(item)
        item = request(); item['payload']['allow_interrupt'] = False; variants.append(item)
        item = request(); item['creator']['login'] = 'untrusted'; variants.append(item)
        item = request(); item['environment'] = 'panel-production'; variants.append(item)
        item = request(); item['created_at'] = '2000-01-01T00:00:00+00:00'; variants.append(item)
        for item in variants:
            with self.subTest(item=item), self.assertRaises(deploy.DeployError):
                cd.authorize(item, read=run_metadata)
        for change in ({'event': 'pull_request'}, {'head_branch': 'feature'}, {'path': '.github/workflows/deploy.yml'},
                       {'head_repository': {'full_name': 'fork/repo'}}, {'run_attempt': 2}, {'status': 'completed'}):
            with self.subTest(change=change), self.assertRaises(deploy.DeployError):
                cd.authorize(request(), read=lambda _: dict(run_metadata(''), **change))

    def test_immutable_component_identity_and_schema_compatibility(self):
        release.validate(manifest())
        for image in ('ghcr.io/boxvtk621/homelab-harness-cursor:latest', manifest('panel')['image'], 'file:///tmp/image'):
            candidate = manifest(); candidate['image'] = image
            with self.assertRaises(deploy.DeployError):
                release.validate(candidate, 'cursor')
        candidate = manifest(version='v0.3.0'); candidate['state_compatibility'] = 'e' * 64
        with self.assertRaisesRegex(deploy.DeployError, 'STATE_CHANGE'):
            host.compatible(manifest(), candidate)
        host.compatible(manifest(), manifest(version='v0.2.1'))

    def test_codex_component_requires_complete_native_runtime(self):
        self.assertEqual(release.buildable('codex')['dockerfile'], 'deploy/components/Dockerfile.codex')
        self.assertEqual(release.adapter_version('codex'), '0.153.4')
        release.validate(manifest('codex'), 'codex')
        for tag in ('v0.2.0', '../panel-v0.2.0', 'cursor-v0.2.0;id', 'fixik-v1.0.0'):
            with self.assertRaises(deploy.DeployError):
                release.selection(tag)

    def test_public_status_is_allowlisted(self):
        class State:
            ledger = {'components': {'cursor': {'current': manifest(), 'previous': None}},
                      'pending': {'secret': 'never-public'}, 'config_sha256': 'private'}
        result = cd.status({'id': 3, 'status': 'success', 'secret': 'never-public'}, State())
        self.assertNotIn('never-public', json.dumps(result))
        self.assertNotIn('config_sha256', json.dumps(result))

    def test_release_must_have_successful_component_ci(self):
        raw = json.dumps(manifest()).encode()
        meta = dict(draft=False, published_at='now', tag_name='cursor-v0.2.0', author={'login': 'github-actions[bot]'},
                    assets=[{'name': 'release.json', 'digest': 'sha256:' + cd.hashlib.sha256(raw).hexdigest()}])
        with patch.object(cd.cd, 'fetch', return_value=raw), patch.object(cd.cd, 'github', side_effect=[meta, {'workflow_runs': []}]):
            with self.assertRaisesRegex(deploy.DeployError, 'CI_NOT_SUCCESSFUL'):
                cd.download('cursor-v0.2.0')

    def test_panel_release_uses_durable_router_schema_compatibility(self):
        schema = release.ROOT / 'api/harness-router-state-v1.schema.json'
        expected = release.hashlib.sha256(b'api/harness-router-state-v1.schema.json\0' +
                                          schema.read_bytes() + b'\0').hexdigest()
        self.assertEqual(release.compatibility('panel'), expected)
        old = manifest('panel')
        old['state_compatibility'] = 'stateless-panel-v1'
        with self.assertRaisesRegex(deploy.DeployError, 'INVALID_STATE_COMPATIBILITY'):
            release.validate(old)

    def test_router_lost_ack_requires_exact_transition_readback(self):
        control = host.RouterControl(Path('/synthetic/control.sock'))
        current = {'mode': 'eligible', 'stateVersion': 1, 'generation': 1, 'identityEpoch': 7,
                   'adapterKind': 'cursor', 'adapterVersion': '1.0.31'}
        committed = dict(current, mode='draining', stateVersion=2, operationId='deploy-3')
        status = {'schema': 1, 'ownerId': 'owner', 'registryVersion': 1,
                  'registrySHA256': 'e' * 64, 'nodes': {'node': committed}}
        with patch.object(control, 'request', side_effect=[deploy.DeployError('LOST_ACK'), status]) as request_call:
            self.assertEqual(control.transition('node', 'drain', current, 'deploy-3'), committed)
        self.assertEqual(request_call.call_count, 2)
        different = copy.deepcopy(status)
        different['nodes']['node']['operationId'] = 'deploy-4'
        with patch.object(control, 'request', side_effect=[deploy.DeployError('LOST_ACK'), different]), \
             self.assertRaisesRegex(deploy.DeployError, 'LOST_ACK'):
            control.transition('node', 'drain', current, 'deploy-3')

    def test_unknown_container_mutation_only_accepts_its_desired_direction(self):
        installer = object.__new__(host.Installer)
        installer.config = {'project': 'fixture', 'compose': '/fixture/compose.yaml',
                            'env_file': '/fixture/env', 'components': {'cursor': {'service': 'cursor'}}}
        target, prior = manifest('cursor', 'v0.2.1'), manifest()
        with patch.object(installer, 'pin_override', return_value=Path('/fixture/override.json')), \
             patch.object(installer, 'compose_mutation', side_effect=host.ContainerMutationUnknown('UNKNOWN')), \
             patch.object(installer, 'wait_runtime_identity', return_value='alternate'), \
             self.assertRaises(host.ContainerMutationUnknown):
            installer.replace('cursor', target, prior)
        with patch.object(installer, 'pin_override', return_value=Path('/fixture/override.json')), \
             patch.object(installer, 'compose_mutation', side_effect=host.ContainerMutationUnknown('UNKNOWN')), \
             patch.object(installer, 'wait_runtime_identity', return_value='desired'):
            self.assertEqual(installer.replace('cursor', target, prior), 'desired_after_unknown')


class FakeRouter:
    def __init__(self, installer):
        self.installer = installer
        self.state = {'mode': 'eligible', 'stateVersion': 1, 'generation': 1,
                      'identityEpoch': 1, 'adapterKind': 'cursor', 'adapterVersion': '1.0.31'}
        self.fail_action = None

    def status(self):
        return {'schema': 1, 'ownerId': 'owner', 'registryVersion': 1,
                'registrySHA256': 'e' * 64, 'nodes': {'fixed': copy.deepcopy(self.state)}}

    def transition(self, node_id, action, current, operation_id, identity=None):
        self.installer.operations.append(('router', action))
        if self.fail_action == action:
            raise deploy.DeployError('SIMULATED_ROUTER_FAILURE')
        self.assert_state(node_id, current)
        self.state['stateVersion'] += 1
        if action == 'drain':
            self.state.update(mode='draining', operationId=operation_id)
        elif action == 'seal':
            self.state['mode'] = 'sealed'
        else:
            self.state.update(mode='eligible', generation=self.state['generation'] + 1,
                              identityEpoch=identity['epoch'], adapterVersion=identity['version'])
            self.state.pop('operationId', None)
        return copy.deepcopy(self.state)

    def transition_many(self, action, current, operation_id, identities=None):
        self.installer.operations.append(('router', action))
        if self.fail_action == action:
            raise deploy.DeployError('SIMULATED_ROUTER_FAILURE')
        if current != {'fixed': self.state}:
            raise deploy.DeployError('SIMULATED_STALE_ROUTER')
        identity = (identities or {}).get('fixed')
        self.state = host.RouterControl.projected(action, self.state, operation_id, identity)
        return {'fixed': copy.deepcopy(self.state)}

    def assert_state(self, node_id, current):
        if node_id != 'fixed' or current != self.state:
            raise deploy.DeployError('SIMULATED_STALE_ROUTER')


class FakeInstaller(host.Installer):
    def __init__(self, root):
        self.root = root
        self.file = root / 'deployment.json'
        self.config = {'components': {'panel': {'service': 'panel'},
                       'cursor': {'service': 'cursor', 'node_id': 'fixed', 'actor_id': 'owner',
                                  'url': 'https://cursor:18443'}}}
        self.ledger = {'components': {name: {'current': manifest(name), 'previous': None} for name in self.config['components']},
                       'pending': None, 'config_sha256': 'fingerprint'}
        self.operations = []
        self.fail_target = False
        self.fail_recovery = False
        self.unknown_target = None
        self.unknown_recovery = None
        self.runtime = {name: copy.deepcopy(slot['current']) for name, slot in self.ledger['components'].items()}
        self.router = FakeRouter(self)

    def fingerprint(self):
        return 'fingerprint'

    def container(self, name):
        return {'panel': 'a' * 64, 'cursor': 'b' * 64, 'codex': 'c' * 64}[name]

    def inspect(self, name, target):
        self.operations.append(('inspect', name, target['version']))
        if self.runtime[name] != target:
            raise deploy.DeployError('SIMULATED_RUNTIME_IMAGE_MISMATCH')

    def health(self, name, manifest):
        return None if name == 'panel' else {'node': 'fixed', 'epoch': 1, 'registry': 1,
                                              'kind': 'cursor', 'version': manifest['adapter_version'],
                                              'quiescent': True}

    def replace(self, name, target, alternate=None, override=None):
        self.operations.append(('replace', name, target['version']))
        # The recovery journal must be persisted before any Docker mutation.
        assert deploy.read_json(self.file)['pending']['component'] == name
        is_target = target['version'] == 'v0.2.1'
        unknown = self.unknown_target if is_target else self.unknown_recovery
        if unknown == 'prior':
            raise host.ContainerMutationUnknown('CONTAINER_MUTATION_OUTCOME_UNKNOWN')
        if unknown == 'target':
            self.runtime[name] = copy.deepcopy(target)
            return 'desired_after_unknown'
        if unknown == 'ambiguous':
            raise host.ContainerMutationUnknown('CONTAINER_MUTATION_OUTCOME_UNKNOWN')
        if self.fail_recovery or (self.fail_target and target['version'] == 'v0.2.1'):
            raise deploy.DeployError('SIMULATED_DOCKER_FAILURE')
        self.runtime[name] = copy.deepcopy(target)
        return 'desired'


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.installer = FakeInstaller(Path(self.tmp.name))
        self.target = manifest('cursor', 'v0.2.1')
        self.mock_run = patch.object(host, 'run', side_effect=self.docker).start()
        self.addCleanup(patch.stopall)

    def docker(self, *args):
        if args[:3] == ('docker', 'image', 'inspect'):
            return json.dumps([{'Os': 'linux', 'Architecture': 'amd64', 'RepoDigests': [self.target['image']],
                                'Config': {'User': '10001:10001', 'Labels': {'org.opencontainers.image.version': self.target['version'],
                                                                          'org.opencontainers.image.revision': self.target['revision']}}}])
        return ''

    def test_selected_container_only_and_exact_prior_saved(self):
        original_panel = copy.deepcopy(self.installer.ledger['components']['panel'])
        self.assertEqual(self.installer.apply('cursor', self.target, operation_id='deploy-3'), 'DEPLOYED')
        self.assertEqual(self.installer.ledger['components']['panel'], original_panel)
        self.assertEqual([x for x in self.installer.operations if x[0] == 'replace'], [('replace', 'cursor', 'v0.2.1')])
        ledger = deploy.read_json(self.installer.file)
        self.assertEqual(ledger['components']['cursor']['previous'], manifest())
        self.assertIsNone(ledger['pending'])
        self.assertEqual([item for item in self.installer.operations if item[0] == 'router'],
                         [('router', 'drain'), ('router', 'seal'), ('router', 'activate')])

    def test_panel_update_drains_every_enrolled_node(self):
        target = manifest('panel', 'v0.2.1', 'f')
        self.target = target
        self.assertEqual(self.installer.apply('panel', target, operation_id='deploy-3'), 'DEPLOYED')
        self.assertEqual([item for item in self.installer.operations if item[0] == 'router'],
                         [('router', 'drain'), ('router', 'seal'), ('router', 'activate')])
        self.assertEqual([item for item in self.installer.operations if item[0] == 'replace'],
                         [('replace', 'panel', 'v0.2.1')])

    def test_pre_replace_fence_failure_never_touches_docker(self):
        self.installer.router.fail_action = 'seal'
        with self.assertRaisesRegex(deploy.DeployError, 'BEFORE_REPLACE_ABORTED'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertEqual([item for item in self.installer.operations if item[0] == 'replace'], [])
        self.assertEqual(self.installer.router.state['mode'], 'eligible')
        self.assertIsNone(deploy.read_json(self.installer.file)['pending'])

    def test_override_failure_before_mutation_realigns_prior_without_docker(self):
        calls = []

        def pin(name, value):
            calls.append(value['version'])
            if value == self.target:
                raise deploy.DeployError('SIMULATED_OVERRIDE_FAILURE')
            return self.installer.root / (name + '.override.json')

        with patch.object(self.installer, 'pin_override', side_effect=pin), \
             self.assertRaisesRegex(deploy.DeployError, 'BEFORE_REPLACE_ABORTED'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertEqual(calls, ['v0.2.1', 'v0.2.0'])
        self.assertEqual([item for item in self.installer.operations if item[0] == 'replace'], [])
        self.assertEqual(self.installer.router.state['mode'], 'eligible')

    def test_activation_failure_keeps_target_sealed_for_operator(self):
        self.installer.router.fail_action = 'activate'
        with self.assertRaisesRegex(deploy.DeployError, 'SIMULATED_ROUTER_FAILURE'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertEqual([item for item in self.installer.operations if item[0] == 'replace'],
                         [('replace', 'cursor', 'v0.2.1')])
        self.assertEqual(self.installer.router.state['mode'], 'sealed')
        pending = deploy.read_json(self.installer.file)['pending']
        self.assertEqual(pending['phase'], 'activation_pending')
        self.assertEqual(deploy.read_json(self.installer.file)['components']['cursor']['current'], self.target)

    def test_failed_apply_restores_exact_prior_and_is_not_success(self):
        self.installer.fail_target = True
        with self.assertRaisesRegex(deploy.DeployError, 'PRIOR_RESTORED'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertEqual([x for x in self.installer.operations if x[0] == 'replace'],
                         [('replace', 'cursor', 'v0.2.1'), ('replace', 'cursor', 'v0.2.0')])
        self.assertEqual(deploy.read_json(self.installer.file)['components']['cursor']['current'], manifest())

    def test_failed_recovery_retains_pending_and_blocks_retry(self):
        self.installer.fail_recovery = True
        with self.assertRaises(deploy.DeployError):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertIsNotNone(deploy.read_json(self.installer.file)['pending'])
        self.installer.operations.clear()
        with self.assertRaisesRegex(deploy.DeployError, 'INTERRUPTED'):
            self.installer.apply('cursor', self.target, operation_id='deploy-4')
        self.assertEqual(self.installer.operations, [])

    def test_unknown_target_result_never_issues_inverse_mutation(self):
        self.installer.unknown_target = 'ambiguous'
        with self.assertRaisesRegex(host.ContainerMutationUnknown, 'OUTCOME_UNKNOWN'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertEqual([item for item in self.installer.operations if item[0] == 'replace'],
                         [('replace', 'cursor', 'v0.2.1')])
        self.assertEqual(self.installer.router.state['mode'], 'sealed')
        self.assertEqual(deploy.read_json(self.installer.file)['pending']['phase'], 'replace_unknown')

    def test_unknown_target_exact_prior_stays_sealed_without_inverse_mutation(self):
        self.installer.unknown_target = 'prior'
        with self.assertRaisesRegex(host.ContainerMutationUnknown, 'OUTCOME_UNKNOWN'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertEqual([item for item in self.installer.operations if item[0] == 'replace'],
                         [('replace', 'cursor', 'v0.2.1')])
        self.assertEqual(self.installer.router.state['mode'], 'sealed')
        self.assertEqual(deploy.read_json(self.installer.file)['pending']['phase'], 'replace_unknown')

    def test_unknown_target_proven_visible_still_forbids_inverse_on_health_failure(self):
        self.installer.unknown_target = 'target'
        with patch.object(self.installer, 'verify_candidate', side_effect=deploy.DeployError('SIMULATED_HEALTH_FAILURE')), \
             self.assertRaisesRegex(deploy.DeployError, 'SIMULATED_HEALTH_FAILURE'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertEqual([item for item in self.installer.operations if item[0] == 'replace'],
                         [('replace', 'cursor', 'v0.2.1')])
        self.assertEqual(self.installer.router.state['mode'], 'sealed')
        self.assertEqual(deploy.read_json(self.installer.file)['pending']['phase'], 'replace_unknown')

    def test_delayed_target_after_unknown_prior_is_never_reopened_as_prior(self):
        self.installer.unknown_target = 'prior'
        with self.assertRaises(host.ContainerMutationUnknown):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        with patch.object(self.installer, 'wait_runtime_identity', return_value='alternate'), \
             self.assertRaisesRegex(deploy.DeployError, 'REQUIRES_OPERATOR'):
            self.installer.reconcile()
        self.assertEqual(self.installer.router.state['mode'], 'sealed')
        self.assertIsNotNone(deploy.read_json(self.installer.file)['pending'])
        self.installer.runtime['cursor'] = copy.deepcopy(self.target)
        with patch.object(self.installer, 'wait_runtime_identity', return_value='desired'):
            self.assertEqual(self.installer.reconcile(), 'DEPLOYED_AFTER_RESTART')
        self.assertEqual(self.installer.router.state['mode'], 'eligible')

    def test_restore_started_target_never_forward_activates(self):
        self.installer.unknown_target = 'prior'
        with self.assertRaises(host.ContainerMutationUnknown):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        pending = self.installer.ledger['pending']
        pending['phase'] = 'restore_started'
        self.installer.runtime['cursor'] = copy.deepcopy(self.target)
        deploy.atomic_json(self.installer.file, self.installer.ledger)
        before = len([item for item in self.installer.operations if item == ('router', 'activate')])
        with patch.object(self.installer, 'wait_runtime_identity', return_value='desired'), \
             self.assertRaisesRegex(deploy.DeployError, 'REQUIRES_OPERATOR'):
            self.installer.reconcile()
        self.assertEqual(len([item for item in self.installer.operations if item == ('router', 'activate')]), before)
        self.assertEqual(self.installer.router.state['mode'], 'sealed')

    def test_restart_reconciles_activation_pending_while_sealed(self):
        self.installer.router.fail_action = 'activate'
        with self.assertRaisesRegex(deploy.DeployError, 'SIMULATED_ROUTER_FAILURE'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.installer.router.fail_action = None
        with patch.object(host.time, 'sleep'):
            self.assertEqual(self.installer.reconcile(), 'DEPLOYED_AFTER_RESTART')
        self.assertEqual(self.installer.router.state['mode'], 'eligible')
        self.assertEqual(deploy.read_json(self.installer.file)['components']['cursor']['current'], self.target)
        self.assertIsNone(deploy.read_json(self.installer.file)['pending'])

    def test_restart_accepts_exact_post_activation_readback_without_replay(self):
        self.installer.router.fail_action = 'activate'
        with self.assertRaises(deploy.DeployError):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        pending = deploy.read_json(self.installer.file)['pending']
        self.installer.router.fail_action = None
        self.installer.router.transition_many('activate', pending['sealed_routes'], 'deploy-3',
                                              pending['target_identities'])
        before = len([item for item in self.installer.operations if item == ('router', 'activate')])
        with patch.object(host.time, 'sleep'):
            self.assertEqual(self.installer.reconcile(), 'DEPLOYED_AFTER_RESTART')
        after = len([item for item in self.installer.operations if item == ('router', 'activate')])
        self.assertEqual(after, before)
        self.assertIsNone(deploy.read_json(self.installer.file)['pending'])

    def test_compatible_sdk_update_checks_target_version_not_prior(self):
        self.target['adapter_version'] = '1.0.32'
        observed = []
        def health(name, manifest):
            observed.append(manifest['adapter_version'])
            return {'node': 'fixed', 'epoch': 1, 'registry': 1, 'kind': 'cursor',
                    'version': manifest['adapter_version'], 'quiescent': True}
        self.installer.health = health
        self.assertEqual(self.installer.apply('cursor', self.target, operation_id='deploy-3'), 'DEPLOYED')
        self.assertEqual(observed[0], '1.0.31')
        self.assertIn('1.0.32', observed)

    def test_recovery_waits_for_transient_startup_then_clears_pending(self):
        self.installer.fail_target = True
        calls = 0
        def health(name, manifest):
            nonlocal calls
            calls += 1
            if calls == 4:
                raise deploy.DeployError('NOT_LISTENING_YET')
            return {'node': 'fixed', 'epoch': 1, 'registry': 1, 'kind': 'cursor',
                    'version': manifest['adapter_version'], 'quiescent': True}
        self.installer.health = health
        with patch.object(host.time, 'sleep'), self.assertRaisesRegex(deploy.DeployError, 'PRIOR_RESTORED'):
            self.installer.apply('cursor', self.target, operation_id='deploy-3')
        self.assertGreaterEqual(calls, 5)
        self.assertIsNone(deploy.read_json(self.installer.file)['pending'])

    def test_state_migration_and_stale_rollback_reject_before_docker(self):
        for rollback, candidate in ((True, self.target), (False, dict(self.target, state_compatibility='d' * 64))):
            with self.subTest(rollback=rollback), self.assertRaises(deploy.DeployError):
                self.installer.apply('cursor', candidate, rollback=rollback, operation_id='deploy-3')
        self.mock_run.assert_not_called()
        self.assertEqual(self.installer.operations, [])

    def test_crashed_request_is_marked_failed_without_replay(self):
        root = Path(self.tmp.name)
        root.chmod(0o700)
        deploy.atomic_json(root / 'request.json', {'id': 3, 'status': 'running', 'component': 'cursor'})
        with patch.object(host, 'ROOT', root), patch.object(host, 'Installer', return_value=self.installer), \
             patch.object(cd, 'publish') as publish, patch.object(cd.cd, 'github') as github:
            cd.poll()
        github.assert_not_called()
        self.assertEqual(deploy.read_json(root / 'request.json')['status'], 'failure')
        publish.assert_called_once()


if __name__ == '__main__':
    unittest.main()
