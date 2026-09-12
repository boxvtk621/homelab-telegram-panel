#!/usr/bin/env python3
"""Operator-only atomic update of the reviewed VM115 component executor."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import tempfile

import component_deploy as host
import deploy
import wire_migration as wire


FILES = ('component_deploy.py', 'component_cd.py', 'wire_migration.py', 'component_release.py')
SERVICE = 'homelab-components-cd'


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def read_source(directory, name, expected):
    descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=directory)
    try:
        before = os.fstat(descriptor)
        deploy.require(stat.S_ISREG(before.st_mode) and 0 < before.st_size <= 131072,
                       'EXECUTOR_SOURCE_MISMATCH')
        content = bytearray()
        while len(content) <= 131072:
            block = os.read(descriptor, min(65536, 131073 - len(content)))
            if not block:
                break
            content.extend(block)
        after = os.fstat(descriptor)
        deploy.require(len(content) == before.st_size and
                       (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) ==
                       (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) and
                       hashlib.sha256(content).hexdigest() == expected,
                       'EXECUTOR_SOURCE_MISMATCH')
        return bytes(content)
    finally:
        os.close(descriptor)


def run(*args):
    subprocess.run(args, check=True, timeout=30)


def atomic_file(path, content):
    descriptor, temporary = tempfile.mkstemp(prefix='.executor-update-', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'wb') as output:
            output.write(content)
            output.flush()
            os.fchmod(output.fileno(), 0o600)
            os.fchown(output.fileno(), 0, 0)
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True, type=Path)
    parser.add_argument('--expected-updater-sha256', required=True)
    parser.add_argument('--recover-operation-id', default='')
    parser.add_argument('--expected-pending-target-revision', default='')
    parser.add_argument('--expected-pending-target-image-sha256', default='')
    parser.add_argument('--expected-pending-prior-revision', default='')
    parser.add_argument('--expected-pending-prior-image-sha256', default='')
    for name in FILES:
        parser.add_argument('--expected-' + name.replace('_', '-').replace('.py', '') + '-sha256', required=True)
    args = parser.parse_args()
    deploy.require(os.geteuid() == 0 and socket.gethostname() == 'alpine-docker', 'WRONG_TARGET')
    deploy.require(digest(Path(__file__).resolve(strict=True)) == args.expected_updater_sha256,
                   'EXECUTOR_UPDATER_MISMATCH')
    recovery_expected = {
        'target_revision': args.expected_pending_target_revision,
        'target_image_sha256': args.expected_pending_target_image_sha256,
        'prior_revision': args.expected_pending_prior_revision,
        'prior_image_sha256': args.expected_pending_prior_image_sha256,
    }
    recovery_values = [args.recover_operation_id, *recovery_expected.values()]
    recovery_requested = all(recovery_values)
    deploy.require(recovery_requested or not any(recovery_values),
                   'EXECUTOR_UPDATE_RECOVERY_ARGUMENTS_INCOMPLETE')
    source = args.source
    deploy.require(source.is_absolute() and source == source.resolve(strict=True) and source.is_dir() and
                   not source.is_symlink(), 'INVALID_EXECUTOR_SOURCE')
    expected = {
        name: getattr(args, 'expected_' + name.replace('.py', '') + '_sha256') for name in FILES
    }
    content = {}
    source_descriptor = os.open(source, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for name in FILES:
            content[name] = read_source(source_descriptor, name, expected[name])
    finally:
        os.close(source_descriptor)
    executor = host.ROOT / 'executor'
    deploy.private(executor, directory=True)
    with deploy.locked(host.ROOT):
        installer = host.Installer()
        ledger = installer.ledger
        request_file = host.ROOT / 'request.json'
        request = deploy.read_json(request_file) if request_file.exists() else {'status': 'idle'}
        pending = ledger.get('pending')
        if not recovery_requested:
            deploy.require(pending is None, 'EXECUTOR_UPDATE_REQUIRES_IDLE_LEDGER')
            deploy.require(request.get('status') != 'running', 'EXECUTOR_UPDATE_REQUIRES_IDLE_LEDGER')
        before = {}
        for name in FILES:
            target = executor / name
            if target.exists():
                deploy.private(target)
                before[name] = target.read_bytes()
            else:
                deploy.require(name == 'wire_migration.py' and not target.is_symlink(),
                               'EXECUTOR_FILE_MISSING')
                before[name] = None
        stopped = False
        started_new = False
        try:
            run('rc-service', SERVICE, 'stop')
            stopped = True
            if recovery_requested:
                result = wire.LegacyPanelRecovery(installer).run(
                    args.recover_operation_id, recovery_expected)
                recovered = deploy.read_json(request_file)
                deploy.require(result == wire.LegacyPanelRecovery.RESULT and
                               installer.ledger['pending'] is None and
                               recovered.get('status') == 'failure' and recovered.get('result') == result,
                               'EXECUTOR_UPDATE_RECOVERY_INCOMPLETE')
            for name in FILES:
                atomic_file(executor / name, content[name])
            deploy.require(all(digest(executor / name) == expected[name] for name in FILES),
                           'EXECUTOR_UPDATE_READBACK_MISMATCH')
            environment = dict(os.environ, PYTHONDONTWRITEBYTECODE='1')
            subprocess.run(['/usr/bin/python3', '-B', '-c',
                            'import component_cd,component_deploy,component_release,wire_migration; '
                            'print(wire_migration.MIGRATION,component_release.compatibility("panel"))'],
                           cwd=executor, env=environment, check=True, timeout=30)
            run('rc-service', SERVICE, 'start')
            started_new = True
            run('rc-service', SERVICE, 'status')
        except Exception:
            if stopped:
                if started_new:
                    run('rc-service', SERVICE, 'stop')
                for name in FILES:
                    target = executor / name
                    if before[name] is None:
                        if target.exists() and not target.is_symlink():
                            target.unlink()
                    else:
                        atomic_file(target, before[name])
                run('rc-service', SERVICE, 'start')
                run('rc-service', SERVICE, 'status')
            raise
        deploy.require(all(digest(executor / name) == expected[name] for name in FILES),
                       'EXECUTOR_UPDATE_READBACK_MISMATCH')
    print(json.dumps({'status': 'COMPONENT_EXECUTOR_UPDATED', 'sha256': expected}, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, deploy.DeployError) else 'COMPONENT_EXECUTOR_UPDATE_FAILED')
        raise SystemExit(1) from None
