#!/usr/bin/env python3
"""Immutable, independently versioned Panel / Cursor / Codex releases."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

import deploy

ROOT = Path(__file__).resolve().parent.parent
REPO = 'boxvtk621/homelab-telegram-panel'
COMPONENTS = {
    'panel': {'image': 'ghcr.io/boxvtk621/homelab-telegram-panel', 'dockerfile': 'Dockerfile'},
    'cursor': {'image': 'ghcr.io/boxvtk621/homelab-harness-cursor', 'dockerfile': 'deploy/components/Dockerfile.cursor'},
    'codex': {'image': 'ghcr.io/boxvtk621/homelab-harness-codex', 'dockerfile': 'deploy/components/Dockerfile.codex'},
}
TAG = re.compile(r'(panel|cursor|codex)-(v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-rc\.[1-9][0-9]*)?)')


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def selection(tag):
    match = TAG.fullmatch(tag)
    deploy.require(match is not None, 'INVALID_COMPONENT_TAG')
    component, version = match.groups()
    return component, version


def buildable(component):
    deploy.require(component in COMPONENTS, 'UNKNOWN_COMPONENT')
    if component == 'codex':
        # HL-258 is a real runtime dependency, not a successful placeholder build.
        deploy.require((ROOT / 'harness/adapters/codex/adapter.go').is_file() and
                       (ROOT / COMPONENTS[component]['dockerfile']).is_file(), 'CODEX_ADAPTER_REQUIRED_HL258')
    return COMPONENTS[component]


def compatibility(component):
    if component == 'panel':
        return 'stateless-panel-v1'
    paths = ['harness/node/sql.go', 'harness/node/schema.go',
             'harness/adapters/' + component + '/store.go']
    digest = hashlib.sha256()
    for name in paths:
        digest.update(name.encode() + b'\0' + (ROOT / name).read_bytes() + b'\0')
    return digest.hexdigest()


def validate(manifest, component=None):
    deploy.require(type(manifest) is dict and set(manifest) ==
                   {'schema', 'component', 'version', 'revision', 'image', 'platform', 'state_compatibility', 'adapter_version'}, 'INVALID_COMPONENT_MANIFEST')
    name = manifest['component']
    deploy.require(name in COMPONENTS and (component is None or component == name), 'COMPONENT_MISMATCH')
    deploy.require(manifest['schema'] == 1 and manifest['platform'] == 'linux/amd64', 'UNSUPPORTED_COMPONENT_MANIFEST')
    deploy.require(isinstance(manifest['version'], str) and deploy.VERSION.fullmatch(manifest['version']), 'INVALID_VERSION')
    deploy.require(isinstance(manifest['revision'], str) and re.fullmatch('[0-9a-f]{40}', manifest['revision']), 'INVALID_REVISION')
    image = manifest['image']
    deploy.require(isinstance(image, str) and image.startswith(COMPONENTS[name]['image'] + '@') and
                   deploy.DIGEST.fullmatch(image.split('@')[-1]), 'INVALID_COMPONENT_IMAGE')
    state = manifest['state_compatibility']
    deploy.require(state == 'stateless-panel-v1' if name == 'panel' else
                   isinstance(state, str) and re.fullmatch('[0-9a-f]{64}', state), 'INVALID_STATE_COMPATIBILITY')
    adapter_version = manifest['adapter_version']
    deploy.require(adapter_version is None if name == 'panel' else isinstance(adapter_version, str) and
                   re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', adapter_version), 'INVALID_ADAPTER_VERSION')
    return manifest


def adapter_version(component):
    if component == 'panel':
        return None
    constant = {'cursor': 'CursorSDKVersion', 'codex': 'CodexAppServerVersion'}[component]
    source = (ROOT / 'internal/harnessadapter/adapter.go').read_text()
    match = re.search(constant + r'\s*=\s*"([0-9.]+)"', source)
    deploy.require(match is not None, 'ADAPTER_PIN_MISSING')
    return match[1]


def plan(tag):
    component, version = selection(tag)
    config = buildable(component)
    revision = os.environ['GITHUB_SHA']
    deploy.require(command('git', 'rev-parse', 'HEAD') == revision and
                   command('git', 'rev-parse', 'refs/tags/' + tag + '^{commit}') == revision, 'SOURCE_TAG_MISMATCH')
    subprocess.run(['git', 'merge-base', '--is-ancestor', revision, 'origin/main'], check=True)
    # Reuse the existing guard: only a definitive 404 allows publication.
    import release
    release.guard(tag)
    with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
        for key, value in dict(component=component, version=version, **config).items():
            output.write(key + '=' + value + '\n')


def publish(tag):
    component, version = selection(tag)
    config = buildable(component)
    revision = os.environ['GITHUB_SHA']
    image_tag = config['image'] + ':' + tag
    data = json.loads(command('docker', 'buildx', 'imagetools', 'inspect', image_tag, '--format', '{{json .Manifest}}'))
    manifest = validate(dict(schema=1, component=component, version=version, revision=revision,
                             image=config['image'] + '@' + data['digest'], platform='linux/amd64',
                             state_compatibility=compatibility(component), adapter_version=adapter_version(component)))
    output = ROOT / '.component-release'
    output.mkdir(exist_ok=False)
    (output / 'release.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (output / 'notes.md').write_text(f'{component} {version}, source {revision}.\n\n'
        f'Image: `{manifest["image"]}`. linux/amd64.\n'
        'Use Deploy component to update only this component. Configuration, credentials and state stay on the host.\n')
    subprocess.run(['gh', 'release', 'create', tag, '--repo', REPO, '--verify-tag', '--draft',
                    '--title', tag, '--notes-file', str(output / 'notes.md'), '--latest=false',
                    *(['--prerelease'] if '-rc.' in version else []), str(output / 'release.json')], check=True)
    subprocess.run(['gh', 'release', 'edit', tag, '--repo', REPO, '--draft=false'], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['plan', 'publish', 'gate'])
    parser.add_argument('target')
    args = parser.parse_args()
    if args.action == 'gate':
        buildable(args.target)
    else:
        globals()[args.action](args.target)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(str(error) if isinstance(error, deploy.DeployError) else 'COMPONENT_RELEASE_FAILED', flush=True)
        raise SystemExit(1) from None
