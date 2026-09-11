#!/usr/bin/env python3
"""Enroll one already-running sealed Codex node into existing component CD."""

import argparse
import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import tempfile

import component_cd as cd
import component_deploy as host
import component_release as release
import deploy
import registry_pair_install as pair
import registry_transition as transition


JOURNAL_FILES = {
    "plan.json", "state.json", "source-config.json", "source-deployment.json",
    "source-compose.yaml", "target-config.json", "target-deployment.json",
    "target-compose.yaml",
}
PHASES = {
    "prepared", "compose-replaced", "config-replaced", "ledger-replaced",
    "activation-pending", "completed",
}
ORDER = ("compose", "config", "ledger")
CODEX_BIND_MOUNTS = {
    "/config": ("codex-config", True),
    "/state": ("codex-state", False),
    "/auth": ("codex-auth", False),
    "/workspace": ("codex-workspace", True),
}
CODEX_IMAGE_ENV = {"PATH", "NODE_VERSION", "YARN_VERSION"}


class EnrollmentError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise EnrollmentError(code)


def require_root():
    require(os.geteuid() == 0, "ROOT_REQUIRED")


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def encode_json(value):
    serialized = json.dumps(value, ensure_ascii=True, separators=(",", ":"), sort_keys=True)
    return (serialized + "\n").encode()


def decode_json(raw, code):
    try:
        return json.loads(raw, object_pairs_hook=transition.pairs)
    except Exception:
        raise EnrollmentError(code) from None


def checkpoint(_name):
    """Fault-injection seam; production execution intentionally does nothing."""


def require_consumer_stopped(pid_path):
    path = Path(pid_path)
    require(path.is_absolute() and path != Path(path.anchor), "INVALID_CONSUMER_PID_PATH")
    try:
        require(path.parent.resolve(strict=True) == path.parent, "INVALID_CONSUMER_PID_PATH")
    except (OSError, RuntimeError):
        raise EnrollmentError("INVALID_CONSUMER_PID_PATH") from None
    require(not os.path.lexists(path), "COMPONENT_CD_CONSUMER_RUNNING")


def require_router_owned(lock_path, owner):
    path = pair.canonical_existing(lock_path, "INVALID_ROUTER_LOCK")
    try:
        descriptor = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    except OSError:
        raise EnrollmentError("INVALID_ROUTER_LOCK") from None
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == owner and
                stat.S_IMODE(info.st_mode) == 0o600,
                "INVALID_ROUTER_LOCK")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        raise EnrollmentError("ROUTER_NOT_RUNNING")
    finally:
        os.close(descriptor)


def read_root_file(path, maximum=1 << 20):
    try:
        return pair.read_private(path, os.geteuid(), maximum)
    except pair.InstallError as error:
        raise EnrollmentError(str(error)) from None


def fingerprint(config_raw, compose_raw, env_raw):
    digest = hashlib.sha256()
    for raw in (config_raw, compose_raw, env_raw):
        digest.update(raw + b"\0")
    return digest.hexdigest()


def router_control(path):
    return host.RouterControl(Path(path))


def installer_view(root, config, ledger, compose_path):
    installer = object.__new__(host.Installer)
    installer.root = Path(root)
    installer.file = installer.root / "deployment.json"
    installer.config = copy.deepcopy(config)
    installer.config["compose"] = str(compose_path)
    installer.ledger = copy.deepcopy(ledger)
    installer.router = router_control(config["router_socket"])
    return installer


def resolved_compose(installer):
    try:
        raw = installer.compose("config", "--format", "json")
        require(isinstance(raw, str) and len(raw.encode()) <= 1 << 20,
                "INVALID_CANDIDATE_COMPOSE")
        return json.loads(raw, object_pairs_hook=deploy.pairs)
    except Exception:
        raise EnrollmentError("INVALID_CANDIDATE_COMPOSE") from None


def compose_config_hash(installer, service):
    help_output = installer.compose("config", "--help")
    require(isinstance(help_output, str) and "--hash" in help_output,
            "INVALID_COMPOSE_CONFIG_HASH")
    output = installer.compose("config", "--hash", service).split()
    require(len(output) == 2 and output[0] == service and
            transition.HEX_SHA256.fullmatch(output[1]), "INVALID_COMPOSE_CONFIG_HASH")
    return output[1]


