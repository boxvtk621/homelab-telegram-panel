#!/usr/bin/env python3
"""Root-only authoritative Panel stop/start seam for offline registry changes."""

import argparse
import fcntl
import json
import os
from pathlib import Path
import stat
import tempfile

import component_deploy
import deploy


ROOT = Path("/opt/homelab-agents-cd")
RUNTIME = Path("/run/homelab-panel")
STATUS_NAME = "panel-status.json"
PID_NAME = "panel.pid"


def require_root():
    deploy.require(os.geteuid() == 0, "ROOT_REQUIRED")


def fsync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def private_directory(path, owner, create=False):
    uid, gid = owner
    path = Path(path)
    deploy.require(path.is_absolute() and path != Path(path.anchor),
                   "INVALID_RUNTIME_DIRECTORY")
    if create and not os.path.lexists(path):
        try:
            os.mkdir(path, 0o700)
            fsync_directory(path.parent)
        except OSError:
            raise deploy.DeployError("INVALID_RUNTIME_DIRECTORY") from None
    try:
        info = os.lstat(path)
    except OSError:
        raise deploy.DeployError("INVALID_RUNTIME_DIRECTORY") from None
    deploy.require(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode) and
                   stat.S_IMODE(info.st_mode) == 0o700 and
                   info.st_uid == uid and info.st_gid == gid,
                   "INVALID_RUNTIME_DIRECTORY")


def read_private(path, owner, maximum=4096):
    uid, gid = owner
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except OSError:
        raise deploy.DeployError("INVALID_SUPERVISOR_PROOF") from None
    try:
        info = os.fstat(descriptor)
        deploy.require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and
                       info.st_uid == uid and info.st_gid == gid and info.st_size <= maximum,
                       "INVALID_SUPERVISOR_PROOF")
        raw = os.read(descriptor, maximum + 1)
        deploy.require(len(raw) <= maximum, "INVALID_SUPERVISOR_PROOF")
        return raw
    finally:
        os.close(descriptor)


