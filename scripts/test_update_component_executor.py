import contextlib
import hashlib
import importlib.util
import io
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

import deploy


SPEC = importlib.util.spec_from_file_location(
    'update_component_executor', Path(__file__).with_name('update-component-executor.py'))
updater = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(updater)


class ExecutorUpdateTests(unittest.TestCase):
    RECOVERY_EXPECTED = {
        'target_revision': '1' * 40,
        'target_image_sha256': '2' * 64,
        'prior_revision': '3' * 40,
        'prior_image_sha256': '4' * 64,
    }

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.source = self.root / 'source'
        self.executor = self.root / 'executor'
        self.source.mkdir()
        self.executor.mkdir()
        self.old = {}
        self.new = {}
        for name in updater.FILES:
            self.old[name] = ('old-' + name).encode()
            self.new[name] = ('new-' + name).encode()
            (self.executor / name).write_bytes(self.old[name])
            (self.source / name).write_bytes(self.new[name])
        self.expected = {name: hashlib.sha256(value).hexdigest() for name, value in self.new.items()}

    def arguments(self, recover=''):
        values = ['update-component-executor.py', '--source', str(self.source),
                  '--expected-updater-sha256', updater.digest(Path(updater.__file__))]
        if recover:
            values += ['--recover-operation-id', recover,
                       '--expected-pending-target-revision', self.RECOVERY_EXPECTED['target_revision'],
                       '--expected-pending-target-image-sha256', self.RECOVERY_EXPECTED['target_image_sha256'],
                       '--expected-pending-prior-revision', self.RECOVERY_EXPECTED['prior_revision'],
                       '--expected-pending-prior-image-sha256', self.RECOVERY_EXPECTED['prior_image_sha256']]
        for name in updater.FILES:
            values += ['--expected-' + name.replace('_', '-').replace('.py', '') + '-sha256',
                       self.expected[name]]
        return values

    def patches(self, installer=None):
        if installer is None:
            class Installer:
                ledger = {'pending': None}

            installer = Installer()

        def atomic(path, content):
            path.write_bytes(content)

        return (patch.object(updater.os, 'geteuid', return_value=0),
                patch.object(updater.socket, 'gethostname', return_value='alpine-docker'),
                patch.object(updater.host, 'ROOT', self.root),
                patch.object(updater.host, 'Installer', return_value=installer),
                patch.object(updater.deploy, 'private'),
                patch.object(updater.deploy, 'locked', return_value=contextlib.nullcontext()),
                patch.object(updater, 'atomic_file', side_effect=atomic),
                patch.object(updater.subprocess, 'run'))

    def test_source_hash_mismatch_stops_before_service_mutation(self):
        self.expected['wire_migration.py'] = '0' * 64
        mocks = self.patches()
        with patch.object(sys, 'argv', self.arguments()), contextlib.ExitStack() as stack:
            entered = [stack.enter_context(value) for value in mocks]
            with self.assertRaisesRegex(deploy.DeployError, 'EXECUTOR_SOURCE_MISMATCH'):
                updater.main()
        self.assertEqual(entered[-1].call_count, 0)
        self.assertEqual({name: (self.executor / name).read_bytes() for name in updater.FILES}, self.old)

    def test_failed_new_service_readback_restores_exact_prior_executor(self):
        service_calls = []

        def service(*args):
            service_calls.append(args)
            if args[-1] == 'status' and service_calls.count(args) == 1:
                raise subprocess.CalledProcessError(1, args)

        mocks = self.patches()
        with patch.object(sys, 'argv', self.arguments()), contextlib.ExitStack() as stack:
            entered = [stack.enter_context(value) for value in mocks]
            with patch.object(updater, 'run', side_effect=service):
                with self.assertRaises(subprocess.CalledProcessError):
                    updater.main()
        self.assertEqual(service_calls,
                         [('rc-service', updater.SERVICE, 'stop'),
                          ('rc-service', updater.SERVICE, 'start'),
                          ('rc-service', updater.SERVICE, 'status'),
                          ('rc-service', updater.SERVICE, 'stop'),
                          ('rc-service', updater.SERVICE, 'start'),
                          ('rc-service', updater.SERVICE, 'status')])
        self.assertEqual({name: (self.executor / name).read_bytes() for name in updater.FILES}, self.old)
        self.assertEqual(entered[-1].call_count, 1)

    def test_exact_legacy_panel_rollback_runs_before_executor_update(self):
        class Installer:
            ledger = {'pending': {'phase': 'activation_pending'}}

        request = {'id': 7, 'status': 'failure', 'component': 'panel', 'operation': 'apply'}
        deploy.atomic_json(self.root / 'request.json', request)
        installer = Installer()
        recovery = Mock()
        result = updater.wire.LegacyPanelRecovery.RESULT
        recovery_factory = Mock(return_value=recovery)
        recovery_factory.RESULT = result

        def recover(operation_id, expected):
            self.assertEqual(operation_id, 'deploy-7')
            self.assertEqual(expected, self.RECOVERY_EXPECTED)
            installer.ledger['pending'] = None
            request.update(result=result)
            deploy.atomic_json(self.root / 'request.json', request)
            return result

        recovery.run.side_effect = recover
        mocks = self.patches(installer)
        with patch.object(sys, 'argv', self.arguments('deploy-7')), contextlib.ExitStack() as stack:
            [stack.enter_context(value) for value in mocks]
            with patch.object(updater.wire, 'LegacyPanelRecovery', recovery_factory), \
                 contextlib.redirect_stdout(io.StringIO()):
                updater.main()
        recovered = deploy.read_json(self.root / 'request.json')
        self.assertEqual(recovered['status'], 'failure')
        self.assertEqual(recovered['result'], result)
        recovery.run.assert_called_once_with('deploy-7', self.RECOVERY_EXPECTED)
        self.assertEqual({name: (self.executor / name).read_bytes() for name in updater.FILES}, self.new)

    def test_partial_recovery_arguments_stop_before_service_mutation(self):
        arguments = self.arguments()
        arguments += ['--recover-operation-id', 'deploy-7']
        mocks = self.patches()
        with patch.object(sys, 'argv', arguments), contextlib.ExitStack() as stack:
            entered = [stack.enter_context(value) for value in mocks]
            with self.assertRaisesRegex(deploy.DeployError,
                                        'EXECUTOR_UPDATE_RECOVERY_ARGUMENTS_INCOMPLETE'):
                updater.main()
        self.assertEqual(entered[-1].call_count, 0)


if __name__ == '__main__':
    unittest.main()
