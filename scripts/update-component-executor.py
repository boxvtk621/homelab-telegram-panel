#!/usr/bin/env python3
"""Crash-safe operator update of the reviewed VM115 component executor."""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import stat
import subprocess
import sys
import tempfile
import types


ROOT = Path('/opt/homelab-agents-cd')
EXECUTOR = ROOT / 'executor'
JOURNAL = ROOT / 'executor-update.json'
LOCK = ROOT / 'deploy.lock'
SERVICE = 'homelab-components-cd'
INIT = Path('/etc/init.d') / SERVICE
RUNLEVEL_LINK = Path('/etc/runlevels/default') / SERVICE
FILES = ('deploy.py', 'cd.py', 'component_release.py', 'component_deploy.py',
         'wire_migration.py', 'component_cd.py')
MODULE_ORDER = ('deploy', 'component_release', 'cd', 'component_deploy',
                'wire_migration', 'component_cd')
UPDATE = 'component-executor-v2'
PHASES = {'prepared', 'default_disabled', 'service_stopped', 'recovery_complete',
          'installing', 'replace_pending', 'files_installed', 'imports_verified',
          'default_enabled', 'complete'}
TRUSTED_UID = os.getuid()
TRUSTED_GID = os.getgid()


class UpdateError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise UpdateError(code)


def pairs(items):
    value = {}
    for key, item in items:
        require(key not in value, 'DUPLICATE_JSON_KEY')
        value[key] = item
    return value


def digest_bytes(content):
    return hashlib.sha256(content).hexdigest()


def read_bytes(path, maximum=131072, owner=TRUSTED_UID, mode=0o600):
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except OSError:
        raise UpdateError('UNSAFE_EXECUTOR_FILE') from None
    try:
        before = os.fstat(descriptor)
        require(stat.S_ISREG(before.st_mode) and before.st_uid == owner and
                stat.S_IMODE(before.st_mode) == mode and 0 < before.st_size <= maximum,
                'UNSAFE_EXECUTOR_FILE')
        content = bytearray()
        while len(content) <= maximum:
            block = os.read(descriptor, min(65536, maximum + 1 - len(content)))
            if not block:
                break
            content.extend(block)
        after = os.fstat(descriptor)
        require(len(content) == before.st_size and
                (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) ==
                (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns),
                'EXECUTOR_FILE_CHANGED')
        return bytes(content)
    finally:
        os.close(descriptor)


def digest(path):
    return digest_bytes(read_bytes(path))


def private_directory(path):
    try:
        info = os.lstat(path)
    except OSError:
        raise UpdateError('UNSAFE_EXECUTOR_DIRECTORY') from None
    require(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode) and
            info.st_uid == TRUSTED_UID and stat.S_IMODE(info.st_mode) == 0o700,
            'UNSAFE_EXECUTOR_DIRECTORY')


def read_source(directory, name, expected):
    try:
        descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=directory)
    except OSError:
        raise UpdateError('EXECUTOR_SOURCE_MISMATCH') from None
    try:
        before = os.fstat(descriptor)
        require(stat.S_ISREG(before.st_mode) and before.st_uid == TRUSTED_UID and
                stat.S_IMODE(before.st_mode) == 0o600 and 0 < before.st_size <= 131072,
                'EXECUTOR_SOURCE_MISMATCH')
        content = bytearray()
        while len(content) <= 131072:
            block = os.read(descriptor, min(65536, 131073 - len(content)))
            if not block:
                break
            content.extend(block)
        after = os.fstat(descriptor)
        require(len(content) == before.st_size and
                (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) ==
                (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) and
                digest_bytes(content) == expected,
                'EXECUTOR_SOURCE_MISMATCH')
        return bytes(content)
    finally:
        os.close(descriptor)


def read_json(path):
    content = read_bytes(path, maximum=262144)
    try:
        return json.loads(content, object_pairs_hook=pairs)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise UpdateError('INVALID_EXECUTOR_UPDATE_JOURNAL') from None


def atomic_file(path, content):
    descriptor, temporary = tempfile.mkstemp(prefix='.executor-update-', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'wb') as output:
            output.write(content)
            output.flush()
            os.fchmod(output.fileno(), 0o600)
            os.fchown(output.fileno(), TRUSTED_UID, TRUSTED_GID)
            os.fsync(output.fileno())
        os.replace(temporary, path)
        temporary = None
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if temporary is not None:
            os.unlink(temporary)


def atomic_json(path, value):
    atomic_file(path, json.dumps(value, separators=(',', ':'), sort_keys=True).encode())


