#!/usr/bin/env python3
"""Switch only the existing VM115 Panel upstream; preserve RC5 for rollback."""
import argparse
import fcntl
import hashlib
import os
from pathlib import Path
import socket
import subprocess
import urllib.error
import urllib.request

CONFIG = Path("/etc/homelab-panel/nginx.conf")
STATE = Path("/opt/homelab-panel-alpha/cutover")
OLD = b"proxy_pass http://127.0.0.1:18080;"
NEW = b"proxy_pass http://127.0.0.1:18081;"


def health(port):
    for path, status in (("healthz", 200), ("session", 401)):
        req = urllib.request.Request(f"http://127.0.0.1:{port}/panel/api/v2/{path}",
            headers={"Host": "h1-cloud.ru", "X-Forwarded-Proto": "https"})
        try:
            with urllib.request.urlopen(req, timeout=10) as response:
                actual = response.status
        except urllib.error.HTTPError as error:
            actual = error.code
        if actual != status:
            raise RuntimeError("Panel health/auth check failed")


def replace(content):
    candidate = CONFIG.with_name("nginx.alpha-candidate.conf")
    with candidate.open("wb") as stream:
        stream.write(content)
        stream.flush()
        os.fsync(stream.fileno())
    subprocess.run(["nginx", "-t", "-c", str(candidate)], check=True)
    os.replace(candidate, CONFIG)
    subprocess.run(["nginx", "-s", "reload", "-c", str(CONFIG)], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("switch", "rollback"))
    parser.add_argument("--expected-config-sha256")
    args = parser.parse_args()
    if os.geteuid() != 0 or socket.gethostname() != "alpine-docker":
        parser.error("requires the approved VM115 root operator")
    os.umask(0o077)
    STATE.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (STATE / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        current = CONFIG.read_bytes()
        before = STATE / "before.conf"
        after = STATE / "after.conf"
        if args.action == "switch":
            if before.exists() or after.exists():
                raise RuntimeError("cutover already recorded; inspect or roll back")
            if hashlib.sha256(current).hexdigest() != args.expected_config_sha256 or current.count(OLD) != 1 or NEW in current:
                raise RuntimeError("ingress baseline changed")
            health(18080)
            health(18081)
            target = current.replace(OLD, NEW)
            before.write_bytes(current)
            after.write_bytes(target)
            try:
                replace(target)
                health(18081)
            except Exception:
                replace(current)
                raise
            print("PANEL_ALPHA_UPSTREAM_ACTIVE")
        else:
            original, candidate = before.read_bytes(), after.read_bytes()
            if current not in (original, candidate):
                raise RuntimeError("ingress drift; refusing to overwrite")
            health(18080)
            replace(original)
            print("PANEL_RC5_UPSTREAM_RESTORED")


if __name__ == "__main__":
    main()
