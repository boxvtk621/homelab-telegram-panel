import base64
import copy
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import router_registry_projection as projection


FIRST_ID = "20000000-0000-4000-8000-000000000001"
SECOND_ID = "20000000-0000-4000-8000-000000000002"
THIRD_ID = "20000000-0000-4000-8000-000000000003"
HOST_ID = "30000000-0000-4000-8000-000000000001"
WORKER_TOKEN = "test-worker-token-0000000000000001"


def node(node_id, name, adapter, pin):
    return {"nodeId": node_id, "name": name, "adapter": adapter,
            "url": "https://" + name.lower() + ".invalid:9443", "certificateSHA256": pin * 64}


def state_node(adapter, epoch, version, mode="eligible", operation=None):
    value = {"mode": mode, "stateVersion": version, "generation": 4,
             "identityEpoch": epoch, "adapterKind": adapter, "adapterVersion": "1.2.3"}
    if operation is not None:
        value["operationId"] = operation
    return value


class FixtureControl:
    def __init__(self, current):
        self.current = copy.deepcopy(current)
        self.requests = []

    def status(self):
        return copy.deepcopy(self.current)

    def install_registry_request(self, request):
        self.requests.append(copy.deepcopy(request))
        manifest = request["registry"]["manifest"]
        projected = {item["nodeId"]: item for item in manifest["nodes"]}
        nodes = {}
        for node_id, old in self.current["nodes"].items():
            item = projected[node_id]
            value = copy.deepcopy(old)
            value.update(registrationRevision=item["registrationRevision"],
                         identityEpoch=item["registrationEpoch"], compatibility=item["compatibility"])
            nodes[node_id] = value
        for node_id, item in projected.items():
            if node_id not in nodes:
                nodes[node_id] = {
                    "mode": "sealed", "stateVersion": 1, "generation": 0,
                    "operationId": request["operationId"], "registrationRevision": 1,
                    "identityEpoch": 1, "compatibility": item["compatibility"],
                    "adapterKind": item["adapter"], "adapterVersion": "",
                }
        manifest_hash = projection.RouterControl.registry_manifest_sha256(request["registry"])
        self.current = {
            "schema": 2, "ownerId": manifest["ownerId"],
            "registryVersion": manifest["registryVersion"], "registrySHA256": manifest_hash,
            "registryOperationId": request["operationId"],
            "registryRequestSHA256": projection.RouterControl.registry_request_sha256(
                request["operationId"], request["expected"], manifest_hash),
            "nodes": nodes,
        }
        return copy.deepcopy(self.current), False


