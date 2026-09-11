#!/usr/bin/env python3
"""Install or recover an already prepared offline registry/router-state pair."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import tempfile

import registry_transition as transition


MAX_FILE = 256 << 10
JOURNAL_FIELDS = {
    "schema", "operation", "bundlePath", "transitionSHA256", "source", "target",
    "direction", "phase",
}
PHASES = {"prepared", "registry-replaced", "state-replaced", "completed"}


class InstallError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise InstallError(code)


def require_root():
    require(os.geteuid() == 0, "ROOT_REQUIRED")


def canonical_existing(path, code):
    path = Path(path)
    require(path.is_absolute(), code)
    try:
        require(path.resolve(strict=True) == path, code)
    except (OSError, RuntimeError):
        raise InstallError(code) from None
    return path


def private_file_info(path, owner=None):
    path = canonical_existing(path, "INVALID_PRIVATE_FILE")
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        info = os.fstat(descriptor)
    except OSError:
        raise InstallError("INVALID_PRIVATE_FILE") from None
    try:
        require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and
                (owner is None or info.st_uid == owner), "INVALID_PRIVATE_FILE")
        return info
    finally:
        os.close(descriptor)


def read_private(path, owner, maximum=MAX_FILE):
    path = canonical_existing(path, "INVALID_PRIVATE_FILE")
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except OSError:
        raise InstallError("INVALID_PRIVATE_FILE") from None
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and
                info.st_uid == owner and info.st_size <= maximum, "INVALID_PRIVATE_FILE")
        content = os.read(descriptor, maximum + 1)
        require(len(content) <= maximum, "INVALID_PRIVATE_FILE")
        return content
    finally:
        os.close(descriptor)


def private_directory(path, owner, exact_names=None):
    path = canonical_existing(path, "INVALID_PRIVATE_DIRECTORY")
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) and not path.is_symlink() and
            stat.S_IMODE(info.st_mode) == 0o700 and info.st_uid == owner,
            "INVALID_PRIVATE_DIRECTORY")
    if exact_names is not None:
        require({entry.name for entry in os.scandir(path)} == set(exact_names),
                "INVALID_PRIVATE_DIRECTORY")
    return path


def private_missing_path(path, owner):
    path = Path(path)
    require(path.is_absolute() and path != Path(path.anchor) and not os.path.lexists(path),
            "NEW_PRIVATE_PATH_REQUIRED")
    private_directory(path.parent, owner)
    return path


def decode_json(raw, code):
    try:
        return json.loads(raw, object_pairs_hook=transition.pairs)
    except Exception:
        raise InstallError(code) from None


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def validate_transition_metadata(metadata, raw, source, target, source_manifest_sha,
                                 target_manifest_sha):
    require(type(metadata) is dict and set(metadata) == {
        "schema", "operation", "ownerId", "registryVersion", "cursorNodeId", "codexNodeId",
        "source", "next",
    }, "INVALID_TRANSITION_METADATA")
    require(metadata["schema"] == 1 and metadata["operation"] == "add-codex-node",
            "INVALID_TRANSITION_METADATA")
    require(type(metadata["source"]) is dict and set(metadata["source"]) == {
        "registrySHA256", "routerStateSHA256", "manifestSHA256",
    } and type(metadata["next"]) is dict and set(metadata["next"]) == {
        "registrySHA256", "routerStateSHA256", "manifestSHA256",
    }, "INVALID_TRANSITION_METADATA")
    expected_source = {
        "registrySHA256": sha256(raw["source_registry"]),
        "routerStateSHA256": sha256(raw["source_state"]),
        "manifestSHA256": source_manifest_sha,
    }
    expected_target = {
        "registrySHA256": sha256(raw["target_registry"]),
        "routerStateSHA256": sha256(raw["target_state"]),
        "manifestSHA256": target_manifest_sha,
    }
    require(metadata["source"] == expected_source and metadata["next"] == expected_target,
            "TRANSITION_HASH_MISMATCH")
    source_manifest, target_manifest = source["manifest"], target["manifest"]
    require(metadata["ownerId"] == source_manifest["ownerId"] and
            metadata["registryVersion"] == source_manifest["registryVersion"] and
            metadata["cursorNodeId"] == source_manifest["nodes"][0]["nodeId"] and
            metadata["codexNodeId"] == target_manifest["nodes"][1]["nodeId"],
            "INVALID_TRANSITION_METADATA")


def load_bundle(bundle_path, public_key_path, openssl, bundle_owner, runtime_owner):
    bundle = private_directory(bundle_path, bundle_owner, {"next", "rollback", "transition.json"})
    next_dir = private_directory(bundle / "next", bundle_owner, {"registry.json", "state.json"})
    rollback_dir = private_directory(bundle / "rollback", bundle_owner,
                                     {"registry.json", "state.json"})
    raw = {
        "transition": read_private(bundle / "transition.json", bundle_owner),
        "source_registry": read_private(rollback_dir / "registry.json", bundle_owner),
        "source_state": read_private(rollback_dir / "state.json", bundle_owner),
        "target_registry": read_private(next_dir / "registry.json", bundle_owner),
        "target_state": read_private(next_dir / "state.json", bundle_owner),
    }
    public_key = read_private(public_key_path, runtime_owner, 16 << 10)
    executable = shutil.which(openssl)
    require(executable is not None, "OPENSSL_REQUIRED")
    snapshots, paths = transition.snapshot_openssl_inputs({"public.pem": public_key})
    try:
        source_registry = decode_json(raw["source_registry"], "INVALID_TRANSITION_BUNDLE")
        target_registry = decode_json(raw["target_registry"], "INVALID_TRANSITION_BUNDLE")
        source_state = decode_json(raw["source_state"], "INVALID_TRANSITION_BUNDLE")
        target_state = decode_json(raw["target_state"], "INVALID_TRANSITION_BUNDLE")
        metadata = decode_json(raw["transition"], "INVALID_TRANSITION_METADATA")
        try:
            source_manifest, source_manifest_sha = transition.validate_registry(
                source_registry, executable, paths["public.pem"])
            target_manifest, target_manifest_sha = transition.validate_registry(
                target_registry, executable, paths["public.pem"])
            transition.validate_state(source_state, source_manifest, source_manifest_sha)
            transition.validate_state(target_state, target_manifest, target_manifest_sha)
        except transition.TransitionError:
            raise InstallError("INVALID_TRANSITION_BUNDLE") from None
    finally:
        try:
            snapshots.cleanup()
        except OSError:
            pass
    require(len(source_manifest["nodes"]) == 1 and source_manifest["nodes"][0]["adapter"] == "cursor",
            "INVALID_TRANSITION_SEMANTICS")
    require(len(target_manifest["nodes"]) == 2 and
            target_manifest["ownerId"] == source_manifest["ownerId"] and
            target_manifest["registryVersion"] == source_manifest["registryVersion"] and
            target_manifest["mode"] == source_manifest["mode"] and
            target_manifest["nodes"][0] == source_manifest["nodes"][0] and
            target_manifest["nodes"][1]["adapter"] == "codex",
            "INVALID_TRANSITION_SEMANTICS")
    cursor_id = source_manifest["nodes"][0]["nodeId"]
    codex_id = target_manifest["nodes"][1]["nodeId"]
    require(target_state["nodes"][cursor_id] == source_state["nodes"][cursor_id] and
            target_state["nodes"][codex_id] == {
                "mode": "sealed", "stateVersion": 1, "generation": 0,
                "operationId": "bootstrap", "identityEpoch": 0,
                "adapterKind": "codex", "adapterVersion": "",
            }, "INVALID_TRANSITION_SEMANTICS")
    validate_transition_metadata(metadata, raw, source_registry, target_registry,
                                 source_manifest_sha, target_manifest_sha)
    return {
        "path": str(bundle), "transitionSHA256": sha256(raw["transition"]),
        "source": {
            "registrySHA256": sha256(raw["source_registry"]),
            "routerStateSHA256": sha256(raw["source_state"]),
        },
        "target": {
            "registrySHA256": sha256(raw["target_registry"]),
            "routerStateSHA256": sha256(raw["target_state"]),
        },
        "raw": raw,
    }


def acquire_lock(path, owner, busy_code):
    path = canonical_existing(path, "INVALID_LOCK_FILE")
    try:
        descriptor = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    except OSError:
        raise InstallError("INVALID_LOCK_FILE") from None
    info = os.fstat(descriptor)
    if not (stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and
            info.st_uid == owner):
        os.close(descriptor)
        raise InstallError("INVALID_LOCK_FILE")
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(descriptor)
        raise InstallError(busy_code) from None
    return descriptor


def release_locks(descriptors):
    for descriptor in reversed(descriptors):
        try:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
        except OSError:
            pass
        try:
            os.close(descriptor)
        except OSError:
            pass


def ensure_consumer_stopped(status_path, pid_path, owner):
    pid_path = Path(pid_path)
    require(pid_path.is_absolute() and pid_path != Path(pid_path.anchor),
            "INVALID_CONSUMER_PID_PATH")
    try:
        require(pid_path.parent.resolve(strict=True) == pid_path.parent,
                "INVALID_CONSUMER_PID_PATH")
    except (OSError, RuntimeError):
        raise InstallError("INVALID_CONSUMER_PID_PATH") from None
    require(not os.path.lexists(pid_path), "CONSUMER_RUNNING")
    status = decode_json(read_private(status_path, owner, 4096), "INVALID_CONSUMER_STATUS")
    require(status == {"schema": 1, "service": "panel", "state": "stopped", "pid": None},
            "CONSUMER_RUNNING")


def journal_base(bundle):
    return {
        "schema": 1, "operation": "install-registry-transition",
        "bundlePath": bundle["path"], "transitionSHA256": bundle["transitionSHA256"],
        "source": bundle["source"], "target": bundle["target"],
    }


def encode_json(value):
    return (json.dumps(value, ensure_ascii=True, separators=(",", ":"), sort_keys=True) + "\n").encode()


def write_atomic(path, raw, uid, gid, create=False):
    parent = private_directory(path.parent, uid)
    parent_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    temporary = None
    try:
        descriptor, temporary_name = tempfile.mkstemp(prefix="." + path.name + "-", dir=parent)
        temporary = Path(temporary_name)
        try:
            os.fchmod(descriptor, 0o600)
            os.fchown(descriptor, uid, gid)
            with os.fdopen(descriptor, "wb", closefd=False) as stream:
                stream.write(raw)
                stream.flush()
                checkpoint("journal.before_file_fsync")
                os.fsync(stream.fileno())
                checkpoint("journal.after_file_fsync")
        finally:
            os.close(descriptor)
        checkpoint("journal.before_replace")
        if create:
            try:
                transition.rename_no_replace(parent_fd, temporary.name, path.name)
            except transition.TransitionError as error:
                if str(error) == "OUTPUT_ALREADY_EXISTS":
                    raise InstallError("JOURNAL_ALREADY_EXISTS") from None
                raise InstallError("JOURNAL_CREATE_FAILED") from None
        else:
            os.replace(temporary.name, path.name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
        temporary = None
        checkpoint("journal.after_replace")
        checkpoint("journal.before_directory_fsync")
        os.fsync(parent_fd)
        checkpoint("journal.after_directory_fsync")
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass
        try:
            os.close(parent_fd)
        except OSError:
            pass


def journal_record(bundle, direction, phase):
    record = journal_base(bundle)
    record.update({"direction": direction, "phase": phase})
    return record


def validate_journal(record, bundle):
    require(type(record) is dict and set(record) == JOURNAL_FIELDS and
            record["schema"] == 1 and record["operation"] == "install-registry-transition" and
            record["bundlePath"] == bundle["path"] and
            record["transitionSHA256"] == bundle["transitionSHA256"] and
            record["source"] == bundle["source"] and record["target"] == bundle["target"] and
            record["direction"] in ("target", "rollback") and record["phase"] in PHASES,
            "JOURNAL_BUNDLE_MISMATCH")


def checkpoint(_name):
    """Fault-injection seam; production execution intentionally does nothing."""


def replace_runtime_file(path, raw, info, label):
    parent = private_directory(path.parent, info.st_uid)
    parent_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    temporary = None
    try:
        descriptor, temporary_name = tempfile.mkstemp(prefix="." + path.name + "-", dir=parent)
        temporary = Path(temporary_name)
        try:
            os.fchmod(descriptor, 0o600)
            os.fchown(descriptor, info.st_uid, info.st_gid)
            with os.fdopen(descriptor, "wb", closefd=False) as stream:
                stream.write(raw)
                stream.flush()
                checkpoint(label + ".before_file_fsync")
                os.fsync(stream.fileno())
                checkpoint(label + ".after_file_fsync")
        finally:
            os.close(descriptor)
        checkpoint(label + ".before_replace")
        os.replace(temporary.name, path.name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
        temporary = None
        checkpoint(label + ".after_replace")
        checkpoint(label + ".before_directory_fsync")
        os.fsync(parent_fd)
        checkpoint(label + ".after_directory_fsync")
    except Exception:
        if temporary is not None:
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass
        raise
    finally:
        try:
            os.close(parent_fd)
        except OSError:
            pass


def validate_completed_pair(registry_raw, state_raw, public_key_path, openssl, owner):
    executable = shutil.which(openssl)
    require(executable is not None, "OPENSSL_REQUIRED")
    public_key = read_private(public_key_path, owner, 16 << 10)
    snapshots, paths = transition.snapshot_openssl_inputs({"public.pem": public_key})
    try:
        registry = decode_json(registry_raw, "COMPLETION_VALIDATION_FAILED")
        state = decode_json(state_raw, "COMPLETION_VALIDATION_FAILED")
        try:
            manifest, manifest_sha = transition.validate_registry(
                registry, executable, paths["public.pem"])
            transition.validate_state(state, manifest, manifest_sha)
        except transition.TransitionError:
            raise InstallError("COMPLETION_VALIDATION_FAILED") from None
    finally:
        try:
            snapshots.cleanup()
        except OSError:
            pass


def execute(direction, bundle, journal_path, registry_path, state_path, registry_info, state_info,
            public_key_path, status_path, pid_path, openssl, journal_owner, journal_gid):
    desired = bundle["target" if direction == "target" else "source"]
    registry_raw = read_private(registry_path, registry_info.st_uid)
    state_raw = read_private(state_path, state_info.st_uid)
    current = {"registrySHA256": sha256(registry_raw), "routerStateSHA256": sha256(state_raw)}
    require(current["registrySHA256"] in {
        bundle["source"]["registrySHA256"], bundle["target"]["registrySHA256"],
    } and current["routerStateSHA256"] in {
        bundle["source"]["routerStateSHA256"], bundle["target"]["routerStateSHA256"],
    }, "UNKNOWN_RUNTIME_PAIR")
    desired_raw = bundle["raw"]
    target_registry = (desired_raw["target_registry"] if direction == "target"
                       else desired_raw["source_registry"])
    target_state = (desired_raw["target_state"] if direction == "target"
                    else desired_raw["source_state"])
    if current == desired:
        validate_completed_pair(registry_raw, state_raw, public_key_path, openssl,
                                registry_info.st_uid)
        write_atomic(journal_path, encode_json(journal_record(bundle, direction, "completed")),
                     journal_owner, journal_gid)
        return True
    write_atomic(journal_path, encode_json(journal_record(bundle, direction, "prepared")),
                 journal_owner, journal_gid)
    ensure_consumer_stopped(status_path, pid_path, journal_owner)
    try:
        if current["registrySHA256"] != desired["registrySHA256"]:
            replace_runtime_file(registry_path, target_registry, registry_info, "registry")
        write_atomic(journal_path,
                     encode_json(journal_record(bundle, direction, "registry-replaced")),
                     journal_owner, journal_gid)
        ensure_consumer_stopped(status_path, pid_path, journal_owner)
        if current["routerStateSHA256"] != desired["routerStateSHA256"]:
            replace_runtime_file(state_path, target_state, state_info, "state")
        write_atomic(journal_path,
                     encode_json(journal_record(bundle, direction, "state-replaced")),
                     journal_owner, journal_gid)
        ensure_consumer_stopped(status_path, pid_path, journal_owner)
        final_registry = read_private(registry_path, registry_info.st_uid)
        final_state = read_private(state_path, state_info.st_uid)
        require(sha256(final_registry) == desired["registrySHA256"] and
                sha256(final_state) == desired["routerStateSHA256"],
                "COMPLETION_VALIDATION_FAILED")
        validate_completed_pair(final_registry, final_state, public_key_path, openssl,
                                registry_info.st_uid)
        write_atomic(journal_path, encode_json(journal_record(bundle, direction, "completed")),
                     journal_owner, journal_gid)
        return False
    except InstallError:
        raise
    except OSError:
        raise InstallError("INSTALL_STATE_UNKNOWN") from None


def install(action, direction, bundle_path, registry_path, state_path, public_key_path,
            router_lock_path, deploy_lock_path, status_path, pid_path, journal_path,
            openssl="openssl"):
    require_root()
    registry_path, state_path = Path(registry_path), Path(state_path)
    registry_info = private_file_info(registry_path)
    state_info = private_file_info(state_path)
    require(registry_info.st_uid == state_info.st_uid and registry_info.st_gid == state_info.st_gid,
            "RUNTIME_OWNER_MISMATCH")
    runtime_owner = registry_info.st_uid
    private_directory(registry_path.parent, runtime_owner)
    private_directory(state_path.parent, runtime_owner)
    private_file_info(public_key_path, runtime_owner)
    journal_owner, journal_gid = os.geteuid(), os.getegid()
    journal_path = Path(journal_path)
    if action == "apply":
        private_missing_path(journal_path, journal_owner)
    else:
        private_directory(journal_path.parent, journal_owner)
    locks = []
    try:
        locks.append(acquire_lock(router_lock_path, runtime_owner, "ROUTER_LOCK_BUSY"))
        locks.append(acquire_lock(deploy_lock_path, journal_owner, "DEPLOY_LOCK_BUSY"))
        ensure_consumer_stopped(status_path, pid_path, journal_owner)
        bundle = load_bundle(bundle_path, public_key_path, openssl, journal_owner, runtime_owner)
        source_registry = read_private(registry_path, runtime_owner)
        source_state = read_private(state_path, runtime_owner)
        if action == "apply":
            require(sha256(source_registry) == bundle["source"]["registrySHA256"] and
                    sha256(source_state) == bundle["source"]["routerStateSHA256"],
                    "APPLY_REQUIRES_EXACT_SOURCE_PAIR")
            record = journal_record(bundle, "target", "prepared")
            write_atomic(journal_path, encode_json(record), journal_owner, journal_gid, create=True)
            chosen = "target"
        else:
            require(direction in ("target", "rollback"), "RECOVERY_DIRECTION_REQUIRED")
            record = decode_json(read_private(journal_path, journal_owner), "INVALID_JOURNAL")
            validate_journal(record, bundle)
            chosen = direction
        idempotent = execute(chosen, bundle, journal_path, registry_path, state_path,
                             registry_info, state_info, public_key_path, status_path, pid_path,
                             openssl, journal_owner, journal_gid)
        return {"status": "REGISTRY_PAIR_INSTALLED" if action == "apply"
                else "REGISTRY_PAIR_RECOVERED", "direction": chosen,
                "idempotent": idempotent}
    except (OSError, transition.TransitionError):
        raise InstallError("INSTALL_STATE_UNKNOWN") from None
    finally:
        release_locks(locks)


def add_common(parser):
    parser.add_argument("--bundle", required=True, type=Path)
    parser.add_argument("--registry", required=True, type=Path)
    parser.add_argument("--router-state", required=True, type=Path)
    parser.add_argument("--signer-public-key", required=True, type=Path)
    parser.add_argument("--router-lock", required=True, type=Path)
    parser.add_argument("--deploy-lock", required=True, type=Path)
    parser.add_argument("--consumer-status", required=True, type=Path)
    parser.add_argument("--consumer-pid-file", required=True, type=Path)
    parser.add_argument("--journal", required=True, type=Path)
    parser.add_argument("--openssl", default="openssl")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    apply_parser = commands.add_parser("apply")
    add_common(apply_parser)
    recover_parser = commands.add_parser("recover")
    add_common(recover_parser)
    recover_parser.add_argument("--direction", required=True, choices=("target", "rollback"))
    args = parser.parse_args()
    os.umask(0o077)
    result = install(args.action, getattr(args, "direction", None), args.bundle, args.registry,
                     args.router_state, args.signer_public_key, args.router_lock, args.deploy_lock,
                     args.consumer_status, args.consumer_pid_file, args.journal, args.openssl)
    print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    try:
        main()
    except (InstallError, transition.TransitionError) as error:
        print(str(error))
        raise SystemExit(1) from None
    except Exception:
        print("REGISTRY_PAIR_INSTALL_FAILED")
        raise SystemExit(1) from None