@contextlib.contextmanager
def locked():
    private_directory(ROOT)
    descriptor = os.open(LOCK, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == TRUSTED_UID and
                stat.S_IMODE(info.st_mode) == 0o600, 'UNSAFE_EXECUTOR_LOCK')
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise UpdateError('DEPLOYMENT_LOCKED') from None
        yield
    finally:
        os.close(descriptor)


def load_verified_modules(content, source):
    """Execute only the bytes already checked against the operator allowlist."""
    loaded = {}
    previous = {name: sys.modules.get(name) for name in MODULE_ORDER}
    try:
        for name in MODULE_ORDER:
            filename = name + '.py'
            module = types.ModuleType(name)
            module.__file__ = str(source / filename)
            module.__package__ = ''
            sys.modules[name] = module
            exec(compile(content[filename], module.__file__, 'exec'), module.__dict__)
            loaded[name] = module
    except Exception:
        raise UpdateError('VERIFIED_EXECUTOR_IMPORT_FAILED') from None
    finally:
        for name, module in previous.items():
            if module is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = module
    return types.SimpleNamespace(deploy=loaded['deploy'], cd=loaded['cd'],
                                 release=loaded['component_release'], host=loaded['component_deploy'],
                                 wire=loaded['wire_migration'], consumer=loaded['component_cd'])


def verify_sources(source, expected):
    require(source.is_absolute() and source == source.resolve(strict=True) and not source.is_symlink(),
            'INVALID_EXECUTOR_SOURCE')
    private_directory(source)
    require(type(expected) is dict and set(expected) == set(FILES) and
            all(re.fullmatch('[0-9a-f]{64}', value) for value in expected.values()),
            'INVALID_EXECUTOR_SOURCE_HASH')
    content = {}
    descriptor = os.open(source, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == TRUSTED_UID and
                stat.S_IMODE(info.st_mode) == 0o700, 'INVALID_EXECUTOR_SOURCE')
        for name in FILES:
            content[name] = read_source(descriptor, name, expected[name])
    finally:
        os.close(descriptor)
    return content


def verify_updater(source, expected):
    require(source.is_absolute() and source == source.resolve(strict=True) and
            not source.is_symlink(), 'INVALID_EXECUTOR_SOURCE')
    private_directory(source)
    self_path = Path(__file__)
    require(self_path.is_absolute() and self_path.parent == source and
            digest(self_path) == expected, 'EXECUTOR_UPDATER_MISMATCH')