class FixtureAgentControl:
    def __init__(self):
        self.request = None
        self.status_value = None
        self.transitions = []

    def reserve(self, request):
        if self.request is not None:
            if self.request != request:
                raise projection.ProjectionError("AGENT_SERVICE_REGISTRY_OPERATION_CONFLICT")
            return copy.deepcopy(self.status_value)
        self.request = copy.deepcopy(request)
        manifest = request["registry"]["manifest"]
        candidate_hash = projection.RouterControl.registry_manifest_sha256(request["registry"])
        affected = sorted(node["nodeId"] for node in manifest["nodes"])
        self.status_value = {
            "schemaId": "agent-registry-operation-status-v1",
            "receipt": {
                "schemaId": "agent-registry-operation-receipt-v1",
                "operationId": request["operationId"], "requestHash": "e" * 64,
                "expectedRegistryVersion": request["expected"]["registryVersion"],
                "expectedRegistrySHA256": request["expected"]["registrySHA256"],
                "candidateRegistryVersion": manifest["registryVersion"],
                "candidateRegistrySHA256": candidate_hash,
                "affectedNodeIds": affected, "acceptedAt": "2026-09-14T10:00:00Z",
            },
            "phase": "accepted", "effectState": "not_sent", "operationVersion": 1,
            "updatedAt": "2026-09-14T10:00:00Z", "resultCode": None,
        }
        return copy.deepcopy(self.status_value)

    def transition(self, status, transition):
        if status["operationVersion"] != self.status_value["operationVersion"]:
            raise projection.ProjectionError("AGENT_SERVICE_REGISTRY_OPERATION_CONFLICT")
        self.transitions.append(transition)
        self.status_value["operationVersion"] += 1
        if transition == "sent":
            self.status_value.update(phase="applying", effectState="sent")
        elif transition == "unknown":
            self.status_value.update(phase="reconciling", effectState="unknown")
        else:
            raise AssertionError(transition)
        return copy.deepcopy(self.status_value)

    def finish(self, status, readback, effect_state):
        if status["operationVersion"] != self.status_value["operationVersion"]:
            raise projection.ProjectionError("AGENT_SERVICE_REGISTRY_OPERATION_CONFLICT")
        if self.status_value["effectState"] == "unknown" and effect_state != "reconciled":
            raise projection.ProjectionError("AGENT_SERVICE_REGISTRY_OPERATION_CONFLICT")
        self.status_value.update(phase="succeeded", effectState=effect_state,
                                 operationVersion=self.status_value["operationVersion"] + 1,
                                 resultCode="registry:" + readback["registrySHA256"])
        return copy.deepcopy(self.status_value)

    def fail(self, status, result_code):
        if status["operationVersion"] != self.status_value["operationVersion"]:
            raise projection.ProjectionError("AGENT_SERVICE_REGISTRY_OPERATION_CONFLICT")
        self.status_value.update(phase="failed", effectState="failed",
                                 operationVersion=self.status_value["operationVersion"] + 1,
                                 resultCode=result_code)
        return copy.deepcopy(self.status_value)


