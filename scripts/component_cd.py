#!/usr/bin/env python3
"""Component selector on the existing outbound GitHub Deployments trust model."""
import argparse
import datetime
import hashlib
import json
import os
import time

import cd
import deploy
import component_release as release
import component_deploy as host
import wire_migration as wire

ENVIRONMENT = 'agents-production'
WORKFLOW = '.github/workflows/component-deploy.yml'
STATUS = cd.PUBLIC + '/components.json'


def download(tag):
    component, version = release.selection(tag)
    metadata = cd.github('/releases/tags/' + tag)
    deploy.require(metadata.get('draft') is False and metadata.get('published_at') and
                   metadata.get('tag_name') == tag and metadata.get('author', {}).get('login') == 'github-actions[bot]', 'UNTRUSTED_COMPONENT_RELEASE')
    assets = metadata.get('assets', [])
    deploy.require(len(assets) == 1 and assets[0].get('name') == 'release.json', 'COMPONENT_RELEASE_ASSETS_MISMATCH')
    raw = cd.fetch('https://github.com/' + release.REPO + '/releases/download/' + tag + '/release.json', 16384)
    deploy.require('sha256:' + hashlib.sha256(raw).hexdigest() == assets[0].get('digest'), 'RELEASE_ASSET_DIGEST_MISMATCH')
    manifest = release.validate(json.loads(raw, object_pairs_hook=deploy.pairs), component)
    deploy.require(manifest['version'] == version, 'RELEASE_VERSION_MISMATCH')
    runs = cd.github('/actions/runs?head_sha=' + manifest['revision'] + '&event=push&status=success&per_page=20')
    deploy.require(any(run.get('path') == '.github/workflows/component-release.yml' and
                       run.get('head_sha') == manifest['revision'] and run.get('event') == 'push' and
                       run.get('conclusion') == 'success' for run in runs.get('workflow_runs', [])), 'COMPONENT_RELEASE_CI_NOT_SUCCESSFUL')
    return manifest


def authorize(request, now=None, read=None):
    read = read or cd.github
    payload = request.get('payload')
    if request.get('task') == wire.TASK:
        deploy.require(cd.positive(request.get('id')) and request.get('environment') == ENVIRONMENT and
                       request.get('creator', {}).get('login') == 'github-actions[bot]' and
                       request.get('creator', {}).get('type') == 'Bot', 'UNAUTHORIZED_DEPLOYMENT')
        deploy.require(type(payload) is dict and set(payload) ==
                       {'schema', 'migration', 'tags', 'target_revision', 'run_id', 'run_attempt',
                        'workflow_sha', 'allow_interrupt'} and
                       payload['schema'] == 1 and payload['migration'] == wire.MIGRATION and
                       type(payload['tags']) is dict and set(payload['tags']) == set(wire.COMPONENTS) and
                       payload['allow_interrupt'] is True and cd.positive(payload['run_id']) and
                       cd.positive(payload['run_attempt']) and isinstance(payload['workflow_sha'], str) and
                       deploy.re.fullmatch('[0-9a-f]{40}', payload['workflow_sha']) and
                       isinstance(payload['target_revision'], str) and
                       deploy.re.fullmatch('[0-9a-f]{40}', payload['target_revision']) and
                       request.get('ref') == payload['tags'].get('panel'), 'INVALID_WIRE_MIGRATION_REQUEST')
        for name, tag in payload['tags'].items():
            component, _ = release.selection(tag)
            deploy.require(component == name, 'INVALID_WIRE_MIGRATION_REQUEST')
        created = datetime.datetime.fromisoformat(request['created_at'].replace('Z', '+00:00')).timestamp()
        deploy.require(0 <= (time.time() if now is None else now) - created <= 900,
                       'DEPLOYMENT_REQUEST_EXPIRED')
        run = read('/actions/runs/' + str(payload['run_id']))
        deploy.require(run.get('event') == 'workflow_dispatch' and run.get('path') == wire.WORKFLOW and
                       run.get('head_branch') == 'main' and run.get('head_sha') == payload['workflow_sha'] and
                       run.get('head_repository', {}).get('full_name') == release.REPO and
                       run.get('status') == 'in_progress' and run.get('run_attempt') == payload['run_attempt'],
                       'UNTRUSTED_WIRE_MIGRATION_WORKFLOW')
        return dict(payload, kind=wire.MIGRATION)
    deploy.require(cd.positive(request.get('id')) and request.get('environment') == ENVIRONMENT and
                   request.get('creator', {}).get('login') == 'github-actions[bot]' and
                   request.get('creator', {}).get('type') == 'Bot', 'UNAUTHORIZED_DEPLOYMENT')
    deploy.require(type(payload) is dict and set(payload) ==
                   {'schema', 'component', 'operation', 'tag', 'run_id', 'run_attempt', 'workflow_sha', 'allow_interrupt'}, 'INVALID_COMPONENT_REQUEST')
    deploy.require(payload['schema'] == 1 and payload['component'] in release.COMPONENTS and
                   payload['operation'] in ('apply', 'rollback') and payload['allow_interrupt'] is True and
                   cd.positive(payload['run_id']) and cd.positive(payload['run_attempt']) and
                   isinstance(payload['workflow_sha'], str) and deploy.re.fullmatch('[0-9a-f]{40}', payload['workflow_sha']), 'INVALID_COMPONENT_REQUEST')
    component, _ = release.selection(payload['tag'])
    deploy.require(component == payload['component'] and request.get('ref') == payload['tag'] and
                   request.get('task') == component + ':' + payload['operation'], 'COMPONENT_REQUEST_TARGET_MISMATCH')
    created = datetime.datetime.fromisoformat(request['created_at'].replace('Z', '+00:00')).timestamp()
    deploy.require(0 <= (time.time() if now is None else now) - created <= 900, 'DEPLOYMENT_REQUEST_EXPIRED')
    run = read('/actions/runs/' + str(payload['run_id']))
    deploy.require(run.get('event') == 'workflow_dispatch' and run.get('path') == WORKFLOW and
                   run.get('head_branch') == 'main' and run.get('head_sha') == payload['workflow_sha'] and
                   run.get('head_repository', {}).get('full_name') == release.REPO and
                   run.get('status') == 'in_progress' and run.get('run_attempt') == payload['run_attempt'], 'UNTRUSTED_COMPONENT_WORKFLOW')
    return payload


