#!/usr/bin/env python3
"""One-shot all-node Harness wire v1 to v2 migration on the trusted host."""
import argparse
import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import stat
import tempfile

import component_deploy as host
import component_release as release
import deploy


MIGRATION = 'harness-wire-v1-to-v2'
LEGACY_PANEL_RECOVERY = 'legacy-panel-v2-on-v1-rollback'
WORKFLOW = '.github/workflows/wire-migration.yml'
TASK = MIGRATION + ':apply'
COMPONENTS = ('cursor', 'codex', 'panel')
HARNESSES = ('cursor', 'codex')
PLAN = {
    'compatibility': {
        'cursor': {
            'from': '33e5ed88c2c20a2c4002d922392d0a04da5108d8b58679eebd69e9a7a1d6b569',
            'to': '1820d3ae7c8caa2f426a5ae8b838a71e8047e0953c8d1669f638a66cf5dc1afb',
        },
        'codex': {
            'from': '136205259ae3e40b35137a98aef364ac5be2320c8feff3888a222a03c104960f',
            'to': '31984079537905b6294d25b63ff641f6f2ef6e1b631a4f6816a4824a13108a4c',
        },
        'panel': {
            'from': 'a3e4b28b29fc523bbc707bef30c10e6cfea6a910c7d663e3fd5c1ae3a83caea0',
            'to': '5f9cbcd409fb7a349c500cbd5d6a71d583ae7f15b7e3d79b18c93cf98da115d0',
        },
    },
    'from_wire': {
        'schemaId': 'harness-wire-v1',
        'schemaSHA256': 'a482f087231d1991e140f074cbea35db675fb204fea443808ee253c58bdd5236',
    },
    'to_wire': {
        'schemaId': 'harness-wire-v2',
        'schemaSHA256': '5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9',
    },
    'from_database': {
        'user_version': 1,
        'fingerprint': '2ef224cb3489121c3b8fb21f38bba849b2a7eb36383eb2fae1a5e255099b987d',
    },
    'to_database': {
        'user_version': 2,
        'fingerprint': '5ae1b8abce397d2cb3e5221757c3069529b302d7842ab791dc430442bcc101df',
    },
}


def private_path(path, owner, directory=False, mode=None):
    try:
        info = os.lstat(path)
    except OSError:
        raise deploy.DeployError('UNSAFE_MIGRATION_STATE_PATH') from None
    deploy.require(not stat.S_ISLNK(info.st_mode) and info.st_uid == owner and not info.st_mode & 0o077 and
                   (mode is None or stat.S_IMODE(info.st_mode) == mode) and
                   (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)),
                   'UNSAFE_MIGRATION_STATE_PATH')
    return info


def sha256(path):
    digest = hashlib.sha256()
    try:
        with path.open('rb') as source:
            while True:
                block = source.read(1024 * 1024)
                if not block:
                    return digest.hexdigest()
                digest.update(block)
    except OSError:
        raise deploy.DeployError('MIGRATION_BACKUP_READ_FAILED') from None


def database_facts(path):
    try:
        database = sqlite3.connect(path.resolve(strict=True).as_uri() + '?mode=ro', uri=True)
        try:
            version = database.execute('PRAGMA user_version').fetchone()[0]
            fingerprint = database.execute(
                'SELECT fingerprint FROM schema_meta WHERE singleton=1').fetchone()[0]
            identity = database.execute(
                'SELECT node_id,owner_id,registry_version FROM node_state WHERE singleton=1').fetchone()
            integrity = database.execute('PRAGMA integrity_check').fetchall()
            foreign_keys = database.execute('PRAGMA foreign_key_check').fetchall()
        finally:
            database.close()
    except Exception:
        raise deploy.DeployError('MIGRATION_DATABASE_INVALID') from None
    deploy.require(integrity == [('ok',)] and foreign_keys == [] and identity is not None,
                   'MIGRATION_DATABASE_INVALID')
    return {'user_version': version, 'fingerprint': fingerprint}, {
        'node_id': identity[0], 'owner_id': identity[1], 'registry_version': identity[2],
    }


def runtime_config_matches_adapter(config, name):
    if config.get('adapter') == name:
        return True
    return (name == 'cursor' and 'adapter' not in config and
            type(config.get('cursor')) is dict and 'codex' not in config)


