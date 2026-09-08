#!/usr/bin/env python3
"""One-time VM115 TLS adapter; run as root over the approved SSH channel.

Does not deploy Panel, open public routes, copy keys, or alter Docker/Portainer.
The certificate is the only output transferable to NPM; its private key stays here.
"""
import os
from pathlib import Path
import socket
import subprocess
import sys

ROOT = Path('/etc/homelab-panel')
SERVICE = Path('/etc/init.d/homelab-panel-ingress')
CONFIG = '''user nginx;
worker_processes 1;
pid /run/homelab-panel-nginx.pid;
error_log /var/log/nginx/homelab-panel-error.log crit;
events { worker_connections 256; }
http {
  access_log off;
  server_tokens off;
  client_body_temp_path /var/lib/nginx/tmp/client_body;
  proxy_temp_path /var/lib/nginx/tmp/proxy;
  server {
    listen 10.202.2.52:18443 ssl;
    server_name panel-backend.homelab.internal;
    ssl_certificate /etc/homelab-panel/backend.crt;
    ssl_certificate_key /etc/homelab-panel/backend.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    allow 10.202.2.110;
    deny all;
    client_max_body_size 72k;
    location ^~ /panel/ {
      proxy_pass http://127.0.0.1:18080;
      proxy_set_header Host $http_host;
      proxy_set_header X-Forwarded-Proto https;
      proxy_connect_timeout 5s;
      proxy_read_timeout 65s;
      proxy_redirect off;
      proxy_buffering off;
    }
    location / { return 404; }
  }
}
'''
INIT = '''#!/sbin/openrc-run
description="Panel private TLS adapter (NPM only)"
command="/usr/sbin/nginx"
command_args="-c /etc/homelab-panel/nginx.conf"
pidfile="/run/homelab-panel-nginx.pid"
depend() { need net; }
start_pre() { /usr/sbin/nginx -t -c /etc/homelab-panel/nginx.conf; }
'''


def run(*args):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=120)


def main():
    if os.geteuid() != 0 or socket.gethostname() != 'alpine-docker':
        raise RuntimeError('WRONG_TARGET')
    if ROOT.exists() or SERVICE.exists():
        raise RuntimeError('ALREADY_PROVISIONED_INSPECT_BEFORE_CHANGING')
    with socket.socket() as probe:
        probe.bind(('10.202.2.52', 18443))
    run('apk', 'add', 'nginx')
    ROOT.mkdir(mode=0o700)
    os.umask(0o077)
    run('openssl', 'req', '-x509', '-newkey', 'rsa:3072', '-sha256', '-nodes',
        '-keyout', str(ROOT / 'backend.key'), '-out', str(ROOT / 'backend.crt'),
        '-days', '365', '-subj', '/CN=panel-backend.homelab.internal',
        '-addext', 'subjectAltName=DNS:panel-backend.homelab.internal',
        '-addext', 'extendedKeyUsage=serverAuth')
    (ROOT / 'nginx.conf').write_text(CONFIG)
    run('nginx', '-t', '-c', str(ROOT / 'nginx.conf'))
    SERVICE.write_text(INIT)
    SERVICE.chmod(0o755)
    run('rc-service', 'homelab-panel-ingress', 'start')
    run('rc-update', 'add', 'homelab-panel-ingress', 'default')
    run('rc-service', 'homelab-panel-ingress', 'status')
    print('PANEL_PRIVATE_TLS_STARTED')


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('TLS_BOOTSTRAP_FAILED_INSPECT_OWN_FILES_BEFORE_RETRY', file=sys.stderr)
        raise SystemExit(1) from None
