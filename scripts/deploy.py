#!/usr/bin/env python3
"""Operator-run deployment of a trusted Panel release. Python stdlib, no SSH.

Checksums detect corruption, not an untrusted publisher. Obtain this program and
its bundle from the verified repository's Release. Never run an arbitrary bundle.
"""
import argparse
import contextlib
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
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

REPOSITORY = "ghcr.io/boxvtk621/homelab-telegram-panel"
VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
FILES = {"compose.yaml", "panel.env.example", "deploy.py"}
CONFIG = {"PANEL_HOST_PORT", "PANEL_PUBLIC_ORIGIN", "PANEL_YOUTRACK_URL", "PANEL_PROJECT_ID", "PANEL_PROJECT_KEY", "PANEL_OWNER_LOGIN", "PANEL_CURSOR_API_KEY", "PANEL_CURSOR_MODEL"}


class DeployError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise DeployError(code)


def pairs(items):
    result = {}
    for key, value in items:
        require(key not in result, "DUPLICATE_JSON_KEY")
        result[key] = value
    return result


def read_json(path):
    require(path.is_file() and not path.is_symlink() and path.stat().st_size <= 65536, "INVALID_METADATA_FILE")
    return json.loads(path.read_text(), object_pairs_hook=pairs)


def checksum(path):
    require(path.is_file() and not path.is_symlink() and path.stat().st_size < 1048576, "INVALID_BUNDLE_FILE")
    return hashlib.sha256(path.read_bytes()).hexdigest()


def bundle(path):
    path = path.resolve(strict=True)
    sums = (path / "SHA256SUMS").read_text().splitlines()
    expected = {}
    for line in sums:
        match = re.fullmatch(r"([0-9a-f]{64})  ([.a-zA-Z0-9_-]+)", line)
        require(match is not None and match[2] not in expected, "INVALID_CHECKSUMS")
        expected[match[2]] = match[1]
    require(set(expected) == FILES | {"release.json"}, "INVALID_CHECKSUMS")
    for name, digest in expected.items():
        require(checksum(path / name) == digest, "BUNDLE_CHECKSUM_MISMATCH")
    data = read_json(path / "release.json")
    require(set(data) == {"schema", "version", "revision", "image", "platforms", "files"} and data["schema"] == 1, "INVALID_MANIFEST")
    require(isinstance(data["version"], str) and VERSION.fullmatch(data["version"]), "INVALID_VERSION")
    require(isinstance(data["revision"], str) and re.fullmatch(r"[0-9a-f]{40}", data["revision"]), "INVALID_REVISION")
    require(isinstance(data["image"], str) and data["image"].startswith(REPOSITORY + "@") and DIGEST.fullmatch(data["image"].split("@")[-1]), "INVALID_IMAGE_DIGEST")
    require(data["platforms"] == ["linux/amd64", "linux/arm64"] and set(data["files"]) == FILES, "INVALID_BUNDLE_SCHEMA")
    for name in FILES:
        require(checksum(path / name) == data["files"][name], "BUNDLE_CHECKSUM_MISMATCH")
    return data


def private(path, directory=False):
    require(not path.is_symlink(), "UNSAFE_PRIVATE_PATH")
    info = path.stat()
    require(info.st_uid == os.geteuid() and not info.st_mode & 0o077, "PRIVATE_PATH_MUST_BE_OWNER_ONLY")
    require(stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode), "INVALID_PRIVATE_PATH")


def configuration(path):
    private(path)
    require(path.stat().st_size <= 16384, "CONFIG_TOO_LARGE")
    values = {}
    for line in path.read_text().splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, sep, value = line.partition("=")
        require(sep and key in CONFIG and key not in values and (value or key == "PANEL_CURSOR_API_KEY") and "\x00" not in value, "INVALID_CONFIG")
        # Literal KEY=value, not a sourced shell script or dotenv interpolation.
        require(value == value.strip() and not value.startswith(("'", '"')), "CONFIG_REQUIRES_LITERAL_VALUES")
        values[key] = value
    require(CONFIG - {"PANEL_CURSOR_MODEL", "PANEL_CURSOR_API_KEY"} <= set(values), "MISSING_CONFIG")
    require(re.fullmatch(r"[0-9]{1,5}", values["PANEL_HOST_PORT"]) and 1024 <= int(values["PANEL_HOST_PORT"]) <= 65535, "INVALID_PORT")
    for key in ["PANEL_PUBLIC_ORIGIN", "PANEL_YOUTRACK_URL"]:
        url = urllib.parse.urlsplit(values[key])
        require(url.scheme == "https" and url.hostname and not url.username and not url.password and not url.query and not url.fragment and url.path in ("", "/"), "INVALID_HTTPS_ORIGIN")
    return values