def validate_codex_service(resolved, config, manifest, compose_path):
    services = resolved["services"]
    name = config["components"]["codex"]["service"]
    service = services[name]
    required = {
        "image", "user", "read_only", "cap_drop", "security_opt", "restart",
        "networks", "volumes", "tmpfs", "cpus", "mem_limit", "pids_limit",
        "stop_grace_period", "logging",
    }
    optional = {"command", "entrypoint", "profiles"}
    require(type(service) is dict and required <= set(service) <= required | optional and
            service["image"] == manifest["image"] and service["user"] == "10001:10001" and
            service["read_only"] is True and service["cap_drop"] == ["ALL"] and
            service["security_opt"] == ["no-new-privileges:true"] and
            service["restart"] == "unless-stopped" and service["cpus"] == 1 and
            service["mem_limit"] in (1073741824, "1073741824") and
            service["pids_limit"] == 128 and service["stop_grace_period"] == "20s" and
            service["logging"] == {
                "driver": "local", "options": {"max-file": "3", "max-size": "10m"},
            } and service.get("command") is None and service.get("entrypoint") is None and
            service.get("profiles") in (None, ["codex"]),
            "INVALID_CODEX_COMPOSE_SERVICE")
    cursor = services[config["components"]["cursor"]["service"]]
    require(type(service["networks"]) is dict and len(service["networks"]) == 1 and
            service["networks"] == cursor.get("networks"), "INVALID_CODEX_COMPOSE_NETWORK")
    require(service["tmpfs"] == ["/tmp:rw,nosuid,nodev,mode=1777,size=134217728"],
            "INVALID_CODEX_COMPOSE_TMPFS")
    base = Path(compose_path).parent / "cutover" / "codex-node"
    volumes = service["volumes"]
    require(type(volumes) is list and len(volumes) == len(CODEX_BIND_MOUNTS),
            "INVALID_CODEX_COMPOSE_MOUNTS")
    observed = {}
    for volume in volumes:
        require(type(volume) is dict and
                {"type", "source", "target", "bind"} <= set(volume) <= {
                    "type", "source", "target", "read_only", "bind",
                } and volume["type"] == "bind" and type(volume["target"]) is str and
                volume["target"] not in observed and
                volume["target"] in CODEX_BIND_MOUNTS,
                "INVALID_CODEX_COMPOSE_MOUNTS")
        suffix, read_only = CODEX_BIND_MOUNTS[volume["target"]]
        require(volume["source"] == str(base / suffix) and
                volume.get("read_only", False) is read_only and
                volume["bind"] == {"create_host_path": False},
                "INVALID_CODEX_COMPOSE_MOUNTS")
        observed[volume["target"]] = volume
    require(set(observed) == set(CODEX_BIND_MOUNTS), "INVALID_CODEX_COMPOSE_MOUNTS")


def validate_compose_transition(source, target, config, manifest, candidate_path):
    require(type(source) is dict and type(target) is dict and
            type(source.get("services")) is dict and type(target.get("services")) is dict and
            set(source) == set(target) and
            all(source[key] == target[key] for key in source if key != "services"),
            "COMPOSE_TOP_LEVEL_CHANGED")
    source_services = source["services"]
    target_services = target["services"]
    expected_source = {
        config["components"]["panel"]["service"],
        config["components"]["cursor"]["service"],
    }
    require(set(source_services) == expected_source and
            set(target_services) == expected_source | {"codex"} and
            all(source_services[name] == target_services[name] for name in expected_source),
            "COMPOSE_NOT_EXACT_CODEX_ADDITION")
    validate_codex_service(target, config, manifest, candidate_path)


def environment_names(values):
    require(type(values) is list, "CODEX_RUNTIME_ENV_REJECTED")
    result = {}
    for value in values:
        require(isinstance(value, str) and "=" in value, "CODEX_RUNTIME_ENV_REJECTED")
        name = value.split("=", 1)[0]
        require(name and name not in result and name in CODEX_IMAGE_ENV,
                "CODEX_RUNTIME_ENV_REJECTED")
        result[name] = value
    return result