class RouterRegistryProjectionTests(unittest.TestCase):
    def setUp(self):
        homebrew = Path("/opt/homebrew/opt/openssl@3/bin/openssl")
        self.openssl = str(homebrew) if homebrew.is_file() else shutil.which("openssl")
        if self.openssl is None:
            self.skipTest("OpenSSL is required")
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        self.run_openssl("genpkey", "-algorithm", "ed25519", "-out", "registry.key")
        self.run_openssl("pkey", "-in", "registry.key", "-pubout", "-out", "registry.pem")
        self.private = self.root / "registry.key"
        self.public = self.root / "registry.pem"
        self.private.chmod(0o600)

    def run_openssl(self, *arguments):
        subprocess.run([self.openssl, *arguments], cwd=self.root, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=15, check=True)

    def signed(self, manifest):
        message = self.root / "manifest.json"
        message.write_bytes(projection.canonical_manifest(manifest))
        signature = subprocess.check_output([
            self.openssl, "pkeyutl", "-sign", "-rawin", "-inkey", str(self.private),
            "-in", str(message),
        ])
        return {"manifest": manifest, "signature": base64.b64encode(signature).decode()}

    def write_json(self, name, value, private=False):
        path = self.root / name
        path.write_text(json.dumps(value, separators=(",", ":")) + "\n")
        if private:
            path.chmod(0o600)
        return path

    def test_forms_installs_and_retains_exact_retry_request(self):
        first = node(FIRST_ID, "First", "cursor", "a")
        second = node(SECOND_ID, "Second", "codex", "b")
        manifest = {"registryVersion": 4, "ownerId": "owner-1", "mode": "fixture",
                    "nodes": [first, second]}
        registry = self.signed(manifest)
        registry_path = self.write_json("registry.json", registry)
        manifest_hash = hashlib.sha256(projection.canonical_manifest(manifest)).hexdigest()
        current = {
            "schema": 1, "ownerId": "owner-1", "registryVersion": 4,
            "registrySHA256": manifest_hash,
            "nodes": {
                FIRST_ID: state_node("cursor", 7, 11),
                SECOND_ID: state_node("codex", 9, 13),
            },
        }
        desired = dict(node(THIRD_ID, "Third", "codex", "c"), compatibility="legacy_readonly")
        desired_path = self.write_json("desired.json", desired)
        output = self.root / "projection-request.json"
        control = FixtureControl(current)
        agent = FixtureAgentControl()

        request, result = projection.form_install(
            registry_path, self.public, self.private, desired_path, "compatible",
            "registry-add-5", output, self.root / "router.sock", self.root / "agent.sock",
            WORKER_TOKEN, HOST_ID, self.openssl, control, agent,
        )
        self.assertEqual(len(control.requests), 1)
        self.assertEqual(agent.transitions, ["sent"])
        self.assertEqual(request, json.loads(output.read_text()))
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        candidate = request["registry"]["manifest"]
        self.assertEqual(candidate["registryVersion"], 5)
        self.assertEqual(candidate["nodes"][0], dict(first, registrationRevision=4,
                                                     registrationEpoch=7, compatibility="compatible"))
        self.assertEqual(candidate["nodes"][1], dict(second, registrationRevision=4,
                                                     registrationEpoch=9, compatibility="compatible"))
        self.assertEqual(candidate["nodes"][2]["nodeId"], THIRD_ID)
        self.assertEqual(control.current["nodes"][FIRST_ID],
                         dict(current["nodes"][FIRST_ID], registrationRevision=4,
                              compatibility="compatible"))
        self.assertEqual(result, {
            "registryVersion": candidate["registryVersion"],
            "registrySHA256": projection.RouterControl.registry_manifest_sha256(request["registry"]),
        })
        projection.validate_envelope(request["registry"], self.openssl, self.public)

        retry = FixtureControl(control.current)
        with patch.object(retry, "install_registry_request", return_value=(control.current, False)) as install:
            repeated_request, repeated = projection.install_published(
                output, self.root / "router.sock", self.root / "agent.sock", WORKER_TOKEN,
                control=retry, agent_control=agent,
            )
        self.assertEqual(repeated_request, request)
        self.assertEqual(repeated, result)
        install.assert_not_called()
        before = output.read_bytes()
        with self.assertRaisesRegex(projection.ProjectionError, "NEW_ABSOLUTE_OUTPUT_REQUIRED"):
            projection.publish_request(output, {"unexpected": True})
        self.assertEqual(output.read_bytes(), before)

    def test_old_terminal_retry_returns_original_receipt_after_later_success(self):
        current = {"schema": 1, "ownerId": "owner-1", "registryVersion": 1,
                   "registrySHA256": "a" * 64,
                   "nodes": {FIRST_ID: state_node("cursor", 3, 2)}}
        control = FixtureControl(current)

        def request(operation_id, version, expected_hash, name):
            manifest = {"schemaId": projection.SCHEMA_ID, "registryVersion": version,
                        "ownerId": "owner-1", "mode": "fixture",
                        "wireSchemaSHA256": projection.WIRE_SCHEMA_SHA256,
                        "nodes": [dict(node(FIRST_ID, name, "cursor", "a"),
                                       registrationRevision=version - 1, registrationEpoch=3,
                                       compatibility="compatible")]}
            return {"schemaId": projection.REGISTRY_OPERATION_SCHEMA_ID,
                    "operationId": operation_id,
                    "expected": {"registryVersion": version - 1,
                                 "registrySHA256": expected_hash},
                    "registry": {"manifest": manifest, "signature": "A" * 86 + "=="},
                    "newNodeHostId": None}

        first = request("upgrade-1", 2, current["registrySHA256"], "First")
        first_agent = FixtureAgentControl()
        first_status, first_result = projection.coordinate_install(first, control, first_agent)
        self.assertEqual(first_status["phase"], "succeeded")

        second = request("upgrade-2", 3, control.current["registrySHA256"], "Second")
        second_agent = FixtureAgentControl()
        second_status, second_result = projection.coordinate_install(second, control, second_agent)
        self.assertEqual(second_status["phase"], "succeeded")
        self.assertEqual(second_result["registryVersion"], 3)
        self.assertEqual(control.current["registryVersion"], 3)

        with patch.object(control, "status", side_effect=AssertionError("terminal replay read Router")), \
             patch.object(control, "install_registry_request",
                          side_effect=AssertionError("terminal replay repeated install")):
            replayed_status, replayed_result = projection.coordinate_install(first, control, first_agent)
        self.assertEqual(replayed_status, first_status)
        self.assertEqual(replayed_result, first_result)
        self.assertEqual(replayed_result["registryVersion"], 2)

    def test_dynamic_update_changes_only_sealed_target(self):
        first = dict(node(FIRST_ID, "First", "cursor", "a"), registrationRevision=3,
                     registrationEpoch=7, compatibility="compatible")
        second = dict(node(SECOND_ID, "Second", "codex", "b"), registrationRevision=2,
                      registrationEpoch=9, compatibility="compatible")
        manifest = {"schemaId": projection.SCHEMA_ID, "registryVersion": 8,
                    "ownerId": "owner-1", "mode": "fixture",
                    "wireSchemaSHA256": projection.WIRE_SCHEMA_SHA256, "nodes": [first, second]}
        manifest_hash = hashlib.sha256(projection.canonical_manifest(manifest)).hexdigest()
        target_state = state_node("cursor", 7, 11, "sealed", "registry-update")
        target_state.update(registrationRevision=3, compatibility="compatible")
        neighbor = state_node("codex", 9, 13)
        neighbor.update(registrationRevision=2, compatibility="compatible")
        state = {"schema": 2, "ownerId": "owner-1", "registryVersion": 8,
                 "registrySHA256": manifest_hash, "registryOperationId": "previous",
                 "registryRequestSHA256": "d" * 64,
                 "nodes": {FIRST_ID: target_state, SECOND_ID: neighbor}}
        desired = dict(node(FIRST_ID, "Updated", "cursor", "c"), compatibility="compatible")

        candidate, expected = projection.project(
            manifest, manifest_hash, state, "registry-update", desired,
        )
        self.assertEqual(candidate["nodes"][0]["registrationRevision"], 4)
        self.assertEqual(candidate["nodes"][0]["registrationEpoch"], 8)
        self.assertEqual(candidate["nodes"][1], second)
        self.assertEqual(expected[SECOND_ID], neighbor)
        self.assertEqual(expected[FIRST_ID]["adapterVersion"], "")
        self.assertEqual(expected[FIRST_ID]["stateVersion"], 12)

        unsealed = copy.deepcopy(state)
        unsealed["nodes"][FIRST_ID].update(mode="eligible")
        unsealed["nodes"][FIRST_ID].pop("operationId")
        with self.assertRaisesRegex(projection.ProjectionError, "TARGET_NODE_MUST_BE_SEALED"):
            projection.project(manifest, manifest_hash, unsealed, "registry-update", desired)

    def test_projection_rejects_drift_implicit_legacy_gate_and_noop(self):
        first = node(FIRST_ID, "First", "cursor", "a")
        manifest = {"registryVersion": 1, "ownerId": "owner-1", "mode": "fixture", "nodes": [first]}
        manifest_hash = hashlib.sha256(projection.canonical_manifest(manifest)).hexdigest()
        state = {"schema": 1, "ownerId": "owner-1", "registryVersion": 1,
                 "registrySHA256": manifest_hash, "nodes": {FIRST_ID: state_node("cursor", 3, 2)}}
        with self.assertRaisesRegex(projection.ProjectionError, "EXPLICIT_LEGACY_COMPATIBILITY_REQUIRED"):
            projection.project(manifest, manifest_hash, state, "upgrade")
        drift = copy.deepcopy(state)
        drift["registrySHA256"] = "f" * 64
        with self.assertRaisesRegex(projection.ProjectionError, "REGISTRY_STATE_DRIFT"):
            projection.project(manifest, manifest_hash, drift, "upgrade", legacy_compatibility="compatible")

        dynamic = {"schemaId": projection.SCHEMA_ID, "registryVersion": 2,
                   "ownerId": "owner-1", "mode": "fixture",
                   "wireSchemaSHA256": projection.WIRE_SCHEMA_SHA256,
                   "nodes": [dict(first, registrationRevision=1, registrationEpoch=3,
                                  compatibility="compatible")]}
        dynamic_hash = hashlib.sha256(projection.canonical_manifest(dynamic)).hexdigest()
        dynamic_state = {"schema": 2, "ownerId": "owner-1", "registryVersion": 2,
                         "registrySHA256": dynamic_hash, "registryOperationId": "upgrade",
                         "registryRequestSHA256": "e" * 64,
                         "nodes": {FIRST_ID: dict(state["nodes"][FIRST_ID], registrationRevision=1,
                                                       compatibility="compatible")}}
        same = dict(first, compatibility="compatible")
        with self.assertRaisesRegex(projection.ProjectionError, "EXACTLY_ONE_NODE_CHANGE_REQUIRED"):
            projection.project(dynamic, dynamic_hash, dynamic_state, "noop", same)

    def test_empty_legacy_registry_can_add_the_first_node(self):
        manifest = {"registryVersion": 1, "ownerId": "owner-1", "mode": "fixture", "nodes": []}
        manifest_hash = hashlib.sha256(projection.canonical_manifest(manifest)).hexdigest()
        state = {"schema": 1, "ownerId": "owner-1", "registryVersion": 1,
                 "registrySHA256": manifest_hash, "nodes": {}}
        desired = dict(node(FIRST_ID, "First", "cursor", "a"), compatibility="compatible")
        candidate, expected = projection.project(
            manifest, manifest_hash, state, "registry-first", desired, "compatible",
        )
        self.assertEqual(candidate["registryVersion"], 2)
        self.assertEqual([item["nodeId"] for item in candidate["nodes"]], [FIRST_ID])
        self.assertEqual(expected[FIRST_ID]["mode"], "sealed")
        self.assertEqual(expected[FIRST_ID]["registrationRevision"], 1)