def create_backup(source, destination, expected_identity, source_owner=10001):
    source_info = private_path(source, source_owner, mode=0o600)
    facts, identity = database_facts(source)
    deploy.require(facts == PLAN['from_database'] and identity == expected_identity,
                   'MIGRATION_SOURCE_DATABASE_MISMATCH')
    private_path(destination.parent, os.geteuid(), directory=True, mode=0o700)
    deploy.require(not destination.exists() and not destination.is_symlink(), 'MIGRATION_BACKUP_EXISTS')
    try:
        descriptor = os.open(destination, os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        os.close(descriptor)
        source_db = sqlite3.connect(source.resolve(strict=True).as_uri() + '?mode=ro', uri=True)
        backup_db = sqlite3.connect(str(destination))
        try:
            source_db.backup(backup_db)
        finally:
            backup_db.close()
            source_db.close()
        with destination.open('rb') as backup:
            os.fsync(backup.fileno())
        folder = os.open(destination.parent, os.O_RDONLY)
        try:
            os.fsync(folder)
        finally:
            os.close(folder)
        backup_info = private_path(destination, os.geteuid(), mode=0o600)
        backup_facts, backup_identity = database_facts(destination)
        current_source = private_path(source, source_owner, mode=0o600)
        deploy.require(backup_facts == PLAN['from_database'] and backup_identity == expected_identity and
                       current_source.st_dev == source_info.st_dev and current_source.st_ino == source_info.st_ino,
                       'MIGRATION_BACKUP_VERIFICATION_FAILED')
        return {'sha256': sha256(destination), 'size': backup_info.st_size,
                'source_device': source_info.st_dev, 'source_inode': source_info.st_ino}
    except deploy.DeployError:
        if destination.exists() and not destination.is_symlink():
            destination.unlink()
        raise
    except Exception:
        if destination.exists() and not destination.is_symlink():
            destination.unlink()
        raise deploy.DeployError('MIGRATION_BACKUP_FAILED') from None


def restore_backup(source, backup, expected_sha256, expected_identity, source_owner=10001):
    source_info = private_path(source, source_owner, mode=0o600)
    backup_info = private_path(backup, os.geteuid(), mode=0o600)
    deploy.require(backup_info.st_size > 0 and sha256(backup) == expected_sha256,
                   'MIGRATION_BACKUP_HASH_MISMATCH')
    facts, identity = database_facts(backup)
    deploy.require(facts == PLAN['from_database'] and identity == expected_identity,
                   'INVALID_MIGRATION_BACKUP')
    lock_path = source.parent / '.harness.lock'
    private_path(lock_path, source_owner, mode=0o600)
    for suffix in ('-journal', '-wal', '-shm'):
        sidecar = Path(str(source) + suffix)
        deploy.require(not sidecar.exists() and not sidecar.is_symlink(), 'MIGRATION_DATABASE_HAS_SIDECAR')
    lock = os.open(lock_path, os.O_RDWR | os.O_NOFOLLOW)
    temporary = None
    try:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise deploy.DeployError('MIGRATION_DATABASE_STILL_RUNNING') from None
        descriptor, temporary = tempfile.mkstemp(prefix='.wire-v1-restore-', dir=source.parent)
        with os.fdopen(descriptor, 'wb') as output, backup.open('rb') as input_file:
            while True:
                block = input_file.read(1024 * 1024)
                if not block:
                    break
                output.write(block)
            os.fchmod(output.fileno(), 0o600)
            if (source_info.st_uid, source_info.st_gid) != (os.geteuid(), os.getegid()):
                os.fchown(output.fileno(), source_info.st_uid, source_info.st_gid)
            output.flush()
            os.fsync(output.fileno())
        candidate = Path(temporary)
        private_path(candidate, source_owner, mode=0o600)
        deploy.require(candidate.stat().st_size == backup_info.st_size and sha256(candidate) == expected_sha256,
                       'MIGRATION_RESTORE_VERIFICATION_FAILED')
        os.replace(candidate, source)
        temporary = None
        folder = os.open(source.parent, os.O_RDONLY)
        try:
            os.fsync(folder)
        finally:
            os.close(folder)
        restored_facts, restored_identity = database_facts(source)
        deploy.require(private_path(source, source_owner, mode=0o600).st_size == backup_info.st_size and
                       sha256(source) == expected_sha256 and restored_facts == PLAN['from_database'] and
                       restored_identity == expected_identity, 'MIGRATION_RESTORE_VERIFICATION_FAILED')
    except deploy.DeployError:
        raise
    except Exception:
        raise deploy.DeployError('MIGRATION_BACKUP_RESTORE_FAILED') from None
    finally:
        if temporary is not None:
            try:
                os.unlink(temporary)
            except OSError:
                pass
        fcntl.flock(lock, fcntl.LOCK_UN)
        os.close(lock)


class Operation:
    BASE = {'kind', 'migration', 'operation_id', 'phase', 'priors', 'targets', 'prior_slots',
            'prior_containers', 'before_identities', 'backups'}
    ACTIVATION = {'sealed_routes', 'target_identities'}
    RESTORE_REOPEN = {'restore_sealed_routes'}
    PHASES = {
        'prepared', 'sealed', 'backups_verified',
        'cursor_replace_started', 'cursor_replace_unknown', 'cursor_replaced',
        'codex_replace_started', 'codex_replace_unknown', 'codex_replaced',
        'panel_replace_started', 'panel_replace_unknown', 'panel_replaced',
        'migration_attention', 'targets_verified', 'activation_pending',
        'restore_stop_started', 'restore_stop_unknown', 'restore_stopped',
        'restore_databases_started', 'restore_databases_complete',
        'restore_start_started', 'restore_start_unknown', 'restore_prior_verified',
        'restore_reopen_pending', 'restore_reopened',
    }

    def __init__(self, installer):
        self.installer = installer

    def wire_identity(self, health):
        return {key: health[key] for key in
                ('node', 'epoch', 'registry', 'kind', 'version', 'schemaId', 'schemaSHA256')}

    def validate_pair(self, priors, targets):
        deploy.require(set(self.installer.config['components']) == set(COMPONENTS) and
                       set(priors) == set(COMPONENTS) and set(targets) == set(COMPONENTS),
                       'WIRE_MIGRATION_REQUIRES_CURSOR_CODEX_PANEL')
        revisions = set()
        for name in COMPONENTS:
            release.validate(priors[name], name)
            release.validate(targets[name], name)
            expected = PLAN['compatibility'][name]
            deploy.require(priors[name]['state_compatibility'] == expected['from'] and
                           targets[name]['state_compatibility'] == expected['to'] and
                           targets[name] != priors[name], 'WIRE_MIGRATION_MANIFEST_PAIR_MISMATCH')
            revisions.add(targets[name]['revision'])
        deploy.require(len(revisions) == 1, 'WIRE_MIGRATION_TARGET_REVISION_MISMATCH')

    def state_database(self, name, container_data):
        deploy.require(name in HARNESSES and container_data.get('Config', {}).get('Cmd') ==
                       ['--config', '/config/node.json'], 'MIGRATION_RUNTIME_CONFIG_MISMATCH')
        mounts = container_data.get('Mounts')
        deploy.require(type(mounts) is list, 'MIGRATION_RUNTIME_MOUNTS_INVALID')

        def mount(destination, writable):
            found = [value for value in mounts if type(value) is dict and value.get('Destination') == destination]
            deploy.require(len(found) == 1 and found[0].get('Type') == 'bind' and
                           found[0].get('RW') is writable and isinstance(found[0].get('Source'), str),
                           'MIGRATION_RUNTIME_MOUNTS_INVALID')
            source = Path(found[0]['Source'])
            deploy.require(source.is_absolute() and source == Path(os.path.normpath(source)) and
                           source.resolve(strict=True) == source, 'MIGRATION_RUNTIME_MOUNTS_INVALID')
            private_path(source, 10001, directory=True, mode=0o700)
            return source

        state = mount('/state', True)
        config = mount('/config', False)
        node_file = config / 'node.json'
        private_path(node_file, 10001, mode=0o600)
        try:
            deploy.require(node_file.stat().st_size <= 65536, 'MIGRATION_RUNTIME_CONFIG_MISMATCH')
            value = json.loads(node_file.read_text(), object_pairs_hook=deploy.pairs)
        except deploy.DeployError:
            raise
        except Exception:
            raise deploy.DeployError('MIGRATION_RUNTIME_CONFIG_MISMATCH') from None
        component = self.installer.config['components'][name]
        deploy.require(type(value) is dict and value.get('dataDir') == '/state/node' and
                       value.get('nodeId') == component['node_id'] and value.get('ownerId') == component['actor_id'] and
                       value.get('registryVersion') == self.installer.routing()['registryVersion'] and
                       runtime_config_matches_adapter(value, name), 'MIGRATION_RUNTIME_CONFIG_MISMATCH')
        data = state / 'node'
        private_path(data, 10001, directory=True, mode=0o700)
        database = data / 'harness.db'
        private_path(database, 10001, mode=0o600)
        return database

    def backup_root(self, operation_id):
        deploy.require(deploy.re.fullmatch(r'wire-migrate-[1-9][0-9]{0,19}', operation_id),
                       'INVALID_WIRE_MIGRATION_OPERATION_ID')
        return self.installer.root / 'migration-backups' / operation_id

    def backup_pair(self, priors, targets, operation_id, before, runtime_data):
        parent = self.installer.root / 'migration-backups'
        if not parent.exists():
            parent.mkdir(mode=0o700)
            descriptor = os.open(self.installer.root, os.O_RDONLY)
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        private_path(parent, os.geteuid(), directory=True, mode=0o700)
        root = self.backup_root(operation_id)
        deploy.require(not root.exists() and not root.is_symlink(), 'MIGRATION_BACKUP_EXISTS')
        root.mkdir(mode=0o700)
        descriptor = os.open(parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        records = {}
        for name in HARNESSES:
            directory = root / name
            directory.mkdir(mode=0o700)
            expected_identity = {'node_id': self.installer.config['components'][name]['node_id'],
                                 'owner_id': self.installer.config['components'][name]['actor_id'],
                                 'registry_version': before[name]['registry']}
            source = self.state_database(name, runtime_data[name])
            details = create_backup(source, directory / 'harness.db', expected_identity)
            record = {'sha256': details['sha256'], 'size': details['size']}
            rollback = {'schema': 1, 'migration': MIGRATION, 'component': name,
                        'operation_id': operation_id, 'source': str(source),
                        'source_device': details['source_device'], 'source_inode': details['source_inode'],
                        'backup_sha256': details['sha256'], 'backup_size': details['size'],
                        'prior': priors[name], 'target': targets[name]}
            deploy.atomic_json(directory / 'rollback.json', rollback)
            records[name] = record
        deploy.atomic_json(root / 'rollback.json', {'schema': 1, 'migration': MIGRATION,
                           'operation_id': operation_id, 'backups': records,
                           'priors': priors, 'targets': targets})
        descriptor = os.open(root, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        self.verify_backups(priors, targets, operation_id, records)
        return records

    def verify_backups(self, priors, targets, operation_id, records):
        deploy.require(type(records) is dict and set(records) == set(HARNESSES), 'INVALID_MIGRATION_BACKUP')
        root = self.backup_root(operation_id)
        private_path(self.installer.root / 'migration-backups', os.geteuid(), directory=True, mode=0o700)
        private_path(root, os.geteuid(), directory=True, mode=0o700)
        pair_file = root / 'rollback.json'
        private_path(pair_file, os.geteuid(), mode=0o600)
        pair = deploy.read_json(pair_file)
        deploy.require(pair == {'schema': 1, 'migration': MIGRATION, 'operation_id': operation_id,
                                'backups': records, 'priors': priors, 'targets': targets},
                       'INVALID_MIGRATION_BACKUP')
        rollbacks = {}
        registry = self.installer.routing()['registryVersion']
        for name in HARNESSES:
            record = records[name]
            deploy.require(type(record) is dict and set(record) == {'sha256', 'size'} and
                           deploy.re.fullmatch('[0-9a-f]{64}', record['sha256']) and
                           type(record['size']) is int and record['size'] > 0, 'INVALID_MIGRATION_BACKUP')
            directory = root / name
            private_path(directory, os.geteuid(), directory=True, mode=0o700)
            backup, rollback_file = directory / 'harness.db', directory / 'rollback.json'
            info = private_path(backup, os.geteuid(), mode=0o600)
            private_path(rollback_file, os.geteuid(), mode=0o600)
            rollback = deploy.read_json(rollback_file)
            expected_fields = {'schema', 'migration', 'component', 'operation_id', 'source',
                               'source_device', 'source_inode', 'backup_sha256', 'backup_size',
                               'prior', 'target'}
            deploy.require(type(rollback) is dict and set(rollback) == expected_fields and
                           rollback['schema'] == 1 and rollback['migration'] == MIGRATION and
                           rollback['component'] == name and rollback['operation_id'] == operation_id and
                           rollback['prior'] == priors[name] and rollback['target'] == targets[name] and
                           rollback['backup_sha256'] == record['sha256'] and
                           rollback['backup_size'] == record['size'] == info.st_size and
                           isinstance(rollback['source'], str) and Path(rollback['source']).is_absolute() and
                           Path(rollback['source']) == Path(os.path.normpath(rollback['source'])) and
                           Path(rollback['source']).resolve(strict=True) == Path(rollback['source']) and
                           type(rollback['source_device']) is int and type(rollback['source_inode']) is int and
                           sha256(backup) == record['sha256'], 'INVALID_MIGRATION_BACKUP')
            facts, identity = database_facts(backup)
            expected_identity = {'node_id': self.installer.config['components'][name]['node_id'],
                                 'owner_id': self.installer.config['components'][name]['actor_id'],
                                 'registry_version': registry}
            deploy.require(facts == PLAN['from_database'] and identity == expected_identity,
                           'INVALID_MIGRATION_BACKUP')
            rollbacks[name] = rollback
        return rollbacks

    def combined_override(self, manifests):
        services = {self.installer.config['components'][name]['service']: {'image': manifest['image']}
                    for name, manifest in manifests.items()}
        override = self.installer.root / 'wire-migration.override.json'
        deploy.atomic_json(override, {'services': services})
        return override

    def validate_candidate_image(self, manifest):
        host.run('docker', 'pull', manifest['image'])
        image = json.loads(host.run('docker', 'image', 'inspect', manifest['image']))[0]
        labels = image['Config'].get('Labels', {})
        deploy.require(image['Os'] == 'linux' and image['Architecture'] == 'amd64' and
                       image['Config']['User'] == '10001:10001' and
                       manifest['image'] in image.get('RepoDigests', []) and
                       labels.get('org.opencontainers.image.version') == manifest['version'] and
                       labels.get('org.opencontainers.image.revision') == manifest['revision'],
                       'CANDIDATE_IMAGE_MISMATCH')

    def expected_wire(self, name, manifest, before, wire):
        return dict(before[name], version=manifest['adapter_version'], **wire)

    def wait_quiescent(self, name, manifest, expected):
        deadline = host.time.monotonic() + 45
        while True:
            try:
                health = self.installer.health(name, manifest)
                deploy.require(self.wire_identity(health) == expected and health['quiescent'],
                               'NODE_NOT_QUIESCENT')
                return health
            except Exception:
                if host.time.monotonic() >= deadline:
                    raise
                host.time.sleep(2)

    def verify_runtime(self, manifests, before, operation_id, wire, database=None, rollbacks=None, sealed=True):
        if sealed:
            self.installer.require_routes(HARNESSES, 'sealed', operation_id)
        self.installer.inspect('panel', manifests['panel'])
        self.installer.health('panel', manifests['panel'])
        observed = {}
        for name in HARNESSES:
            self.installer.inspect(name, manifests[name])
            health = self.wait_quiescent(name, manifests[name],
                                         self.expected_wire(name, manifests[name], before, wire))
            observed[name] = health
            if database is not None:
                data, _ = self.installer.inspect(name, manifests[name])
                path = self.state_database(name, data)
                facts, identity = database_facts(path)
                expected_identity = {'node_id': self.installer.config['components'][name]['node_id'],
                                     'owner_id': self.installer.config['components'][name]['actor_id'],
                                     'registry_version': before[name]['registry']}
                deploy.require(facts == database and identity == expected_identity,
                               'MIGRATION_TARGET_DATABASE_MISMATCH')
                if rollbacks is not None:
                    info = private_path(path, 10001, mode=0o600)
                    rollback = rollbacks[name]
                    deploy.require(str(path) == rollback['source'] and
                                   info.st_dev == rollback['source_device'] and
                                   info.st_ino == rollback['source_inode'],
                                   'MIGRATION_DATABASE_IDENTITY_CHANGED')
        return observed

    def validate_pending(self):
        pending = self.installer.ledger['pending']
        deploy.require(type(pending) is dict and pending.get('kind') == MIGRATION and
                       set(pending) in (self.BASE, self.BASE | self.ACTIVATION,
                                        self.BASE | self.RESTORE_REOPEN) and
                       pending.get('migration') == MIGRATION and pending.get('phase') in self.PHASES and
                       deploy.re.fullmatch(r'wire-migrate-[1-9][0-9]{0,19}', pending.get('operation_id', '')),
                       'INVALID_WIRE_MIGRATION_JOURNAL')
        self.validate_pair(pending['priors'], pending['targets'])
        deploy.require(type(pending['prior_slots']) is dict and set(pending['prior_slots']) == set(COMPONENTS) and
                       type(pending['prior_containers']) is dict and set(pending['prior_containers']) == set(COMPONENTS),
                       'INVALID_WIRE_MIGRATION_JOURNAL')
        for name in COMPONENTS:
            deploy.require(type(pending['prior_slots'][name]) is dict and
                           set(pending['prior_slots'][name]) == {'current', 'previous'} and
                           pending['prior_slots'][name]['current'] == pending['priors'][name] and
                           deploy.re.fullmatch('[0-9a-f]{12,64}', pending['prior_containers'][name]),
                           'INVALID_WIRE_MIGRATION_JOURNAL')
            if pending['prior_slots'][name]['previous'] is not None:
                release.validate(pending['prior_slots'][name]['previous'], name)
        before = pending['before_identities']
        deploy.require(type(before) is dict and set(before) == set(HARNESSES),
                       'INVALID_WIRE_MIGRATION_JOURNAL')
        for name in HARNESSES:
            identity = before[name]
            deploy.require(type(identity) is dict and set(identity) ==
                           {'node', 'epoch', 'registry', 'kind', 'version', 'schemaId', 'schemaSHA256'} and
                           identity['node'] == self.installer.config['components'][name]['node_id'] and
                           identity['kind'] == name and identity['version'] == pending['priors'][name]['adapter_version'] and
                           identity['schemaId'] == PLAN['from_wire']['schemaId'] and
                           identity['schemaSHA256'] == PLAN['from_wire']['schemaSHA256'] and
                           type(identity['epoch']) is int and identity['epoch'] > 0 and
                           type(identity['registry']) is int and identity['registry'] > 0,
                           'INVALID_WIRE_MIGRATION_JOURNAL')
        if pending['phase'] in ('prepared', 'sealed'):
            deploy.require(pending['backups'] is None, 'INVALID_WIRE_MIGRATION_JOURNAL')
        else:
            self.verify_backups(pending['priors'], pending['targets'], pending['operation_id'], pending['backups'])
        if pending['phase'] == 'activation_pending':
            deploy.require(set(pending) == self.BASE | self.ACTIVATION, 'INVALID_WIRE_MIGRATION_JOURNAL')
            deploy.require(all(self.installer.ledger['components'][name] ==
                           {'current': pending['targets'][name], 'previous': pending['priors'][name]}
                           for name in COMPONENTS), 'INVALID_WIRE_MIGRATION_JOURNAL')
            node_ids = {self.installer.config['components'][name]['node_id'] for name in HARNESSES}
            deploy.require(type(pending['sealed_routes']) is dict and
                           set(pending['sealed_routes']) == node_ids and
                           type(pending['target_identities']) is dict and
                           set(pending['target_identities']) == node_ids,
                           'INVALID_WIRE_MIGRATION_JOURNAL')
            for node_id, route in pending['sealed_routes'].items():
                host.RouterControl.node(route)
                deploy.require(route['mode'] == 'sealed' and
                               route.get('operationId') == pending['operation_id'],
                               'INVALID_WIRE_MIGRATION_JOURNAL')
            for name in HARNESSES:
                node_id = self.installer.config['components'][name]['node_id']
                identity = pending['target_identities'][node_id]
                deploy.require(type(identity) is dict and set(identity) ==
                               {'node', 'epoch', 'registry', 'kind', 'version'} and
                               identity['node'] == node_id and identity['kind'] == name and
                               identity['version'] == pending['targets'][name]['adapter_version'] and
                               type(identity['epoch']) is int and identity['epoch'] > 0 and
                               type(identity['registry']) is int and identity['registry'] > 0,
                               'INVALID_WIRE_MIGRATION_JOURNAL')
        elif pending['phase'] in ('restore_reopen_pending', 'restore_reopened'):
            deploy.require(set(pending) == self.BASE | self.RESTORE_REOPEN and
                           all(self.installer.ledger['components'][name] == pending['prior_slots'][name]
                               for name in COMPONENTS), 'INVALID_WIRE_MIGRATION_JOURNAL')
            node_ids = {self.installer.config['components'][name]['node_id'] for name in HARNESSES}
            deploy.require(type(pending['restore_sealed_routes']) is dict and
                           set(pending['restore_sealed_routes']) == node_ids,
                           'INVALID_WIRE_MIGRATION_JOURNAL')
            for route in pending['restore_sealed_routes'].values():
                host.RouterControl.node(route)
                deploy.require(route['mode'] == 'sealed' and
                               route.get('operationId') == pending['operation_id'],
                               'INVALID_WIRE_MIGRATION_JOURNAL')
        else:
            deploy.require(set(pending) == self.BASE and
                           all(self.installer.ledger['components'][name] == pending['prior_slots'][name]
                               for name in COMPONENTS), 'INVALID_WIRE_MIGRATION_JOURNAL')
        return pending

    def apply(self, targets, operation_id):
        priors = {name: self.installer.ledger['components'][name]['current'] for name in COMPONENTS}
        self.validate_pair(priors, targets)
        deploy.require(self.installer.ledger['pending'] is None and
                       deploy.re.fullmatch(r'wire-migrate-[1-9][0-9]{0,19}', operation_id),
                       'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
        prior_slots = {name: dict(self.installer.ledger['components'][name]) for name in COMPONENTS}
        prior_containers = {name: self.installer.container(name) for name in COMPONENTS}
        runtime_data = {}
        for name in COMPONENTS:
            inspected = self.installer.inspect(name, priors[name])
            if name in HARNESSES:
                runtime_data[name] = inspected[0]
        self.installer.health('panel', priors['panel'])
        routes = self.installer.require_routes(HARNESSES, 'eligible')
        before = {}
        for name in HARNESSES:
            identity = self.wire_identity(self.installer.health(name, priors[name]))
            route = routes['nodes'][self.installer.config['components'][name]['node_id']]
            deploy.require(route['identityEpoch'] == identity['epoch'] and
                           route['adapterVersion'] == identity['version'] and
                           routes['registryVersion'] == identity['registry'] and
                           identity['schemaId'] == PLAN['from_wire']['schemaId'] and
                           identity['schemaSHA256'] == PLAN['from_wire']['schemaSHA256'],
                           'WIRE_MIGRATION_SOURCE_IDENTITY_MISMATCH')
            before[name] = identity
        for name in COMPONENTS:
            self.validate_candidate_image(targets[name])
        deploy.require(self.installer.fingerprint() == self.installer.ledger['config_sha256'],
                       'CONFIG_CHANGED_DURING_DEPLOY')
        for name in COMPONENTS:
            inspected = self.installer.inspect(name, priors[name])
            deploy.require(self.installer.container(name) == prior_containers[name], 'COMPONENT_RESTARTED')
            if name in HARNESSES:
                runtime_data[name] = inspected[0]
                deploy.require(self.wire_identity(self.installer.health(name, priors[name])) == before[name],
                               'NODE_CHANGED_DURING_DEPLOY')
        self.installer.ledger['pending'] = {
            'kind': MIGRATION, 'migration': MIGRATION, 'operation_id': operation_id,
            'phase': 'prepared', 'priors': priors, 'targets': targets,
            'prior_slots': prior_slots, 'prior_containers': prior_containers,
            'before_identities': before, 'backups': None,
        }
        deploy.atomic_json(self.installer.file, self.installer.ledger)
        replaced = False
        try:
            routes = self.installer.require_routes(HARNESSES, 'eligible')
            drained = self.installer.router.transition_many(
                'drain', self.installer.route_subset(HARNESSES, routes), operation_id)
            for name in HARNESSES:
                self.wait_quiescent(name, priors[name], before[name])
            sealed = self.installer.router.transition_many('seal', drained, operation_id)
            self.installer.ledger['pending']['phase'] = 'sealed'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            backups = self.backup_pair(priors, targets, operation_id, before, runtime_data)
            self.installer.ledger['pending'].update(phase='backups_verified', backups=backups)
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            overrides = {name: self.installer.pin_override(name, targets[name]) for name in COMPONENTS}
            for name in COMPONENTS:
                self.installer.ledger['pending']['phase'] = name + '_replace_started'
                deploy.atomic_json(self.installer.file, self.installer.ledger)
                replaced = True
                try:
                    outcome = self.installer.replace(name, targets[name], priors[name], overrides[name])
                except host.ContainerMutationUnknown:
                    self.installer.ledger['pending']['phase'] = name + '_replace_unknown'
                    deploy.atomic_json(self.installer.file, self.installer.ledger)
                    raise
                if outcome == 'desired_after_unknown':
                    self.installer.ledger['pending']['phase'] = name + '_replace_unknown'
                    deploy.atomic_json(self.installer.file, self.installer.ledger)
                    raise host.ContainerMutationUnknown('CONTAINER_MUTATION_OUTCOME_UNKNOWN')
                deploy.require(outcome == 'desired', 'CONTAINER_MUTATION_OUTCOME_UNKNOWN')
                self.installer.ledger['pending']['phase'] = name + '_replaced'
                deploy.atomic_json(self.installer.file, self.installer.ledger)
            rollbacks = self.verify_backups(priors, targets, operation_id, backups)
            observed = self.verify_runtime(targets, before, operation_id, PLAN['to_wire'],
                                           PLAN['to_database'], rollbacks)
            self.installer.ledger['pending']['phase'] = 'targets_verified'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            routes = self.installer.require_routes(HARNESSES, 'sealed', operation_id)
            sealed = self.installer.route_subset(HARNESSES, routes)
            identities = self.installer.identity_subset(
                HARNESSES, {name: self.installer.identity(observed[name]) for name in HARNESSES})
            for name in COMPONENTS:
                self.installer.ledger['components'][name] = {'current': targets[name], 'previous': priors[name]}
            self.installer.ledger['pending'].update(phase='activation_pending', sealed_routes=sealed,
                                                    target_identities=identities)
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            activated = self.installer.router.transition_many('activate', sealed, operation_id, identities)
            self.installer.verify_transition(activated)
        except Exception as error:
            if not replaced:
                try:
                    for name in COMPONENTS:
                        self.installer.pin_override(name, priors[name])
                    self.retain_priors(self.installer.ledger['pending'])
                except Exception:
                    raise deploy.DeployError('WIRE_MIGRATION_FAILED_FENCE_REMAINS') from None
                raise deploy.DeployError('WIRE_MIGRATION_FAILED_BEFORE_REPLACE') from error
            pending = self.installer.ledger['pending']
            if pending['phase'] != 'activation_pending' and not pending['phase'].endswith('_replace_unknown'):
                pending['phase'] = 'migration_attention'
                deploy.atomic_json(self.installer.file, self.installer.ledger)
            raise deploy.DeployError('WIRE_MIGRATION_BACKUP_RESTORE_REQUIRED') from error
        self.installer.ledger['pending'] = None
        deploy.atomic_json(self.installer.file, self.installer.ledger)
        return 'WIRE_MIGRATION_DEPLOYED'

    def retain_priors(self, pending):
        for name in COMPONENTS:
            self.installer.inspect(name, pending['priors'][name])
            deploy.require(self.installer.container(name) == pending['prior_containers'][name],
                           'COMPONENT_RESTARTED')
        self.installer.health('panel', pending['priors']['panel'])
        for name in HARNESSES:
            self.wait_quiescent(name, pending['priors'][name], pending['before_identities'][name])
        routes = self.installer.routing()
        current = self.installer.route_subset(HARNESSES, routes)
        deploy.require(all(state['mode'] == 'eligible' or
                       state['mode'] in ('draining', 'sealed') and
                       state.get('operationId') == pending['operation_id'] for state in current.values()),
                       'WIRE_MIGRATION_REQUIRES_OPERATOR')
        if not all(state['mode'] == 'eligible' for state in current.values()):
            reopened = self.installer.router.transition_many(
                'abort', current, pending['operation_id'],
                self.installer.identity_subset(HARNESSES, pending['before_identities']))
            self.installer.verify_transition(reopened)
        for name in COMPONENTS:
            self.installer.ledger['components'][name] = pending['prior_slots'][name]
        self.installer.ledger['pending'] = None
        deploy.atomic_json(self.installer.file, self.installer.ledger)

    def reconcile(self):
        pending = self.validate_pending()
        phase = pending['phase']
        if phase in ('prepared', 'sealed', 'backups_verified'):
            for name in COMPONENTS:
                self.installer.pin_override(name, pending['priors'][name])
            self.retain_priors(pending)
            return 'WIRE_MIGRATION_PRIORS_RETAINED'
        routes = self.installer.routing()
        current = self.installer.route_subset(HARNESSES, routes)
        if phase in ('targets_verified', 'activation_pending'):
            if phase == 'activation_pending':
                sealed, identities = pending['sealed_routes'], pending['target_identities']
            else:
                deploy.require(all(state['mode'] == 'sealed' and
                               state.get('operationId') == pending['operation_id'] for state in current.values()),
                               'WIRE_MIGRATION_REQUIRES_OPERATOR')
                rollbacks = self.verify_backups(pending['priors'], pending['targets'],
                                                pending['operation_id'], pending['backups'])
                observed = self.verify_runtime(pending['targets'], pending['before_identities'],
                                               pending['operation_id'], PLAN['to_wire'],
                                               PLAN['to_database'], rollbacks)
                sealed = current
                identities = self.installer.identity_subset(
                    HARNESSES, {name: self.installer.identity(observed[name]) for name in HARNESSES})
                for name in COMPONENTS:
                    self.installer.ledger['components'][name] = {
                        'current': pending['targets'][name], 'previous': pending['priors'][name]}
                pending.update(phase='activation_pending', sealed_routes=sealed,
                               target_identities=identities)
                deploy.atomic_json(self.installer.file, self.installer.ledger)
            expected = {node_id: host.RouterControl.projected('activate', state, pending['operation_id'],
                        identities[node_id]) for node_id, state in sealed.items()}
            eligible = all(state['mode'] == 'eligible' for state in current.values())
            deploy.require((eligible and current == expected) or current == sealed,
                           'WIRE_MIGRATION_REQUIRES_OPERATOR')
            rollbacks = self.verify_backups(pending['priors'], pending['targets'],
                                            pending['operation_id'], pending['backups'])
            observed = self.verify_runtime(pending['targets'], pending['before_identities'],
                                           pending['operation_id'], PLAN['to_wire'],
                                           PLAN['to_database'], rollbacks, sealed=not eligible)
            deploy.require(self.installer.identity_subset(
                HARNESSES, {name: self.installer.identity(observed[name]) for name in HARNESSES}) == identities,
                'WIRE_MIGRATION_REQUIRES_OPERATOR')
            if not eligible:
                activated = self.installer.router.transition_many(
                    'activate', sealed, pending['operation_id'], identities)
                self.installer.verify_transition(activated)
            self.installer.ledger['pending'] = None
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            return 'WIRE_MIGRATION_DEPLOYED_AFTER_RESTART'
        if phase in ('restore_reopen_pending', 'restore_reopened'):
            return self.finish_restore_reopen(
                pending, 'WIRE_MIGRATION_EXACT_BACKUPS_RESTORED_AFTER_RESTART')
        deploy.require(all(state['mode'] == 'sealed' and
                       state.get('operationId') == pending['operation_id'] for state in current.values()),
                       'WIRE_MIGRATION_REQUIRES_OPERATOR')
        raise deploy.DeployError('WIRE_MIGRATION_BACKUP_RESTORE_REQUIRED')

    def finish_restore_reopen(self, pending, result):
        operation_id = pending['operation_id']
        sealed = pending['restore_sealed_routes']
        identities = self.installer.identity_subset(HARNESSES, pending['before_identities'])
        expected = {
            node_id: host.RouterControl.projected('abort', route, operation_id, identities[node_id])
            for node_id, route in sealed.items()
        }
        current = self.installer.route_subset(HARNESSES, self.installer.routing())
        deploy.require(current == sealed or current == expected, 'WIRE_MIGRATION_REQUIRES_OPERATOR')
        self.verify_runtime(pending['priors'], pending['before_identities'], operation_id,
                            PLAN['from_wire'], PLAN['from_database'], sealed=current == sealed)
        if current == sealed:
            reopened = self.installer.router.transition_many('abort', sealed, operation_id, identities)
            self.installer.verify_transition(reopened)
        else:
            self.installer.verify_transition(expected)
        pending['phase'] = 'restore_reopened'
        deploy.atomic_json(self.installer.file, self.installer.ledger)
        self.installer.ledger['pending'] = None
        deploy.atomic_json(self.installer.file, self.installer.ledger)
        return result

    def restore(self, operation_id, expected_hashes):
        pending = self.validate_pending()
        deploy.require(pending['operation_id'] == operation_id and set(expected_hashes) == set(HARNESSES) and
                       all(expected_hashes[name] == pending['backups'][name]['sha256'] for name in HARNESSES) and
                       pending['phase'] not in ('prepared', 'sealed', 'backups_verified'),
                       'WIRE_MIGRATION_RESTORE_REQUEST_MISMATCH')
        rollbacks = self.verify_backups(pending['priors'], pending['targets'], operation_id, pending['backups'])
        phase = pending['phase']
        if phase in ('restore_reopen_pending', 'restore_reopened'):
            return self.finish_restore_reopen(pending, 'WIRE_MIGRATION_EXACT_BACKUPS_RESTORED')
        self.installer.require_routes(HARNESSES, 'sealed', operation_id)
        if not phase.startswith('restore_'):
            for name in COMPONENTS:
                self.installer.ledger['components'][name] = pending['prior_slots'][name]
            pending.pop('sealed_routes', None)
            pending.pop('target_identities', None)
            pending['phase'] = 'restore_stop_started'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            phase = 'restore_stop_started'
        if phase in ('restore_stop_started', 'restore_stop_unknown'):
            services = [self.installer.config['components'][name]['service'] for name in COMPONENTS]
            try:
                self.installer.compose_mutation('stop', *services)
            except host.ContainerMutationUnknown:
                pending['phase'] = 'restore_stop_unknown'
                deploy.atomic_json(self.installer.file, self.installer.ledger)
                raise deploy.DeployError('WIRE_MIGRATION_RESTORE_STOP_UNKNOWN') from None
            running = self.installer.compose('ps', '--status', 'running', '-q', *services).splitlines()
            deploy.require(running == [], 'WIRE_MIGRATION_RESTORE_STOP_UNKNOWN')
            pending['phase'] = 'restore_stopped'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            phase = 'restore_stopped'
        if phase in ('restore_stopped', 'restore_databases_started'):
            pending['phase'] = 'restore_databases_started'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            registry = self.installer.routing()['registryVersion']
            for name in HARNESSES:
                rollback = rollbacks[name]
                expected_identity = {'node_id': self.installer.config['components'][name]['node_id'],
                                     'owner_id': self.installer.config['components'][name]['actor_id'],
                                     'registry_version': registry}
                source = Path(rollback['source'])
                restore_backup(source, self.backup_root(operation_id) / name / 'harness.db',
                               expected_hashes[name], expected_identity)
            pending['phase'] = 'restore_databases_complete'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            phase = 'restore_databases_complete'
        if phase in ('restore_databases_complete', 'restore_start_started', 'restore_start_unknown'):
            override = self.combined_override(pending['priors'])
            pending['phase'] = 'restore_start_started'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            services = [self.installer.config['components'][name]['service'] for name in COMPONENTS]
            try:
                self.installer.compose_mutation('up', '-d', '--no-deps', '--pull', 'never', *services,
                                                override=override)
            except host.ContainerMutationUnknown:
                pending['phase'] = 'restore_start_unknown'
                deploy.atomic_json(self.installer.file, self.installer.ledger)
                raise deploy.DeployError('WIRE_MIGRATION_RESTORE_START_UNKNOWN') from None
            self.verify_runtime(pending['priors'], pending['before_identities'], operation_id,
                                PLAN['from_wire'], PLAN['from_database'])
            pending['phase'] = 'restore_prior_verified'
            deploy.atomic_json(self.installer.file, self.installer.ledger)
            phase = 'restore_prior_verified'
        deploy.require(phase == 'restore_prior_verified', 'WIRE_MIGRATION_RESTORE_PHASE_INVALID')
        routes = self.installer.require_routes(HARNESSES, 'sealed', operation_id)
        pending.update(phase='restore_reopen_pending',
                       restore_sealed_routes=self.installer.route_subset(HARNESSES, routes))
        deploy.atomic_json(self.installer.file, self.installer.ledger)
        return self.finish_restore_reopen(pending, 'WIRE_MIGRATION_EXACT_BACKUPS_RESTORED')


class LegacyPanelRecovery:
    """Exact rollback for the one known Panel-v2/Harness-v1 failed cutover.

    This is deliberately separate from generic component reconciliation.  It
    only moves the Panel back to the journalled prior image, proves both
    Harnesses still expose the exact v1 wire identity, and then aborts both
    Router fences in one batch.
    """

    FIELDS = {'schema', 'recovery', 'operation_id', 'phase', 'pending', 'request',
              'expected', 'target_container'}
    EXPECTED = {'target_revision', 'target_image_sha256',
                'prior_revision', 'prior_image_sha256'}
    PHASES = {'prepared', 'replace_started', 'replace_unknown', 'prior_visible',
              'prior_verified', 'reopen_pending', 'reopened', 'ledger_cleared', 'complete'}
    RESULT = 'PANEL_WIRE_MISMATCH_PRIOR_RESTORED'

    def __init__(self, installer):
        self.installer = installer
        self.file = installer.root / 'legacy-panel-wire-recovery.json'
        self.request_file = installer.root / 'request.json'

    @staticmethod
    def image_sha256(manifest):
        marker = '@sha256:'
        deploy.require(marker in manifest['image'], 'LEGACY_PANEL_RECOVERY_MANIFEST_MISMATCH')
        return manifest['image'].rsplit(marker, 1)[1]

    def validate_expected(self, expected):
        deploy.require(type(expected) is dict and set(expected) == self.EXPECTED and
                       deploy.re.fullmatch('[0-9a-f]{40}', expected['target_revision']) and
                       deploy.re.fullmatch('[0-9a-f]{64}', expected['target_image_sha256']) and
                       deploy.re.fullmatch('[0-9a-f]{40}', expected['prior_revision']) and
                       deploy.re.fullmatch('[0-9a-f]{64}', expected['prior_image_sha256']),
                       'LEGACY_PANEL_RECOVERY_EXPECTATION_INVALID')

    def recovered_request(self, record):
        return dict(record['request'], status='failure', result=self.RESULT)

    def verify_harnesses(self, pending, expected_routes):
        deploy.require(self.installer.fingerprint() == self.installer.ledger['config_sha256'],
                       'LEGACY_PANEL_RECOVERY_CONFIG_CHANGED')
        current = self.installer.route_subset(HARNESSES, self.installer.routing())
        deploy.require(current == expected_routes, 'LEGACY_PANEL_RECOVERY_ROUTER_CHANGED')
        for name in HARNESSES:
            manifest = self.installer.ledger['components'][name]['current']
            deploy.require(manifest['state_compatibility'] == PLAN['compatibility'][name]['from'] and
                           self.installer.container(name) == pending['other_containers'][name],
                           'LEGACY_PANEL_RECOVERY_HARNESS_CHANGED')
            self.installer.inspect(name, manifest, pending['other_containers'][name])
            health = self.installer.health(name, manifest)
            expected = dict(pending['before_identities'][name], **PLAN['from_wire'])
            deploy.require(Operation(self.installer).wire_identity(health) == expected and
                           health['quiescent'], 'LEGACY_PANEL_RECOVERY_REQUIRES_WIRE_V1')

    def verify_prior(self, record, expected_routes):
        pending = record['pending']
        self.installer.inspect('panel', pending['prior'])
        self.installer.health('panel', pending['prior'])
        self.verify_harnesses(pending, expected_routes)

    def verify_target(self, record):
        pending = record['pending']
        deploy.require(self.installer.container('panel') == record['target_container'],
                       'LEGACY_PANEL_RECOVERY_TARGET_CHANGED')
        self.installer.inspect('panel', pending['target'], record['target_container'])
        self.installer.health('panel', pending['target'])
        self.verify_harnesses(pending, pending['sealed_routes'])

    def validate_record(self, operation_id, expected):
        deploy.private(self.file)
        record = deploy.read_json(self.file)
        deploy.require(type(record) is dict and set(record) == self.FIELDS and record['schema'] == 1 and
                       record['recovery'] == LEGACY_PANEL_RECOVERY and
                       record['operation_id'] == operation_id and record['phase'] in self.PHASES and
                       record['expected'] == expected and
                       deploy.re.fullmatch('[0-9a-f]{12,64}', record['target_container']),
                       'LEGACY_PANEL_RECOVERY_JOURNAL_MISMATCH')
        pending = record['pending']
        request = record['request']
        deploy.require(type(pending) is dict and pending.get('operation_id') == operation_id and
                       pending.get('component') == 'panel' and pending.get('phase') == 'activation_pending' and
                       pending.get('rollback') is False and set(pending.get('affected', ())) == set(HARNESSES) and
                       type(request) is dict and request.get('status') == 'failure' and
                       request.get('component') == 'panel' and request.get('operation') == 'apply' and
                       request.get('target') == pending.get('target') and
                       operation_id == 'deploy-' + str(request.get('id')),
                       'LEGACY_PANEL_RECOVERY_JOURNAL_MISMATCH')
        prior, target = pending['prior'], pending['target']
        release.validate(prior, 'panel')
        release.validate(target, 'panel')
        deploy.require(prior['state_compatibility'] == target['state_compatibility'] ==
                       PLAN['compatibility']['panel']['from'] and target != prior and
                       target['revision'] == expected['target_revision'] and
                       self.image_sha256(target) == expected['target_image_sha256'] and
                       prior['revision'] == expected['prior_revision'] and
                       self.image_sha256(prior) == expected['prior_image_sha256'],
                       'LEGACY_PANEL_RECOVERY_MANIFEST_MISMATCH')
        recovered = self.recovered_request(record)
        deploy.private(self.request_file)
        observed_request = deploy.read_json(self.request_file)
        deploy.require(observed_request in (request, recovered) and
                       (record['phase'] != 'complete' or observed_request == recovered),
                       'LEGACY_PANEL_RECOVERY_REQUEST_CHANGED')
        slot = self.installer.ledger['components']['panel']
        if record['phase'] in ('prepared', 'replace_started', 'replace_unknown', 'prior_visible',
                               'prior_verified', 'reopen_pending'):
            deploy.require(self.installer.validate_pending() == pending and
                           slot == {'current': target, 'previous': prior},
                           'LEGACY_PANEL_RECOVERY_LEDGER_CHANGED')
        elif record['phase'] == 'reopened':
            deploy.require((self.installer.ledger['pending'] == pending and
                            slot == {'current': target, 'previous': prior}) or
                           (self.installer.ledger['pending'] is None and slot == pending['prior_slot']),
                           'LEGACY_PANEL_RECOVERY_LEDGER_CHANGED')
        else:
            deploy.require(self.installer.ledger['pending'] is None and slot == pending['prior_slot'],
                           'LEGACY_PANEL_RECOVERY_LEDGER_CHANGED')
        return record

    def prepare(self, operation_id, expected):
        if self.file.exists() or self.file.is_symlink():
            return self.validate_record(operation_id, expected)
        pending = self.installer.validate_pending()
        deploy.require(pending['component'] == 'panel' and pending['phase'] == 'activation_pending' and
                       pending['rollback'] is False and pending['operation_id'] == operation_id and
                       set(pending['affected']) == set(HARNESSES) and
                       set(pending['other_containers']) == set(HARNESSES),
                       'LEGACY_PANEL_RECOVERY_PENDING_MISMATCH')
        deploy.private(self.request_file)
        request = deploy.read_json(self.request_file)
        deploy.require(request.get('status') == 'failure' and request.get('component') == 'panel' and
                       request.get('operation') == 'apply' and request.get('target') == pending['target'] and
                       operation_id == 'deploy-' + str(request.get('id')),
                       'LEGACY_PANEL_RECOVERY_REQUEST_MISMATCH')
        prior, target = pending['prior'], pending['target']
        deploy.require(prior['state_compatibility'] == target['state_compatibility'] ==
                       PLAN['compatibility']['panel']['from'] and target != prior and
                       target['revision'] == expected['target_revision'] and
                       self.image_sha256(target) == expected['target_image_sha256'] and
                       prior['revision'] == expected['prior_revision'] and
                       self.image_sha256(prior) == expected['prior_image_sha256'],
                       'LEGACY_PANEL_RECOVERY_MANIFEST_MISMATCH')
        sealed = pending['sealed_routes']
        deploy.require(self.installer.route_subset(HARNESSES, self.installer.routing()) == sealed and
                       pending['target_identities'] ==
                       self.installer.identity_subset(HARNESSES, pending['before_identities']),
                       'LEGACY_PANEL_RECOVERY_REQUIRES_EXACT_SEAL')
        target_container = self.installer.container('panel')
        self.installer.inspect('panel', target, target_container)
        self.installer.health('panel', target)
        self.verify_harnesses(pending, sealed)
        record = {'schema': 1, 'recovery': LEGACY_PANEL_RECOVERY, 'operation_id': operation_id,
                  'phase': 'prepared', 'pending': copy.deepcopy(pending),
                  'request': copy.deepcopy(request), 'expected': dict(expected),
                  'target_container': target_container}
        deploy.atomic_json(self.file, record)
        return self.validate_record(operation_id, expected)

    def finish_reopen(self, record):
        pending = record['pending']
        sealed = pending['sealed_routes']
        identities = self.installer.identity_subset(HARNESSES, pending['before_identities'])
        eligible = {node_id: host.RouterControl.projected('abort', state, record['operation_id'],
                    identities[node_id]) for node_id, state in sealed.items()}
        current = self.installer.route_subset(HARNESSES, self.installer.routing())
        deploy.require(current == sealed or current == eligible,
                       'LEGACY_PANEL_RECOVERY_ROUTER_CHANGED')
        self.verify_prior(record, current)
        if current == sealed:
            reopened = self.installer.router.transition_many('abort', sealed, record['operation_id'], identities)
            self.installer.verify_transition(reopened)
        else:
            self.installer.verify_transition(eligible)
        record['phase'] = 'reopened'
        deploy.atomic_json(self.file, record)
        slot = self.installer.ledger['components']['panel']
        if self.installer.ledger['pending'] is not None:
            deploy.require(self.installer.ledger['pending'] == pending and
                           slot == {'current': pending['target'], 'previous': pending['prior']},
                           'LEGACY_PANEL_RECOVERY_LEDGER_CHANGED')
            self.installer.ledger['components']['panel'] = pending['prior_slot']
            self.installer.ledger['pending'] = None
            deploy.atomic_json(self.installer.file, self.installer.ledger)
        else:
            deploy.require(slot == pending['prior_slot'], 'LEGACY_PANEL_RECOVERY_LEDGER_CHANGED')
        record['phase'] = 'ledger_cleared'
        deploy.atomic_json(self.file, record)
        deploy.atomic_json(self.request_file, self.recovered_request(record))
        record['phase'] = 'complete'
        deploy.atomic_json(self.file, record)
        return self.RESULT

    def run(self, operation_id, expected):
        deploy.require(deploy.re.fullmatch(r'deploy-[1-9][0-9]{0,19}', operation_id),
                       'LEGACY_PANEL_RECOVERY_OPERATION_INVALID')
        self.validate_expected(expected)
        record = self.prepare(operation_id, expected)
        phase = record['phase']
        pending = record['pending']
        if phase == 'complete':
            eligible = {node_id: host.RouterControl.projected(
                        'abort', state, operation_id,
                        self.installer.identity_subset(HARNESSES, pending['before_identities'])[node_id])
                        for node_id, state in pending['sealed_routes'].items()}
            self.verify_prior(record, eligible)
            return self.RESULT
        if phase in ('prepared', 'replace_started'):
            if phase == 'prepared':
                self.verify_target(record)
            if phase == 'replace_started':
                outcome = self.installer.wait_runtime_identity('panel', pending['prior'], pending['target'])
                if outcome == 'desired':
                    record['phase'] = 'prior_visible'
                    deploy.atomic_json(self.file, record)
                else:
                    deploy.require(outcome == 'alternate', 'LEGACY_PANEL_RECOVERY_REPLACE_UNKNOWN')
                    self.verify_target(record)
            if record['phase'] != 'prior_visible':
                record['phase'] = 'replace_started'
                deploy.atomic_json(self.file, record)
                override = self.installer.pin_override('panel', pending['prior'])
                try:
                    outcome = self.installer.replace('panel', pending['prior'], pending['target'], override)
                except host.ContainerMutationUnknown:
                    record['phase'] = 'replace_unknown'
                    deploy.atomic_json(self.file, record)
                    raise deploy.DeployError('LEGACY_PANEL_RECOVERY_REPLACE_UNKNOWN') from None
                deploy.require(outcome in ('desired', 'desired_after_unknown'),
                               'LEGACY_PANEL_RECOVERY_REPLACE_UNKNOWN')
                record['phase'] = 'prior_visible'
                deploy.atomic_json(self.file, record)
            phase = record['phase']
        if phase == 'replace_unknown':
            outcome = self.installer.wait_runtime_identity('panel', pending['prior'], pending['target'])
            deploy.require(outcome == 'desired', 'LEGACY_PANEL_RECOVERY_REPLACE_UNKNOWN')
            record['phase'] = 'prior_visible'
            deploy.atomic_json(self.file, record)
            phase = 'prior_visible'
        if phase == 'prior_visible':
            self.verify_prior(record, pending['sealed_routes'])
            record['phase'] = 'prior_verified'
            deploy.atomic_json(self.file, record)
            phase = 'prior_verified'
        if phase == 'prior_verified':
            self.verify_prior(record, pending['sealed_routes'])
            record['phase'] = 'reopen_pending'
            deploy.atomic_json(self.file, record)
            phase = 'reopen_pending'
        if phase in ('reopen_pending', 'reopened'):
            return self.finish_reopen(record)
        if phase == 'ledger_cleared':
            eligible = {node_id: host.RouterControl.projected(
                        'abort', state, operation_id,
                        self.installer.identity_subset(HARNESSES, pending['before_identities'])[node_id])
                        for node_id, state in pending['sealed_routes'].items()}
            self.verify_prior(record, eligible)
            deploy.atomic_json(self.request_file, self.recovered_request(record))
            record['phase'] = 'complete'
            deploy.atomic_json(self.file, record)
            return self.RESULT
        raise deploy.DeployError('LEGACY_PANEL_RECOVERY_PHASE_INVALID')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('restore',))
    parser.add_argument('--operation-id', required=True)
    parser.add_argument('--cursor-backup-sha256', required=True)
    parser.add_argument('--codex-backup-sha256', required=True)
    args = parser.parse_args()
    deploy.require(os.geteuid() == 0, 'WIRE_MIGRATION_RESTORE_REQUIRES_ROOT')
    hashes = {'cursor': args.cursor_backup_sha256, 'codex': args.codex_backup_sha256}
    deploy.require(all(deploy.re.fullmatch('[0-9a-f]{64}', value) for value in hashes.values()),
                   'INVALID_MIGRATION_BACKUP_HASH')
    with deploy.locked(host.ROOT):
        result = Operation(host.Installer()).restore(args.operation_id, hashes)
    print(result)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, deploy.DeployError) else 'WIRE_MIGRATION_RESTORE_FAILED')
        raise SystemExit(1) from None