def validate_codex_runtime(data, image, resolved, config, provenance_compose_path):
    require(type(data) is dict and type(image) is dict and
            type(data.get("Config")) is dict and type(data.get("HostConfig")) is dict and
            type(data.get("NetworkSettings")) is dict and type(data.get("Mounts")) is list and
            type(image.get("Config")) is dict, "CODEX_RUNTIME_CANDIDATE_MISMATCH")
    container = data["Config"]
    image_config = image["Config"]
    host_config = data["HostConfig"]
    network_settings = data["NetworkSettings"]
    service_name = config["components"]["codex"]["service"]
    service = resolved["services"][service_name]
    require({
        "Image", "User", "Env", "Entrypoint", "Cmd", "WorkingDir", "StopTimeout",
        "ExposedPorts", "Labels",
    } <= set(container) and {
        "ReadonlyRootfs", "Privileged", "CapAdd", "CapDrop", "SecurityOpt",
        "Devices", "DeviceRequests", "PublishAllPorts", "AutoRemove", "Init",
        "RestartPolicy", "NanoCpus", "Memory", "PidsLimit", "LogConfig", "Tmpfs",
        "NetworkMode", "PortBindings",
    } <= set(host_config) and {"Networks", "Ports"} <= set(network_settings) and
            container.get("User") == "10001:10001" and
            container.get("Image") == service["image"] and
            host_config.get("ReadonlyRootfs") is True and
            host_config.get("Privileged") is False and host_config.get("CapDrop") == ["ALL"] and
            host_config.get("SecurityOpt") == ["no-new-privileges:true"] and
            host_config.get("CapAdd") in (None, []) and
            host_config.get("Devices") in (None, []) and
            host_config.get("DeviceRequests") in (None, []) and
            host_config.get("PublishAllPorts") is False and
            host_config.get("AutoRemove") is False and host_config.get("Init") is None and
            host_config.get("RestartPolicy") == {
                "Name": "unless-stopped", "MaximumRetryCount": 0,
            } and host_config.get("NanoCpus") == 1_000_000_000 and
            host_config.get("Memory") == 1_073_741_824 and
            host_config.get("PidsLimit") == 128 and
            host_config.get("LogConfig") == {
                "Type": "local", "Config": {"max-file": "3", "max-size": "10m"},
            } and
            container.get("Entrypoint") == image_config.get("Entrypoint") == ["/harness-node"] and
            container.get("Cmd") == image_config.get("Cmd") == ["--config", "/config/node.json"] and
            container.get("WorkingDir") == image_config.get("WorkingDir") == "/opt/codex" and
            container.get("StopTimeout") == 20,
            "CODEX_RUNTIME_CANDIDATE_MISMATCH")
    container_env = environment_names(container.get("Env"))
    image_env = environment_names(image_config.get("Env"))
    require(container_env == image_env and
            container_env.get("PATH") ==
            "PATH=/opt/codex/node_modules/.bin:/usr/local/bin:/usr/bin:/bin",
            "CODEX_RUNTIME_ENV_REJECTED")
    expected_mounts = {
        volume["target"]: (
            "bind", volume["source"], not volume.get("read_only", False),
        )
        for volume in service["volumes"]
    }
    expected_mounts["/tmp"] = ("tmpfs", "", True)
    mounts = {}
    for mount in data["Mounts"]:
        require(type(mount) is dict and type(mount.get("Destination")) is str and
                mount["Destination"] not in mounts,
                "CODEX_RUNTIME_MOUNTS_CHANGED")
        mounts[mount["Destination"]] = (
            mount.get("Type"), mount.get("Source", ""), mount.get("RW"),
        )
    require(mounts == expected_mounts and host_config.get("Tmpfs") == {
        "/tmp": "rw,nosuid,nodev,mode=1777,size=134217728",
    }, "CODEX_RUNTIME_MOUNTS_CHANGED")
    network_names = {
        resolved["networks"][name]["name"] for name in service["networks"]
    }
    require(type(network_settings.get("Networks")) is dict and
            set(network_settings["Networks"]) == network_names and len(network_names) == 1 and
            host_config.get("NetworkMode") in network_names and
            not container.get("ExposedPorts") and not host_config.get("PortBindings") and
            not network_settings.get("Ports"), "CODEX_RUNTIME_NETWORK_CHANGED")
    labels = container.get("Labels")
    provenance = {
        "com.docker.compose.project": config["project"],
        "com.docker.compose.service": service_name,
        "com.docker.compose.container-number": "1",
        "com.docker.compose.oneoff": "False",
        "com.docker.compose.project.working_dir": str(Path(provenance_compose_path).parent),
        "com.docker.compose.project.config_files": str(provenance_compose_path),
        "com.docker.compose.project.environment_file": config["env_file"],
    }
    require(type(labels) is dict and
            all(labels.get(name) == value for name, value in provenance.items()) and
            transition.HEX_SHA256.fullmatch(
                labels.get("com.docker.compose.config-hash", "")),
            "CODEX_RUNTIME_COMPOSE_PROVENANCE_CHANGED")


def runtime_evidence(root, config, ledger, compose_path, codex_manifest,
                     allow_activated=False, resolved=None, provenance_compose_path=None):
    installer = installer_view(root, config, ledger, compose_path)
    resolved = resolved if resolved is not None else resolved_compose(installer)
    validate_codex_service(resolved, config, codex_manifest, compose_path)
    service = config["components"]["codex"]["service"]
    config_hash = compose_config_hash(installer, service)
    containers = {name: installer.container(name) for name in ("panel", "cursor", "codex")}
    require(len(set(containers.values())) == 3, "COMPONENT_CONTAINER_IDENTITY_COLLISION")
    codex_details = None
    for name in ("panel", "cursor", "codex"):
        details = installer.inspect(
            name, ledger["components"][name]["current"], containers[name])
        if name == "codex":
            codex_details = details
    require(type(codex_details) is tuple and len(codex_details) == 2,
            "CODEX_RUNTIME_CANDIDATE_MISMATCH")
    validate_codex_runtime(
        *codex_details, resolved, config, provenance_compose_path or compose_path)
    cursor_health = installer.health("cursor", ledger["components"]["cursor"]["current"])
    codex_health = installer.health("codex", codex_manifest)
    cursor_identity = installer.identity(cursor_health)
    codex_identity = installer.identity(codex_health)
    require(codex_health["quiescent"], "CODEX_NOT_QUIESCENT")
    routes = installer.routing()
    cursor_id = config["components"]["cursor"]["node_id"]
    codex_id = config["components"]["codex"]["node_id"]
    cursor_route, codex_route = routes["nodes"][cursor_id], routes["nodes"][codex_id]
    require(cursor_route["mode"] == "eligible" and "operationId" not in cursor_route and
            cursor_route["identityEpoch"] == cursor_identity["epoch"] and
            cursor_route["adapterVersion"] == cursor_identity["version"] and
            cursor_identity["registry"] == routes["registryVersion"],
            "CURSOR_ROUTE_CHANGED")
    sealed = (codex_route["mode"] == "sealed" and codex_route.get("operationId") == "bootstrap" and
              codex_route["generation"] == 0 and codex_route["identityEpoch"] == 0 and
              codex_route["adapterKind"] == "codex" and codex_route["adapterVersion"] == "")
    eligible = (codex_route["mode"] == "eligible" and "operationId" not in codex_route and
                codex_route["identityEpoch"] == codex_identity["epoch"] and
                codex_route["adapterVersion"] == codex_identity["version"])
    require(sealed or allow_activated and eligible, "CODEX_NOT_EXACT_SEALED_BOOTSTRAP")
    require(codex_identity["node"] == codex_id and codex_identity["kind"] == "codex" and
            codex_identity["version"] == codex_manifest["adapter_version"] and
            codex_identity["registry"] == routes["registryVersion"],
            "CODEX_NATIVE_IDENTITY_MISMATCH")
    return {
        "containers": containers, "cursorIdentity": cursor_identity,
        "codexIdentity": codex_identity, "cursorRoute": cursor_route,
        "codexRoute": codex_route, "registryVersion": routes["registryVersion"],
        "registrySHA256": routes["registrySHA256"], "composeConfigSHA256": config_hash,
    }


