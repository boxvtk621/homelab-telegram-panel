#!/usr/bin/env python3
"""Prepare one isolated Codex Harness node under the existing alpha trust roots."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import uuid

import registry_transition as transition


MODEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
EFFORTS = {"minimal", "low", "medium", "high", "xhigh"}
POLICY = ("You are the owner's autonomous assistant. Answer the user's request clearly and "
          "concisely. This chat alpha has no tools; do not claim to have executed commands or "
          "changed external systems.\n")


class ProvisionError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise ProvisionError(code)


def run(executable, *arguments, input_data=None):
    try:
        result = subprocess.run([executable, *arguments], input=input_data, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=20, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ProvisionError("OPENSSL_FAILED") from None
    require(result.returncode == 0, "OPENSSL_REJECTED_INPUT")
    return result.stdout


def check(executable, *arguments):
    try:
        result = subprocess.run([executable, *arguments], stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, timeout=20, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ProvisionError("OPENSSL_FAILED") from None
    return result.returncode == 0


def write_owned(path, content, uid, gid):
    transition.write_private(path, content)
    os.chown(path, uid, gid)
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def mkdir_owned(path, uid, gid):
    path.mkdir(mode=0o700)
    os.chown(path, uid, gid)


def require_owned(path, uid, gid, directory=False):
    info = path.lstat()
    expected = stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)
    require(expected and not path.is_symlink() and info.st_uid == uid and info.st_gid == gid and
            stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600),
            "INVALID_OUTPUT_PERMISSIONS")


def json_bytes(value):
    return (json.dumps(value, ensure_ascii=True, separators=(",", ":"), sort_keys=True) + "\n").encode()


def prepare(registry_path, signer_public_key, ca_certificate, ca_private_key,
            cursor_node_config, gateway_certificate, output, model, effort,
            valid_days=14, node_id=None, runtime_uid=10001, runtime_gid=10001,
            openssl="openssl"):
    paths = [Path(value) for value in (registry_path, signer_public_key, ca_certificate,
                                       ca_private_key, cursor_node_config, gateway_certificate)]
    registry_path, signer_public_key, ca_certificate, ca_private_key, cursor_node_config, gateway_certificate = paths
    output = Path(output)
    require(len(set(paths)) == len(paths) and all(path.is_absolute() and
            path.resolve(strict=True) == path for path in paths), "INVALID_INPUT_PATH")
    require(type(valid_days) is int and 1 <= valid_days <= 21, "INVALID_CERTIFICATE_LIFETIME")
    require(type(runtime_uid) is int and type(runtime_gid) is int and
            runtime_uid > 0 and runtime_gid > 0, "INVALID_RUNTIME_IDENTITY")
    require(type(model) is str and MODEL.fullmatch(model), "INVALID_CODEX_MODEL")
    require(effort in EFFORTS, "INVALID_CODEX_EFFORT")
    executable = shutil.which(openssl)
    require(executable is not None, "OPENSSL_REQUIRED")
    require(run(executable, "version").startswith(b"OpenSSL 3."), "OPENSSL_3_REQUIRED")

    limits = ((registry_path, 256 << 10, False), (signer_public_key, 16 << 10, False),
              (ca_certificate, 256 << 10, False), (ca_private_key, 16 << 10, True),
              (cursor_node_config, 64 << 10, True), (gateway_certificate, 64 << 10, False))
    source = {path: transition.read_regular(path, maximum, private)
              for path, maximum, private in limits}
    try:
        parent, parent_fd, parent_identity = transition.open_output_parent(output)
    except transition.TransitionError as error:
        raise ProvisionError(str(error)) from None
    input_snapshot = None
    staging = None
    published = False
    try:
        input_snapshot, snapshots = transition.snapshot_openssl_inputs({
            "registry-signing.pem": source[signer_public_key],
            "ca.pem": source[ca_certificate],
            "ca.key": source[ca_private_key],
            "gateway.pem": source[gateway_certificate],
        })
        public = snapshots["registry-signing.pem"]
        ca = snapshots["ca.pem"]
        ca_key = snapshots["ca.key"]
        gateway = snapshots["gateway.pem"]
        try:
            envelope = json.loads(source[registry_path], object_pairs_hook=transition.pairs)
            cursor = json.loads(source[cursor_node_config], object_pairs_hook=transition.pairs)
        except transition.TransitionError:
            raise
        except Exception:
            raise ProvisionError("INVALID_JSON") from None
        manifest, _ = transition.validate_registry(envelope, executable, public)
        require(len(manifest["nodes"]) == 1 and manifest["nodes"][0]["adapter"] == "cursor",
                "EXPECTED_SINGLE_CURSOR_REGISTRY")
        require(type(cursor) is dict and cursor.get("nodeId") == manifest["nodes"][0]["nodeId"] and
                cursor.get("ownerId") == manifest["ownerId"] and
                cursor.get("registryVersion") == manifest["registryVersion"],
                "CURSOR_IDENTITY_MISMATCH")
        gateway_pin = cursor.get("gatewayCertificateSHA256")
        require(type(gateway_pin) is str and transition.HEX_SHA256.fullmatch(gateway_pin),
                "INVALID_GATEWAY_PIN")
        require(check(executable, "verify", "-CAfile", str(ca), "-purpose", "sslclient", str(gateway)),
                "INVALID_GATEWAY_CERTIFICATE")
        gateway_der = run(executable, "x509", "-in", str(gateway), "-outform", "DER")
        require(hashlib.sha256(gateway_der).hexdigest() == gateway_pin, "GATEWAY_PIN_MISMATCH")
        private_public = run(executable, "pkey", "-in", str(ca_key), "-pubout", "-outform", "DER")
        certificate_public_pem = run(executable, "x509", "-in", str(ca), "-pubkey", "-noout")
        certificate_public = run(executable, "pkey", "-pubin", "-outform", "DER",
                                 input_data=certificate_public_pem)
        require(private_public == certificate_public, "CA_KEY_MISMATCH")
        require(check(executable, "x509", "-checkend", str(valid_days * 86400 + 3600),
                      "-noout", "-in", str(ca)), "CA_VALIDITY_TOO_SHORT")
        selected_id = node_id or str(uuid.uuid4())
        require(type(selected_id) is str and transition.UUID.fullmatch(selected_id) and
                selected_id != manifest["nodes"][0]["nodeId"], "INVALID_CODEX_NODE_ID")

        staging = Path(tempfile.mkdtemp(prefix="." + output.name + "-", dir=parent))
        staging.chmod(0o700)
        generation = Path(tempfile.mkdtemp(prefix="codex-node-certificate-"))
        try:
            generation.chmod(0o700)
            key = generation / "node.key"
            request = generation / "node.csr"
            extension = generation / "node.ext"
            extension.write_text("basicConstraints=critical,CA:FALSE\n"
                                 "keyUsage=critical,digitalSignature\n"
                                 "extendedKeyUsage=serverAuth\n"
                                 "subjectAltName=DNS:codex\n")
            run(executable, "req", "-new", "-newkey", "ed25519", "-noenc", "-keyout", str(key),
                "-out", str(request), "-subj", "/CN=codex")
            certificate = generation / "node.pem"
            run(executable, "x509", "-req", "-in", str(request), "-CA", str(ca),
                "-CAkey", str(ca_key), "-set_serial", str(uuid.uuid4().int), "-days", str(valid_days),
                "-out", str(certificate), "-extfile", str(extension))
            require(check(executable, "verify", "-CAfile", str(ca), "-purpose", "sslserver",
                          "-verify_hostname", "codex", str(certificate)), "INVALID_CODEX_CERTIFICATE")
            require(check(executable, "x509", "-checkend", str(valid_days * 86400 - 60),
                          "-noout", "-in", str(certificate)), "INVALID_CODEX_CERTIFICATE_LIFETIME")
            node_key = transition.read_regular(key, 16 << 10, private=True)
            node_certificate = transition.read_regular(certificate, 64 << 10)
        finally:
            shutil.rmtree(generation)
        certificate_file = staging / "codex-config" / "node.pem"
        certificate_pin = hashlib.sha256(run(executable, "x509", "-inform", "PEM", "-outform", "DER",
                                                 input_data=node_certificate)).hexdigest()
        owned_directories = [
            staging / "codex-config", staging / "codex-state", staging / "codex-state" / "node",
            staging / "codex-state" / "codex", staging / "codex-state" / "codex" / "home",
            staging / "codex-auth", staging / "codex-auth" / "codex", staging / "codex-workspace",
        ]
        for directory in owned_directories:
            if not directory.parent.exists():
                raise ProvisionError("INVALID_OUTPUT_LAYOUT")
            mkdir_owned(directory, runtime_uid, runtime_gid)
        node_config = {
            "adapter": "codex", "certificateFile": "/config/node.pem",
            "clientCAFile": "/config/ca.pem", "dataDir": "/state/node",
            "gatewayCertificateSHA256": gateway_pin, "keyFile": "/config/node.key",
            "listen": "0.0.0.0:18443", "nodeId": selected_id,
            "ownerId": manifest["ownerId"], "policyFile": "/config/policy.txt",
            "policyRevision": "alpha-chat-v1", "registryVersion": manifest["registryVersion"],
            "toolManifestFile": "/config/tools.json",
            "codex": {"codexHome": "/auth/codex", "effort": effort,
                      "executable": "/opt/codex/node_modules/.bin/codex",
                      "homeDir": "/state/codex/home", "model": model,
                      "stateDir": "/state/codex", "workingDir": "/workspace"},
        }
        config = staging / "codex-config"
        for path, content in (
            (config / "ca.pem", source[ca_certificate]),
            (config / "node.pem", node_certificate),
            (config / "node.key", node_key),
            (config / "policy.txt", POLICY.encode()),
            (config / "tools.json", b"[]\n"),
            (config / "node.json", json_bytes(node_config)),
        ):
            write_owned(path, content, runtime_uid, runtime_gid)
        metadata = {
            "schema": 1, "operation": "provision-codex-node", "nodeId": selected_id,
            "ownerId": manifest["ownerId"], "registryVersion": manifest["registryVersion"],
            "certificateSHA256": certificate_pin, "certificateValidDays": valid_days,
            "model": model, "effort": effort, "authState": "empty",
            "source": {"registrySHA256": hashlib.sha256(source[registry_path]).hexdigest(),
                       "caCertificateSHA256": hashlib.sha256(source[ca_certificate]).hexdigest(),
                       "gatewayCertificateSHA256": hashlib.sha256(source[gateway_certificate]).hexdigest()},
        }
        transition.write_private(staging / "provision.json", json_bytes(metadata))
        for directory in reversed(owned_directories):
            transition.sync_directory(directory)
        transition.sync_directory(staging)
        for directory in owned_directories:
            require_owned(directory, runtime_uid, runtime_gid, directory=True)
        for name in ("ca.pem", "node.pem", "node.key", "policy.txt", "tools.json", "node.json"):
            require_owned(config / name, runtime_uid, runtime_gid)
        require(not any((staging / "codex-auth" / "codex").iterdir()), "CODEX_AUTH_NOT_EMPTY")
        require(all(transition.read_regular(path, maximum, private) == source[path]
                    for path, maximum, private in limits), "SOURCE_CHANGED_DURING_PREPARATION")
        input_snapshot.cleanup()
        input_snapshot = None
        transition.require_same_directory(parent, parent_fd, parent_identity)
        transition.rename_no_replace(parent_fd, staging.name, output.name)
        published = True
        staging = None
        try:
            os.fsync(parent_fd)
        except OSError:
            raise ProvisionError("PUBLICATION_DURABILITY_UNKNOWN") from None
        return metadata
    except transition.TransitionError as error:
        raise ProvisionError(str(error)) from None
    finally:
        if staging is not None:
            shutil.rmtree(staging)
        if input_snapshot is not None:
            input_snapshot.cleanup()
        if published:
            try:
                os.close(parent_fd)
            except OSError:
                pass
        else:
            os.close(parent_fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--registry", required=True, type=Path)
    parser.add_argument("--signer-public-key", required=True, type=Path)
    parser.add_argument("--ca-certificate", required=True, type=Path)
    parser.add_argument("--ca-private-key", required=True, type=Path)
    parser.add_argument("--cursor-node-config", required=True, type=Path)
    parser.add_argument("--gateway-certificate", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--model", required=True)
    parser.add_argument("--effort", choices=sorted(EFFORTS), default="low")
    parser.add_argument("--valid-days", type=int, default=14)
    parser.add_argument("--node-id")
    parser.add_argument("--runtime-uid", type=int, default=10001)
    parser.add_argument("--runtime-gid", type=int, default=10001)
    parser.add_argument("--openssl", default="openssl")
    args = parser.parse_args()
    os.umask(0o077)
    require(os.geteuid() == 0, "ROOT_REQUIRED")
    metadata = prepare(args.registry, args.signer_public_key, args.ca_certificate,
                       args.ca_private_key, args.cursor_node_config, args.gateway_certificate,
                       args.output, args.model, args.effort, args.valid_days, args.node_id,
                       args.runtime_uid, args.runtime_gid, args.openssl)
    print(json.dumps({"status": "CODEX_NODE_PROVISIONED", "nodeId": metadata["nodeId"],
                      "certificateSHA256": metadata["certificateSHA256"]}, separators=(",", ":")))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, ProvisionError) else "CODEX_NODE_PROVISION_FAILED")
        raise SystemExit(1) from None