def run_checked(*args):
    subprocess.run(args, check=True, timeout=30,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


class OpenRC:
    def enabled(self):
        try:
            info = os.lstat(RUNLEVEL_LINK)
        except FileNotFoundError:
            return False
        except OSError:
            raise UpdateError('OPENRC_DEFAULT_READBACK_FAILED') from None
        require(stat.S_ISLNK(info.st_mode) and info.st_uid == TRUSTED_UID and
                RUNLEVEL_LINK.resolve(strict=True) == INIT,
                'OPENRC_DEFAULT_READBACK_FAILED')
        return True

    def disable(self):
        if self.enabled():
            run_checked('rc-update', 'del', SERVICE, 'default')
        require(not self.enabled(), 'OPENRC_DEFAULT_DISABLE_FAILED')

    def enable(self):
        if not self.enabled():
            run_checked('rc-update', 'add', SERVICE, 'default')
        require(self.enabled(), 'OPENRC_DEFAULT_ENABLE_FAILED')

    def stop(self):
        run_checked('rc-service', SERVICE, 'stop')

    def start(self):
        run_checked('rc-service', SERVICE, 'start')

    def status(self):
        run_checked('rc-service', SERVICE, 'status')


def file_hash(path):
    try:
        os.lstat(path)
    except FileNotFoundError:
        return None
    except OSError:
        raise UpdateError('UNSAFE_EXECUTOR_FILE') from None
    return digest(path)


class Transaction:
    FIELDS = {'schema', 'operation', 'phase', 'index', 'targets', 'priors', 'recovery'}

    def __init__(self, content, expected, modules, service, recovery):
        self.content = content
        self.expected = expected
        self.modules = modules
        self.service = service
        self.recovery = recovery

    def current_hashes(self):
        return {name: file_hash(EXECUTOR / name) for name in FILES}

    def validate_files(self, record):
        current = self.current_hashes()
        phase, index = record['phase'], record['index']
        if phase in ('prepared', 'default_disabled', 'service_stopped', 'recovery_complete'):
            require(current == record['priors'], 'EXECUTOR_UPDATE_STATE_MISMATCH')
            return
        if phase in ('files_installed', 'imports_verified', 'default_enabled', 'complete'):
            require(current == record['targets'], 'EXECUTOR_UPDATE_STATE_MISMATCH')
            return
        require(phase in ('installing', 'replace_pending') and 0 <= index < len(FILES),
                'INVALID_EXECUTOR_UPDATE_JOURNAL')
        for position, name in enumerate(FILES):
            allowed = {record['targets'][name]} if position < index else {record['priors'][name]}
            if phase == 'replace_pending' and position == index:
                allowed.add(record['targets'][name])
            require(current[name] in allowed, 'EXECUTOR_UPDATE_STATE_MISMATCH')

    def validate_record_shape(self, record):
        require(type(record) is dict and set(record) == self.FIELDS and record['schema'] == 1 and
                record['operation'] == UPDATE and record['phase'] in PHASES and
                type(record['index']) is int and type(record['targets']) is dict and
                set(record['targets']) == set(FILES) and
                all(re.fullmatch('[0-9a-f]{64}', value) for value in record['targets'].values()) and
                record['recovery'] == self.recovery and type(record['priors']) is dict and
                set(record['priors']) == set(FILES) and
                all(value is None or re.fullmatch('[0-9a-f]{64}', value)
                    for value in record['priors'].values()),
                'INVALID_EXECUTOR_UPDATE_JOURNAL')
        if record['phase'] not in ('installing', 'replace_pending'):
            require(record['index'] == 0, 'INVALID_EXECUTOR_UPDATE_JOURNAL')
        return record

    def validate_record(self, record):
        self.validate_record_shape(record)
        require(record['targets'] == self.expected, 'INVALID_EXECUTOR_UPDATE_JOURNAL')
        self.validate_files(record)
        enabled = self.service.enabled()
        if record['phase'] in ('prepared', 'imports_verified'):
            return record
        require(enabled == (record['phase'] in ('default_enabled', 'complete')),
                'EXECUTOR_UPDATE_OPENRC_MISMATCH')
        return record

    def retarget(self, record):
        self.validate_record_shape(record)
        require(record['phase'] == 'service_stopped' and record['index'] == 0,
                'EXECUTOR_UPDATE_RETARGET_NOT_ALLOWED')
        self.validate_files(record)
        require(not self.service.enabled(), 'EXECUTOR_UPDATE_OPENRC_MISMATCH')
        require(type(self.content) is dict and set(self.content) == set(FILES) and
                type(self.expected) is dict and set(self.expected) == set(FILES) and
                all(type(self.content[name]) is bytes and
                    digest_bytes(self.content[name]) == self.expected[name]
                    for name in FILES), 'EXECUTOR_UPDATE_SOURCE_MISMATCH')
        replacement = dict(record, targets=dict(self.expected))
        atomic_json(JOURNAL, replacement)
        return self.validate_record(read_json(JOURNAL))

    def prepare(self):
        if JOURNAL.exists() or JOURNAL.is_symlink():
            record = read_json(JOURNAL)
            self.validate_record_shape(record)
            if record['targets'] != self.expected:
                return self.retarget(record)
            return self.validate_record(record)
        require(self.service.enabled(), 'EXECUTOR_UPDATE_REQUIRES_DEFAULT_SERVICE')
        priors = self.current_hashes()
        require(all(priors[name] is not None or name == 'wire_migration.py' for name in FILES),
                'EXECUTOR_FILE_MISSING')
        record = {'schema': 1, 'operation': UPDATE, 'phase': 'prepared', 'index': 0,
                  'targets': self.expected, 'priors': priors, 'recovery': self.recovery}
        atomic_json(JOURNAL, record)
        return self.validate_record(record)

    def persist(self, record, phase, index=0):
        record.update(phase=phase, index=index)
        atomic_json(JOURNAL, record)

    def install(self, record):
        if record['phase'] == 'recovery_complete':
            self.persist(record, 'installing', 0)
        while record['phase'] in ('installing', 'replace_pending'):
            index = record['index']
            name = FILES[index]
            if record['phase'] == 'installing':
                self.persist(record, 'replace_pending', index)
            atomic_file(EXECUTOR / name, self.content[name])
            require(file_hash(EXECUTOR / name) == self.expected[name],
                    'EXECUTOR_UPDATE_READBACK_MISMATCH')
            if index + 1 == len(FILES):
                self.persist(record, 'files_installed')
            else:
                self.persist(record, 'installing', index + 1)

    def run(self, installer):
        record = self.prepare()
        if record['phase'] == 'complete':
            self.service.status()
            return 'COMPONENT_EXECUTOR_UPDATED'
        if record['phase'] == 'prepared':
            self.service.disable()
            self.persist(record, 'default_disabled')
        if record['phase'] == 'default_disabled':
            self.service.stop()
            self.persist(record, 'service_stopped')
        if record['phase'] in ('service_stopped', 'recovery_complete'):
            if self.recovery is not None:
                result = self.modules.wire.LegacyPanelRecovery(installer).run(
                    self.recovery['operation_id'], self.recovery['expected'])
                request = self.modules.deploy.read_json(ROOT / 'request.json')
                require(result == self.modules.wire.LegacyPanelRecovery.RESULT and
                        installer.ledger['pending'] is None and request.get('status') == 'failure' and
                        request.get('result') == result, 'EXECUTOR_UPDATE_RECOVERY_INCOMPLETE')
            else:
                request_file = ROOT / 'request.json'
                request = (self.modules.deploy.read_json(request_file) if request_file.exists()
                           else {'status': 'idle'})
                require(installer.ledger.get('pending') is None and request.get('status') != 'running',
                        'EXECUTOR_UPDATE_REQUIRES_IDLE_LEDGER')
            if record['phase'] == 'service_stopped':
                self.persist(record, 'recovery_complete')
        self.install(record)
        if record['phase'] == 'files_installed':
            require(self.current_hashes() == self.expected, 'EXECUTOR_UPDATE_READBACK_MISMATCH')
            import_installed()
            self.persist(record, 'imports_verified')
        if record['phase'] == 'imports_verified':
            self.service.enable()
            self.persist(record, 'default_enabled')
        if record['phase'] == 'default_enabled':
            self.service.start()
            self.service.status()
            self.persist(record, 'complete')
        require(record['phase'] == 'complete' and self.current_hashes() == self.expected and
                self.service.enabled(), 'EXECUTOR_UPDATE_INCOMPLETE')
        return 'COMPONENT_EXECUTOR_UPDATED'


def import_installed():
    environment = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'PYTHONDONTWRITEBYTECODE': '1'}
    code = ('import sys;sys.path.insert(0,sys.argv[1]);'
            'import deploy,cd,component_release,component_deploy,wire_migration,component_cd')
    subprocess.run(['/usr/bin/python3', '-I', '-B', '-c', code, str(EXECUTOR)], cwd='/',
                   env=environment, check=True, timeout=30,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True, type=Path)
    parser.add_argument('--expected-updater-sha256', required=True)
    parser.add_argument('--recover-operation-id', default='')
    parser.add_argument('--expected-pending-target-revision', default='')
    parser.add_argument('--expected-pending-target-image-sha256', default='')
    parser.add_argument('--expected-pending-prior-revision', default='')
    parser.add_argument('--expected-pending-prior-image-sha256', default='')
    for name in FILES:
        parser.add_argument('--expected-' + name.replace('_', '-').replace('.py', '') + '-sha256',
                            required=True)
    return parser.parse_args()