def validate_runtime_evidence(value, config, manifest):
    require(type(value) is dict and set(value) == {
        "containers", "cursorIdentity", "codexIdentity", "cursorRoute", "codexRoute",
        "registryVersion", "registrySHA256", "composeConfigSHA256",
    }, "INVALID_RUNTIME_EVIDENCE")
    require(type(value["containers"]) is dict and
            set(value["containers"]) == {"panel", "cursor", "codex"} and
            all(isinstance(item, str) and deploy.re.fullmatch("[0-9a-f]{12,64}", item)
                for item in value["containers"].values()) and
            len(set(value["containers"].values())) == 3, "INVALID_RUNTIME_EVIDENCE")
    for name in ("cursorIdentity", "codexIdentity"):
        identity = value[name]
        require(type(identity) is dict and
                set(identity) == {"node", "epoch", "registry", "kind", "version"} and
                type(identity["epoch"]) is int and identity["epoch"] > 0 and
                type(identity["registry"]) is int and identity["registry"] > 0,
                "INVALID_RUNTIME_EVIDENCE")
    host.RouterControl.node(value["cursorRoute"])
    host.RouterControl.node(value["codexRoute"])
    cursor_id = config["components"]["cursor"]["node_id"]
    codex_id = config["components"]["codex"]["node_id"]
    require(value["cursorIdentity"]["node"] == cursor_id and
            value["cursorIdentity"]["kind"] == "cursor" and
            value["codexIdentity"]["node"] == codex_id and
            value["codexIdentity"]["kind"] == "codex" and
            value["codexIdentity"]["version"] == manifest["adapter_version"] and
            value["registryVersion"] == value["cursorIdentity"]["registry"] ==
            value["codexIdentity"]["registry"] and
            transition.HEX_SHA256.fullmatch(value["registrySHA256"]) and
            transition.HEX_SHA256.fullmatch(value["composeConfigSHA256"]),
            "INVALID_RUNTIME_EVIDENCE")
    require(value["cursorRoute"]["mode"] == "eligible" and
            value["cursorRoute"]["identityEpoch"] == value["cursorIdentity"]["epoch"] and
            value["cursorRoute"]["adapterKind"] == "cursor" and
            value["cursorRoute"]["adapterVersion"] == value["cursorIdentity"]["version"] and
            "operationId" not in value["cursorRoute"], "INVALID_RUNTIME_EVIDENCE")
    require(value["codexRoute"]["mode"] == "sealed" and
            value["codexRoute"].get("operationId") == "bootstrap" and
            value["codexRoute"]["generation"] == 0 and
            value["codexRoute"]["identityEpoch"] == 0 and
            value["codexRoute"]["adapterKind"] == "codex" and
            value["codexRoute"]["adapterVersion"] == "", "INVALID_RUNTIME_EVIDENCE")


def validate_plan(plan, files):
    require(type(plan) is dict and set(plan) == {
        "schema", "operation", "paths", "codexTag", "codexManifest", "nodeId",
        "source", "target", "runtime",
    } and plan["schema"] == 1 and plan["operation"] == "enroll-codex-component-cd",
            "INVALID_ENROLLMENT_PLAN")
    paths = plan["paths"]
    require(type(paths) is dict and set(paths) == {
        "root", "compose", "candidateCompose", "env", "routerLock", "consumerPid",
    } and all(isinstance(value, str) and Path(value).is_absolute() for value in paths.values()),
            "INVALID_ENROLLMENT_PLAN")
    component, _ = release.selection(plan["codexTag"])
    require(component == "codex", "INVALID_CODEX_TAG")
    manifest = release.validate(plan["codexManifest"], "codex")
    require(manifest["version"] == release.selection(plan["codexTag"])[1] and
            transition.UUID.fullmatch(plan["nodeId"]), "INVALID_ENROLLMENT_PLAN")
    for direction in ("source", "target"):
        hashes = plan[direction]
        require(type(hashes) is dict and
                set(hashes) == {"configSHA256", "ledgerSHA256", "composeSHA256", "envSHA256"} and
                all(transition.HEX_SHA256.fullmatch(value) for value in hashes.values()),
                "INVALID_ENROLLMENT_PLAN")
        for name in ORDER:
            key = name + "SHA256" if name != "ledger" else "ledgerSHA256"
            require(sha256(files[direction][name]) == hashes[key],
                    "ENROLLMENT_PLAN_HASH_MISMATCH")
    require(plan["source"]["envSHA256"] == plan["target"]["envSHA256"],
            "ENROLLMENT_ENV_CHANGED")
    source_config = decode_json(files["source"]["config"], "INVALID_SOURCE_CONFIG")
    target_config = decode_json(files["target"]["config"], "INVALID_TARGET_CONFIG")
    source_ledger = decode_json(files["source"]["ledger"], "INVALID_SOURCE_LEDGER")
    target_ledger = decode_json(files["target"]["ledger"], "INVALID_TARGET_LEDGER")
    require(set(source_config) == {
        "project", "compose", "env_file", "router_socket", "components",
    } and
            set(target_config) == set(source_config) and
            set(source_config["components"]) == {"panel", "cursor"} and
            set(target_config["components"]) == {"panel", "cursor", "codex"},
            "INVALID_COMPONENT_TRANSITION")
    stripped = copy.deepcopy(target_config)
    codex = stripped["components"].pop("codex")
    require(stripped == source_config and
            plan["nodeId"] != source_config["components"]["cursor"]["node_id"] and
            codex == {
        "service": "codex", "url": "https://codex:18443", "node_id": plan["nodeId"],
        "actor_id": source_config["components"]["cursor"]["actor_id"],
    }, "PANEL_CURSOR_CONFIG_CHANGED")
    require(source_config["compose"] == target_config["compose"] == paths["compose"] and
            source_config["env_file"] == target_config["env_file"] == paths["env"] and
            Path(paths["candidateCompose"]).parent == Path(paths["compose"]).parent and
            paths["candidateCompose"] != paths["compose"] and
            Path(paths["routerLock"]) == Path(source_config["router_socket"]).parent / "router.lock",
            "ENROLLMENT_PATH_MISMATCH")
    require(set(source_ledger) == {"components", "pending", "config_sha256"} and
            set(target_ledger) == set(source_ledger) and
            source_ledger.get("pending") is None and target_ledger.get("pending") is None and
            set(source_ledger.get("components", {})) == {"panel", "cursor"} and
            set(target_ledger.get("components", {})) == {"panel", "cursor", "codex"} and
            all(target_ledger["components"][name] == source_ledger["components"][name]
                for name in ("panel", "cursor")) and
            target_ledger["components"]["codex"] == {"current": manifest, "previous": None},
            "PANEL_CURSOR_LEDGER_CHANGED")
    for name, slot in source_ledger["components"].items():
        require(type(slot) is dict and set(slot) == {"current", "previous"},
                "INVALID_SOURCE_LEDGER")
        release.validate(slot["current"], name)
        if slot["previous"] is not None:
            release.validate(slot["previous"], name)
    validate_runtime_evidence(plan["runtime"], target_config, manifest)
    return source_config, target_config, source_ledger, target_ledger


