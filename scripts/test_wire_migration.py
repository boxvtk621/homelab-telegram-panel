import copy
import datetime
import fcntl
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

import component_cd
import component_deploy as host
import component_release as release
import deploy
import wire_migration as wire


def manifest(component, side, version, digest):
    adapter = {'panel': None, 'cursor': '1.0.31', 'codex': '0.153.4'}[component]
    return {'schema': 1, 'component': component, 'version': version, 'revision': 'b' * 40,
            'image': release.COMPONENTS[component]['image'] + '@sha256:' + digest * 64,
            'platform': 'linux/amd64', 'adapter_version': adapter,
            'state_compatibility': wire.PLAN['compatibility'][component][side]}


def pair():
    priors = {name: manifest(name, 'from', 'v0.2.0', {'cursor': 'a', 'codex': 'c', 'panel': 'e'}[name])
              for name in wire.COMPONENTS}
    targets = {name: manifest(name, 'to', 'v0.2.1', {'cursor': 'b', 'codex': 'd', 'panel': 'f'}[name])
               for name in wire.COMPONENTS}
    return priors, targets


class FakeRouter:
    def __init__(self, installer):
        self.installer = installer
        self.fail_activation = False
        self.nodes = {
            name: {'mode': 'eligible', 'stateVersion': 1, 'generation': 1, 'identityEpoch': 1,
                   'adapterKind': name, 'adapterVersion': installer.priors[name]['adapter_version']}
            for name in wire.HARNESSES
        }

    def status(self):
        return {'schema': 1, 'ownerId': 'owner', 'registryVersion': 1,
                'registrySHA256': '9' * 64,
                'nodes': {self.installer.node_ids[name]: copy.deepcopy(value)
                          for name, value in self.nodes.items()}}

    def transition_many(self, action, current, operation_id, identities=None):
        self.installer.operations.append(('router', action, tuple(sorted(current))))
        if action == 'activate' and self.fail_activation:
            raise deploy.DeployError('SIMULATED_ACTIVATION_FAILURE')
        result = {}
        for name in wire.HARNESSES:
            node_id = self.installer.node_ids[name]
            if node_id not in current:
                continue
            identity = (identities or {}).get(node_id)
            self.nodes[name] = host.RouterControl.projected(action, self.nodes[name], operation_id, identity)
            result[node_id] = copy.deepcopy(self.nodes[name])
        return result


