#!/usr/bin/env python3
"""Panel pull-based CD: GitHub request producer and credential-free host consumer.

Only the installed local installer executes. Requests cannot supply code, URLs,
paths, configuration, credentials, or arbitrary commands. GitHub is the trusted
release publisher; checksums are integrity checks, not publisher authentication.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
import urllib.error
import urllib.request

import deploy

REPO = 'boxvtk621/homelab-telegram-panel'
API = 'https://api.github.com/repos/' + REPO
PUBLIC = 'https://h1-cloud.ru/panel'
STATUS = PUBLIC + '/deployment-status.json'
ENVIRONMENT = 'panel-production'
WORKFLOW = '.github/workflows/deploy.yml'
STATE = Path('/opt/homelab-panel/state')
CD_STATE = Path('/opt/homelab-panel/cd')
CONFIG = Path('/etc/homelab-panel.env')
PUBLIC_FILE = Path('/var/lib/homelab-panel-public/deployment-status.json')


def fetch(url, maximum=1048576, payload=None, token=None):
    headers = {'Accept': 'application/vnd.github+json', 'User-Agent': 'homelab-panel-cd'}
    if token:
        deploy.require(url.startswith(API + '/'), 'AUTH_DESTINATION_REJECTED')
        headers['Authorization'] = 'Bearer ' + token
    if payload is not None:
        headers['Content-Type'] = 'application/json'
    request = urllib.request.Request(url, headers=headers, data=None if payload is None else json.dumps(payload).encode())
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    opener = urllib.request.build_opener(NoRedirect()) if token else urllib.request.build_opener()
    with opener.open(request, timeout=20) as response:
        data = response.read(maximum + 1)
    deploy.require(len(data) <= maximum, 'RESPONSE_TOO_LARGE')
    return data


def github(path, payload=None, token=None):
    return json.loads(fetch(API + path, payload=payload, token=token), object_pairs_hook=deploy.pairs)


def positive(value):
    return type(value) is int and value > 0


def authorize(request, read=None, now=None):
    read = read or github
    # GitHub deployments:write is the repository-level production capability.
    # Run fields validate context, not a cryptographic request-to-run binding.
    # Any other Actions writer explicitly granted this permission is also trusted.
    p = request.get('payload', {})
    deploy.require(positive(request.get('id')) and request.get('environment') == ENVIRONMENT and
        request.get('creator', {}).get('login') == 'github-actions[bot]' and
        request.get('creator', {}).get('type') == 'Bot', 'UNAUTHORIZED_DEPLOYMENT')
    deploy.require(type(p) is dict and set(p) == {'schema', 'operation', 'version', 'run_id', 'run_attempt', 'workflow_sha', 'allow_interrupt'} and
        p['schema'] == 1 and p['operation'] in ('apply', 'rollback') and p['allow_interrupt'] is True and
        isinstance(p['version'], str) and deploy.VERSION.fullmatch(p['version']) and
        positive(p['run_id']) and positive(p['run_attempt']) and
        isinstance(p['workflow_sha'], str) and deploy.re.fullmatch(r'[0-9a-f]{40}', p['workflow_sha']), 'INVALID_DEPLOYMENT_REQUEST')
    deploy.require(request.get('task') == 'panel:' + p['operation'] and request.get('ref') == p['version'], 'DEPLOYMENT_TARGET_MISMATCH')
    created = datetime.datetime.fromisoformat(request['created_at'].replace('Z', '+00:00')).timestamp()
    now = time.time() if now is None else now
    deploy.require(0 <= now - created <= 900, 'EXPIRED_DEPLOYMENT_REQUEST')
    run = read('/actions/runs/' + str(p['run_id']))
    deploy.require(run.get('event') == 'workflow_dispatch' and run.get('path') == WORKFLOW and
        run.get('head_branch') == 'main' and run.get('head_sha') == p['workflow_sha'] and
        run.get('head_repository', {}).get('full_name') == REPO and
        run.get('status') == 'in_progress' and run.get('run_attempt') == p['run_attempt'], 'UNTRUSTED_WORKFLOW')
    return p


def released(version, read=None):
    read = read or github
    deploy.require(isinstance(version, str) and deploy.VERSION.fullmatch(version), 'INVALID_VERSION')
    metadata = read('/releases/tags/' + version)
    deploy.require(metadata.get('tag_name') == version and metadata.get('draft') is False and metadata.get('published_at') and
        metadata.get('author', {}).get('login') == 'github-actions[bot]', 'UNPUBLISHED_OR_UNTRUSTED_RELEASE')
    assets = metadata.get('assets', [])
    deploy.require(len(assets) == 5 and {a.get('name') for a in assets} == deploy.FILES | {'release.json', 'SHA256SUMS'}, 'RELEASE_ASSET_MISMATCH')
    return {a['name']: a.get('digest') for a in assets}


def download(version, target):
    hashes = released(version)
    for name in sorted(hashes):
        data = fetch('https://github.com/' + REPO + '/releases/download/' + version + '/' + name)
        deploy.require('sha256:' + hashlib.sha256(data).hexdigest() == hashes[name], 'GITHUB_ASSET_DIGEST_MISMATCH')
        (target / name).write_bytes(data)
    manifest = deploy.bundle(target)
    deploy.require(manifest['version'] == version, 'RELEASE_VERSION_MISMATCH')
    runs = github('/actions/runs?head_sha=' + manifest['revision'] + '&event=push&status=success&per_page=20')['workflow_runs']
    deploy.require(any(r.get('path') == '.github/workflows/release.yml' and r.get('head_sha') == manifest['revision'] and
        r.get('event') == 'push' and r.get('conclusion') == 'success' for r in runs), 'RELEASE_CI_NOT_SUCCESSFUL')
    return manifest


def summary(record):
    if not record:
        return None
    m = record['manifest']
    return {k: m[k] for k in ('version', 'revision', 'image')}


def snapshot(request_id, status, result):
    file = STATE / 'deployment.json'
    ledger = deploy.read_json(file) if file.exists() else {'current': None, 'previous': None, 'pending': None}
    return {'schema': 1, 'request_id': request_id, 'status': status, 'result': result,
        'current': summary(ledger['current']),
        'rollback_candidate': summary(ledger['current'] if ledger['pending'] else ledger['previous'])}


def publish(value):
    # This file contains ONLY bounded release metadata/status; never operator config.
    fd, name = tempfile.mkstemp(prefix='.status-', dir=PUBLIC_FILE.parent)
    try:
        os.fchmod(fd, 0o644)
        with os.fdopen(fd, 'w') as out:
            json.dump(value, out, sort_keys=True)
            out.flush()
            os.fsync(out.fileno())
        os.replace(name, PUBLIC_FILE)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def public_health():
    deploy.require(json.loads(fetch(PUBLIC + '/api/v2/healthz', 1024)) == {'panel': 'up'}, 'PUBLIC_HEALTH_FAILED')
    try:
        fetch(PUBLIC + '/api/v2/issues', 1024)
        raise deploy.DeployError('PUBLIC_AUTH_FAILED')
    except urllib.error.HTTPError as error:
        deploy.require(error.code == 401, 'PUBLIC_AUTH_FAILED')


def poll():
    with deploy.locked(CD_STATE):
        file = CD_STATE / 'request.json'
        last = deploy.read_json(file) if file.exists() else {'id': 0, 'status': 'idle'}
        if last['status'] == 'running':
            # Crash recovery does not replay an accepted mutation. The installer
            # pending journal is authoritative; keep it for explicit Rollback.
            deploy.atomic_json(file, {'id': last['id'], 'status': 'failure', 'result': 'INTERRUPTED_CHECK_ROLLBACK'})
            publish(snapshot(last['id'], 'failure', 'INTERRUPTED_CHECK_ROLLBACK'))
            return
        requests = github('/deployments?environment=' + ENVIRONMENT + '&per_page=1')
        if not requests or requests[0]['id'] <= last['id']:
            # Repair a crash between private completion and public publication.
            publish(snapshot(last['id'], last['status'], last.get('result', 'NO_PENDING_REQUEST')))
            return
        request = requests[0]
        request_id = request['id']
        # Authorization/expiry failures cannot touch the installer or its ledger.
        try:
            p = authorize(request)
        except (deploy.DeployError, KeyError, ValueError):
            deploy.atomic_json(file, {'id': request_id, 'status': 'failure', 'result': 'DEPLOYMENT_REQUEST_REJECTED'})
            publish(snapshot(request_id, 'failure', 'DEPLOYMENT_REQUEST_REJECTED'))
            return
        with deploy.locked(STATE):
            config = deploy.configuration(CONFIG)
            installer = deploy.Installer(STATE, config)
            deploy.atomic_json(file, {'id': request_id, 'status': 'running'})
            publish(snapshot(request_id, 'running', 'VALIDATING_RELEASE'))
            try:
                with tempfile.TemporaryDirectory(prefix='release-', dir=CD_STATE) as directory:
                    target = Path(directory)
                    manifest = download(p['version'], target)
                    deploy.require(request['sha'] == manifest['revision'], 'DEPLOYMENT_REVISION_MISMATCH')
                    if p['operation'] == 'rollback':
                        expected = snapshot(request_id, 'running', '')['rollback_candidate']
                        deploy.require(expected == summary({'manifest': manifest}), 'ROLLBACK_TARGET_CHANGED')
                    prior_file = STATE / 'deployment.json'
                    prior = deploy.read_json(prior_file) if prior_file.exists() else {'current': None, 'previous': None, 'pending': None}
                    deploy.require(not prior['current'] or prior['current'].get('config_fingerprint') == installer.fingerprint(),
                        'CONFIG_CHANGED_USE_OPERATOR_APPLY')
                    result = installer.perform(target, rollback=p['operation'] == 'rollback', allow_interrupt=True)
                    try:
                        public_health()
                    except Exception:
                        if result in ('DEPLOYED', 'ROLLED_BACK'):
                            attempted = deploy.read_json(prior_file)['current']
                            restored = installer.restore(prior['current'], attempted)
                            deploy.atomic_json(prior_file, dict(prior, current=restored))
                            raise deploy.DeployError('PUBLIC_CHECK_FAILED_ROLLBACK_COMPLETED') from None
                        raise deploy.DeployError('PUBLIC_CHECK_FAILED_NO_VERSION_CHANGE') from None
                outcome = 'success'
            except (Exception, KeyboardInterrupt) as error:
                result = str(error) if isinstance(error, deploy.DeployError) else 'DEPLOYMENT_FAILED_CHECK_HOST'
                outcome = 'failure'
            deploy.atomic_json(file, {'id': request_id, 'status': outcome, 'result': result})
            publish(snapshot(request_id, outcome, result))


def ci():
    deploy.require(os.environ.get('GITHUB_REPOSITORY') == REPO and os.environ.get('GITHUB_REF') == 'refs/heads/main' and
        os.environ.get('GITHUB_EVENT_NAME') == 'workflow_dispatch', 'MAIN_DISPATCH_REQUIRED')
    deploy.require(os.environ.get('ALLOW_INTERRUPT') == 'true', 'ACKNOWLEDGE_AGENT_INTERRUPTION_REQUIRED')
    token = os.environ['GITHUB_TOKEN']
    operation = os.environ['DEPLOY_OPERATION']
    deploy.require(operation in ('apply', 'rollback'), 'INVALID_OPERATION')
    version = os.environ.get('DEPLOY_VERSION', '')
    if operation == 'rollback':
        previous = json.loads(fetch(STATUS, 16384)).get('rollback_candidate')
        deploy.require(previous is not None, 'NO_PREVIOUS_RELEASE')
        version = previous['version']
    hashes = released(version)
    raw = fetch('https://github.com/' + REPO + '/releases/download/' + version + '/release.json', 65536)
    deploy.require('sha256:' + hashlib.sha256(raw).hexdigest() == hashes['release.json'], 'MANIFEST_DIGEST_MISMATCH')
    manifest = json.loads(raw)
    request = github('/deployments', {'ref': version, 'task': 'panel:' + operation, 'environment': ENVIRONMENT,
        'auto_merge': False, 'required_contexts': [], 'production_environment': True,
        'payload': {'schema': 1, 'operation': operation, 'version': version, 'run_id': int(os.environ['GITHUB_RUN_ID']),
            'run_attempt': int(os.environ['GITHUB_RUN_ATTEMPT']), 'workflow_sha': os.environ['GITHUB_SHA'], 'allow_interrupt': True}}, token)
    request_id = request['id']
    log_url = 'https://github.com/' + REPO + '/actions/runs/' + os.environ['GITHUB_RUN_ID']

    def record(state, message):
        github('/deployments/' + str(request_id) + '/statuses', {'state': state, 'description': message,
            'log_url': log_url, 'environment_url': PUBLIC + '/', 'auto_inactive': False}, token)

    record('in_progress', 'Server is validating and deploying the published release')
    print('DEPLOYMENT_REQUESTED', request_id, version, flush=True)
    try:
        deadline = time.monotonic() + 600
        while time.monotonic() < deadline:
            try:
                status = json.loads(fetch(STATUS + '?request=' + str(request_id), 16384))
            except (OSError, ValueError):
                status = {}
            if status.get('request_id') == request_id and status.get('status') in ('success', 'failure'):
                deploy.require(status['status'] == 'success', 'HOST_DEPLOYMENT_FAILED')
                deploy.require(status.get('current') == summary({'manifest': manifest}), 'HOST_RELEASE_READBACK_MISMATCH')
                public_health()
                record('success', 'Exact release and public health/auth verified')
                print('DEPLOYMENT_VERIFIED', request_id, version, manifest['image'], flush=True)
                return
            time.sleep(5)
        raise deploy.DeployError('DEPLOYMENT_RESULT_TIMEOUT_CHECK_HOST')
    except BaseException:
        record('failure', 'Deployment or public readback failed; inspect host status and rollback')
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['ci', 'poll', 'serve'])
    args = parser.parse_args()
    if args.action == 'ci':
        ci()
    elif args.action == 'poll':
        poll()
    else:
        while True:
            try:
                poll()
            except Exception:
                print('CD_POLL_FAILED_RETRY_IN_120S', flush=True)
            time.sleep(120)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, deploy.DeployError) else 'CD_FAILED_CHECK_AUTHORIZATION_AND_HOST', flush=True)
        raise SystemExit(1) from None