def journal_state(plan_raw, direction, phase):
    return {"schema": 1, "operation": "enroll-codex-component-cd",
            "planSHA256": sha256(plan_raw), "direction": direction, "phase": phase}


def publish_journal(output, plan, source, target):
    output = Path(output)
    try:
        parent, parent_fd, parent_identity = transition.open_output_parent(output)
    except transition.TransitionError as error:
        raise EnrollmentError(str(error)) from None
    staging = None
    published = False
    try:
        staging = Path(tempfile.mkdtemp(prefix="." + output.name + "-", dir=parent))
        staging.chmod(0o700)
        plan_raw = encode_json(plan)
        content = {
            "plan.json": plan_raw,
            "state.json": encode_json(journal_state(plan_raw, "target", "prepared")),
            "source-config.json": source["config"],
            "source-deployment.json": source["ledger"],
            "source-compose.yaml": source["compose"],
            "target-config.json": target["config"],
            "target-deployment.json": target["ledger"],
            "target-compose.yaml": target["compose"],
        }
        for name, raw in content.items():
            transition.write_private(staging / name, raw)
        transition.sync_directory(staging)
        transition.require_same_directory(parent, parent_fd, parent_identity)
        transition.rename_no_replace(parent_fd, staging.name, output.name)
        published = True
        staging = None
        try:
            os.fsync(parent_fd)
        except OSError:
            raise EnrollmentError("PUBLICATION_DURABILITY_UNKNOWN") from None
    except transition.TransitionError as error:
        raise EnrollmentError(str(error)) from None
    finally:
        if staging is not None:
            shutil.rmtree(staging)
        if published:
            try:
                os.close(parent_fd)
            except OSError:
                pass
        else:
            os.close(parent_fd)


def load_journal(path):
    root = pair.private_directory(path, os.geteuid(), JOURNAL_FILES)
    plan_raw = read_root_file(root / "plan.json", 256 << 10)
    plan = decode_json(plan_raw, "INVALID_ENROLLMENT_PLAN")
    files = {
        direction: {
            "config": read_root_file(root / (direction + "-config.json"), 64 << 10),
            "ledger": read_root_file(root / (direction + "-deployment.json"), 64 << 10),
            "compose": read_root_file(root / (direction + "-compose.yaml"), 256 << 10),
        } for direction in ("source", "target")
    }
    decoded = validate_plan(plan, files)
    state = decode_json(read_root_file(root / "state.json", 16 << 10), "INVALID_ENROLLMENT_STATE")
    require(type(state) is dict and set(state) == {
        "schema", "operation", "planSHA256", "direction", "phase",
    } and state["schema"] == 1 and state["operation"] == plan["operation"] and
            state["planSHA256"] == sha256(plan_raw) and state["direction"] in ("target", "rollback") and
            state["phase"] in PHASES, "INVALID_ENROLLMENT_STATE")
    return root, plan_raw, plan, files, decoded, state


def update_state(root, plan_raw, direction, phase):
    pair.write_atomic(root / "state.json", encode_json(journal_state(plan_raw, direction, phase)),
                      os.geteuid(), os.getegid())