class RouterControlProjectionTests(unittest.TestCase):
    def test_status_rejects_malformed_hash_without_raw_type_error(self):
        malformed = {"schema": 1, "ownerId": "owner-1", "registryVersion": 1,
                     "registrySHA256": 7, "nodes": {}}
        with self.assertRaisesRegex(projection.ProjectionError, "INVALID_ROUTER_STATE"):
            projection.RouterControl.status_value(malformed)

    def test_v2_status_and_lost_ack_use_exact_operation_readback(self):
        control = projection.RouterControl(Path("/synthetic/control.sock"))
        current = {"schema": 1, "ownerId": "owner-1", "registryVersion": 1,
                   "registrySHA256": "a" * 64,
                   "nodes": {FIRST_ID: state_node("cursor", 3, 2)}}
        manifest = {"schemaId": projection.SCHEMA_ID, "registryVersion": 2,
                    "ownerId": "owner-1", "mode": "fixture",
                    "wireSchemaSHA256": projection.WIRE_SCHEMA_SHA256,
                    "nodes": [dict(node(FIRST_ID, "First", "cursor", "a"), registrationRevision=1,
                                   registrationEpoch=3, compatibility="compatible")]}
        registry = {"manifest": manifest, "signature": "A" * 86 + "=="}
        manifest_hash = projection.RouterControl.registry_manifest_sha256(registry)
        request_hash = projection.RouterControl.registry_request_sha256("upgrade", current, manifest_hash)
        readback = {"schema": 2, "ownerId": "owner-1", "registryVersion": 2,
                    "registrySHA256": manifest_hash, "registryOperationId": "upgrade",
                    "registryRequestSHA256": request_hash,
                    "nodes": {FIRST_ID: dict(current["nodes"][FIRST_ID], registrationRevision=1,
                                                  compatibility="compatible")}}
        request = {"operationId": "upgrade", "expected": {
            "registryVersion": current["registryVersion"],
            "registrySHA256": current["registrySHA256"],
        }, "registry": registry}
        with patch.object(control, "request",
                          side_effect=[projection.ProjectionError("LOST_ACK"), readback]) as call:
            self.assertEqual(control.install_registry_request(request), (readback, True))
        self.assertEqual(call.call_count, 2)
        self.assertEqual(control.status_value(readback), readback)

    def test_manifest_hash_uses_go_field_order_and_escaping(self):
        manifest = {"nodes": [dict(node(FIRST_ID, "<Agent>&", "cursor", "a"),
                                        registrationRevision=1, registrationEpoch=3,
                                        compatibility="compatible")],
                    "mode": "fixture", "ownerId": "owner-1", "registryVersion": 2,
                    "wireSchemaSHA256": projection.WIRE_SCHEMA_SHA256,
                    "schemaId": projection.SCHEMA_ID}
        registry = {"signature": "A" * 86 + "==", "manifest": manifest}
        self.assertEqual(projection.RouterControl.registry_manifest_sha256(registry),
                         hashlib.sha256(projection.canonical_manifest(manifest)).hexdigest())

    def test_coordinator_terminalizes_known_rejection_and_blocks_unknown_retry(self):
        current = {"schema": 1, "ownerId": "owner-1", "registryVersion": 1,
                   "registrySHA256": "a" * 64,
                   "nodes": {FIRST_ID: state_node("cursor", 3, 2)}}
        manifest = {"schemaId": projection.SCHEMA_ID, "registryVersion": 2,
                    "ownerId": "owner-1", "mode": "fixture",
                    "wireSchemaSHA256": projection.WIRE_SCHEMA_SHA256,
                    "nodes": [dict(node(FIRST_ID, "First", "cursor", "a"),
                                   registrationRevision=1, registrationEpoch=3,
                                   compatibility="compatible")]}
        request = {"schemaId": projection.REGISTRY_OPERATION_SCHEMA_ID,
                   "operationId": "upgrade", "expected": {
                       "registryVersion": 1, "registrySHA256": "a" * 64,
                   }, "registry": {"manifest": manifest, "signature": "A" * 86 + "=="},
                   "newNodeHostId": None}

        rejected_control = FixtureControl(current)
        rejected_agent = FixtureAgentControl()
        with patch.object(rejected_control, "install_registry_request",
                          side_effect=projection.ProjectionError("ROUTER_NODE_NOT_SEALED")):
            with self.assertRaisesRegex(projection.ProjectionError, "ROUTER_NODE_NOT_SEALED"):
                projection.coordinate_install(request, rejected_control, rejected_agent)
        self.assertEqual(rejected_agent.status_value["phase"], "failed")
        self.assertEqual(rejected_agent.status_value["resultCode"], "router.node_not_sealed")

        unknown_control = FixtureControl(current)
        unknown_agent = FixtureAgentControl()
        with patch.object(unknown_control, "install_registry_request",
                          side_effect=projection.ProjectionError("ROUTER_CONTROL_UNAVAILABLE")):
            with self.assertRaisesRegex(projection.ProjectionError, "ROUTER_CONTROL_UNAVAILABLE"):
                projection.coordinate_install(request, unknown_control, unknown_agent)
        self.assertEqual(unknown_agent.status_value["phase"], "reconciling")
        with patch.object(unknown_control, "install_registry_request") as install:
            with self.assertRaisesRegex(projection.ProjectionError, "REGISTRY_RESULT_UNKNOWN"):
                projection.coordinate_install(request, unknown_control, unknown_agent)
        install.assert_not_called()


if __name__ == "__main__":
    unittest.main()