def atomic_private(path, raw, owner):
    uid, gid = owner
    descriptor, temporary = tempfile.mkstemp(prefix=".panel-supervisor-", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        os.fchown(descriptor, uid, gid)
        with os.fdopen(descriptor, "wb") as output:
            output.write(raw)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        fsync_directory(path.parent)
    except OSError:
        raise deploy.DeployError("SUPERVISOR_PROOF_WRITE_FAILED") from None
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def atomic_json(path, value, owner):
    atomic_private(path, (json.dumps(value, separators=(",", ":"), sort_keys=True) + "\n").encode(),
                   owner)


def remove_private(path, owner):
    if not os.path.lexists(path):
        return
    read_private(path, owner)
    try:
        os.unlink(path)
        fsync_directory(path.parent)
    except OSError:
        raise deploy.DeployError("SUPERVISOR_PROOF_WRITE_FAILED") from None


def acquire_lock(path, owner):
    uid, gid = owner
    try:
        descriptor = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    except OSError:
        raise deploy.DeployError("INVALID_DEPLOY_LOCK") from None
    info = os.fstat(descriptor)
    if not (stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and
            info.st_uid == uid and info.st_gid == gid):
        os.close(descriptor)
        raise deploy.DeployError("INVALID_DEPLOY_LOCK")
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(descriptor)
        raise deploy.DeployError("DEPLOY_LOCK_BUSY") from None
    return descriptor


def release_lock(descriptor):
    try:
        fcntl.flock(descriptor, fcntl.LOCK_UN)
    finally:
        os.close(descriptor)


def proof(runtime, owner):
    status_path, pid_path = runtime / STATUS_NAME, runtime / PID_NAME
    status_exists, pid_exists = os.path.lexists(status_path), os.path.lexists(pid_path)
    if not status_exists and not pid_exists:
        return None
    deploy.require(status_exists, "SUPERVISOR_PROOF_RUNTIME_DRIFT")
    try:
        status_value = json.loads(read_private(status_path, owner), object_pairs_hook=deploy.pairs)
    except deploy.DeployError:
        raise
    except Exception:
        raise deploy.DeployError("INVALID_SUPERVISOR_PROOF") from None
    stopped = {"schema": 1, "service": "panel", "state": "stopped", "pid": None}
    if status_value == stopped:
        deploy.require(not pid_exists, "SUPERVISOR_PROOF_RUNTIME_DRIFT")
        return status_value
    deploy.require(type(status_value) is dict and set(status_value) ==
                   {"schema", "service", "state", "pid"} and
                   status_value["schema"] == 1 and status_value["service"] == "panel" and
                   status_value["state"] == "running" and
                   type(status_value["pid"]) is int and status_value["pid"] > 0 and pid_exists,
                   "INVALID_SUPERVISOR_PROOF")
    try:
        pid_text = read_private(pid_path, owner, 64).decode("ascii")
    except UnicodeDecodeError:
        raise deploy.DeployError("INVALID_SUPERVISOR_PROOF") from None
    deploy.require(pid_text == str(status_value["pid"]) + "\n",
                   "SUPERVISOR_PROOF_RUNTIME_DRIFT")
    return status_value


def container_id(installer, name):
    service = installer.config["components"][name]["service"]
    ids = installer.compose("ps", "-aq", service).splitlines()
    deploy.require(len(ids) == 1 and deploy.re.fullmatch("[0-9a-f]{12,64}", ids[0]),
                   "COMPONENT_CONTAINER_MISSING")
    return ids[0]


def container_state(container):
    try:
        value = json.loads(component_deploy.run("docker", "inspect", container),
                           object_pairs_hook=deploy.pairs)
        deploy.require(type(value) is list and len(value) == 1 and type(value[0]) is dict and
                       type(value[0].get("State")) is dict,
                       "INVALID_CONTAINER_STATE")
        state = value[0]["State"]
        deploy.require(type(state.get("Running")) is bool and type(state.get("Pid")) is int and
                       state["Pid"] >= 0, "INVALID_CONTAINER_STATE")
        return state
    except deploy.DeployError:
        raise
    except Exception:
        raise deploy.DeployError("INVALID_CONTAINER_STATE") from None


def stopped_panel_identity(installer, container):
    """Verify the stopped container against the authoritative current manifest."""
    manifest = installer.ledger["components"]["panel"]["current"]
    try:
        data = json.loads(component_deploy.run("docker", "inspect", container))[0]
        image = json.loads(component_deploy.run("docker", "image", "inspect",
                                                manifest["image"]))[0]
        labels = image["Config"].get("Labels", {})
        deploy.require(not data["State"]["Running"] and data["State"]["Pid"] == 0 and
                       data["Image"] == image["Id"] and
                       manifest["image"] in image.get("RepoDigests", []) and
                       image["Os"] == "linux" and image["Architecture"] == "amd64" and
                       image["Config"]["User"] == "10001:10001" and
                       labels.get("org.opencontainers.image.version") == manifest["version"] and
                       labels.get("org.opencontainers.image.revision") == manifest["revision"],
                       "PANEL_STOPPED_RUNTIME_MISMATCH")
        deploy.require(data["Config"]["User"] == "10001:10001" and
                       data["HostConfig"]["ReadonlyRootfs"] and
                       not data["HostConfig"]["Privileged"] and
                       "ALL" in data["HostConfig"]["CapDrop"] and
                       "no-new-privileges:true" in data["HostConfig"]["SecurityOpt"],
                       "PANEL_STOPPED_RUNTIME_MISMATCH")
        return data
    except deploy.DeployError:
        raise
    except Exception:
        raise deploy.DeployError("PANEL_STOPPED_RUNTIME_MISMATCH") from None


def verify_panel_stopped(component_root, expected_container=None):
    """Read-only authoritative stopped check; caller must hold deploy.lock."""
    installer = component_deploy.Installer(Path(component_root))
    deploy.require(installer.ledger["pending"] is None,
                   "INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR")
    observed = container_id(installer, "panel")
    if expected_container is not None:
        deploy.require(observed == expected_container, "PANEL_CONTAINER_CHANGED")
    stopped_panel_identity(installer, observed)
    return observed


def exact_override(installer, owner):
    path = installer.root / "panel.override.json"
    try:
        value = json.loads(read_private(path, owner, 65536), object_pairs_hook=deploy.pairs)
    except deploy.DeployError:
        raise
    except Exception:
        raise deploy.DeployError("INVALID_PANEL_OVERRIDE") from None
    expected = {"services": {installer.config["components"]["panel"]["service"]: {
        "image": installer.ledger["components"]["panel"]["current"]["image"]}}}
    deploy.require(value == expected, "INVALID_PANEL_OVERRIDE")
    return path


def verify_others(installer, before):
    for name, expected in before.items():
        observed = container_id(installer, name)
        deploy.require(observed == expected, "OTHER_COMPONENT_CONTAINER_CHANGED")
        installer.inspect(name, installer.ledger["components"][name]["current"], observed)


def supervise(action, root=ROOT, runtime=RUNTIME):
    require_root()
    deploy.require(action in ("stop", "start"), "INVALID_SUPERVISOR_ACTION")
    owner = (os.geteuid(), os.getegid())
    root, runtime = Path(root), Path(runtime)
    lock = acquire_lock(root / "deploy.lock", owner)
    try:
        private_directory(runtime, owner, create=True)
        installer = component_deploy.Installer(root)
        deploy.require(installer.ledger["pending"] is None,
                       "INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR")
        panel_id = container_id(installer, "panel")
        others = {name: container_id(installer, name)
                  for name in installer.config["components"] if name != "panel"}
        verify_others(installer, others)
        state = container_state(panel_id)
        current_proof = proof(runtime, owner)
        stopped_proof = {"schema": 1, "service": "panel", "state": "stopped", "pid": None}

        if state["Running"]:
            deploy.require(state["Pid"] > 0, "INVALID_CONTAINER_STATE")
            installer.inspect("panel", installer.ledger["components"]["panel"]["current"], panel_id)
            running_proof = {"schema": 1, "service": "panel", "state": "running",
                             "pid": state["Pid"]}
            deploy.require(current_proof in (None, running_proof),
                           "SUPERVISOR_PROOF_RUNTIME_DRIFT")
            if action == "start":
                deploy.require(current_proof == running_proof,
                               "SUPERVISOR_PROOF_RUNTIME_DRIFT")
                installer.health("panel", installer.ledger["components"]["panel"]["current"])
                return {"status": "PANEL_RUNNING", "pid": state["Pid"], "idempotent": True}
            installer.compose_mutation("stop", installer.config["components"]["panel"]["service"])
            after = container_state(panel_id)
            deploy.require(not after["Running"] and after["Pid"] == 0,
                           "PANEL_STOP_READBACK_MISMATCH")
            deploy.require(container_id(installer, "panel") == panel_id,
                           "PANEL_CONTAINER_CHANGED")
            verify_others(installer, others)
            remove_private(runtime / PID_NAME, owner)
            atomic_json(runtime / STATUS_NAME, stopped_proof, owner)
            deploy.require(proof(runtime, owner) == stopped_proof,
                           "SUPERVISOR_PROOF_RUNTIME_DRIFT")
            return {"status": "PANEL_STOPPED", "pid": None, "idempotent": False}

        deploy.require(state["Pid"] == 0 and current_proof == stopped_proof,
                       "SUPERVISOR_PROOF_RUNTIME_DRIFT")
        if action == "stop":
            verify_others(installer, others)
            return {"status": "PANEL_STOPPED", "pid": None, "idempotent": True}

        override = exact_override(installer, owner)
        remove_private(runtime / STATUS_NAME, owner)
        try:
            installer.compose_mutation("up", "-d", "--no-deps", "--pull", "never",
                                       installer.config["components"]["panel"]["service"],
                                       override=override)
        except component_deploy.ContainerMutationUnknown:
            raise
        deploy.require(container_id(installer, "panel") == panel_id,
                       "PANEL_CONTAINER_CHANGED")
        verify_others(installer, others)
        after = container_state(panel_id)
        deploy.require(after["Running"] and after["Pid"] > 0,
                       "PANEL_START_READBACK_MISMATCH")
        installer.inspect("panel", installer.ledger["components"]["panel"]["current"], panel_id)
        installer.health("panel", installer.ledger["components"]["panel"]["current"])
        running_proof = {"schema": 1, "service": "panel", "state": "running",
                         "pid": after["Pid"]}
        atomic_private(runtime / PID_NAME, (str(after["Pid"]) + "\n").encode(), owner)
        atomic_json(runtime / STATUS_NAME, running_proof, owner)
        deploy.require(proof(runtime, owner) == running_proof,
                       "SUPERVISOR_PROOF_RUNTIME_DRIFT")
        return {"status": "PANEL_RUNNING", "pid": after["Pid"], "idempotent": False}
    finally:
        release_lock(lock)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("stop", "start"))
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--runtime-dir", type=Path, default=RUNTIME)
    args = parser.parse_args()
    try:
        print(json.dumps(supervise(args.action, args.root, args.runtime_dir), sort_keys=True))
    except (deploy.DeployError, component_deploy.ContainerMutationUnknown) as error:
        parser.exit(1, str(error) + "\n")


if __name__ == "__main__":
    main()