def main():
    args = parse_args()
    require(os.geteuid() == 0 and socket.gethostname() == 'alpine-docker',
            'WRONG_TARGET')
    source = args.source
    verify_updater(source, args.expected_updater_sha256)
    recovery_expected = {
        'target_revision': args.expected_pending_target_revision,
        'target_image_sha256': args.expected_pending_target_image_sha256,
        'prior_revision': args.expected_pending_prior_revision,
        'prior_image_sha256': args.expected_pending_prior_image_sha256,
    }
    recovery_values = [args.recover_operation_id, *recovery_expected.values()]
    recovery_requested = all(recovery_values)
    require(recovery_requested or not any(recovery_values),
            'EXECUTOR_UPDATE_RECOVERY_ARGUMENTS_INCOMPLETE')
    recovery = ({'operation_id': args.recover_operation_id, 'expected': recovery_expected}
                if recovery_requested else None)
    expected = {name: getattr(args, 'expected_' + name.replace('.py', '') + '_sha256')
                for name in FILES}
    content = verify_sources(source, expected)
    modules = load_verified_modules(content, source)
    require(modules.host.ROOT == ROOT, 'VERIFIED_EXECUTOR_ROOT_MISMATCH')
    private_directory(EXECUTOR)
    with locked():
        installer = modules.host.Installer()
        result = Transaction(content, expected, modules, OpenRC(), recovery).run(installer)
    print(json.dumps({'status': result, 'sha256': expected}, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        message = str(error)
        if not (isinstance(error, UpdateError) or
                (error.__class__.__name__ == 'DeployError' and re.fullmatch('[A-Z0-9_]+', message))):
            message = 'COMPONENT_EXECUTOR_UPDATE_FAILED'
        print(message)
        raise SystemExit(1) from None