def prepare_plan(root, candidate_compose, current_compose_sha, candidate_compose_sha,
                 codex_tag, node_id, router_lock, consumer_pid):
    installer = host.Installer(root)
    require(set(installer.config["components"]) == {"panel", "cursor"} and
            installer.ledger["pending"] is None, "EXACT_PANEL_CURSOR_ENROLLMENT_REQUIRED")
    require(transition.UUID.fullmatch(node_id), "INVALID_CODEX_NODE_ID")
    config_path = root / "config.json"
    ledger_path = root / "deployment.json"
    compose_path = Path(installer.config["compose"])
    env_path = Path(installer.config["env_file"])
    candidate_compose = pair.canonical_existing(candidate_compose, "INVALID_CANDIDATE_COMPOSE")
    require(candidate_compose.parent == compose_path.parent and candidate_compose != compose_path,
            "CANDIDATE_COMPOSE_MUST_SHARE_LIVE_PARENT")
    candidate_raw = read_root_file(candidate_compose, 256 << 10)
    source = {"config": read_root_file(config_path, 64 << 10),
              "ledger": read_root_file(ledger_path, 64 << 10),
              "compose": read_root_file(compose_path, 256 << 10)}
    env_raw = read_root_file(env_path, 64 << 10)
    require(transition.HEX_SHA256.fullmatch(current_compose_sha) and
            transition.HEX_SHA256.fullmatch(candidate_compose_sha) and
            sha256(source["compose"]) == current_compose_sha and
            sha256(candidate_raw) == candidate_compose_sha,
            "COMPOSE_HASH_MISMATCH")
    manifest = cd.download(codex_tag)
    require(manifest["component"] == "codex", "INVALID_CODEX_TAG")
    require(candidate_raw.count(manifest["image"].encode()) == 1,
            "CANDIDATE_CODEX_DIGEST_NOT_PINNED")
    target_config = copy.deepcopy(installer.config)
    actor_id = target_config["components"]["cursor"]["actor_id"]
    target_config["components"]["codex"] = {
        "service": "codex", "url": "https://codex:18443", "node_id": node_id,
        "actor_id": actor_id,
    }
    target_config_raw = encode_json(target_config)
    target_ledger = copy.deepcopy(installer.ledger)
    target_ledger["components"]["codex"] = {"current": manifest, "previous": None}
    target_ledger["config_sha256"] = fingerprint(target_config_raw, candidate_raw, env_raw)
    target_ledger_raw = encode_json(target_ledger)
    target = {"config": target_config_raw, "ledger": target_ledger_raw, "compose": candidate_raw}
    source_resolved = resolved_compose(installer_view(
        root, installer.config, installer.ledger, compose_path))
    target_view = installer_view(root, target_config, target_ledger, candidate_compose)
    target_resolved = resolved_compose(target_view)
    validate_compose_transition(
        source_resolved, target_resolved, target_config, manifest, candidate_compose)
    evidence = runtime_evidence(
        root, target_config, target_ledger, candidate_compose, manifest,
        resolved=target_resolved)
    plan = {
        "schema": 1, "operation": "enroll-codex-component-cd",
        "paths": {"root": str(root), "compose": str(compose_path),
                  "candidateCompose": str(candidate_compose), "env": str(env_path),
                  "routerLock": str(router_lock), "consumerPid": str(consumer_pid)},
        "codexTag": codex_tag, "codexManifest": manifest, "nodeId": node_id,
        "source": {"configSHA256": sha256(source["config"]),
                   "ledgerSHA256": sha256(source["ledger"]),
                   "composeSHA256": sha256(source["compose"]), "envSHA256": sha256(env_raw)},
        "target": {"configSHA256": sha256(target["config"]),
                   "ledgerSHA256": sha256(target["ledger"]),
                   "composeSHA256": sha256(target["compose"]), "envSHA256": sha256(env_raw)},
        "runtime": evidence,
    }
    return plan, source, target


def paths_for(plan):
    paths = plan["paths"]
    return {"compose": Path(paths["compose"]), "config": Path(paths["root"]) / "config.json",
            "ledger": Path(paths["root"]) / "deployment.json"}


def current_hashes(plan):
    return {name: sha256(read_root_file(path, 256 << 10)) for name, path in paths_for(plan).items()}


def recorded_hashes(plan, direction):
    return {
        name: plan[direction][name + "SHA256" if name != "ledger" else "ledgerSHA256"]
        for name in ORDER
    }


def require_known_matrix(plan, current):
    source = recorded_hashes(plan, "source")
    target = recorded_hashes(plan, "target")
    known = set()
    value = dict(source)
    known.add(tuple(value[name] for name in ORDER))
    for name in ORDER:
        value[name] = target[name]
        known.add(tuple(value[item] for item in ORDER))
    require(tuple(current[name] for name in ORDER) in known, "UNKNOWN_ENROLLMENT_FILE_STATE")


def guard(plan, files):
    require_consumer_stopped(plan["paths"]["consumerPid"])
    socket_parent = Path(decode_json(read_root_file(Path(plan["paths"]["root"]) / "config.json"),
                                     "INVALID_HOST_CONFIG")["router_socket"]).parent
    runtime_owner = socket_parent.stat().st_uid
    require_router_owned(plan["paths"]["routerLock"], runtime_owner)
    env_raw = read_root_file(plan["paths"]["env"], 64 << 10)
    require(sha256(env_raw) == plan["source"]["envSHA256"],
            "ENROLLMENT_ENV_CHANGED")
    source_ledger = decode_json(files["source"]["ledger"], "INVALID_SOURCE_LEDGER")
    target_ledger = decode_json(files["target"]["ledger"], "INVALID_TARGET_LEDGER")
    require(source_ledger["config_sha256"] == fingerprint(
        files["source"]["config"], files["source"]["compose"], env_raw),
            "INVALID_SOURCE_FINGERPRINT")
    require(target_ledger["config_sha256"] == fingerprint(
        files["target"]["config"], files["target"]["compose"], env_raw),
            "INVALID_TARGET_FINGERPRINT")


