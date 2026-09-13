import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import component_deploy as host
import component_release as release
import deploy
import provision_codex_node as provision
import tool_activation as tools


def manifest(name, version, compatibility, digest):
    return {
        'schema': 1, 'component': name, 'version': version,
        'revision': 'a' * 40,
        'image': release.COMPONENTS[name]['image'] + '@sha256:' + digest * 64,
        'platform': 'linux/amd64',
        'adapter_version': {'cursor': '1.0.31', 'codex': '0.153.4'}[name],
        'state_compatibility': compatibility,
    }


class FakeRouter:
    def __init__(self, installer):
        self.installer = installer

    def transition_many(self, action, current, operation_id, identities=None):
        result = {}
        for node_id, value in current.items():
            result[node_id] = host.RouterControl.projected(
                action, value, operation_id, (identities or {}).get(node_id))
        self.installer.routes.update(copy.deepcopy(result))
        return result


class FakeInstaller:
    def __init__(self, root, layout, priors):
        self.root = root
        self.file = root / 'deployment.json'
        self.config = {
            'project': 'homelab-panel-alpha', 'compose': str(layout['compose']),
            'env_file': str(root / 'env'), 'router_socket': str(root / 'router/control.sock'),
            'components': {
                'panel': {'service': 'panel'},
                'cursor': {'service': 'cursor', 'url': 'https://cursor:18443',
                           'node_id': '11111111-1111-1111-1111-111111111111',
                           'actor_id': 'owner'},
                'codex': {'service': 'codex', 'url': 'https://codex:18443',
                          'node_id': '22222222-2222-2222-2222-222222222222',
                          'actor_id': 'owner'},
            },
        }
        self.routes = {
            self.config['components'][name]['node_id']: {
                'mode': 'eligible', 'stateVersion': 1, 'generation': 1,
                'identityEpoch': 7, 'adapterKind': name,
                'adapterVersion': priors[name]['adapter_version'],
            } for name in tools.COMPONENTS
        }
        self.router = FakeRouter(self)
        self.target_runtime = False
        self.fail_mutation = False
        self.priors = priors
        self.ledger = {
            'components': {
                'panel': {'current': manifest('cursor', 'v0.1.0', 'f' * 64, 'f'),
                          'previous': None},
                **{name: {'current': copy.deepcopy(priors[name]), 'previous': None}
                   for name in tools.COMPONENTS},
            },
            'pending': None, 'config_sha256': '',
        }

    def fingerprint(self):
        digest = hashlib.sha256()
        files = [self.root / 'config.json', Path(self.config['compose']),
                 Path(self.config['env_file'])]
        files.extend(Path(value) for value in self.config.get('compose_overrides', []))
        for path in files:
            digest.update(path.read_bytes() + b'\0')
        return digest.hexdigest()

    def container(self, name):
        return {'cursor': 'b' * 64, 'codex': 'c' * 64}[name]

    def inspect(self, name, _manifest, container=None):
        mounts = [{'Type': 'bind', 'Source': str(TEST_LAYOUT[name + '_config']),
                   'Destination': '/config', 'RW': False}]
        if name == 'codex' or self.target_runtime:
            mounts.append({'Type': 'bind', 'Source': str(TEST_LAYOUT[name + '_workspace']),
                           'Destination': '/workspace',
                           'RW': self.target_runtime})
        return {'Mounts': mounts}, {}

    def health(self, name, manifest_value):
        return {'node': self.config['components'][name]['node_id'], 'epoch': 7,
                'registry': 1, 'kind': name, 'version': manifest_value['adapter_version'],
                'schemaId': 'fixture', 'schemaSHA256': 'e' * 64, 'quiescent': True}

    @staticmethod
    def identity(health):
        return {key: health[key] for key in ('node', 'epoch', 'registry', 'kind', 'version')}

    def routing(self):
        return {'registryVersion': 1, 'nodes': copy.deepcopy(self.routes)}

    def require_routes(self, names, mode, operation_id=None):
        expected = {self.config['components'][name]['node_id'] for name in names}
        selected = {key: value for key, value in self.routes.items() if key in expected}
        assert set(selected) == expected
        assert all(value['mode'] == mode and
                   (operation_id is None or value.get('operationId') == operation_id)
                   for value in selected.values())
        return {'registryVersion': 1, 'nodes': copy.deepcopy(self.routes)}

    def route_subset(self, names, state):
        return {self.config['components'][name]['node_id']:
                copy.deepcopy(state['nodes'][self.config['components'][name]['node_id']])
                for name in names}

    def identity_subset(self, names, identities):
        return {self.config['components'][name]['node_id']: identities[name]
                for name in names}

    def verify_transition(self, _):
        return None

    def pin_override(self, name, manifest_value):
        path = self.root / (name + '.override.json')
        deploy.atomic_json(path, {'services': {name: {'image': manifest_value['image']}}})
        return path

    def compose_mutation(self, *args, override=None):
        if self.fail_mutation:
            self.fail_mutation = False
            raise host.ContainerMutationUnknown('CONTAINER_MUTATION_OUTCOME_UNKNOWN')
        if args[0] == 'up':
            self.target_runtime = override is not None and '.target.' in str(override)
        elif args[0] == 'stop':
            self.target_runtime = False

    def compose(self, *args, override=None):
        if args[:4] == ('ps', '--status', 'running', '-q'):
            return ''
        raise AssertionError(args)

    def wait_runtime_identity(self, name, desired, alternate, timeout=45):
        return 'desired' if desired == self.priors[name] and not self.target_runtime else 'alternate'


