import ast
import hashlib
import importlib.util
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    'update_component_executor', Path(__file__).with_name('update-component-executor.py'))
updater = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(updater)


class FakeService:
    def __init__(self):
        self.is_enabled = True
        self.running = True
        self.fail_before = None
        self.fail_after = None
        self.calls = []

    def enabled(self):
        return self.is_enabled

    def action(self, name):
        self.calls.append(name)
        if self.fail_after == name:
            self.fail_after = None
            raise KeyboardInterrupt

    def before(self, name):
        self.calls.append(name + '_before')
        if self.fail_before == name:
            self.fail_before = None
            raise KeyboardInterrupt

    def disable(self):
        self.before('disable')
        self.is_enabled = False
        self.action('disable')

    def enable(self):
        self.before('enable')
        self.is_enabled = True
        self.action('enable')

    def stop(self):
        self.before('stop')
        self.running = False
        self.action('stop')

    def start(self):
        self.before('start')
        self.running = True
        self.action('start')

    def status(self):
        self.action('status')
        if not self.running:
            raise updater.UpdateError('SERVICE_NOT_RUNNING')


class FakeDeploy:
    request = {'status': 'idle'}

    @classmethod
    def read_json(cls, _):
        return dict(cls.request)


class FakeWire:
    class LegacyPanelRecovery:
        RESULT = 'PANEL_WIRE_MISMATCH_PRIOR_RESTORED'
        calls = []

        def __init__(self, installer):
            self.installer = installer

        def run(self, operation_id, expected):
            self.calls.append((operation_id, expected))
            self.installer.ledger['pending'] = None
            FakeDeploy.request = {'status': 'failure', 'result': self.RESULT}
            return self.RESULT