def compare_runtime(expected, observed, allow_activated=False):
    require(observed["containers"] == expected["containers"] and
            observed["cursorIdentity"] == expected["cursorIdentity"] and
            observed["codexIdentity"] == expected["codexIdentity"] and
            observed["cursorRoute"] == expected["cursorRoute"] and
            observed["registryVersion"] == expected["registryVersion"] and
            observed["registrySHA256"] == expected["registrySHA256"] and
            observed["composeConfigSHA256"] == expected["composeConfigSHA256"],
            "ENROLLMENT_RUNTIME_CHANGED")
    if not allow_activated:
        require(observed["codexRoute"] == expected["codexRoute"], "CODEX_ROUTE_CHANGED")


def replace_file(plan, files, direction, name):
    live = paths_for(plan)[name]
    info = pair.private_file_info(live, os.geteuid())
    raw = files["target" if direction == "target" else "source"][name]
    pair.replace_runtime_file(live, raw, info, name)
    require(sha256(read_root_file(live, 256 << 10)) == sha256(raw), "ENROLLMENT_FILE_READBACK_FAILED")


def router_state(plan):
    return router_control(decode_json(
        read_root_file(Path(plan["paths"]["root"]) / "config.json"),
        "INVALID_HOST_CONFIG")["router_socket"]).status()


def require_sealed_for_rollback(plan):
    state = router_state(plan)
    config = decode_json(read_root_file(Path(plan["paths"]["root"]) / "config.json"), "INVALID_HOST_CONFIG")
    codex_id = plan["nodeId"]
    require(state["nodes"].get(codex_id) == plan["runtime"]["codexRoute"],
            "CODEX_ALREADY_ACTIVATED_ROLLBACK_FORBIDDEN")
    cursor_id = config["components"]["cursor"]["node_id"]
    require(state["nodes"].get(cursor_id) == plan["runtime"]["cursorRoute"], "CURSOR_ROUTE_CHANGED")


def activate_codex(journal, plan_raw, plan, target_config, target_ledger,
                   allow_sealed=True, persist_pending=True):
    require(current_hashes(plan) == recorded_hashes(plan, "target"),
            "TARGET_ENROLLMENT_READBACK_MISMATCH")
    manifest = plan["codexManifest"]
    observed = runtime_evidence(plan["paths"]["root"], target_config, target_ledger,
                                plan["paths"]["compose"], manifest, allow_activated=True,
                                provenance_compose_path=plan["paths"]["candidateCompose"])
    compare_runtime(plan["runtime"], observed, allow_activated=True)
    sealed = plan["runtime"]["codexRoute"]
    identity = plan["runtime"]["codexIdentity"]
    expected = host.RouterControl.projected("activate", sealed, "bootstrap", identity)
    route = observed["codexRoute"]
    require(route in (sealed, expected), "CODEX_ACTIVATION_STATE_UNKNOWN")
    require(allow_sealed or route == expected, "COMPLETED_ACTIVATION_CHANGED")
    if persist_pending:
        update_state(journal, plan_raw, "target", "activation-pending")
    if route == sealed:
        checkpoint("activation.before_request")
        control = router_control(target_config["router_socket"])
        result = control.transition_many("activate", {plan["nodeId"]: sealed}, "bootstrap",
                                         {plan["nodeId"]: identity})
        checkpoint("activation.after_request")
        require(result == {plan["nodeId"]: expected}, "CODEX_ACTIVATION_READBACK_MISMATCH")
    final = runtime_evidence(plan["paths"]["root"], target_config, target_ledger,
                             plan["paths"]["compose"], manifest, allow_activated=True,
                             provenance_compose_path=plan["paths"]["candidateCompose"])
    compare_runtime(plan["runtime"], final, allow_activated=True)
    require(final["codexRoute"] == expected, "CODEX_ACTIVATION_READBACK_MISMATCH")
    require(current_hashes(plan) == recorded_hashes(plan, "target"),
            "TARGET_ENROLLMENT_READBACK_MISMATCH")


