#!/usr/bin/env python3
"""Provision an isolated local alpha without reading provider credentials."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path)
    parser.add_argument("--owner-id", required=True, help="Opaque owner ID signed into the Harness registry")
    parser.add_argument("--adapter", choices=("cursor", "codex"), default="cursor")
    parser.add_argument("--cursor-key-file", type=Path)
    parser.add_argument("--codex-executable", type=Path)
    parser.add_argument("--codex-home", type=Path)
    parser.add_argument("--codex-model")
    parser.add_argument("--codex-effort", choices=("minimal", "low", "medium", "high", "xhigh"), default="medium")
    parser.add_argument("--openssl", default="openssl", help="OpenSSL 3 executable")
    parser.add_argument("--container", action="store_true", help="Prepare separate Panel/Harness container config directories")
    args = parser.parse_args()
    if not args.directory.is_absolute():
        parser.error("directory must be absolute")
    if args.adapter == "cursor" and (args.cursor_key_file is None or not args.cursor_key_file.is_absolute()):
        parser.error("cursor key file path must be absolute")
    if args.adapter == "codex" and (args.codex_executable is None or not args.codex_executable.is_absolute() or
                                    args.codex_home is None or not args.codex_home.is_absolute() or not args.codex_model):
        parser.error("codex executable, home and model are required")
    if not args.owner_id or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:@-" for c in args.owner_id) or len(args.owner_id) > 128:
        parser.error("invalid owner ID")
    executable = shutil.which(args.openssl)
    node = shutil.which("node") if args.adapter == "cursor" else None
    if not executable or (args.adapter == "cursor" and not node):
        parser.error("OpenSSL 3 and the selected adapter runtime are required")
    os.umask(0o077)
    root = args.directory
    root.mkdir(mode=0o700)  # A second invocation must never overwrite state.
    code = Path(__file__).resolve().parents[2]

    def write(name, value):
        with (root / name).open("xb") as stream:
            stream.write(value.encode() if isinstance(value, str) else value)
            stream.flush()
            os.fsync(stream.fileno())

    def save(name, value):
        write(name, json.dumps(value, ensure_ascii=True, separators=(",", ":")) + "\n")

    def openssl(*values):
        result = subprocess.run([executable, *values], cwd=root, capture_output=True, timeout=15)
        if result.returncode:
            raise RuntimeError("OpenSSL operation failed; existing files were preserved")
        return result.stdout

    def cert(name, client=False):
        openssl("req", "-new", "-newkey", "ed25519", "-noenc", "-keyout", name + ".key", "-out", name + ".csr", "-subj", "/CN=" + name)
        write(name + ".ext", "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=" + ("clientAuth" if client else "serverAuth") + "\nsubjectAltName=DNS:localhost,DNS:harness,IP:127.0.0.1\n")
        openssl("x509", "-req", "-in", name + ".csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-set_serial", str(uuid.uuid4().int), "-days", "30", "-out", name + ".pem", "-extfile", name + ".ext")
        return hashlib.sha256(openssl("x509", "-in", name + ".pem", "-outform", "DER")).hexdigest()

    openssl("req", "-x509", "-newkey", "ed25519", "-noenc", "-keyout", "ca.key", "-out", "ca.pem", "-subj", "/CN=Harness local alpha", "-days", "30", "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign")
    node_pin = cert("node")
    gateway_pin = cert("gateway", client=True)
    cert("panel")
    node_id = str(uuid.uuid4())
    adapter_name = "Cursor" if args.adapter == "cursor" else "Codex"
    manifest = {"registryVersion": 1, "ownerId": args.owner_id, "mode": "live", "nodes": [{"nodeId": node_id, "name": adapter_name + " alpha", "adapter": args.adapter, "url": "https://127.0.0.1:18443", "certificateSHA256": node_pin}]}
    if args.container:
        manifest["nodes"][0]["url"] = "https://harness:18443" if args.adapter == "cursor" else "https://codex:18443"
    write("manifest.json", json.dumps(manifest, separators=(",", ":")))
    openssl("genpkey", "-algorithm", "ed25519", "-out", "registry-signing.key")
    openssl("pkey", "-in", "registry-signing.key", "-pubout", "-out", "registry-signing.pem")
    signature = openssl("pkeyutl", "-sign", "-rawin", "-inkey", "registry-signing.key", "-in", "manifest.json")
    save("registry.json", {"manifest": manifest, "signature": base64.b64encode(signature).decode()})
    write("policy.txt", "You are the owner's autonomous assistant. Answer the user's request clearly and concisely. This chat alpha has no tools; do not claim to have executed commands or changed external systems.\n")
    write("tools.json", "[]\n")
    state_name = args.adapter + "-state"
    for directory in ("node-data", state_name, "router-state", "workspace"):
        (root / directory).mkdir(mode=0o700)
    node_config = {
        "listen": "127.0.0.1:18443", "nodeId": node_id, "ownerId": args.owner_id,
        "dataDir": str(root / "node-data"), "registryVersion": 1,
        "certificateFile": str(root / "node.pem"), "keyFile": str(root / "node.key"),
        "clientCAFile": str(root / "ca.pem"), "gatewayCertificateSHA256": gateway_pin,
        "policyFile": str(root / "policy.txt"), "toolManifestFile": str(root / "tools.json"),
        "policyRevision": "alpha-chat-v1", "adapter": args.adapter,
    }
    if args.adapter == "cursor":
        node_config["cursor"] = {"nodeExecutable": node, "workerEntrypoint": str(code / "harness/adapters/cursor/worker/worker.mjs"), "stateDir": str(root / state_name), "apiKeyFile": str(args.cursor_key_file), "model": "composer-2.5"}
    else:
        node_config["codex"] = {"executable": str(args.codex_executable), "stateDir": str(root / state_name),
            "workingDir": str(root / "workspace"), "codexHome": str(args.codex_home), "homeDir": str(root / state_name / "home"),
            "model": args.codex_model, "effort": args.codex_effort}
    save("node.json", node_config)
    save("panel-environment.json", {
        "PANEL_LISTEN": "127.0.0.1:18444", "PANEL_PUBLIC_ORIGIN": "https://localhost:18444",
        "PANEL_HARNESS_COMMANDS_ENABLED": "true",
        "PANEL_TLS_CERTIFICATE": str(root / "panel.pem"), "PANEL_TLS_KEY": str(root / "panel.key"),
        "PANEL_HARNESS_REGISTRY": str(root / "registry.json"), "PANEL_HARNESS_SIGNER_PUBLIC_KEY": str(root / "registry-signing.pem"),
        "PANEL_HARNESS_CA": str(root / "ca.pem"), "PANEL_HARNESS_CLIENT_CERT": str(root / "gateway.pem"), "PANEL_HARNESS_CLIENT_KEY": str(root / "gateway.key"),
        "PANEL_HARNESS_ROUTER_STATE": "/router/state.json" if args.container else str(root / "router-state/state.json"),
        "PANEL_HARNESS_ROUTER_SOCKET": "/router/control.sock" if args.container else str(root / "router-state/control.sock"),
    })
    if args.container:
        panel = root / "panel-config"
        harness = root / "node-config"
        panel.mkdir(mode=0o700)
        harness.mkdir(mode=0o700)
        for name in ("ca.pem", "gateway.pem", "gateway.key", "registry.json", "registry-signing.pem"):
            shutil.copyfile(root / name, panel / name)
        for name in ("ca.pem", "node.pem", "node.key", "policy.txt", "tools.json"):
            shutil.copyfile(root / name, harness / name)
        cfg = json.loads((root / "node.json").read_text())
        cfg.update(listen="0.0.0.0:18443", dataDir="/state/node")
        for field in ("certificateFile", "keyFile", "clientCAFile", "policyFile", "toolManifestFile"):
            cfg[field] = "/config/" + Path(cfg[field]).name
        if args.adapter == "cursor":
            cfg["cursor"].update(nodeExecutable="/usr/local/bin/node", workerEntrypoint="/opt/worker/worker.mjs",
                stateDir="/state/cursor", apiKeyFile="/run/secrets/cursor-key")
        else:
            cfg["codex"].update(executable="/opt/codex/node_modules/.bin/codex", stateDir="/state/codex", workingDir="/workspace",
                codexHome="/auth/codex", homeDir="/state/codex/home")
        (harness / "node.json").write_text(json.dumps(cfg, separators=(",", ":")) + "\n")
    print(json.dumps({"directory": str(root), "nodeId": node_id, "ownerId": args.owner_id, "panelURL": "https://localhost:18444"}))


if __name__ == "__main__":
    main()