TEST_LAYOUT = {}


class ToolActivationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name).resolve()
        root.chmod(0o700)
        global TEST_LAYOUT
        TEST_LAYOUT = {
            'project': 'homelab-panel-alpha', 'compose': root / 'compose.yaml',
            'cursor_config': root / 'node-config',
            'codex_config': root / 'cutover/codex-node/codex-config',
            'cursor_workspace': root / 'cursor-workspace',
            'codex_workspace': root / 'cutover/codex-node/codex-workspace',
        }
        for directory in (TEST_LAYOUT['cursor_config'], TEST_LAYOUT['codex_config'],
                          TEST_LAYOUT['codex_workspace'], root / 'router'):
            directory.mkdir(parents=True, mode=0o700)
            directory.chmod(0o700)
        TEST_LAYOUT['compose'].write_text('services: {}\n')
        (root / 'env').write_text('FIXTURE=true\n')
        for path in (TEST_LAYOUT['compose'], root / 'env'):
            path.chmod(0o600)
        self.plan = {
            'compatibility': {
                'cursor': {'from': '1' * 64, 'to': '3' * 64},
                'codex': {'from': '2' * 64, 'to': '4' * 64},
            },
            'tool_manifests': copy.deepcopy(tools.TOOL_MANIFESTS),
            'policy': tools.POLICY,
        }
        self.priors = {name: manifest(name, 'v0.2.0',
                                      self.plan['compatibility'][name]['from'], name[0])
                       for name in tools.COMPONENTS}
        self.targets = {name: manifest(name, 'v0.3.0',
                                       self.plan['compatibility'][name]['to'],
                                       {'cursor': 'a', 'codex': 'b'}[name])
                        for name in tools.COMPONENTS}
        for name in tools.COMPONENTS:
            node = {
                'listen': '0.0.0.0:18443',
                'nodeId': {'cursor': '11111111-1111-1111-1111-111111111111',
                           'codex': '22222222-2222-2222-2222-222222222222'}[name],
                'ownerId': 'owner', 'registryVersion': 1,
                'policyFile': '/config/policy.txt',
                'toolManifestFile': '/config/tools.json',
                'policyRevision': tools.SOURCE_POLICY_REVISION,
                name: {'workingDir': '/workspace'} if name == 'codex' else {},
            }
            if name == 'codex':
                node['adapter'] = 'codex'
            config = TEST_LAYOUT[name + '_config']
            (config / 'node.json').write_text(json.dumps(node) + '\n')
            (config / 'policy.txt').write_text('legacy deny policy\n')
            (config / 'tools.json').write_bytes(tools.SOURCE_TOOL_MANIFEST)
            for path in config.iterdir():
                path.chmod(0o600)
        self.installer = FakeInstaller(root, TEST_LAYOUT, self.priors)
        config_file = root / 'config.json'
        config_file.write_text(json.dumps(self.installer.config) + '\n')
        config_file.chmod(0o600)
        self.installer.ledger['config_sha256'] = self.installer.fingerprint()
        deploy.atomic_json(self.installer.file, self.installer.ledger)
        self.patches = patch.multiple(tools, PLAN=self.plan, LAYOUT=TEST_LAYOUT)
        self.patches.start()
        self.addCleanup(self.patches.stop)
        self.private = patch.object(tools, 'private_path', side_effect=lambda path, owner, directory=False, mode=None: Path(path).lstat())
        self.private.start()
        self.addCleanup(self.private.stop)

    def test_contract_is_fail_closed_until_exact_adapter_contract_is_integrated(self):
        with patch.object(tools, 'PLAN', {
            'compatibility': copy.deepcopy(self.plan['compatibility']),
            'tool_manifests': {'cursor': None, 'codex': None}, 'policy': None,
        }), self.assertRaisesRegex(deploy.DeployError, 'CONTRACT_NOT_INTEGRATED'):
            tools.contract()

    def test_contract_rejects_reordered_or_extended_manifest(self):
        for name, content in (
            ('cursor', b'[{"name":"cursor.file_change"},{"name":"cursor.command"}]\n'),
            ('codex', b'[{"name":"codex.command"},{"name":"codex.file_change","extra":true}]\n'),
        ):
            changed = copy.deepcopy(self.plan)
            changed['tool_manifests'][name] = content
            with self.subTest(name=name), patch.object(tools, 'PLAN', changed), \
                 self.assertRaisesRegex(deploy.DeployError, 'CONTRACT_NOT_INTEGRATED'):
                tools.contract()

    def test_codex_provisioning_uses_the_same_reviewed_policy_contract(self):
        self.assertEqual(provision.TOOL_MANIFEST, tools.TOOL_MANIFESTS['codex'])
        self.assertEqual(provision.POLICY.encode(), tools.POLICY)
        self.assertEqual(provision.POLICY_REVISION, tools.POLICY_REVISION)
        self.assertEqual(provision.APPROVAL_MODE, tools.APPROVAL_MODE)

    def test_reviewed_contract_byte_hashes_are_pinned(self):
        self.assertEqual({name: hashlib.sha256(value).hexdigest()
                          for name, value in tools.TOOL_MANIFESTS.items()},
                         tools.TOOL_MANIFEST_SHA256)
        self.assertEqual(hashlib.sha256(tools.POLICY).hexdigest(), tools.POLICY_SHA256)

    def test_bundle_preserves_exact_sources_and_builds_distinct_targets(self):
        operation = tools.Operation(self.installer)
        paths = operation.layout({name: self.installer.inspect(name, self.priors[name])[0]
                                  for name in tools.COMPONENTS})
        bundle = operation.prepare_bundle('agent-tools-7', paths)
        root = operation.backup_root('agent-tools-7')
        self.assertEqual((root / 'cursor-tools.json.target').read_bytes(),
                         tools.TOOL_MANIFESTS['cursor'])
        self.assertEqual((root / 'codex-tools.json.target').read_bytes(),
                         tools.TOOL_MANIFESTS['codex'])
        self.assertEqual((root / 'cursor-policy.txt.target').read_bytes(), tools.POLICY)
        self.assertEqual((root / 'codex-policy.txt.target').read_bytes(), tools.POLICY)
        cursor = json.loads((root / 'cursor-node.json.target').read_text())
        codex = json.loads((root / 'codex-node.json.target').read_text())
        self.assertEqual(cursor['cursor']['workingDir'], '/workspace')
        self.assertEqual(cursor['adapter'], 'cursor')
        self.assertEqual(codex['codex']['workingDir'], '/workspace')
        self.assertEqual({cursor['approvalMode'], codex['approvalMode']}, {'explicit_once'})
        self.assertEqual({cursor['policyRevision'], codex['policyRevision']}, {'agent-tools-v1'})
        source = json.loads((root / 'host-config.source').read_text())
        target = json.loads((root / 'host-config.target').read_text())
        self.assertNotIn('compose_overrides', source)
        self.assertEqual(target['compose_overrides'],
                         [str(self.installer.root / 'agent-tools-7.compose.json')])
        self.assertEqual(operation.verify_bundle(bundle), bundle)

    def test_success_activates_both_targets_only_after_runtime_readback(self):
        operation = tools.Operation(self.installer)
        with patch.object(operation, 'validate_candidate_image'), \
             patch.object(operation, 'create_cursor_workspace', side_effect=self._create_workspace), \
             patch.object(host, 'run', return_value='0:0:555:regular file'):
            result = operation.apply(self.targets, 'agent-tools-8')
        self.assertEqual(result, 'TOOL_ACTIVATION_DEPLOYED')
        self.assertIsNone(self.installer.ledger['pending'])
        self.assertTrue(self.installer.target_runtime)
        self.assertTrue(all(value['mode'] == 'eligible' for value in self.installer.routes.values()))
        for name in tools.COMPONENTS:
            self.assertEqual(self.installer.ledger['components'][name],
                             {'current': self.targets[name], 'previous': self.priors[name]})
        config = json.loads((self.installer.root / 'config.json').read_text())
        self.assertEqual(config['compose_overrides'],
                         [str(self.installer.root / 'agent-tools-8.compose.json')])
        with self.assertRaisesRegex(deploy.DeployError, 'INVALID_TOOL_ACTIVATION_JOURNAL'):
            operation.restore('agent-tools-8', operation.bundle_sha256('agent-tools-8'))

    def test_unknown_replace_remains_sealed_and_forbids_an_inverse_mutation(self):
        operation = tools.Operation(self.installer)
        self.installer.fail_mutation = True
        with patch.object(operation, 'validate_candidate_image'), \
             patch.object(operation, 'create_cursor_workspace', side_effect=self._create_workspace), \
             self.assertRaisesRegex(deploy.DeployError, 'RESTORE_REQUIRED'):
            operation.apply(self.targets, 'agent-tools-9')
        self.assertEqual(self.installer.ledger['pending']['phase'], 'replace_unknown')
        self.assertTrue(all(value['mode'] == 'sealed' for value in self.installer.routes.values()))
        with self.assertRaisesRegex(deploy.DeployError, 'RESTORE_REQUEST_MISMATCH'):
            operation.restore('agent-tools-9', operation.bundle_sha256('agent-tools-9'))
        self.assertIsNotNone(self.installer.ledger['pending'])
        self.assertTrue(all(value['mode'] == 'sealed' for value in self.installer.routes.values()))

    def test_known_replacement_failure_can_restore_exact_priors(self):
        operation = tools.Operation(self.installer)
        with patch.object(operation, 'validate_candidate_image'), \
             patch.object(operation, 'create_cursor_workspace', side_effect=self._create_workspace), \
             patch.object(operation, 'verify_runtime', side_effect=deploy.DeployError('FIXTURE_READBACK_FAILED')), \
             self.assertRaisesRegex(deploy.DeployError, 'RESTORE_REQUIRED'):
            operation.apply(self.targets, 'agent-tools-10')
        self.assertEqual(self.installer.ledger['pending']['phase'], 'replaced')
        self.assertTrue(all(value['mode'] == 'sealed' for value in self.installer.routes.values()))
        with patch.object(host, 'run', return_value='0:0:755:regular file'):
            self.assertEqual(operation.restore('agent-tools-10',
                                               operation.bundle_sha256('agent-tools-10')),
                             'TOOL_ACTIVATION_PRIORS_RESTORED')
        self.assertIsNone(self.installer.ledger['pending'])
        self.assertFalse(self.installer.target_runtime)
        self.assertTrue(all(value['mode'] == 'eligible' for value in self.installer.routes.values()))
        self.assertNotIn('compose_overrides', json.loads((self.installer.root / 'config.json').read_text()))

    def test_runtime_rejects_helper_with_writable_owner_mode(self):
        operation = tools.Operation(self.installer)
        with patch.object(operation, 'validate_candidate_image'), \
             patch.object(operation, 'create_cursor_workspace', side_effect=self._create_workspace), \
             patch.object(host, 'run', return_value='0:0:755:regular file'), \
             self.assertRaisesRegex(deploy.DeployError, 'RESTORE_REQUIRED'):
            operation.apply(self.targets, 'agent-tools-11')
        self.assertEqual(self.installer.ledger['pending']['phase'], 'replaced')
        self.assertTrue(all(value['mode'] == 'sealed' for value in self.installer.routes.values()))

    def _create_workspace(self):
        path = TEST_LAYOUT['cursor_workspace']
        path.mkdir(mode=0o700)
        path.chmod(0o700)


if __name__ == '__main__':
    unittest.main()