class ExecutorUpdateTests(unittest.TestCase):
    def workspace(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name).resolve()
        root.chmod(0o700)
        executor = root / 'executor'
        executor.mkdir(mode=0o700)
        content = {}
        expected = {}
        for name in updater.FILES:
            prior = ('prior:' + name).encode()
            target = ('target:' + name).encode()
            path = executor / name
            path.write_bytes(prior)
            path.chmod(0o600)
            content[name] = target
            expected[name] = hashlib.sha256(target).hexdigest()
        service = FakeService()
        FakeDeploy.request = {'status': 'idle'}
        modules = types.SimpleNamespace(deploy=FakeDeploy, wire=FakeWire)
        installer = types.SimpleNamespace(ledger={'pending': None})
        values = {'root': root, 'executor': executor, 'journal': root / 'executor-update.json',
                  'content': content, 'expected': expected, 'service': service,
                  'modules': modules, 'installer': installer}
        patches = patch.multiple(updater, ROOT=root, EXECUTOR=executor,
                                 JOURNAL=values['journal'], LOCK=root / 'deploy.lock')
        return values, patches

    def transaction(self, values, recovery=None):
        return updater.Transaction(values['content'], values['expected'], values['modules'],
                                   values['service'], recovery)

    def test_updater_has_no_project_import_before_source_verification(self):
        tree = ast.parse(Path(updater.__file__).read_text())
        imports = set()
        for statement in tree.body:
            if isinstance(statement, ast.Import):
                imports.update(alias.name.split('.')[0] for alias in statement.names)
            elif isinstance(statement, ast.ImportFrom):
                imports.add((statement.module or '').split('.')[0])
        self.assertFalse({'deploy', 'cd', 'component_release', 'component_deploy',
                          'wire_migration', 'component_cd'} & imports)

    def test_every_transitive_source_is_private_and_hash_bound_before_load(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        source = Path(temporary.name).resolve()
        source.chmod(0o700)
        expected = {}
        for name in updater.FILES:
            content = ('verified:' + name).encode()
            path = source / name
            path.write_bytes(content)
            path.chmod(0o600)
            expected[name] = hashlib.sha256(content).hexdigest()
        self.assertEqual(set(updater.verify_sources(source, expected)), set(updater.FILES))
        expected['cd.py'] = '0' * 64
        with self.assertRaisesRegex(updater.UpdateError, 'EXECUTOR_SOURCE_MISMATCH'):
            updater.verify_sources(source, expected)

    def test_updater_must_be_private_hash_bound_and_inside_verified_source(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        source = Path(temporary.name).resolve()
        source.chmod(0o700)
        self_path = source / 'update-component-executor.py'
        self_path.write_bytes(b'verified operator updater')
        self_path.chmod(0o600)
        expected = hashlib.sha256(self_path.read_bytes()).hexdigest()
        with patch.object(updater, '__file__', str(self_path)):
            updater.verify_updater(source, expected)
            foreign = source.parent / 'foreign-updater.py'
            foreign.write_bytes(self_path.read_bytes())
            foreign.chmod(0o600)
            with patch.object(updater, '__file__', str(foreign)):
                with self.assertRaisesRegex(updater.UpdateError, 'EXECUTOR_UPDATER_MISMATCH'):
                    updater.verify_updater(source, expected)

    def test_verified_bytes_load_all_six_modules_without_path_reread(self):
        source = Path(updater.__file__).parent
        content = {name: (source / name).read_bytes() for name in updater.FILES}
        loaded = updater.load_verified_modules(content, source)
        self.assertEqual(loaded.host.ROOT, Path('/opt/homelab-agents-cd'))
        self.assertEqual(loaded.wire.MIGRATION, 'harness-wire-v1-to-v2')
        self.assertTrue(callable(loaded.consumer.poll))

    def test_openrc_default_disable_and_enable_require_exact_link_readback(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name).resolve()
        init = root / 'homelab-components-cd'
        link = root / 'default-link'
        init.write_text('service')
        link.symlink_to(init)

        def mutate(*args):
            if args[1] == 'del':
                link.unlink()
            elif args[1] == 'add':
                link.symlink_to(init)
            else:
                raise AssertionError(args)

        with patch.multiple(updater, INIT=init, RUNLEVEL_LINK=link), \
             patch.object(updater, 'run_checked', side_effect=mutate):
            service = updater.OpenRC()
            service.disable()
            self.assertFalse(service.enabled())
            service.enable()
            self.assertTrue(service.enabled())
            link.unlink()
            foreign = root / 'foreign'
            foreign.write_text('foreign')
            link.symlink_to(foreign)
            with self.assertRaisesRegex(updater.UpdateError, 'OPENRC_DEFAULT_READBACK_FAILED'):
                service.enabled()

    def test_sigkill_after_every_file_replace_stays_disabled_and_rerun_converges(self):
        for crash_name in updater.FILES:
            with self.subTest(crash_name=crash_name):
                values, patches = self.workspace()
                with patches, patch.object(updater, 'import_installed'):
                    original = updater.atomic_file
                    crashed = False

                    def crash_after_replace(path, content):
                        nonlocal crashed
                        original(path, content)
                        if path.parent == values['executor'] and path.name == crash_name and not crashed:
                            crashed = True
                            raise KeyboardInterrupt

                    with patch.object(updater, 'atomic_file', side_effect=crash_after_replace):
                        with self.assertRaises(KeyboardInterrupt):
                            self.transaction(values).run(values['installer'])
                    self.assertFalse(values['service'].enabled())
                    self.assertFalse(values['service'].running)
                    self.assertEqual(values['journal'].stat().st_mode & 0o777, 0o600)
                    self.assertEqual(updater.read_json(values['journal'])['phase'], 'replace_pending')
                    self.assertEqual(self.transaction(values).run(values['installer']),
                                     'COMPONENT_EXECUTOR_UPDATED')
                    self.assertTrue(values['service'].enabled())
                    self.assertTrue(values['service'].running)
                    self.assertEqual(self.transaction(values).current_hashes(), values['expected'])
                    self.assertEqual(updater.read_json(values['journal'])['phase'], 'complete')

    def test_crash_after_default_disable_cannot_boot_mixed_executor(self):
        values, patches = self.workspace()
        values['service'].fail_after = 'disable'
        with patches, patch.object(updater, 'import_installed'):
            with self.assertRaises(KeyboardInterrupt):
                self.transaction(values).run(values['installer'])
            self.assertFalse(values['service'].enabled())
            self.assertEqual(updater.read_json(values['journal'])['phase'], 'prepared')
            self.assertEqual(self.transaction(values).run(values['installer']),
                             'COMPONENT_EXECUTOR_UPDATED')

    def test_crash_before_default_disable_keeps_exact_prior_and_rerun_converges(self):
        values, patches = self.workspace()
        values['service'].fail_before = 'disable'
        with patches, patch.object(updater, 'import_installed'):
            with self.assertRaises(KeyboardInterrupt):
                self.transaction(values).run(values['installer'])
            self.assertTrue(values['service'].enabled())
            self.assertEqual(updater.read_json(values['journal'])['phase'], 'prepared')
            self.assertEqual(self.transaction(values).run(values['installer']),
                             'COMPONENT_EXECUTOR_UPDATED')

    def test_crash_after_default_enable_is_safe_and_rerun_finishes(self):
        values, patches = self.workspace()
        values['service'].fail_after = 'enable'
        with patches, patch.object(updater, 'import_installed'):
            with self.assertRaises(KeyboardInterrupt):
                self.transaction(values).run(values['installer'])
            self.assertTrue(values['service'].enabled())
            self.assertEqual(self.transaction(values).current_hashes(), values['expected'])
            self.assertEqual(updater.read_json(values['journal'])['phase'], 'imports_verified')
            self.assertEqual(self.transaction(values).run(values['installer']),
                             'COMPONENT_EXECUTOR_UPDATED')

    def test_crash_before_default_enable_keeps_target_disabled_until_retry(self):
        values, patches = self.workspace()
        values['service'].fail_before = 'enable'
        with patches, patch.object(updater, 'import_installed'):
            with self.assertRaises(KeyboardInterrupt):
                self.transaction(values).run(values['installer'])
            self.assertFalse(values['service'].enabled())
            self.assertEqual(self.transaction(values).current_hashes(), values['expected'])
            self.assertEqual(updater.read_json(values['journal'])['phase'], 'imports_verified')
            self.assertEqual(self.transaction(values).run(values['installer']),
                             'COMPONENT_EXECUTOR_UPDATED')

    def test_import_failure_keeps_default_disabled_until_verified_retry(self):
        values, patches = self.workspace()
        with patches:
            with patch.object(updater, 'import_installed', side_effect=RuntimeError('bad import')):
                with self.assertRaises(RuntimeError):
                    self.transaction(values).run(values['installer'])
            self.assertFalse(values['service'].enabled())
            self.assertEqual(self.transaction(values).current_hashes(), values['expected'])
            self.assertEqual(updater.read_json(values['journal'])['phase'], 'files_installed')
            with patch.object(updater, 'import_installed'):
                self.assertEqual(self.transaction(values).run(values['installer']),
                                 'COMPONENT_EXECUTOR_UPDATED')

    def test_legacy_panel_recovery_finishes_before_first_executor_replace(self):
        values, patches = self.workspace()
        FakeWire.LegacyPanelRecovery.calls = []
        FakeDeploy.request = {'status': 'failure'}
        values['installer'].ledger['pending'] = {'phase': 'activation_pending'}
        recovery = {'operation_id': 'deploy-7', 'expected': {
            'target_revision': '1' * 40, 'target_image_sha256': '2' * 64,
            'prior_revision': '3' * 40, 'prior_image_sha256': '4' * 64}}
        with patches, patch.object(updater, 'import_installed'):
            original = updater.atomic_file

            def check_recovery(path, content):
                if path.parent == values['executor']:
                    self.assertEqual(FakeWire.LegacyPanelRecovery.calls,
                                     [('deploy-7', recovery['expected'])])
                original(path, content)

            with patch.object(updater, 'atomic_file', side_effect=check_recovery):
                self.assertEqual(self.transaction(values, recovery).run(values['installer']),
                                 'COMPONENT_EXECUTOR_UPDATED')
        self.assertIsNone(values['installer'].ledger['pending'])

    def test_cli_requires_hashes_for_all_six_modules(self):
        arguments = ['update-component-executor.py', '--source', '/verified',
                     '--expected-updater-sha256', '0' * 64]
        for name in updater.FILES:
            arguments.extend(['--expected-' + name.replace('_', '-').replace('.py', '') + '-sha256',
                              '1' * 64])
        with patch.object(sys, 'argv', arguments):
            parsed = updater.parse_args()
        for name in updater.FILES:
            self.assertEqual(getattr(parsed, 'expected_' + name.replace('.py', '') + '_sha256'),
                             '1' * 64)

    def test_runbook_externally_verifies_exact_updater_before_root_execution(self):
        root = Path(__file__).resolve().parents[1]
        readme = (root / 'deploy/components/README.md').read_text()
        updater_hash = hashlib.sha256(
            (root / 'scripts/update-component-executor.py').read_bytes()).hexdigest()
        updater_path = '/home/alpine/hl240-wire-v2-executor/update-component-executor.py'
        gate = f"doas sha256sum -c - <<'EOF'\n{updater_hash}  {updater_path}\nEOF"
        stop = 'If it exits non-zero, stop immediately and do not run the Python'
        execute = f'doas python3 {updater_path}'
        self.assertIn(gate, readme)
        self.assertLess(readme.index(gate), readme.index(stop))
        self.assertLess(readme.index(stop), readme.index(execute))


if __name__ == '__main__':
    unittest.main()
