"""Offline CD authorization and recovery tests, no network/credentials."""
import copy
import json
import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch

import cd
import deploy
import release
from test_release import FixtureInstaller

SHA = 'a' * 40
REQUEST = {'id': 100, 'environment': cd.ENVIRONMENT, 'creator': {'login': 'github-actions[bot]', 'type': 'Bot'},
    'created_at': '2026-09-08T00:00:00Z', 'ref': 'v0.1.0-rc.5', 'task': 'panel:apply', 'sha': SHA,
    'payload': {'schema': 1, 'operation': 'apply', 'version': 'v0.1.0-rc.5', 'run_id': 20, 'run_attempt': 1,
        'workflow_sha': SHA, 'allow_interrupt': True}}
RUN = {'event': 'workflow_dispatch', 'path': cd.WORKFLOW, 'head_branch': 'main', 'head_sha': SHA,
    'head_repository': {'full_name': cd.REPO}, 'status': 'in_progress', 'run_attempt': 1}
NOW = 1788825601


class AuthorizationTest(unittest.TestCase):
    def test_only_main_dispatch_with_acknowledgement(self):
        self.assertEqual(cd.authorize(REQUEST, lambda _: RUN, NOW), REQUEST['payload'])
        for field, value in [('event', 'pull_request'), ('path', '.github/workflows/quality.yml'),
                ('head_branch', 'codex/evil'), ('head_sha', 'b' * 40), ('status', 'completed'), ('run_attempt', 2),
                ('head_repository', {'full_name': 'other/repository'})]:
            run = dict(RUN, **{field: value})
            with self.assertRaisesRegex(deploy.DeployError, 'UNTRUSTED_WORKFLOW'):
                cd.authorize(REQUEST, lambda _: run, NOW)

    def test_request_scope_age_and_input_validation(self):
        changes = [('environment', 'production'), ('task', 'arbitrary:command'), ('ref', 'main'),
            ('creator', {'login': 'some-user', 'type': 'User'}), ('id', True)]
        for key, value in changes:
            request = copy.deepcopy(REQUEST)
            request[key] = value
            with self.assertRaises(deploy.DeployError):
                cd.authorize(request, lambda _: RUN, NOW)
        for key, value in [('version', '../bad'), ('version', 'v1.0.0;touch /tmp/x'), ('allow_interrupt', False),
                ('operation', 'shell'), ('run_id', True), ('run_attempt', 0), ('shell', 'bad')]:
            request = copy.deepcopy(REQUEST)
            request['payload'][key] = value
            with self.assertRaises(deploy.DeployError):
                cd.authorize(request, lambda _: RUN, NOW)
        for now in [NOW - 2, NOW + 901]:
            with self.assertRaisesRegex(deploy.DeployError, 'EXPIRED'):
                cd.authorize(REQUEST, lambda _: RUN, now)

    def test_only_published_action_release_with_exact_assets(self):
        good = {'tag_name': 'v0.1.0-rc.5', 'draft': False, 'published_at': 'date',
            'author': {'login': 'github-actions[bot]'},
            'assets': [{'name': name, 'digest': 'sha256:' + SHA} for name in deploy.FILES | {'release.json', 'SHA256SUMS'}]}
        self.assertEqual(set(cd.released('v0.1.0-rc.5', lambda _: good)), deploy.FILES | {'release.json', 'SHA256SUMS'})
        for key, value in [('draft', True), ('author', {'login': 'other'}), ('assets', []), ('tag_name', 'v9.0.0')]:
            with self.assertRaises(deploy.DeployError):
                cd.released('v0.1.0-rc.5', lambda _: dict(good, **{key: value}))

    def test_auth_header_cannot_be_sent_to_other_destination(self):
        with self.assertRaisesRegex(deploy.DeployError, 'AUTH_DESTINATION_REJECTED'):
            cd.fetch('https://example.test/', token='synthetic-not-a-secret')


class PollTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        self.cd = root / 'cd'
        self.state = root / 'state'
        self.public = root / 'public'
        self.public.mkdir()
        for name, value in [('CD_STATE', self.cd), ('STATE', self.state), ('PUBLIC_FILE', self.public / 'status.json')]:
            p = patch.object(cd, name, value)
            p.start()
            self.addCleanup(p.stop)

    def test_replay_and_empty_queue_do_not_invoke_installer(self):
        with patch.object(cd, 'github', return_value=[]), patch.object(cd, 'download') as download:
            cd.poll()
            download.assert_not_called()
        deploy.atomic_json(self.cd / 'request.json', {'id': 100, 'status': 'success'})
        with patch.object(cd, 'github', return_value=[REQUEST]), patch.object(cd, 'authorize') as authorize:
            cd.poll()
            authorize.assert_not_called()

    def test_interrupted_operation_is_not_replayed(self):
        self.cd.mkdir(mode=0o700)
        deploy.atomic_json(self.cd / 'request.json', {'id': 100, 'status': 'running'})
        with patch.object(cd, 'github') as github, patch.object(cd, 'download') as download:
            cd.poll()
            github.assert_not_called()
            download.assert_not_called()
        result = json.loads((self.public / 'status.json').read_text())
        self.assertEqual(result['result'], 'INTERRUPTED_CHECK_ROLLBACK')
        self.assertEqual(result['status'], 'failure')
        with patch.object(cd, 'github', return_value=[]):
            cd.poll()
        self.assertEqual(json.loads((self.public / 'status.json').read_text())['result'], 'INTERRUPTED_CHECK_ROLLBACK')

    def test_rejected_request_cannot_touch_deployment_state(self):
        with patch.object(cd, 'github', return_value=[REQUEST]), patch.object(cd, 'authorize', side_effect=deploy.DeployError('NO')), patch.object(cd, 'download') as download:
            cd.poll()
            download.assert_not_called()
        self.assertFalse(self.state.exists())
        self.assertEqual(json.loads((self.public / 'status.json').read_text())['status'], 'failure')
        with patch.object(cd, 'github', return_value=[]):
            cd.poll()
        self.assertEqual(json.loads((self.public / 'status.json').read_text())['result'], 'DEPLOYMENT_REQUEST_REJECTED')

    def run_apply(self, public_failure=False, prior=True, config_changed=False):
        self.state.mkdir(mode=0o700)
        installer = FixtureInstaller(self.state)
        image = deploy.REPOSITORY + '@sha256:' + 'b' * 64
        if prior:
            old = self.state / 'old-fixture'
            release.package('v0.1.0-rc.4', SHA, image, old)
            installer.perform(old, allow_interrupt=True)
        if config_changed:
            installer.config = {'PANEL_CURSOR_API_KEY': 'synthetic-new-value'}
        def download(version, target):
            # package wants a new directory; use a bounded fixture child.
            fixture = target / 'fixture'
            release.package(version, SHA, image, fixture)
            for file in fixture.iterdir():
                (target / file.name).write_bytes(file.read_bytes())
            return deploy.bundle(target)
        with patch.object(cd, 'github', return_value=[REQUEST]), patch.object(cd, 'authorize', return_value=REQUEST['payload']), patch.object(cd, 'download', side_effect=download), patch.object(deploy, 'configuration', return_value={}), patch.object(deploy, 'Installer', return_value=installer), patch.object(cd, 'public_health', side_effect=RuntimeError('synthetic secret must not leak') if public_failure else None):
            cd.poll()
        return installer, json.loads((self.public / 'status.json').read_text())

    def test_success_records_exact_release_without_private_fields(self):
        installer, status = self.run_apply()
        self.assertEqual(status['status'], 'success')
        self.assertEqual(status['current']['version'], REQUEST['ref'])
        self.assertEqual(status['rollback_candidate']['version'], 'v0.1.0-rc.4')
        self.assertEqual(set(status['current']), {'version', 'revision', 'image'})
        self.assertNotIn('container', json.dumps(status))

    def test_public_failure_restores_previous_release(self):
        installer, status = self.run_apply(public_failure=True)
        self.assertEqual(status['status'], 'failure')
        self.assertEqual(status['result'], 'PUBLIC_CHECK_FAILED_ROLLBACK_COMPLETED')
        self.assertEqual(installer.running['manifest']['version'], 'v0.1.0-rc.4')
        self.assertNotIn('synthetic secret', json.dumps(status))

    def test_first_install_public_failure_removes_only_attempted_panel(self):
        installer, status = self.run_apply(public_failure=True, prior=False)
        self.assertIsNone(installer.running)
        self.assertIsNone(status['current'])
        self.assertEqual(status['status'], 'failure')

    def test_private_completion_repairs_missing_public_result(self):
        self.cd.mkdir(mode=0o700)
        deploy.atomic_json(self.cd / 'request.json', {'id': 100, 'status': 'failure', 'result': 'BOUNDED_FAILURE'})
        with patch.object(cd, 'github', return_value=[]):
            cd.poll()
        self.assertEqual(json.loads((self.public / 'status.json').read_text())['result'], 'BOUNDED_FAILURE')

    def test_operator_config_change_requires_separate_operator_apply(self):
        installer, status = self.run_apply(config_changed=True)
        self.assertEqual(status['result'], 'CONFIG_CHANGED_USE_OPERATOR_APPLY')
        self.assertEqual(status['status'], 'failure')
        self.assertEqual(installer.running['manifest']['version'], 'v0.1.0-rc.4')
        self.assertEqual(installer.starts, ['v0.1.0-rc.4'])


if __name__ == '__main__':
    unittest.main()
