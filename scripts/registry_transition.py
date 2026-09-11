#!/usr/bin/env python3
"""Prepare an offline, fail-closed Cursor-to-Cursor+Codex registry transition."""

import argparse
import base64
import copy
import ctypes
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import urllib.parse


MAXIMUM_SAFE_INTEGER = 9007199254740991
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}")
ACTOR = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}")
HEX_SHA256 = re.compile(r"[0-9a-f]{64}")
VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
OPERATION = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}")
NODE_FIELDS = {"nodeId", "name", "adapter", "url", "certificateSHA256"}
STATE_NODE_FIELDS = {"mode", "stateVersion", "generation", "identityEpoch", "adapterKind", "adapterVersion"}


class TransitionError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise TransitionError(code)


def pairs(items):
    value = {}
    for key, item in items:
        require(key not in value, "DUPLICATE_JSON_KEY")
        value[key] = item
    return value


def read_regular(path, maximum, private=False):
    path = Path(path)
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except OSError:
        raise TransitionError("INVALID_INPUT_FILE") from None
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISREG(info.st_mode) and info.st_size <= maximum, "INVALID_INPUT_FILE")
        if private:
            require(info.st_mode & 0o077 == 0, "PRIVATE_INPUT_REQUIRED")
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            content = stream.read(maximum + 1)
        require(len(content) <= maximum, "INVALID_INPUT_FILE")
        return content
    finally:
        os.close(descriptor)


def read_json(path, maximum=256 << 10, private=False):
    try:
        return json.loads(read_regular(path, maximum, private), object_pairs_hook=pairs)
    except TransitionError:
        raise
    except Exception:
        raise TransitionError("INVALID_JSON") from None


def safe_integer(value, minimum=0):
    return type(value) is int and minimum <= value <= MAXIMUM_SAFE_INTEGER