class FakeInstaller:
    def __init__(self, root):
        self.root = root
        self.file = root / 'deployment.json'
        self.priors, self.targets = pair()
        self.node_ids = {'cursor': 'cursor-node', 'codex': 'codex-node'}
        self.config = {'project': 'fixture', 'compose': '/fixture/compose.yml', 'env_file': '/fixture/env',
                       'components': {
                           'cursor': {'service': 'cursor', 'node_id': self.node_ids['cursor'],
                                      'actor_id': 'owner', 'url': 'https://cursor:18443'},
                           'codex': {'service': 'codex', 'node_id': self.node_ids['codex'],
                                    'actor_id': 'owner', 'url': 'https://codex:18443'},
                           'panel': {'service': 'panel'},
                       }}
        self.ledger = {'components': {name: {'current': copy.deepcopy(self.priors[name]), 'previous': None}
                                     for name in wire.COMPONENTS},
                       'pending': None, 'config_sha256': 'fingerprint'}
        self.runtime = copy.deepcopy(self.priors)
        self.operations = []
        self.unknown = None
        self.router = FakeRouter(self)

    def fingerprint(self):
        return 'fingerprint'

    def container(self, name):
        return {'cursor': 'a' * 64, 'codex': 'c' * 64, 'panel': 'e' * 64}[name]

    def inspect(self, name, candidate, container=None):
        self.operations.append(('inspect', name, candidate['version']))
        if self.runtime[name] != candidate:
            raise deploy.DeployError('SIMULATED_RUNTIME_MISMATCH')
        return {'name': name}, {}

    def health(self, name, candidate):
        if name == 'panel':
            self.operations.append(('panel_health', candidate['version']))
            return None
        side = ('to_wire' if candidate['state_compatibility'] ==
                wire.PLAN['compatibility'][name]['to'] else 'from_wire')
        return {'node': self.node_ids[name], 'epoch': 1, 'registry': 1, 'kind': name,
                'version': candidate['adapter_version'], **wire.PLAN[side], 'quiescent': True}

    def routing(self):
        return self.router.status()

    def require_routes(self, names, mode, operation_id=''):
        state = self.routing()
        for name in names:
            node = state['nodes'][self.node_ids[name]]
            if node['mode'] != mode or (mode != 'eligible' and node.get('operationId') != operation_id):
                raise deploy.DeployError('SIMULATED_ROUTE_MISMATCH')
        return state

    def route_subset(self, names, state):
        return {self.node_ids[name]: state['nodes'][self.node_ids[name]] for name in names}

    def identity_subset(self, names, identities):
        return {self.node_ids[name]: identities[name] for name in names}

    @staticmethod
    def identity(health):
        return {key: health[key] for key in ('node', 'epoch', 'registry', 'kind', 'version')}

    def verify_transition(self, expected):
        observed = self.routing()['nodes']
        if any(observed[node_id] != state for node_id, state in expected.items()):
            raise deploy.DeployError('SIMULATED_TRANSITION_MISMATCH')

    def pin_override(self, name, candidate):
        self.operations.append(('pin', name, candidate['version']))
        return self.root / (name + '.override.json')

    def replace(self, name, candidate, alternate=None, override=None):
        self.operations.append(('replace', name, candidate['version']))
        if self.unknown == name:
            self.runtime[name] = copy.deepcopy(candidate)
            return 'desired_after_unknown'
        self.runtime[name] = copy.deepcopy(candidate)
        return 'desired'

    def compose_mutation(self, action, *args, override=None):
        self.operations.append(('compose', action))
        if action == 'stop':
            return ''
        if action == 'up':
            self.runtime = copy.deepcopy(self.priors)
            return ''
        raise AssertionError(action)

    def compose(self, *args, override=None):
        return ''


class FakeOperation(wire.Operation):
    def __init__(self, installer):
        super().__init__(installer)
        self.fail_target_verification = False

    def validate_candidate_image(self, candidate):
        self.installer.operations.append(('image', candidate['component']))

    def backup_pair(self, priors, targets, operation_id, before, runtime_data):
        self.installer.operations.append(('backup_pair', tuple(sorted(runtime_data))))
        self.installer.require_routes(wire.HARNESSES, 'sealed', operation_id)
        return {name: {'sha256': {'cursor': '1', 'codex': '2'}[name] * 64, 'size': 4096}
                for name in wire.HARNESSES}

    def verify_backups(self, priors, targets, operation_id, records):
        self.installer.operations.append(('verify_backups', operation_id))
        return {name: {'source': '/fixture/' + name + '/harness.db',
                       'source_device': 1, 'source_inode': {'cursor': 2, 'codex': 3}[name],
                       'backup_sha256': records[name]['sha256']}
                for name in wire.HARNESSES}

    def verify_runtime(self, manifests, before, operation_id, wire_identity, database=None, rollbacks=None,
                       sealed=True):
        self.installer.operations.append(('verify_runtime', tuple(manifests[name]['version'] for name in wire.COMPONENTS),
                                          wire_identity['schemaId'], None if database is None else database['user_version']))
        if sealed:
            self.installer.require_routes(wire.HARNESSES, 'sealed', operation_id)
        if self.fail_target_verification and wire_identity == wire.PLAN['to_wire']:
            raise deploy.DeployError('SIMULATED_V2_VERIFICATION_FAILURE')
        self.installer.inspect('panel', manifests['panel'])
        self.installer.health('panel', manifests['panel'])
        observed = {}
        for name in wire.HARNESSES:
            self.installer.inspect(name, manifests[name])
            health = self.installer.health(name, manifests[name])
            if self.wire_identity(health) != self.expected_wire(name, manifests[name], before, wire_identity):
                raise deploy.DeployError('SIMULATED_WIRE_MISMATCH')
            observed[name] = health
        return observed

    def combined_override(self, manifests):
        self.installer.operations.append(('combined_override', tuple(manifests[name]['version'] for name in wire.COMPONENTS)))
        return self.installer.root / 'combined.json'


class WireMigrationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.root.chmod(0o700)
        self.installer = FakeInstaller(self.root)
        self.operation = FakeOperation(self.installer)

    def test_all_routes_stay_sealed_until_panel_and_both_v2_nodes_are_verified(self):
        self.assertEqual(self.operation.apply(self.installer.targets, 'wire-migrate-7'),
                         'WIRE_MIGRATION_DEPLOYED')
        operations = self.installer.operations
        backup = operations.index(('backup_pair', ('codex', 'cursor')))
        cursor = operations.index(('replace', 'cursor', 'v0.2.1'))
        codex = operations.index(('replace', 'codex', 'v0.2.1'))
        panel = operations.index(('replace', 'panel', 'v0.2.1'))
        verified = operations.index(('verify_runtime', ('v0.2.1', 'v0.2.1', 'v0.2.1'),
                                     'harness-wire-v2', 2))
        activation = next(index for index, value in enumerate(operations)
                          if value[0:2] == ('router', 'activate'))
        self.assertLess(backup, cursor)
        self.assertLess(cursor, codex)
        self.assertLess(codex, panel)
        self.assertLess(panel, verified)
        self.assertLess(verified, activation)
        activated_nodes = operations[activation][2]
        self.assertEqual(set(activated_nodes), {'cursor-node', 'codex-node'})
        self.assertEqual(self.installer.router.nodes['cursor']['mode'], 'eligible')
        self.assertEqual(self.installer.router.nodes['codex']['mode'], 'eligible')

    def test_partial_or_unknown_replacement_never_activates_or_runs_image_inverse(self):
        self.installer.unknown = 'codex'
        with self.assertRaisesRegex(deploy.DeployError, 'WIRE_MIGRATION_BACKUP_RESTORE_REQUIRED'):
            self.operation.apply(self.installer.targets, 'wire-migrate-7')
        replacements = [value for value in self.installer.operations if value[0] == 'replace']
        self.assertEqual(replacements, [('replace', 'cursor', 'v0.2.1'), ('replace', 'codex', 'v0.2.1')])
        self.assertFalse(any(value[0:2] == ('router', 'activate') for value in self.installer.operations))
        self.assertTrue(all(node['mode'] == 'sealed' for node in self.installer.router.nodes.values()))
        with self.assertRaisesRegex(deploy.DeployError, 'WIRE_MIGRATION_BACKUP_RESTORE_REQUIRED'):
            self.operation.reconcile()

    def test_crash_after_batch_seal_reopens_only_the_exact_priors(self):
        with patch.object(self.operation, 'backup_pair', side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                self.operation.apply(self.installer.targets, 'wire-migrate-7')
        self.assertEqual(self.installer.ledger['pending']['phase'], 'sealed')
        self.assertTrue(all(node['mode'] == 'sealed' for node in self.installer.router.nodes.values()))
        self.assertEqual(self.operation.reconcile(), 'WIRE_MIGRATION_PRIORS_RETAINED')
        self.assertEqual(self.installer.runtime, self.installer.priors)
        self.assertTrue(all(node['mode'] == 'eligible' for node in self.installer.router.nodes.values()))

    def test_activation_crash_rechecks_all_targets_then_batches_once(self):
        self.installer.router.fail_activation = True
        with self.assertRaisesRegex(deploy.DeployError, 'WIRE_MIGRATION_BACKUP_RESTORE_REQUIRED'):
            self.operation.apply(self.installer.targets, 'wire-migrate-7')
        self.assertEqual(self.installer.ledger['pending']['phase'], 'activation_pending')
        self.assertTrue(all(node['mode'] == 'sealed' for node in self.installer.router.nodes.values()))
        self.installer.router.fail_activation = False
        self.assertEqual(self.operation.reconcile(), 'WIRE_MIGRATION_DEPLOYED_AFTER_RESTART')
        self.assertIsNone(self.installer.ledger['pending'])

    def test_exact_pair_restore_stops_all_restores_both_and_reopens_together(self):
        self.operation.fail_target_verification = True
        with self.assertRaises(deploy.DeployError):
            self.operation.apply(self.installer.targets, 'wire-migrate-7')
        hashes = {name: self.installer.ledger['pending']['backups'][name]['sha256'] for name in wire.HARNESSES}
        self.operation.fail_target_verification = False
        restored = []
        with patch.object(wire, 'restore_backup', side_effect=lambda source, backup, digest, identity: restored.append((source, digest))):
            self.assertEqual(self.operation.restore('wire-migrate-7', hashes),
                             'WIRE_MIGRATION_EXACT_BACKUPS_RESTORED')
        self.assertEqual(len(restored), 2)
        self.assertEqual(self.installer.runtime, self.installer.priors)
        self.assertTrue(all(node['mode'] == 'eligible' for node in self.installer.router.nodes.values()))
        self.assertIsNone(self.installer.ledger['pending'])

    def test_restore_crash_keeps_both_routes_sealed_until_explicit_retry(self):
        self.operation.fail_target_verification = True
        with self.assertRaises(deploy.DeployError):
            self.operation.apply(self.installer.targets, 'wire-migrate-7')
        hashes = {name: self.installer.ledger['pending']['backups'][name]['sha256'] for name in wire.HARNESSES}
        calls = 0

        def crash_once(source, backup, digest, identity):
            nonlocal calls
            calls += 1
            if calls == 1:
                raise KeyboardInterrupt

        with patch.object(wire, 'restore_backup', side_effect=crash_once):
            with self.assertRaises(KeyboardInterrupt):
                self.operation.restore('wire-migrate-7', hashes)
        self.assertEqual(self.installer.ledger['pending']['phase'], 'restore_databases_started')
        self.assertTrue(all(node['mode'] == 'sealed' for node in self.installer.router.nodes.values()))
        self.operation.fail_target_verification = False
        with patch.object(wire, 'restore_backup') as restore:
            self.assertEqual(self.operation.restore('wire-migrate-7', hashes),
                             'WIRE_MIGRATION_EXACT_BACKUPS_RESTORED')
        self.assertEqual(restore.call_count, 2)

    def test_restore_requires_both_exact_hashes_before_stopping_anything(self):
        self.operation.fail_target_verification = True
        with self.assertRaises(deploy.DeployError):
            self.operation.apply(self.installer.targets, 'wire-migrate-7')
        hashes = {name: self.installer.ledger['pending']['backups'][name]['sha256'] for name in wire.HARNESSES}
        hashes['codex'] = '0' * 64
        before = len(self.installer.operations)
        with self.assertRaisesRegex(deploy.DeployError, 'WIRE_MIGRATION_RESTORE_REQUEST_MISMATCH'):
            self.operation.restore('wire-migrate-7', hashes)
        self.assertFalse(any(value[0] == 'compose' for value in self.installer.operations[before:]))

    def test_manifest_pair_rejects_missing_node_or_unreviewed_hash(self):
        targets = copy.deepcopy(self.installer.targets)
        targets['cursor']['state_compatibility'] = '0' * 64
        with self.assertRaisesRegex(deploy.DeployError, 'WIRE_MIGRATION_MANIFEST_PAIR_MISMATCH'):
            self.operation.apply(targets, 'wire-migrate-7')
        del self.installer.config['components']['codex']
        with self.assertRaisesRegex(deploy.DeployError, 'WIRE_MIGRATION_REQUIRES_CURSOR_CODEX_PANEL'):
            self.operation.apply(self.installer.targets, 'wire-migrate-7')

    def test_dedicated_request_requires_exact_workflow_and_three_tags(self):
        now = datetime.datetime.now(datetime.timezone.utc)
        payload = {'schema': 1, 'migration': wire.MIGRATION,
                   'tags': {name: name + '-v0.2.1' for name in wire.COMPONENTS},
                   'target_revision': 'b' * 40, 'run_id': 8, 'run_attempt': 1,
                   'workflow_sha': 'b' * 40,
                   'allow_interrupt': True}
        request = {'id': 7, 'environment': component_cd.ENVIRONMENT,
                   'creator': {'login': 'github-actions[bot]', 'type': 'Bot'},
                   'created_at': now.isoformat(), 'task': wire.TASK, 'ref': 'panel-v0.2.1',
                   'payload': payload}
        run = {'event': 'workflow_dispatch', 'path': wire.WORKFLOW, 'head_branch': 'main',
               'head_sha': 'b' * 40, 'head_repository': {'full_name': release.REPO},
               'status': 'in_progress', 'run_attempt': 1}
        self.assertEqual(component_cd.authorize(request, read=lambda _: run)['kind'], wire.MIGRATION)
        request['payload']['tags'].pop('codex')
        with self.assertRaises(deploy.DeployError):
            component_cd.authorize(request, read=lambda _: run)


class BackupRestoreTests(unittest.TestCase):
    def test_private_backup_and_hash_bound_restore_require_exclusive_lock(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o700)
            data = root / 'node'
            data.mkdir(mode=0o700)
            database = data / 'harness.db'
            connection = sqlite3.connect(database)
            connection.executescript('''
                CREATE TABLE schema_meta(singleton INTEGER PRIMARY KEY, fingerprint TEXT NOT NULL);
                CREATE TABLE node_state(singleton INTEGER PRIMARY KEY, node_id TEXT NOT NULL,
                                        owner_id TEXT NOT NULL, registry_version INTEGER NOT NULL);
                INSERT INTO schema_meta VALUES(1, '2ef224cb3489121c3b8fb21f38bba849b2a7eb36383eb2fae1a5e255099b987d');
                INSERT INTO node_state VALUES(1, 'node', 'owner', 1);
                PRAGMA user_version=1;
            ''')
            connection.commit()
            connection.close()
            database.chmod(0o600)
            lock_path = data / '.harness.lock'
            lock_path.touch(mode=0o600)
            lock_path.chmod(0o600)
            backups = root / 'backups'
            backups.mkdir(mode=0o700)
            backup = backups / 'harness.db'
            identity = {'node_id': 'node', 'owner_id': 'owner', 'registry_version': 1}
            details = wire.create_backup(database, backup, identity, os.geteuid())
            self.assertEqual(backup.stat().st_mode & 0o777, 0o600)
            connection = sqlite3.connect(database)
            connection.execute("UPDATE schema_meta SET fingerprint='changed' WHERE singleton=1")
            connection.execute('PRAGMA user_version=2')
            connection.commit()
            connection.close()
            descriptor = os.open(lock_path, os.O_RDWR | os.O_NOFOLLOW)
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            try:
                with self.assertRaisesRegex(deploy.DeployError, 'MIGRATION_DATABASE_STILL_RUNNING'):
                    wire.restore_backup(database, backup, details['sha256'], identity, os.geteuid())
            finally:
                fcntl.flock(descriptor, fcntl.LOCK_UN)
                os.close(descriptor)
            with self.assertRaisesRegex(deploy.DeployError, 'MIGRATION_BACKUP_HASH_MISMATCH'):
                wire.restore_backup(database, backup, '0' * 64, identity, os.geteuid())
            wire.restore_backup(database, backup, details['sha256'], identity, os.geteuid())
            self.assertEqual(wire.sha256(database), details['sha256'])
            self.assertEqual(wire.database_facts(database), (wire.PLAN['from_database'], identity))


if __name__ == '__main__':
    unittest.main()