def status(record, installer):
    # This is an allowlist projection; neither configuration nor private pending
    # details become part of the publicly served result.
    pending = installer.ledger['pending']
    phase = pending.get('phase') if type(pending) is dict else None
    normal_phases = {'prepared', 'sealed', 'replace_started', 'replace_unknown',
                     'target_verified', 'restore_started', 'prior_verified', 'activation_pending'}
    if phase not in normal_phases | wire.Operation.PHASES:
        phase = 'invalid' if pending is not None else None
    value = dict(schema=1, request_id=record['id'], status=record['status'],
                 result=record.get('result', 'IDLE'), component=record.get('component'), components={},
                 operator_action_required=pending is not None,
                 pending_phase=phase)
    for name, slot in installer.ledger['components'].items():
        value['components'][name] = {'current': slot['current'], 'rollback_candidate': slot['previous']}
    return value


def publish(record, installer):
    # Reuse the atomic non-secret publisher, selecting this consumer's own file.
    original = cd.PUBLIC_FILE
    try:
        cd.PUBLIC_FILE = host.PUBLIC
        cd.publish(status(record, installer))
    finally:
        cd.PUBLIC_FILE = original


def poll():
    with deploy.locked(host.ROOT):
        installer = host.Installer()
        file = host.ROOT / 'request.json'
        record = deploy.read_json(file) if file.exists() else {'id': 0, 'status': 'idle'}
        pending = installer.ledger['pending']
        retry_failed_recovery = (record['status'] == 'failure' and record.get('component') != 'panel' and
                                 type(pending) is dict and
                                 pending.get('operation_id') == 'deploy-' + str(record['id']))
        if record['status'] == 'running' or retry_failed_recovery:
            try:
                pending = installer.ledger['pending']
                migration = type(pending) is dict and pending.get('kind') == wire.MIGRATION
                recovered = wire.Operation(installer).reconcile() if migration else installer.reconcile()
                if type(record.get('targets')) is dict:
                    exact_finished = (set(record['targets']) == set(wire.COMPONENTS) and
                                      all(installer.ledger['components'][name]['current'] == target
                                          for name, target in record['targets'].items()))
                else:
                    exact_finished = (type(record.get('target')) is dict and
                                      record.get('component') in installer.ledger['components'] and
                                      installer.ledger['components'][record['component']]['current'] == record['target'])
                successful_recovery = recovered in ('DEPLOYED_AFTER_RESTART', 'ROLLED_BACK_AFTER_RESTART',
                                                     'WIRE_MIGRATION_DEPLOYED_AFTER_RESTART')
                if successful_recovery and exact_finished:
                    record.update(status='success', result=recovered)
                elif recovered is None and exact_finished:
                    result = ('WIRE_MIGRATION_DEPLOYED_BEFORE_RESTART' if 'targets' in record else
                              'ROLLED_BACK_BEFORE_RESTART' if record.get('operation') == 'rollback' else
                              'DEPLOYED_BEFORE_RESTART')
                    record.update(status='success', result=result)
                else:
                    record.update(status='failure', result=recovered or 'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
            except Exception as error:
                record.update(status='failure', result=str(error) if isinstance(error, deploy.DeployError)
                              else 'INTERRUPTED_DEPLOYMENT_REQUIRES_OPERATOR')
            deploy.atomic_json(file, record)
            publish(record, installer)
            return
        if installer.ledger['pending'] is not None:
            publish(record, installer)
            return
        requests = cd.github('/deployments?environment=' + ENVIRONMENT + '&per_page=1')
        if not requests or requests[0]['id'] <= record['id']:
            publish(record, installer)
            return
        request = requests[0]
        record = {'id': request['id'], 'status': 'running'}
        try:
            payload = authorize(request)
            record['component'] = payload.get('component', wire.MIGRATION)
            deploy.atomic_json(file, record)
            publish(record, installer)
            if payload.get('kind') == wire.MIGRATION:
                targets = {name: download(payload['tags'][name]) for name in wire.COMPONENTS}
                deploy.require(request.get('sha') == payload['target_revision'] and
                               all(target['revision'] == payload['target_revision'] for target in targets.values()),
                               'WIRE_MIGRATION_SOURCE_MISMATCH')
                record.update(targets=targets, operation=wire.MIGRATION)
                deploy.atomic_json(file, record)
                result = wire.Operation(installer).apply(targets, 'wire-migrate-' + str(record['id']))
            else:
                manifest = download(payload['tag'])
                deploy.require(request.get('sha') == manifest['revision'], 'DEPLOYMENT_SOURCE_MISMATCH')
                record.update(target=manifest, operation=payload['operation'])
                deploy.atomic_json(file, record)
                result = installer.apply(payload['component'], manifest, payload['operation'] == 'rollback',
                                         'deploy-' + str(record['id']))
            record.update(status='success', result=result)
        except Exception as error:
            record.update(status='failure', result=str(error) if isinstance(error, deploy.DeployError) else 'COMPONENT_DEPLOYMENT_FAILED')
        deploy.atomic_json(file, record)
        publish(record, installer)


def ci():
    deploy.require(os.environ.get('GITHUB_REF') == 'refs/heads/main' and
                   os.environ.get('GITHUB_EVENT_NAME') == 'workflow_dispatch', 'MAIN_DISPATCH_REQUIRED')
    deploy.require(os.environ.get('ALLOW_INTERRUPT') == 'true', 'ACKNOWLEDGE_COMPONENT_RESTART_REQUIRED')
    component, operation = os.environ['DEPLOY_COMPONENT'], os.environ['DEPLOY_OPERATION']
    deploy.require(component in release.COMPONENTS and operation in ('apply', 'rollback'), 'INVALID_COMPONENT_OPERATION')
    release.buildable(component)
    token = os.environ['GITHUB_TOKEN']
    version = os.environ.get('DEPLOY_VERSION', '')
    if operation == 'rollback':
        value = json.loads(cd.fetch(STATUS, 32768))
        previous = value.get('components', {}).get(component, {}).get('rollback_candidate')
        deploy.require(previous is not None, 'NO_COMPONENT_ROLLBACK')
        version = previous['version']
    tag = component + '-' + version
    manifest = download(tag)
    payload = dict(schema=1, component=component, operation=operation, tag=tag,
                   run_id=int(os.environ['GITHUB_RUN_ID']), run_attempt=int(os.environ['GITHUB_RUN_ATTEMPT']),
                   workflow_sha=os.environ['GITHUB_SHA'], allow_interrupt=True)
    request = cd.github('/deployments', dict(ref=tag, task=component + ':' + operation, environment=ENVIRONMENT,
                         auto_merge=False, required_contexts=[], production_environment=True, payload=payload), token)
    request_id = request['id']
    log_url = 'https://github.com/' + release.REPO + '/actions/runs/' + str(payload['run_id'])

    def report(state, description):
        cd.github('/deployments/' + str(request_id) + '/statuses', dict(state=state, description=description,
                  log_url=log_url, environment_url=cd.PUBLIC + '/', auto_inactive=False), token)

    report('in_progress', 'Waiting for selected component deployment and exact runtime readback')
    print('COMPONENT_DEPLOYMENT_REQUESTED', request_id, tag, flush=True)
    try:
        deadline = time.monotonic() + 600
        while time.monotonic() < deadline:
            try:
                value = json.loads(cd.fetch(STATUS + '?request=' + str(request_id), 32768))
            except (OSError, ValueError):
                value = {}
            if value.get('request_id') == request_id and value.get('status') in ('success', 'failure'):
                deploy.require(value['status'] == 'success' and value.get('component') == component and
                               value.get('components', {}).get(component, {}).get('current') == manifest, 'COMPONENT_DEPLOYMENT_READBACK_FAILED')
                cd.public_health()
                report('success', 'Selected component digest, version and health verified')
                return
            time.sleep(5)
        raise deploy.DeployError('COMPONENT_DEPLOYMENT_TIMEOUT_CHECK_HOST')
    except BaseException:
        report('failure', 'Deployment incomplete or failed; inspect component result before retry')
        raise


def wire_ci():
    deploy.require(os.environ.get('GITHUB_REF') == 'refs/heads/main' and
                   os.environ.get('GITHUB_EVENT_NAME') == 'workflow_dispatch', 'MAIN_DISPATCH_REQUIRED')
    deploy.require(os.environ.get('ALLOW_INTERRUPT') == 'true', 'ACKNOWLEDGE_WIRE_MIGRATION_REQUIRED')
    tags = {name: name + '-' + os.environ['DEPLOY_' + name.upper() + '_VERSION'] for name in wire.COMPONENTS}
    targets = {name: download(tag) for name, tag in tags.items()}
    workflow_sha = os.environ['GITHUB_SHA']
    target_revisions = {targets[name]['revision'] for name in wire.COMPONENTS}
    deploy.require(len(target_revisions) == 1, 'WIRE_MIGRATION_TARGET_REVISION_MISMATCH')
    target_revision = target_revisions.pop()
    token = os.environ['GITHUB_TOKEN']
    payload = {'schema': 1, 'migration': wire.MIGRATION, 'tags': tags,
               'target_revision': target_revision,
               'run_id': int(os.environ['GITHUB_RUN_ID']),
               'run_attempt': int(os.environ['GITHUB_RUN_ATTEMPT']),
               'workflow_sha': workflow_sha, 'allow_interrupt': True}
    request = cd.github('/deployments', dict(ref=tags['panel'], task=wire.TASK, environment=ENVIRONMENT,
                         auto_merge=False, required_contexts=[], production_environment=True, payload=payload), token)
    request_id = request['id']
    log_url = 'https://github.com/' + release.REPO + '/actions/runs/' + str(payload['run_id'])

    def report(state, description):
        cd.github('/deployments/' + str(request_id) + '/statuses', dict(state=state, description=description,
                  log_url=log_url, environment_url=cd.PUBLIC + '/', auto_inactive=False), token)

    report('in_progress', 'Waiting for sealed all-node wire migration and exact readback')
    print('WIRE_MIGRATION_REQUESTED', request_id, flush=True)
    try:
        deadline = time.monotonic() + 900
        while time.monotonic() < deadline:
            try:
                value = json.loads(cd.fetch(STATUS + '?request=' + str(request_id), 32768))
            except (OSError, ValueError):
                value = {}
            if value.get('request_id') == request_id and value.get('status') in ('success', 'failure'):
                deploy.require(value['status'] == 'success' and value.get('component') == wire.MIGRATION and
                               all(value.get('components', {}).get(name, {}).get('current') == targets[name]
                                   for name in wire.COMPONENTS), 'WIRE_MIGRATION_READBACK_FAILED')
                cd.public_health()
                report('success', 'Panel and both Harness targets activated together after v2 verification')
                return
            time.sleep(5)
        raise deploy.DeployError('WIRE_MIGRATION_TIMEOUT_CHECK_HOST')
    except BaseException:
        report('failure', 'Migration incomplete; nodes remain sealed pending exact recovery')
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['ci', 'wire-ci', 'poll', 'serve'])
    args = parser.parse_args()
    if args.action != 'serve':
        (wire_ci if args.action == 'wire-ci' else globals()[args.action])()
        return
    while True:
        try:
            poll()
        except Exception:
            print('COMPONENT_POLL_FAILED', flush=True)
        # 15 polls/hour leave headroom alongside the legacy 30 polls/hour.
        time.sleep(240)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, deploy.DeployError) else 'COMPONENT_CD_FAILED', flush=True)
        raise SystemExit(1) from None