def canonical_manifest(manifest):
    ordered = {
        "registryVersion": manifest["registryVersion"],
        "ownerId": manifest["ownerId"],
        "mode": manifest["mode"],
        "nodes": [{key: node[key] for key in
                   ("nodeId", "name", "adapter", "url", "certificateSHA256")}
                  for node in manifest["nodes"]],
    }
    try:
        encoded = json.dumps(ordered, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
        # Match encoding/json's default HTML and JavaScript line-separator escaping.
        encoded = (encoded.replace("&", "\\u0026").replace("<", "\\u003c").replace(">", "\\u003e")
                   .replace("\u2028", "\\u2028").replace("\u2029", "\\u2029"))
        return encoded.encode("utf-8")
    except Exception:
        raise TransitionError("INVALID_REGISTRY_TEXT") from None


def validate_url(value):
    require(type(value) is str and len(value) <= 2048, "INVALID_NODE_URL")
    try:
        parsed = urllib.parse.urlsplit(value)
        port = parsed.port
    except ValueError:
        raise TransitionError("INVALID_NODE_URL") from None
    require(parsed.scheme == "https" and parsed.hostname and parsed.username is None and
            parsed.password is None and parsed.path == "" and parsed.query == "" and
            parsed.fragment == "" and (port is None or 1 <= port <= 65535), "INVALID_NODE_URL")
    return parsed.hostname.lower(), port or 443


def validate_node(node):
    require(type(node) is dict and set(node) == NODE_FIELDS, "INVALID_REGISTRY_NODE")
    require(type(node["nodeId"]) is str and UUID.fullmatch(node["nodeId"]), "INVALID_NODE_ID")
    name = node["name"]
    require(type(name) is str and name and len(name.encode("utf-8")) <= 200 and
            not any(mark in name for mark in ("\x00", "\r", "\n")), "INVALID_NODE_NAME")
    require(node["adapter"] in ("cursor", "codex"), "INVALID_NODE_ADAPTER")
    validate_url(node["url"])
    require(type(node["certificateSHA256"]) is str and HEX_SHA256.fullmatch(node["certificateSHA256"]),
            "INVALID_NODE_CERTIFICATE_PIN")


def openssl_run(executable, *arguments, input_data=None):
    try:
        result = subprocess.run([executable, *arguments], input=input_data, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=15, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise TransitionError("OPENSSL_FAILED") from None
    require(result.returncode == 0, "OPENSSL_REJECTED_INPUT")
    return result.stdout


def verify_signature(executable, public_key, payload, signature):
    with tempfile.TemporaryDirectory(prefix="registry-transition-verify-") as directory:
        root = Path(directory)
        message, signed = root / "manifest.json", root / "signature.bin"
        message.write_bytes(payload)
        signed.write_bytes(signature)
        openssl_run(executable, "pkeyutl", "-verify", "-pubin", "-inkey", str(public_key),
                    "-rawin", "-in", str(message), "-sigfile", str(signed))


def sign(executable, private_key, payload):
    with tempfile.TemporaryDirectory(prefix="registry-transition-sign-") as directory:
        message = Path(directory) / "manifest.json"
        message.write_bytes(payload)
        signature = openssl_run(executable, "pkeyutl", "-sign", "-rawin", "-inkey",
                                str(private_key), "-in", str(message))
    require(len(signature) == 64, "INVALID_SIGNER_KEY")
    return signature


def validate_registry(envelope, executable, public_key):
    require(type(envelope) is dict and set(envelope) == {"manifest", "signature"},
            "INVALID_REGISTRY")
    manifest = envelope["manifest"]
    require(type(manifest) is dict and set(manifest) == {"registryVersion", "ownerId", "mode", "nodes"},
            "INVALID_REGISTRY")
    require(safe_integer(manifest["registryVersion"], 1) and type(manifest["ownerId"]) is str and
            ACTOR.fullmatch(manifest["ownerId"]) and manifest["mode"] == "live" and
            type(manifest["nodes"]) is list and 0 < len(manifest["nodes"]) <= 16,
            "INVALID_REGISTRY_IDENTITY")
    node_ids, pins = set(), set()
    for node in manifest["nodes"]:
        validate_node(node)
        require(node["nodeId"] not in node_ids, "DUPLICATE_NODE_ID")
        require(node["certificateSHA256"] not in pins, "DUPLICATE_NODE_CERTIFICATE")
        node_ids.add(node["nodeId"])
        pins.add(node["certificateSHA256"])
    require(type(envelope["signature"]) is str, "INVALID_REGISTRY_SIGNATURE")
    try:
        signature = base64.b64decode(envelope["signature"], validate=True)
    except Exception:
        raise TransitionError("INVALID_REGISTRY_SIGNATURE") from None
    require(len(signature) == 64, "INVALID_REGISTRY_SIGNATURE")
    payload = canonical_manifest(manifest)
    verify_signature(executable, public_key, payload, signature)
    return manifest, hashlib.sha256(payload).hexdigest()


def validate_state(state, manifest, manifest_sha256):
    require(type(state) is dict and set(state) ==
            {"schema", "ownerId", "registryVersion", "registrySHA256", "nodes"},
            "INVALID_ROUTER_STATE")
    require(state["schema"] == 1 and state["ownerId"] == manifest["ownerId"] and
            state["registryVersion"] == manifest["registryVersion"] and
            state["registrySHA256"] == manifest_sha256 and type(state["nodes"]) is dict,
            "REGISTRY_STATE_DRIFT")
    registry_nodes = {node["nodeId"]: node for node in manifest["nodes"]}
    require(set(state["nodes"]) == set(registry_nodes), "REGISTRY_STATE_DRIFT")
    for node_id, node_state in state["nodes"].items():
        require(type(node_state) is dict, "INVALID_ROUTER_NODE_STATE")
        fields = set(node_state)
        base = STATE_NODE_FIELDS
        require(fields in (base, base | {"operationId"}), "INVALID_ROUTER_NODE_STATE")
        require(node_state["adapterKind"] == registry_nodes[node_id]["adapter"] and
                safe_integer(node_state["stateVersion"], 1) and
                safe_integer(node_state["generation"]) and
                safe_integer(node_state["identityEpoch"]) and
                type(node_state["adapterVersion"]) is str,
                "INVALID_ROUTER_NODE_STATE")
        mode = node_state["mode"]
        if mode == "eligible":
            require(fields == base and node_state["generation"] >= 1 and
                    node_state["identityEpoch"] >= 1 and VERSION.fullmatch(node_state["adapterVersion"]),
                    "INVALID_ROUTER_NODE_STATE")
        elif mode in ("draining", "sealed"):
            operation = node_state.get("operationId")
            require(type(operation) is str and OPERATION.fullmatch(operation),
                    "INVALID_ROUTER_NODE_STATE")
            initial = (mode == "sealed" and operation == "bootstrap" and
                       node_state["generation"] == 0 and node_state["identityEpoch"] == 0 and
                       node_state["adapterVersion"] == "")
            require(initial or (node_state["generation"] >= 1 and node_state["identityEpoch"] >= 1 and
                                VERSION.fullmatch(node_state["adapterVersion"])),
                    "INVALID_ROUTER_NODE_STATE")
        else:
            raise TransitionError("INVALID_ROUTER_NODE_STATE")


def validate_codex_config(config, manifest):
    require(type(config) is dict and config.get("adapter") == "codex" and
            type(config.get("codex")) is dict and "cursor" not in config,
            "INVALID_CODEX_NODE_CONFIG")
    require(config.get("ownerId") == manifest["ownerId"] and
            config.get("registryVersion") == manifest["registryVersion"],
            "CODEX_IDENTITY_MISMATCH")
    require(type(config.get("nodeId")) is str and UUID.fullmatch(config["nodeId"]),
            "INVALID_CODEX_NODE_ID")
    return config["nodeId"]


def certificate_pin(executable, certificate, ca, hostname):
    openssl_run(executable, "verify", "-CAfile", str(ca), "-purpose", "sslserver",
                "-verify_hostname", hostname, str(certificate))
    der = openssl_run(executable, "x509", "-in", str(certificate), "-outform", "DER")
    require(der, "INVALID_CODEX_CERTIFICATE")
    return hashlib.sha256(der).hexdigest()


def write_private(path, content):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(descriptor, "wb", closefd=False) as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        os.close(descriptor)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def open_output_parent(output):
    require(output.is_absolute() and output != Path(output.anchor) and
            not os.path.lexists(output), "NEW_ABSOLUTE_OUTPUT_REQUIRED")
    parent = output.parent
    try:
        require(parent.resolve(strict=True) == parent, "INVALID_OUTPUT_PARENT")
        descriptor = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    except (OSError, RuntimeError):
        raise TransitionError("INVALID_OUTPUT_PARENT") from None
    info = os.fstat(descriptor)
    if not (stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid() and
            stat.S_IMODE(info.st_mode) == 0o700):
        os.close(descriptor)
        raise TransitionError("INVALID_OUTPUT_PARENT")
    return parent, descriptor, (info.st_dev, info.st_ino)


def require_same_directory(path, descriptor, identity):
    try:
        path_info = path.lstat()
        descriptor_info = os.fstat(descriptor)
    except OSError:
        raise TransitionError("OUTPUT_PARENT_CHANGED") from None
    require(not path.is_symlink() and stat.S_ISDIR(path_info.st_mode) and
            path_info.st_uid == os.geteuid() and stat.S_IMODE(path_info.st_mode) == 0o700 and
            stat.S_ISDIR(descriptor_info.st_mode) and descriptor_info.st_uid == os.geteuid() and
            stat.S_IMODE(descriptor_info.st_mode) == 0o700 and
            (path_info.st_dev, path_info.st_ino) == identity and
            (descriptor_info.st_dev, descriptor_info.st_ino) == identity,
            "OUTPUT_PARENT_CHANGED")


def rename_no_replace(parent_fd, source_name, destination_name):
    """Atomically publish a directory without replacing a raced destination."""
    library = ctypes.CDLL(None, use_errno=True)
    source_raw, destination_raw = os.fsencode(source_name), os.fsencode(destination_name)
    if sys.platform == "darwin":
        operation = library.renameatx_np
        operation.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int,
                              ctypes.c_char_p, ctypes.c_uint)
        operation.restype = ctypes.c_int
        result = operation(parent_fd, source_raw, parent_fd, destination_raw,
                           0x00000004)  # RENAME_EXCL
    else:
        operation = getattr(library, "renameat2", None)
        require(operation is not None, "NO_REPLACE_RENAME_UNAVAILABLE")
        operation.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int,
                              ctypes.c_char_p, ctypes.c_uint)
        operation.restype = ctypes.c_int
        result = operation(parent_fd, source_raw, parent_fd, destination_raw,
                           1)  # RENAME_NOREPLACE
    if result == 0:
        return
    failure = ctypes.get_errno()
    if failure in (errno.EEXIST, errno.ENOTEMPTY):
        raise TransitionError("OUTPUT_ALREADY_EXISTS")
    raise TransitionError("REGISTRY_TRANSITION_FAILED")