def execute(direction, journal, plan_raw, plan, files, decoded, prior_state):
    require(direction in ("target", "rollback"), "RECOVERY_DIRECTION_REQUIRED")
    _, target_config, _, target_ledger = decoded
    guard(plan, files)
    current = current_hashes(plan)
    require_known_matrix(plan, current)
    target_hashes = recorded_hashes(plan, "target")
    resume_activated = (direction == "target" and prior_state["direction"] == "target" and
                        prior_state["phase"] in ("activation-pending", "completed") and
                        current == target_hashes)
    if resume_activated:
        installer = host.Installer(Path(plan["paths"]["root"]))
        require(installer.config == target_config and installer.ledger == target_ledger,
                "TARGET_ENROLLMENT_READBACK_MISMATCH")
        activate_codex(journal, plan_raw, plan, target_config, target_ledger,
                       allow_sealed=prior_state["phase"] == "activation-pending",
                       persist_pending=prior_state["phase"] != "completed")
        update_state(journal, plan_raw, direction, "completed")
        return {"status": "CODEX_COMPONENT_CD_ENROLLED", "direction": "target",
                "activated": True}
    if direction == "rollback":
        require_sealed_for_rollback(plan)
        update_state(journal, plan_raw, direction, "prepared")
        for name, phase in (("ledger", "ledger-replaced"), ("config", "config-replaced"),
                            ("compose", "compose-replaced")):
            guard(plan, files)
            if current[name] != plan["source"][name + "SHA256" if name != "ledger" else "ledgerSHA256"]:
                replace_file(plan, files, direction, name)
            current = current_hashes(plan)
            require_known_matrix(plan, current)
            update_state(journal, plan_raw, direction, phase)
        host.Installer(Path(plan["paths"]["root"]))
        update_state(journal, plan_raw, direction, "completed")
        return {"status": "CODEX_COMPONENT_CD_RECOVERED", "direction": "rollback",
                "activated": False}
    update_state(journal, plan_raw, direction, "prepared")
    for name, phase in (("compose", "compose-replaced"), ("config", "config-replaced"),
                        ("ledger", "ledger-replaced")):
        guard(plan, files)
        if current[name] != plan["target"][name + "SHA256" if name != "ledger" else "ledgerSHA256"]:
            replace_file(plan, files, direction, name)
        current = current_hashes(plan)
        require_known_matrix(plan, current)
        update_state(journal, plan_raw, direction, phase)
        if name == "compose":
            observed = runtime_evidence(plan["paths"]["root"], target_config, target_ledger,
                                        plan["paths"]["compose"], plan["codexManifest"],
                                        allow_activated=False,
                                        provenance_compose_path=plan["paths"]["candidateCompose"])
            compare_runtime(plan["runtime"], observed, allow_activated=False)
    installer = host.Installer(Path(plan["paths"]["root"]))
    require(installer.config == target_config and installer.ledger == target_ledger,
            "TARGET_ENROLLMENT_READBACK_MISMATCH")
    activate_codex(journal, plan_raw, plan, target_config, target_ledger)
    update_state(journal, plan_raw, direction, "completed")
    return {"status": "CODEX_COMPONENT_CD_ENROLLED", "direction": "target", "activated": True}


def enroll(action, direction, root, journal, router_lock, consumer_pid,
           candidate_compose=None, current_compose_sha=None, candidate_compose_sha=None,
           codex_tag=None, node_id=None):
    require_root()
    root = pair.private_directory(root, os.geteuid())
    lock = pair.acquire_lock(root / "deploy.lock", os.geteuid(), "DEPLOY_LOCK_BUSY")
    try:
        require_consumer_stopped(consumer_pid)
        if action == "apply":
            require(not os.path.lexists(journal), "ENROLLMENT_JOURNAL_ALREADY_EXISTS")
            installer = host.Installer(root)
            runtime_owner = Path(installer.config["router_socket"]).parent.stat().st_uid
            require_router_owned(router_lock, runtime_owner)
            plan, source, target = prepare_plan(
                root, candidate_compose, current_compose_sha, candidate_compose_sha,
                codex_tag, node_id, router_lock, consumer_pid,
            )
            publish_journal(journal, plan, source, target)
        else:
            require(action == "recover" and direction in ("target", "rollback"),
                    "RECOVERY_DIRECTION_REQUIRED")
        journal, plan_raw, plan, files, decoded, state = load_journal(journal)
        require(plan["paths"]["root"] == str(root) and
                plan["paths"]["routerLock"] == str(router_lock) and
                plan["paths"]["consumerPid"] == str(consumer_pid),
                "ENROLLMENT_TARGET_MISMATCH")
        return execute("target" if action == "apply" else direction,
                       journal, plan_raw, plan, files, decoded, state)
    except (OSError, pair.InstallError, transition.TransitionError) as error:
        code = str(error)
        if isinstance(error, OSError):
            code = "ENROLLMENT_STATE_UNKNOWN"
        raise EnrollmentError(code) from None
    finally:
        pair.release_locks([lock])


def add_common(parser):
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--journal", required=True, type=Path)
    parser.add_argument("--router-lock", required=True, type=Path)
    parser.add_argument("--consumer-pid-file", required=True, type=Path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    apply_parser = commands.add_parser("apply")
    add_common(apply_parser)
    apply_parser.add_argument("--candidate-compose", required=True, type=Path)
    apply_parser.add_argument("--expected-current-compose-sha256", required=True)
    apply_parser.add_argument("--candidate-compose-sha256", required=True)
    apply_parser.add_argument("--codex-tag", required=True)
    apply_parser.add_argument("--codex-node-id", required=True)
    recover_parser = commands.add_parser("recover")
    add_common(recover_parser)
    recover_parser.add_argument("--direction", required=True, choices=("target", "rollback"))
    args = parser.parse_args()
    os.umask(0o077)
    result = enroll(
        args.action, getattr(args, "direction", None), args.root, args.journal,
        args.router_lock, args.consumer_pid_file,
        getattr(args, "candidate_compose", None),
        getattr(args, "expected_current_compose_sha256", None),
        getattr(args, "candidate_compose_sha256", None),
        getattr(args, "codex_tag", None), getattr(args, "codex_node_id", None),
    )
    print(json.dumps(result, separators=(",", ":"), sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, EnrollmentError) else "CODEX_COMPONENT_CD_ENROLLMENT_FAILED")
        raise SystemExit(1) from None
