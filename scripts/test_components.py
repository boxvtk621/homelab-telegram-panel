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
    return dict(schema=1, component=component, version=version, revision='b' * 40,
                image=release.COMPONENTS[component]['image'] + '@sha256:' + digest * 64,
                platform='linux/amd64', adapter_version=None if component == 'panel' else '1.0.31', state_compatibility='stateless-panel-v1' if component == 'panel' else 'c' * 64)


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

    def test_codex_missing_is_an_error_before_publication(self):
        with self.assertRaisesRegex(deploy.DeployError, 'CODEX_ADAPTER_REQUIRED_HL258'):
            release.buildable('codex')
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


class FakeInstaller(host.Installer):
    def __init__(self, root):
        self.root = root
        self.file = root / 'deployment.json'
        self.config = {'components': {'panel': {'service': 'panel'}, 'cursor': {'service': 'cursor'}}}
        self.ledger = {'components': {name: {'current': manifest(name), 'previous': None} for name in self.config['components']},
                       'pending': None, 'config_sha256': 'fingerprint'}
        self.operations = []
        self.fail_target = False
        self.fail_recovery = False

    def fingerprint(self):
        return 'fingerprint'

    def container(self, name):
        return name + '-same-container'

    def inspect(self, name, target):
        self.operations.append(('inspect', name, target['version']))

    def health(self, name, manifest):
        return None if name == 'panel' else {'node': 'fixed', 'epoch': 1, 'paused': True}

    def replace(self, name, target):
        self.operations.append(('replace', name, target['version']))
        # The recovery journal must be persisted before any Docker mutation.
        assert deploy.read_json(self.file)['pending']['component'] == name
        if self.fail_recovery or (self.fail_target and target['version'] == 'v0.2.1'):
            raise deploy.DeployError('SIMULATED_DOCKER_FAILURE')


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
        self.assertEqual(self.installer.apply('cursor', self.target), 'DEPLOYED')
        self.assertEqual(self.installer.ledger['components']['panel'], original_panel)
        self.assertEqual([x for x in self.installer.operations if x[0] == 'replace'], [('replace', 'cursor', 'v0.2.1')])
        ledger = deploy.read_json(self.installer.file)
        self.assertEqual(ledger['components']['cursor']['previous'], manifest())
        self.assertIsNone(ledger['pending'])

    def test_failed_apply_restores_exact_prior_and_is_not_success(self):
        self.installer.fail_target = True
        with self.assertRaisesRegex(deploy.DeployError, 'PRIOR_RESTORED'):
            self.installer.apply('cursor', self.target)
        self.assertEqual([x for x in self.installer.operations if x[0] == 'replace'],
                         [('replace', 'cursor', 'v0.2.1'), ('replace', 'cursor', 'v0.2.0')])
        self.assertEqual(deploy.read_json(self.installer.file)['components']['cursor']['current'], manifest())

    def test_failed_recovery_retains_pending_and_blocks_retry(self):
        self.installer.fail_recovery = True
        with self.assertRaises(deploy.DeployError):
            self.installer.apply('cursor', self.target)
        self.assertIsNotNone(deploy.read_json(self.installer.file)['pending'])
        self.installer.operations.clear()
        with self.assertRaisesRegex(deploy.DeployError, 'INTERRUPTED'):
            self.installer.apply('cursor', self.target)
        self.assertEqual(self.installer.operations, [])

    def test_compatible_sdk_update_checks_target_version_not_prior(self):
        self.target['adapter_version'] = '1.0.32'
        observed = []
        def health(name, manifest):
            observed.append(manifest['adapter_version'])
            return {'node': 'fixed', 'epoch': 1, 'kind': 'cursor', 'paused': True}
        self.installer.health = health
        self.assertEqual(self.installer.apply('cursor', self.target), 'DEPLOYED')
        self.assertEqual(observed, ['1.0.31', '1.0.31', '1.0.32'])

    def test_recovery_waits_for_transient_startup_then_clears_pending(self):
        self.installer.fail_target = True
        calls = 0
        def health(name, manifest):
            nonlocal calls
            calls += 1
            if calls == 3:
                raise deploy.DeployError('NOT_LISTENING_YET')
            return {'node': 'fixed', 'epoch': 1, 'paused': True}
        self.installer.health = health
        with patch.object(host.time, 'sleep'), self.assertRaisesRegex(deploy.DeployError, 'PRIOR_RESTORED'):
            self.installer.apply('cursor', self.target)
        self.assertEqual(calls, 4)
        self.assertIsNone(deploy.read_json(self.installer.file)['pending'])

    def test_state_migration_and_stale_rollback_reject_before_docker(self):
        for rollback, candidate in ((True, self.target), (False, dict(self.target, state_compatibility='d' * 64))):
            with self.subTest(rollback=rollback), self.assertRaises(deploy.DeployError):
                self.installer.apply('cursor', candidate, rollback=rollback)
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