def snapshot_openssl_inputs(input_bytes):
    directory = tempfile.TemporaryDirectory(prefix="registry-transition-inputs-")
    root = Path(directory.name)
    try:
        root.chmod(0o700)
        snapshots = {}
        for name, content in input_bytes.items():
            path = root / name
            write_private(path, content)
            snapshots[name] = path
        sync_directory(root)
        return directory, snapshots
    except Exception:
        directory.cleanup()
        raise


def encode_json(value):
    return (json.dumps(value, ensure_ascii=True, separators=(",", ":"), sort_keys=True) + "\n").encode()


def prepare(registry_path, state_path, public_key, private_key, ca, codex_config_path,
            codex_certificate, codex_name, codex_url, output, openssl="openssl"):
    paths = [Path(value) for value in (registry_path, state_path, public_key, private_key, ca,
                                      codex_config_path, codex_certificate)]
    registry_path, state_path, public_key, private_key, ca, codex_config_path, codex_certificate = paths
    output = Path(output)
    executable = shutil.which(openssl)
    require(executable is not None, "OPENSSL_REQUIRED")
    input_limits = ((public_key, 16 << 10, False), (private_key, 16 << 10, True),
                    (ca, 256 << 10, False), (codex_certificate, 64 << 10, False),
                    (codex_config_path, 64 << 10, True))
    input_bytes = {path: read_regular(path, maximum, private)
                   for path, maximum, private in input_limits}
    input_hashes = {path: hashlib.sha256(content).digest() for path, content in input_bytes.items()}
    require(state_path.is_absolute() and state_path.parent != Path(state_path.anchor),
            "INVALID_ROUTER_STATE_PATH")
    lock_path = state_path.parent / "router.lock"
    read_regular(lock_path, 4096, private=True)
    parent, parent_fd, parent_identity = open_output_parent(output)
    try:
        lock_fd = os.open(lock_path, os.O_RDWR | os.O_NOFOLLOW)
    except OSError:
        os.close(parent_fd)
        raise TransitionError("INVALID_INPUT_FILE") from None
    temporary = None
    snapshot_directory = None
    try:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise TransitionError("ROUTER_MUST_BE_OFFLINE") from None
        snapshot_directory, snapshots = snapshot_openssl_inputs({
            "signer-public.pem": input_bytes[public_key],
            "signer-private.pem": input_bytes[private_key],
            "ca.pem": input_bytes[ca],
            "codex-certificate.pem": input_bytes[codex_certificate],
        })
        snapshot_public_key = snapshots["signer-public.pem"]
        snapshot_private_key = snapshots["signer-private.pem"]
        snapshot_ca = snapshots["ca.pem"]
        snapshot_codex_certificate = snapshots["codex-certificate.pem"]
        source_registry = read_regular(registry_path, 256 << 10)
        source_state = read_regular(state_path, 256 << 10, private=True)
        registry = json.loads(source_registry, object_pairs_hook=pairs)
        state = json.loads(source_state, object_pairs_hook=pairs)
        manifest, source_manifest_sha = validate_registry(registry, executable, snapshot_public_key)
        validate_state(state, manifest, source_manifest_sha)
        require(len(manifest["nodes"]) == 1 and manifest["nodes"][0]["adapter"] == "cursor",
                "EXPECTED_SINGLE_CURSOR_REGISTRY")
        try:
            codex_config = json.loads(input_bytes[codex_config_path], object_pairs_hook=pairs)
        except TransitionError:
            raise
        except Exception:
            raise TransitionError("INVALID_JSON") from None
        codex_node_id = validate_codex_config(codex_config, manifest)
        hostname, _ = validate_url(codex_url)
        pin = certificate_pin(executable, snapshot_codex_certificate, snapshot_ca, hostname)
        cursor = manifest["nodes"][0]
        require(codex_node_id != cursor["nodeId"], "DUPLICATE_NODE_ID")
        require(pin != cursor["certificateSHA256"], "DUPLICATE_NODE_CERTIFICATE")
        require(validate_url(codex_url) != validate_url(cursor["url"]),
                "DUPLICATE_NODE_ENDPOINT")
        candidate = copy.deepcopy(manifest)
        candidate["nodes"].append({"nodeId": codex_node_id, "name": codex_name, "adapter": "codex",
                                   "url": codex_url, "certificateSHA256": pin})
        validate_node(candidate["nodes"][1])
        target_manifest = canonical_manifest(candidate)
        target_signature = sign(executable, snapshot_private_key, target_manifest)
        verify_signature(executable, snapshot_public_key, target_manifest, target_signature)
        target_registry = {"manifest": candidate,
                           "signature": base64.b64encode(target_signature).decode("ascii")}
        target_manifest_sha = hashlib.sha256(target_manifest).hexdigest()
        target_state = copy.deepcopy(state)
        target_state["registrySHA256"] = target_manifest_sha
        target_state["nodes"][codex_node_id] = {
            "mode": "sealed", "stateVersion": 1, "generation": 0,
            "operationId": "bootstrap", "identityEpoch": 0,
            "adapterKind": "codex", "adapterVersion": "",
        }
        validate_state(target_state, candidate, target_manifest_sha)
        target_registry_raw, target_state_raw = encode_json(target_registry), encode_json(target_state)
        metadata = {
            "schema": 1, "operation": "add-codex-node", "ownerId": manifest["ownerId"],
            "registryVersion": manifest["registryVersion"], "cursorNodeId": cursor["nodeId"],
            "codexNodeId": codex_node_id,
            "source": {"registrySHA256": hashlib.sha256(source_registry).hexdigest(),
                       "routerStateSHA256": hashlib.sha256(source_state).hexdigest(),
                       "manifestSHA256": source_manifest_sha},
            "next": {"registrySHA256": hashlib.sha256(target_registry_raw).hexdigest(),
                     "routerStateSHA256": hashlib.sha256(target_state_raw).hexdigest(),
                     "manifestSHA256": target_manifest_sha},
        }
        temporary = Path(tempfile.mkdtemp(prefix="." + output.name + "-", dir=parent))
        temporary.chmod(0o700)
        for directory in (temporary / "next", temporary / "rollback"):
            directory.mkdir(mode=0o700)
        write_private(temporary / "next" / "registry.json", target_registry_raw)
        write_private(temporary / "next" / "state.json", target_state_raw)
        write_private(temporary / "rollback" / "registry.json", source_registry)
        write_private(temporary / "rollback" / "state.json", source_state)
        write_private(temporary / "transition.json", encode_json(metadata))
        for directory in (temporary / "next", temporary / "rollback", temporary):
            sync_directory(directory)
        require(read_regular(registry_path, 256 << 10) == source_registry and
                read_regular(state_path, 256 << 10, private=True) == source_state,
                "SOURCE_CHANGED_DURING_PREPARATION")
        require(all(hashlib.sha256(read_regular(path, maximum, private)).digest() == input_hashes[path]
                    for path, maximum, private in input_limits), "SOURCE_CHANGED_DURING_PREPARATION")
        require_same_directory(parent, parent_fd, parent_identity)
        rename_no_replace(parent_fd, temporary.name, output.name)
        temporary = None
        try:
            os.fsync(parent_fd)
        except OSError:
            raise TransitionError("PUBLICATION_DURABILITY_UNKNOWN") from None
        return metadata
    except (TransitionError, json.JSONDecodeError):
        raise
    except Exception:
        raise TransitionError("REGISTRY_TRANSITION_FAILED") from None
    finally:
        if temporary is not None:
            shutil.rmtree(temporary)
        if snapshot_directory is not None:
            snapshot_directory.cleanup()
        fcntl.flock(lock_fd, fcntl.LOCK_UN)
        os.close(lock_fd)
        os.close(parent_fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--registry", required=True, type=Path)
    parser.add_argument("--router-state", required=True, type=Path)
    parser.add_argument("--signer-public-key", required=True, type=Path)
    parser.add_argument("--signer-private-key", required=True, type=Path)
    parser.add_argument("--ca", required=True, type=Path)
    parser.add_argument("--codex-node-config", required=True, type=Path)
    parser.add_argument("--codex-certificate", required=True, type=Path)
    parser.add_argument("--codex-name", required=True)
    parser.add_argument("--codex-url", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--openssl", default="openssl")
    args = parser.parse_args()
    os.umask(0o077)
    metadata = prepare(args.registry, args.router_state, args.signer_public_key,
                       args.signer_private_key, args.ca, args.codex_node_config,
                       args.codex_certificate, args.codex_name, args.codex_url,
                       args.output, args.openssl)
    print(json.dumps({"status": "REGISTRY_TRANSITION_PREPARED",
                      "codexNodeId": metadata["codexNodeId"],
                      "manifestSHA256": metadata["next"]["manifestSHA256"]},
                     separators=(",", ":")))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, TransitionError) else "REGISTRY_TRANSITION_FAILED")
        raise SystemExit(1) from None