def atomic_json(path, data):
    fd, name = tempfile.mkstemp(prefix=".pending-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as out:
            json.dump(data, out, sort_keys=True)
            out.flush()
            os.fsync(out.fileno())
        os.replace(name, path)
        folder = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(folder)
        finally:
            os.close(folder)
    finally:
        if os.path.exists(name):
            os.unlink(name)


@contextlib.contextmanager
def locked(path):
    require(path.is_absolute() and path != Path(path.anchor) and path != Path.home(), "SPECIFIC_STATE_DIRECTORY_REQUIRED")
    if not path.exists():
        path.mkdir(mode=0o700, parents=False)
    private(path, directory=True)
    fd = os.open(path / "deploy.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise DeployError("DEPLOYMENT_LOCKED") from None
        yield
    finally:
        os.close(fd)


class Installer:
    def __init__(self, state, config):
        self.state = state
        self.config = config
        # A state directory has exactly one project identity; another directory
        # cannot accidentally adopt/stop this installation's containers.
        self.project = "homelab-panel-" + hashlib.sha256(str(state).encode()).hexdigest()[:12]
        self.env = {k: os.environ[k] for k in ["PATH", "HOME"] if k in os.environ}
        self.env.update(config)
        self.env.update(PANEL_LISTEN="0.0.0.0:18080", PANEL_WRITES_ENABLED="false")
        self.env["COMPOSE_ANSI"] = "never"

    def command(self, args, image="", timeout=300):
        env = dict(self.env, PANEL_IMAGE=image)
        try:
            reply = subprocess.run(["docker", "--context", "default", *args], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=timeout, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise DeployError("DOCKER_UNAVAILABLE_OR_TIMEOUT") from None
        require(reply.returncode == 0, "DOCKER_COMMAND_FAILED")
        return reply.stdout.strip()

    def compose(self, record, *args):
        return self.command(["compose", "--env-file", "/dev/null", "--project-name", self.project, "-f", str(self.location(record) / "compose.yaml"), *args], record["manifest"]["image"])

    def location(self, record):
        # Never use a path from the state file as an executable input.
        return self.state / "releases" / hashlib.sha256(json.dumps(record["manifest"], sort_keys=True).encode()).hexdigest()

    def remember(self, source):
        manifest = bundle(source)
        record = {"manifest": manifest, "container": ""}
        dest = self.location(record)
        dest.parent.mkdir(mode=0o700, exist_ok=True)
        if not dest.exists():
            temporary = Path(tempfile.mkdtemp(prefix=".bundle-", dir=dest.parent))
            try:
                for name in FILES | {"release.json", "SHA256SUMS"}:
                    shutil.copyfile(source / name, temporary / name)
                    with (temporary / name).open("rb") as copied:
                        os.fsync(copied.fileno())
                require(bundle(temporary) == manifest, "STORED_BUNDLE_MISMATCH")
                os.rename(temporary, dest)
                folder = os.open(dest.parent, os.O_RDONLY)
                try:
                    os.fsync(folder)
                finally:
                    os.close(folder)
            finally:
                if temporary.exists():
                    shutil.rmtree(temporary)  # Only this generated staging directory.
        require(bundle(dest) == manifest, "STORED_BUNDLE_MISMATCH")
        return record

    def containers(self):
        return self.command(["ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project=" + self.project]).split()

    def validate_current(self, current):
        ids = self.containers()
        require(ids == ([current["container"]] if current else []), "UNMANAGED_OR_CHANGED_CONTAINER")

    def preflight(self, record):
        require(bundle(self.location(record)) == record["manifest"], "STORED_BUNDLE_MISMATCH")
        self.compose(record, "config", "--quiet")  # Never print interpolated secrets.
        self.command(["pull", record["manifest"]["image"]])
        image = record["manifest"]["image"]
        for key in ("version", "revision"):
            label = self.command(["image", "inspect", "--format", '{{index .Config.Labels "org.opencontainers.image.' + key + '"}}', image])
            require(label == record["manifest"][key], "IMAGE_METADATA_MISMATCH")
        require(self.command(["image", "inspect", "--format", "{{.Config.User}}", image]) == "10001:10001", "IMAGE_USER_MISMATCH")
        version = self.probe(record, "version")
        require(version == "homelab-panel " + record["manifest"]["version"], "VERSION_MISMATCH")
        self.probe(record, "validate")

    def probe(self, record, operation):
        name = self.project + "-preflight-" + uuid.uuid4().hex
        try:
            args = ["run", "--rm", "--name", name, "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--pids-limit", "64", "--memory", "128m"]
            for key in sorted(set(self.config) | {"PANEL_LISTEN", "PANEL_WRITES_ENABLED"}):
                args += ["--env", key]  # Names only; values are in the child env.
            return self.command([*args, record["manifest"]["image"], operation], timeout=30)
        finally:
            # --rm alone does not handle a lost/timed-out Docker CLI.
            try:
                self.command(["rm", "--force", name], timeout=30)
            except DeployError:
                pass  # Normally already removed by --rm; confirm below.
            remaining = self.command(["ps", "-aq", "--filter", "name=^/" + name + "$"], timeout=30)
            require(not remaining, "PREFLIGHT_CLEANUP_UNCONFIRMED")

    def healthy(self, record):
        ids = self.compose(record, "ps", "-aq", "panel").split()
        require(len(ids) == 1 and re.fullmatch(r"[0-9a-f]{64}", ids[0]), "CONTAINER_NOT_FOUND")
        cid = ids[0]
        # Filter output at the daemon; never capture container environment.
        status = json.loads(self.command(["inspect", "--format", '{{json .State.Running}}', cid]))
        image = self.command(["inspect", "--format", '{{.Config.Image}}', cid])
        require(status is True and image == record["manifest"]["image"], "CONTAINER_IDENTITY_MISMATCH")
        version = self.command(["exec", cid, "/fixik-next-mobile-gateway", "version"])
        require(version == "homelab-panel " + record["manifest"]["version"], "VERSION_MISMATCH")
        revision = self.command(["inspect", "--format", '{{index .Config.Labels "org.opencontainers.image.revision"}}', cid])
        require(revision == record["manifest"]["revision"], "REVISION_MISMATCH")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        base = "http://127.0.0.1:" + self.config["PANEL_HOST_PORT"]
        host = urllib.parse.urlsplit(self.config["PANEL_PUBLIC_ORIGIN"]).netloc
        with opener.open(urllib.request.Request(base + "/api/v2/healthz", headers={"Host": host}), timeout=2) as reply:
            require(reply.status == 200 and json.loads(reply.read(1024)) == {"panel": "up"}, "HEALTH_FAILED")
        try:
            opener.open(urllib.request.Request(base + "/api/v2/issues", headers={"Host": host}), timeout=2).close()
            raise DeployError("UNAUTHENTICATED_DATA_EXPOSED")
        except urllib.error.HTTPError as exc:
            require(exc.code == 401, "AUTH_BOUNDARY_FAILED")
        return dict(record, container=cid)

    def start(self, record):
        self.compose(record, "up", "-d", "--no-deps", "--pull", "never", "panel")
        for _ in range(20):
            try:
                return self.healthy(record)
            except (DeployError, OSError, ValueError):
                time.sleep(1)
        raise DeployError("STARTUP_CHECK_FAILED")

    def restore(self, current, attempted):
        for record in (current, attempted):
            if record:
                require(bundle(self.location(record)) == record["manifest"], "STORED_BUNDLE_MISMATCH")
        self.validate_recovery(current, attempted)
        if current:
            # The protected operator config is deliberately not backed up here.
            return self.start(dict(current, config_fingerprint=self.fingerprint()))
        # First installation failed: remove only this exact project's service.
        self.compose(attempted, "rm", "--stop", "--force", "panel")
        require(not self.containers(), "FIRST_INSTALL_CLEANUP_FAILED")
        return None

    def fingerprint(self):
        # Private change detector, never logged; raw credentials are not stored.
        return hashlib.sha256(json.dumps(self.config, sort_keys=True).encode()).hexdigest()

    def validate_recovery(self, current, attempted):
        ids = self.containers()
        require(len(ids) <= 1, "UNMANAGED_RECOVERY_CONTAINER")
        allowed = {r["manifest"]["image"] for r in (current, attempted) if r}
        for cid in ids:
            image = self.command(["inspect", "--format", "{{.Config.Image}}", cid])
            service = self.command(["inspect", "--format", '{{index .Config.Labels "com.docker.compose.service"}}', cid])
            require(image in allowed and service == "panel", "UNMANAGED_RECOVERY_CONTAINER")

    def perform(self, source, rollback=False, allow_interrupt=False):
        require(allow_interrupt, "ACKNOWLEDGE_AGENT_INTERRUPTION_REQUIRED")
        ledger_file = self.state / "deployment.json"
        ledger = read_json(ledger_file) if ledger_file.exists() else {"current": None, "previous": None, "pending": None}
        require(set(ledger) == {"current", "previous", "pending"}, "INVALID_DEPLOYMENT_STATE")
        if ledger["pending"]:
            require(rollback, "INCOMPLETE_DEPLOYMENT_USE_ROLLBACK")
            restored = self.restore(ledger["current"], ledger["pending"])
            atomic_json(ledger_file, dict(ledger, current=restored, pending=None))
            return "RECOVERED"
        self.validate_current(ledger["current"])
        target = ledger["previous"] if rollback else self.remember(source)
        require(target is not None, "NO_PREVIOUS_RELEASE")
        target = dict(target, config_fingerprint=self.fingerprint())
        if ledger["current"] and target["manifest"] == ledger["current"]["manifest"] and target["config_fingerprint"] == ledger["current"].get("config_fingerprint"):
            self.healthy(ledger["current"])
            return "ALREADY_CURRENT"
        self.preflight(target)  # No existing service replacement before this.
        self.validate_current(ledger["current"])
        atomic_json(ledger_file, dict(ledger, pending=target))
        try:
            running = self.start(target)
            # Config-only changes must not consume the previous image slot.
            previous = ledger["current"]
            if previous and previous["manifest"] == target["manifest"]:
                previous = ledger["previous"]
            atomic_json(ledger_file, {"current": running, "previous": previous, "pending": None})
        except (Exception, KeyboardInterrupt):
            try:
                restored = self.restore(ledger["current"], target)
                atomic_json(ledger_file, dict(ledger, current=restored, pending=None))
            except (Exception, KeyboardInterrupt):
                raise DeployError("DEPLOY_AND_ROLLBACK_FAILED_STATE_PENDING") from None
            raise DeployError("DEPLOY_FAILED_ROLLBACK_COMPLETED") from None
        return "ROLLED_BACK" if rollback else "DEPLOYED"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["verify", "apply", "rollback"])
    parser.add_argument("--bundle", type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument("--config", type=Path)
    parser.add_argument("--state", type=Path)
    parser.add_argument("--allow-interrupt", action="store_true", help="Acknowledge loss of active runs/web sessions during container replacement")
    args = parser.parse_args()
    try:
        if args.action == "verify":
            data = bundle(args.bundle)
            print("BUNDLE_VERIFIED", data["version"], data["image"])
            return 0
        require(args.config is not None and args.state is not None and args.state.is_absolute(), "CONFIG_AND_ABSOLUTE_STATE_REQUIRED")
        values = configuration(args.config)
        require(not args.state.is_symlink(), "UNSAFE_PRIVATE_PATH")
        with locked(args.state):
            result = Installer(args.state.resolve(), values).perform(args.bundle.resolve(), rollback=args.action == "rollback", allow_interrupt=args.allow_interrupt)
        print(result)
        return 0
    except DeployError as exc:
        print(str(exc), file=sys.stderr)
    except (Exception, KeyboardInterrupt):
        # Never show raw Docker/config/network errors (they may contain keys).
        print("DEPLOYMENT_FAILED_CHECK_PRIVATE_CONFIGURATION", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
