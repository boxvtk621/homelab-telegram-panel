#!/usr/bin/env python3
"""Form and CAS-install one signed dynamic Harness Router projection."""

import argparse
import base64
import copy
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import stat
import tempfile

import registry_transition as legacy


SCHEMA_ID = "harness-router-registry-v1"
WIRE_SCHEMA_SHA256 = "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9"
MANIFEST_FIELDS = ("schemaId", "registryVersion", "ownerId", "mode", "wireSchemaSHA256", "nodes")
LEGACY_MANIFEST_FIELDS = ("registryVersion", "ownerId", "mode", "nodes")
NODE_FIELDS = ("nodeId", "name", "adapter", "url", "certificateSHA256")
PROJECTED_NODE_FIELDS = NODE_FIELDS + ("registrationRevision", "registrationEpoch", "compatibility")
DESIRED_NODE_FIELDS = NODE_FIELDS + ("compatibility",)
REGISTRY_OPERATION_SCHEMA_ID = "agent-registry-operation-v1"
REGISTRY_COMMAND_SCHEMA_ID = "agent-registry-operation-command-v1"
REGISTRY_FINISH_SCHEMA_ID = "agent-registry-operation-finish-v1"
REGISTRY_FAILURE_SCHEMA_ID = "agent-registry-operation-failure-v1"
KNOWN_NOT_APPLIED = {
    "ROUTER_INVALID", "ROUTER_STALE", "ROUTER_ID_CONFLICT", "ROUTER_INCOMPATIBLE",
    "ROUTER_NODE_NOT_READY", "ROUTER_NODE_NOT_SEALED", "ROUTER_REGISTRY_REJECTED",
    "ROUTER_VERSION_EXHAUSTED",
}


class ProjectionError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise ProjectionError(code)


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("router", timeout=35)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class RouterControl:
    NODE_FIELDS = {"mode", "stateVersion", "generation", "identityEpoch", "adapterKind", "adapterVersion"}
    PROJECTED_NODE_FIELDS = NODE_FIELDS | {"registrationRevision", "compatibility"}
    STATE_FIELDS = {"schema", "ownerId", "registryVersion", "registrySHA256", "nodes"}
    PROJECTED_STATE_FIELDS = STATE_FIELDS | {"registryOperationId", "registryRequestSHA256"}

    def __init__(self, path):
        self.path = Path(path)

    def request(self, method, path, payload=None):
        try:
            directory = os.lstat(self.path.parent)
            info = os.lstat(self.path)
            require(self.path.is_absolute() and self.path == Path(os.path.normpath(self.path)) and
                    stat.S_ISDIR(directory.st_mode) and directory.st_mode & 0o777 == 0o700 and
                    stat.S_ISSOCK(info.st_mode) and info.st_mode & 0o777 == 0o600 and
                    directory.st_uid == os.geteuid() and info.st_uid == directory.st_uid,
                    "UNSAFE_ROUTER_CONTROL_SOCKET")
            body = None if payload is None else json.dumps(payload, separators=(",", ":"), sort_keys=True)
            connection = UnixHTTPConnection(self.path)
            connection.request(method, path, body=body,
                               headers={"Content-Type": "application/json"} if body else {})
            response = connection.getresponse()
            raw = response.read(524289)
            status = response.status
            connection.close()
            require(len(raw) <= 524288, "ROUTER_CONTROL_RESPONSE_TOO_LARGE")
            value = json.loads(raw, object_pairs_hook=legacy.pairs)
        except ProjectionError:
            raise
        except legacy.TransitionError as error:
            raise ProjectionError(str(error)) from None
        except Exception:
            raise ProjectionError("ROUTER_CONTROL_UNAVAILABLE") from None
        if status != 200:
            code = value.get("error") if type(value) is dict and set(value) == {"error"} else ""
            allowed = {"invalid", "not_found", "stale", "identity_changed", "version_exhausted",
                       "state_unavailable", "node_not_ready", "node_not_sealed", "registry_rejected",
                       "incompatible", "id_conflict"}
            require(code in allowed, "ROUTER_CONTROL_INVALID_RESPONSE")
            raise ProjectionError("ROUTER_" + code.upper())
        return value

    @classmethod
    def node(cls, value, projected):
        fields = set(value) if type(value) is dict else set()
        expected = cls.PROJECTED_NODE_FIELDS if projected else cls.NODE_FIELDS
        require(type(value) is dict and fields in (expected, expected | {"operationId"}) and
                value["mode"] in ("eligible", "draining", "sealed") and
                type(value["stateVersion"]) is int and value["stateVersion"] > 0 and
                type(value["generation"]) is int and value["generation"] >= 0 and
                type(value["identityEpoch"]) is int and value["identityEpoch"] >= 0 and
                value["adapterKind"] in ("cursor", "codex") and isinstance(value["adapterVersion"], str),
                "INVALID_ROUTER_STATE")
        if projected:
            require(type(value["registrationRevision"]) is int and value["registrationRevision"] > 0 and
                    value["identityEpoch"] > 0 and value["compatibility"] in ("compatible", "legacy_readonly") and
                    (value["mode"] != "eligible" or value["compatibility"] == "compatible"),
                    "INVALID_ROUTER_STATE")
        if value["mode"] == "eligible":
            require("operationId" not in value, "INVALID_ROUTER_STATE")
        else:
            require(isinstance(value.get("operationId"), str) and value["operationId"],
                    "INVALID_ROUTER_STATE")
        return value

    @classmethod
    def status_value(cls, value):
        projected = type(value) is dict and value.get("schema") == 2
        fields = cls.PROJECTED_STATE_FIELDS if projected else cls.STATE_FIELDS
        require(type(value) is dict and set(value) == fields and value["schema"] in (1, 2) and
                isinstance(value["ownerId"], str) and value["ownerId"] and
                type(value["registryVersion"]) is int and value["registryVersion"] > 0 and
                isinstance(value["registrySHA256"], str) and
                legacy.HEX_SHA256.fullmatch(value["registrySHA256"]) and type(value["nodes"]) is dict,
                "INVALID_ROUTER_STATE")
        if projected:
            require(isinstance(value["registryOperationId"], str) and value["registryOperationId"] and
                    isinstance(value["registryRequestSHA256"], str) and
                    legacy.HEX_SHA256.fullmatch(value["registryRequestSHA256"]), "INVALID_ROUTER_STATE")
        for node in value["nodes"].values():
            cls.node(node, projected)
        return value

    def status(self):
        return self.status_value(self.request("GET", "/v1/state"))

    @staticmethod
    def registry_manifest_sha256(registry):
        require(type(registry) is dict and set(registry) == {"manifest", "signature"} and
                isinstance(registry["signature"], str), "INVALID_ROUTER_REGISTRY")
        validate_manifest(registry["manifest"])
        return hashlib.sha256(canonical_manifest(registry["manifest"])).hexdigest()

    @staticmethod
    def registry_request_sha256(operation_id, expected, manifest_sha256):
        value = {"operationId": operation_id,
                 "expected": {"registryVersion": expected["registryVersion"],
                              "registrySHA256": expected["registrySHA256"]},
                 "manifestSHA256": manifest_sha256}
        return hashlib.sha256(json.dumps(value, separators=(",", ":")).encode()).hexdigest()

    @classmethod
    def exact_registry_readback(cls, payload, readback):
        try:
            operation_id, expected, registry = payload["operationId"], payload["expected"], payload["registry"]
            manifest_sha256 = cls.registry_manifest_sha256(registry)
            manifest = registry["manifest"]
            request_sha256 = cls.registry_request_sha256(operation_id, expected, manifest_sha256)
            return (cls.status_value(readback)["schema"] == 2 and
                    readback["ownerId"] == manifest["ownerId"] and
                    readback["registryVersion"] == manifest["registryVersion"] and
                    readback["registrySHA256"] == manifest_sha256 and
                    readback["registryOperationId"] == operation_id and
                    readback["registryRequestSHA256"] == request_sha256 and
                    set(readback["nodes"]) == {node["nodeId"] for node in manifest["nodes"]})
        except (KeyError, TypeError, ProjectionError):
            return False

    def install_registry_request(self, payload):
        require(type(payload) is dict and set(payload) == {"operationId", "expected", "registry"} and
                isinstance(payload["operationId"], str) and legacy.OPERATION.fullmatch(payload["operationId"]) and
                type(payload["expected"]) is dict and
                set(payload["expected"]) == {"registryVersion", "registrySHA256"} and
                legacy.safe_integer(payload["expected"]["registryVersion"], 1) and
                type(payload["expected"]["registrySHA256"]) is str and
                legacy.HEX_SHA256.fullmatch(payload["expected"]["registrySHA256"]),
                "INVALID_PROJECTION_REQUEST")
        operation_id, expected, registry = payload["operationId"], payload["expected"], payload["registry"]
        manifest_sha256 = self.registry_manifest_sha256(registry)
        manifest = registry["manifest"]
        require(manifest["schemaId"] == SCHEMA_ID and
                manifest["registryVersion"] == expected["registryVersion"] + 1,
                "INVALID_PROJECTION_REQUEST")
        try:
            result = self.request("POST", "/v1/registry/install", payload)
            require(self.exact_registry_readback(payload, result), "ROUTER_REGISTRY_READBACK_MISMATCH")
            return result, False
        except ProjectionError:
            observed = self.status()
            if self.exact_registry_readback(payload, observed):
                return observed, True
            raise


class AgentServiceControl:
    STATUS_FIELDS = {"schemaId", "receipt", "phase", "effectState", "operationVersion", "updatedAt", "resultCode"}
    RECEIPT_FIELDS = {"schemaId", "operationId", "requestHash", "expectedRegistryVersion",
                      "expectedRegistrySHA256", "candidateRegistryVersion", "candidateRegistrySHA256",
                      "affectedNodeIds", "acceptedAt"}

    def __init__(self, path, owner_id, worker_token):
        self.path = Path(path)
        self.owner_id = owner_id
        self.worker_token = worker_token
        require(type(owner_id) is str and legacy.ACTOR.fullmatch(owner_id), "INVALID_AGENT_SERVICE_OWNER")
        require(type(worker_token) is str and 32 <= len(worker_token) <= 128 and
                legacy.ACTOR.fullmatch(worker_token), "INVALID_AGENT_SERVICE_WORKER_TOKEN")

    def request(self, method, path, payload=None):
        try:
            directory = os.lstat(self.path.parent)
            info = os.lstat(self.path)
            require(self.path.is_absolute() and self.path == Path(os.path.normpath(self.path)) and
                    stat.S_ISDIR(directory.st_mode) and directory.st_mode & 0o777 == 0o700 and
                    stat.S_ISSOCK(info.st_mode) and info.st_mode & 0o777 == 0o600 and
                    directory.st_uid == os.geteuid() and info.st_uid == directory.st_uid,
                    "UNSAFE_AGENT_SERVICE_SOCKET")
            body = None if payload is None else json.dumps(payload, separators=(",", ":"), sort_keys=True)
            headers = {"X-Agent-Service-Owner": self.owner_id,
                       "X-Agent-Service-Worker-Token": self.worker_token}
            if body is not None:
                headers["Content-Type"] = "application/json"
            connection = UnixHTTPConnection(self.path)
            connection.request(method, path, body=body, headers=headers)
            response = connection.getresponse()
            raw = response.read(524289)
            status = response.status
            connection.close()
            require(len(raw) <= 524288, "AGENT_SERVICE_RESPONSE_TOO_LARGE")
            value = json.loads(raw, object_pairs_hook=legacy.pairs)
        except ProjectionError:
            raise
        except legacy.TransitionError as error:
            raise ProjectionError(str(error)) from None
        except Exception:
            raise ProjectionError("AGENT_SERVICE_UNAVAILABLE") from None
        if status not in (200, 202):
            try:
                error = value["error"]
                require(type(value) is dict and set(value) == {"error"} and type(error) is dict and
                        set(error) == {"code", "message", "retryable"} and
                        type(error["code"]) is str, "AGENT_SERVICE_INVALID_RESPONSE")
                code = error["code"].upper()
            except (KeyError, TypeError):
                raise ProjectionError("AGENT_SERVICE_INVALID_RESPONSE") from None
            raise ProjectionError("AGENT_SERVICE_" + code)
        return value

    @classmethod
    def status_value(cls, value, operation_id=None):
        require(type(value) is dict and set(value) == cls.STATUS_FIELDS and
                value.get("schemaId") == "agent-registry-operation-status-v1" and
                type(value.get("receipt")) is dict and set(value["receipt"]) == cls.RECEIPT_FIELDS,
                "INVALID_AGENT_SERVICE_STATUS")
        receipt = value["receipt"]
        require(receipt["schemaId"] == "agent-registry-operation-receipt-v1" and
                type(receipt["operationId"]) is str and legacy.OPERATION.fullmatch(receipt["operationId"]) and
                (operation_id is None or receipt["operationId"] == operation_id) and
                type(receipt["requestHash"]) is str and legacy.HEX_SHA256.fullmatch(receipt["requestHash"]) and
                legacy.safe_integer(receipt["expectedRegistryVersion"], 1) and
                receipt["candidateRegistryVersion"] == receipt["expectedRegistryVersion"] + 1 and
                type(receipt["expectedRegistrySHA256"]) is str and
                legacy.HEX_SHA256.fullmatch(receipt["expectedRegistrySHA256"]) and
                type(receipt["candidateRegistrySHA256"]) is str and
                legacy.HEX_SHA256.fullmatch(receipt["candidateRegistrySHA256"]) and
                type(receipt["affectedNodeIds"]) is list and len(receipt["affectedNodeIds"]) > 0 and
                receipt["affectedNodeIds"] == sorted(set(receipt["affectedNodeIds"])) and
                all(type(node_id) is str and legacy.UUID.fullmatch(node_id)
                    for node_id in receipt["affectedNodeIds"]) and
                type(receipt["acceptedAt"]) is str and receipt["acceptedAt"] and
                value["phase"] in ("accepted", "applying", "reconciling", "succeeded", "failed") and
                value["effectState"] in ("not_sent", "sent", "unknown", "acknowledged", "reconciled", "failed") and
                legacy.safe_integer(value["operationVersion"], 1) and
                type(value["updatedAt"]) is str and value["updatedAt"] and
                (value["resultCode"] is None or
                 type(value["resultCode"]) is str and legacy.ACTOR.fullmatch(value["resultCode"])),
                "INVALID_AGENT_SERVICE_STATUS")
        pairs = {"accepted": "not_sent", "applying": "sent", "reconciling": "unknown",
                 "failed": "failed"}
        require(value["phase"] not in pairs or value["effectState"] == pairs[value["phase"]],
                "INVALID_AGENT_SERVICE_STATUS")
        require(value["phase"] != "succeeded" or value["effectState"] in ("acknowledged", "reconciled"),
                "INVALID_AGENT_SERVICE_STATUS")
        require(value["phase"] != "succeeded" or
                value["resultCode"] == "registry:" + receipt["candidateRegistrySHA256"],
                "INVALID_AGENT_SERVICE_STATUS")
        return value

    def reserve(self, request):
        status = self.status_value(self.request("POST", "/internal/v1/registry-operations", request),
                                   request["operationId"])
        receipt, manifest = status["receipt"], request["registry"]["manifest"]
        candidate_hash = RouterControl.registry_manifest_sha256(request["registry"])
        require(receipt["expectedRegistryVersion"] == request["expected"]["registryVersion"] and
                receipt["expectedRegistrySHA256"] == request["expected"]["registrySHA256"] and
                receipt["candidateRegistryVersion"] == manifest["registryVersion"] and
                receipt["candidateRegistrySHA256"] == candidate_hash,
                "AGENT_SERVICE_RECEIPT_MISMATCH")
        return status

    def status(self, operation_id):
        return self.status_value(self.request("GET", "/internal/v1/registry-operations/" + operation_id),
                                 operation_id)

    def transition(self, status, transition):
        command = {"schemaId": REGISTRY_COMMAND_SCHEMA_ID,
                   "operationId": status["receipt"]["operationId"],
                   "requestHash": status["receipt"]["requestHash"],
                   "operationVersion": status["operationVersion"]}
        return self.status_value(self.request("POST", "/internal/v1/registry-operations/" + transition,
                                              command), command["operationId"])

    def finish(self, status, readback, effect_state):
        finish = {"schemaId": REGISTRY_FINISH_SCHEMA_ID,
                  "operationId": status["receipt"]["operationId"],
                  "requestHash": status["receipt"]["requestHash"],
                  "operationVersion": status["operationVersion"],
                  "registryVersion": readback["registryVersion"],
                  "registrySHA256": readback["registrySHA256"],
                  "effectState": effect_state}
        return self.status_value(self.request("POST", "/internal/v1/registry-operations/finish", finish),
                                 finish["operationId"])

    def fail(self, status, result_code):
        failure = {"schemaId": REGISTRY_FAILURE_SCHEMA_ID,
                   "operationId": status["receipt"]["operationId"],
                   "requestHash": status["receipt"]["requestHash"],
                   "operationVersion": status["operationVersion"],
                   "resultCode": result_code}
        return self.status_value(self.request("POST", "/internal/v1/registry-operations/fail", failure),
                                 failure["operationId"])


def json_bytes(value):
    try:
        encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    except Exception:
        raise ProjectionError("INVALID_REGISTRY_TEXT") from None
    # Match encoding/json's default escaping, which is part of the signature.
    return (encoded.replace("&", "\\u0026").replace("<", "\\u003c").replace(">", "\\u003e")
            .replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")).encode("utf-8")


def canonical_manifest(manifest):
    require(type(manifest) is dict, "INVALID_REGISTRY")
    dynamic = manifest.get("schemaId") == SCHEMA_ID
    fields = MANIFEST_FIELDS if dynamic else LEGACY_MANIFEST_FIELDS
    node_fields = PROJECTED_NODE_FIELDS if dynamic else NODE_FIELDS
    require(type(manifest) is dict and set(manifest) == set(fields) and type(manifest.get("nodes")) is list,
            "INVALID_REGISTRY")
    ordered = {key: manifest[key] for key in fields if key != "nodes"}
    ordered["nodes"] = [{key: node[key] for key in node_fields} for node in manifest["nodes"]]
    return json_bytes(ordered)


def base_node(node):
    return {key: node[key] for key in NODE_FIELDS}


def validate_manifest(manifest):
    dynamic = type(manifest) is dict and manifest.get("schemaId") == SCHEMA_ID
    fields = MANIFEST_FIELDS if dynamic else LEGACY_MANIFEST_FIELDS
    node_fields = PROJECTED_NODE_FIELDS if dynamic else NODE_FIELDS
    require(type(manifest) is dict and set(manifest) == set(fields) and
            legacy.safe_integer(manifest.get("registryVersion"), 1) and
            type(manifest.get("ownerId")) is str and legacy.ACTOR.fullmatch(manifest["ownerId"]) and
            manifest.get("mode") in ("live", "fixture") and
            type(manifest.get("nodes")) is list and
            (not dynamic or 0 < len(manifest["nodes"]) <= 1000),
            "INVALID_REGISTRY")
    if dynamic:
        require(manifest["wireSchemaSHA256"] == WIRE_SCHEMA_SHA256, "INCOMPATIBLE_WIRE_SCHEMA")
    node_ids, pins = set(), set()
    for node in manifest["nodes"]:
        require(type(node) is dict and set(node) == set(node_fields), "INVALID_REGISTRY_NODE")
        try:
            legacy.validate_node(base_node(node))
        except legacy.TransitionError as error:
            raise ProjectionError(str(error)) from None
        require(node["nodeId"] not in node_ids and node["certificateSHA256"] not in pins,
                "DUPLICATE_NODE_BINDING")
        if dynamic:
            require(legacy.safe_integer(node["registrationRevision"], 1) and
                    legacy.safe_integer(node["registrationEpoch"], 1) and
                    node["compatibility"] in ("compatible", "legacy_readonly"),
                    "INVALID_NODE_REGISTRATION")
        node_ids.add(node["nodeId"])
        pins.add(node["certificateSHA256"])
    return dynamic


def validate_envelope(value, executable, public_key):
    require(type(value) is dict and set(value) == {"manifest", "signature"}, "INVALID_REGISTRY")
    manifest = value["manifest"]
    validate_manifest(manifest)
    require(type(value["signature"]) is str, "INVALID_REGISTRY_SIGNATURE")
    try:
        signature = base64.b64decode(value["signature"], validate=True)
    except Exception:
        raise ProjectionError("INVALID_REGISTRY_SIGNATURE") from None
    require(len(signature) == 64, "INVALID_REGISTRY_SIGNATURE")
    canonical = canonical_manifest(manifest)
    try:
        legacy.verify_signature(executable, public_key, canonical, signature)
    except legacy.TransitionError as error:
        raise ProjectionError(str(error)) from None
    return manifest, hashlib.sha256(canonical).hexdigest()


def decode_registry(raw, executable, public_key):
    try:
        value = json.loads(raw, object_pairs_hook=legacy.pairs)
    except legacy.TransitionError as error:
        raise ProjectionError(str(error)) from None
    except Exception:
        raise ProjectionError("INVALID_JSON") from None
    if type(value) is dict and set(value) in (
            {"operationId", "expected", "registry"},
            {"schemaId", "operationId", "expected", "registry", "newNodeHostId"}):
        value = value["registry"]
    return validate_envelope(value, executable, public_key)


def validate_desired(value):
    require(type(value) is dict and set(value) == set(DESIRED_NODE_FIELDS), "INVALID_DESIRED_NODE")
    try:
        legacy.validate_node(base_node(value))
    except legacy.TransitionError as error:
        raise ProjectionError(str(error)) from None
    require(value["compatibility"] in ("compatible", "legacy_readonly"), "INVALID_NODE_COMPATIBILITY")
    return value


def project(manifest, manifest_sha256, state, operation_id, desired=None, legacy_compatibility=None):
    dynamic = validate_manifest(manifest)
    require(type(state) is dict, "INVALID_ROUTER_STATE")
    require(type(operation_id) is str and legacy.OPERATION.fullmatch(operation_id), "INVALID_OPERATION_ID")
    if dynamic:
        require(legacy_compatibility is None and state.get("schema") == 2, "INVALID_PROJECTION_SOURCE")
    else:
        require(legacy_compatibility in ("compatible", "legacy_readonly") and state.get("schema") == 1,
                "EXPLICIT_LEGACY_COMPATIBILITY_REQUIRED")
    require(state.get("ownerId") == manifest["ownerId"] and
            state.get("registryVersion") == manifest["registryVersion"] and
            state.get("registrySHA256") == manifest_sha256 and type(state.get("nodes")) is dict,
            "REGISTRY_STATE_DRIFT")
    source = {node["nodeId"]: node for node in manifest["nodes"]}
    require(set(state["nodes"]) == set(source), "REGISTRY_STATE_DRIFT")
    desired = validate_desired(desired) if desired is not None else None
    require(desired is not None or not dynamic, "DYNAMIC_PROJECTION_REQUIRES_NODE_CHANGE")
    require(manifest["registryVersion"] < legacy.MAXIMUM_SAFE_INTEGER, "REGISTRY_VERSION_EXHAUSTED")

    changes = 0
    projected_nodes = []
    expected_nodes = {}
    for current in manifest["nodes"]:
        node_id = current["nodeId"]
        current_state = state["nodes"][node_id]
        require(current_state.get("adapterKind") == current["adapter"], "REGISTRY_STATE_DRIFT")
        target = desired if desired is not None and desired["nodeId"] == node_id else None
        next_base = base_node(target) if target is not None else base_node(current)
        compatibility = (target["compatibility"] if target is not None else
                         current["compatibility"] if dynamic else legacy_compatibility)
        require(compatibility != "legacy_readonly" or current_state.get("mode") == "sealed",
                "LEGACY_READONLY_NODE_MUST_BE_SEALED")
        binding_changed = next_base != base_node(current)
        compatibility_changed = dynamic and compatibility != current["compatibility"]
        changed = binding_changed or compatibility_changed
        revision = current["registrationRevision"] if dynamic else manifest["registryVersion"]
        epoch = current["registrationEpoch"] if dynamic else max(1, current_state.get("identityEpoch", 0))
        if dynamic:
            require(current_state.get("registrationRevision") == revision and
                    current_state.get("identityEpoch") == epoch and
                    current_state.get("compatibility") == current["compatibility"],
                    "REGISTRY_STATE_DRIFT")
        if changed:
            changes += 1
            require(current_state.get("mode") == "sealed" and
                    current_state.get("operationId") in (operation_id, "bootstrap"),
                    "TARGET_NODE_MUST_BE_SEALED")
            require(revision < legacy.MAXIMUM_SAFE_INTEGER and
                    current_state.get("identityEpoch", 0) < legacy.MAXIMUM_SAFE_INTEGER,
                    "NODE_REGISTRATION_EXHAUSTED")
            revision += 1
            epoch = current_state["identityEpoch"] + 1
        projected = dict(next_base, registrationRevision=revision,
                         registrationEpoch=epoch, compatibility=compatibility)
        projected_nodes.append(projected)
        expected = copy.deepcopy(current_state)
        expected.update(registrationRevision=revision, identityEpoch=epoch, compatibility=compatibility)
        if changed:
            expected.update(mode="sealed", operationId=operation_id,
                            stateVersion=current_state["stateVersion"] + 1,
                            adapterKind=projected["adapter"], adapterVersion="")
        expected_nodes[node_id] = expected

    if desired is not None and desired["nodeId"] not in source:
        require(len(projected_nodes) < 1000, "REGISTRY_CAPACITY_EXCEEDED")
        changes += 1
        projected = dict(base_node(desired), registrationRevision=1,
                         registrationEpoch=1, compatibility=desired["compatibility"])
        projected_nodes.append(projected)
        expected_nodes[desired["nodeId"]] = {
            "mode": "sealed", "stateVersion": 1, "generation": 0,
            "operationId": operation_id, "registrationRevision": 1,
            "identityEpoch": 1, "compatibility": desired["compatibility"],
            "adapterKind": desired["adapter"], "adapterVersion": "",
        }
    require(changes <= 1 and (changes == 1 or not dynamic), "EXACTLY_ONE_NODE_CHANGE_REQUIRED")
    candidate = {
        "schemaId": SCHEMA_ID,
        "registryVersion": manifest["registryVersion"] + 1,
        "ownerId": manifest["ownerId"],
        "mode": manifest["mode"],
        "wireSchemaSHA256": WIRE_SCHEMA_SHA256,
        "nodes": projected_nodes,
    }
    validate_manifest(candidate)
    return candidate, expected_nodes


def publish_request(path, value):
    path = Path(path)
    try:
        parent, parent_fd, identity = legacy.open_output_parent(path)
    except legacy.TransitionError as error:
        raise ProjectionError(str(error)) from None
    temporary = None
    published = False
    try:
        legacy.require_same_directory(parent, parent_fd, identity)
        descriptor, temporary_name = tempfile.mkstemp(prefix="." + path.name + "-", dir=parent)
        temporary = Path(temporary_name)
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "wb", closefd=False) as stream:
                stream.write(legacy.encode_json(value))
                stream.flush()
                os.fsync(stream.fileno())
        finally:
            os.close(descriptor)
        legacy.require_same_directory(parent, parent_fd, identity)
        legacy.rename_no_replace(parent_fd, temporary.name, path.name)
        published = True
        temporary = None
        legacy.require_same_directory(parent, parent_fd, identity)
        try:
            os.fsync(parent_fd)
        except OSError:
            raise ProjectionError("PUBLICATION_DURABILITY_UNKNOWN") from None
    except legacy.TransitionError as error:
        raise ProjectionError(str(error)) from None
    except OSError:
        raise ProjectionError("PROJECTION_REQUEST_WRITE_FAILED") from None
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except OSError:
                pass
        os.close(parent_fd)
    require(published, "PROJECTION_REQUEST_NOT_PUBLISHED")


def read_request(path):
    try:
        value = legacy.read_json(path, 320 << 10, private=True)
    except legacy.TransitionError as error:
        raise ProjectionError(str(error)) from None
    require(type(value) is dict and
            set(value) == {"schemaId", "operationId", "expected", "registry", "newNodeHostId"} and
            value["schemaId"] == REGISTRY_OPERATION_SCHEMA_ID and
            type(value["operationId"]) is str and legacy.OPERATION.fullmatch(value["operationId"]) and
            type(value["expected"]) is dict and
            set(value["expected"]) == {"registryVersion", "registrySHA256"} and
            legacy.safe_integer(value["expected"]["registryVersion"], 1) and
            type(value["expected"]["registrySHA256"]) is str and
            legacy.HEX_SHA256.fullmatch(value["expected"]["registrySHA256"]) and
            (value["newNodeHostId"] is None or
             type(value["newNodeHostId"]) is str and legacy.UUID.fullmatch(value["newNodeHostId"])),
            "INVALID_PROJECTION_REQUEST")
    return value


def router_request(request):
    return {"operationId": request["operationId"], "expected": request["expected"],
            "registry": request["registry"]}


def registry_operation_result(status):
    require(status["phase"] == "succeeded" and
            status["effectState"] in ("acknowledged", "reconciled") and
            status["resultCode"] == "registry:" + status["receipt"]["candidateRegistrySHA256"],
            "INVALID_AGENT_SERVICE_STATUS")
    return {"registryVersion": status["receipt"]["candidateRegistryVersion"],
            "registrySHA256": status["receipt"]["candidateRegistrySHA256"]}


def coordinate_install(request, control, agent_control, expected_nodes=None):
    status = agent_control.reserve(request)
    direct = router_request(request)
    if status["phase"] == "failed":
        raise ProjectionError("REGISTRY_OPERATION_FAILED")
    if status["phase"] == "succeeded":
        # The saved terminal receipt is the immutable result of this operation.
        # Router may legitimately contain a later CAS generation; current-state
        # diagnosis is separate from replay and must not rewrite past success.
        return status, registry_operation_result(status)

    if status["effectState"] == "unknown":
        readback = control.status()
        if not RouterControl.exact_registry_readback(direct, readback):
            raise ProjectionError("REGISTRY_RESULT_UNKNOWN")
        status = agent_control.finish(status, readback, "reconciled")
    else:
        if status["effectState"] == "not_sent":
            status = agent_control.transition(status, "sent")
        require(status["phase"] == "applying" and status["effectState"] == "sent",
                "INVALID_AGENT_SERVICE_STATUS")
        try:
            readback, reconciled = control.install_registry_request(direct)
        except ProjectionError as error:
            error_code = str(error)
            try:
                if error_code in KNOWN_NOT_APPLIED:
                    agent_control.fail(status, "router." + error_code.removeprefix("ROUTER_").lower())
                else:
                    agent_control.transition(status, "unknown")
            except ProjectionError:
                raise ProjectionError("REGISTRY_RESULT_UNKNOWN") from None
            raise
        status = agent_control.finish(status, readback, "reconciled" if reconciled else "acknowledged")

    require(status["phase"] == "succeeded" and
            status["effectState"] in ("acknowledged", "reconciled"),
            "INVALID_AGENT_SERVICE_STATUS")
    if expected_nodes is not None:
        require(readback["nodes"] == expected_nodes, "ROUTER_NODE_READBACK_MISMATCH")
    return status, registry_operation_result(status)


def form_install(registry_path, public_key, private_key, desired_path, legacy_compatibility,
                 operation_id, output, router_socket, agent_service_socket, worker_token,
                 new_node_host_id=None, openssl="openssl", control=None, agent_control=None):
    executable = shutil.which(openssl)
    require(executable is not None, "OPENSSL_REQUIRED")
    try:
        public_bytes = legacy.read_regular(public_key, 16 << 10)
        private_bytes = legacy.read_regular(private_key, 16 << 10, private=True)
        source_raw = legacy.read_regular(registry_path, 320 << 10)
        desired = None if desired_path is None else legacy.read_json(desired_path, 16 << 10)
    except legacy.TransitionError as error:
        raise ProjectionError(str(error)) from None
    snapshots = None
    try:
        try:
            snapshots, paths = legacy.snapshot_openssl_inputs({
                "signer-public.pem": public_bytes, "signer-private.pem": private_bytes,
            })
        except legacy.TransitionError as error:
            raise ProjectionError(str(error)) from None
        manifest, manifest_sha256 = decode_registry(source_raw, executable, paths["signer-public.pem"])
        control = control or RouterControl(Path(router_socket))
        current = control.status()
        candidate, expected_nodes = project(manifest, manifest_sha256, current, operation_id,
                                            desired, legacy_compatibility)
        adding_node = (desired is not None and
                       desired["nodeId"] not in {node["nodeId"] for node in manifest["nodes"]})
        require((new_node_host_id is not None) == adding_node and
                (new_node_host_id is None or
                 type(new_node_host_id) is str and legacy.UUID.fullmatch(new_node_host_id)),
                "NEW_NODE_HOST_REQUIRED")
        canonical = canonical_manifest(candidate)
        try:
            signature = legacy.sign(executable, paths["signer-private.pem"], canonical)
            legacy.verify_signature(executable, paths["signer-public.pem"], canonical, signature)
        except legacy.TransitionError as error:
            raise ProjectionError(str(error)) from None
        registry = {"manifest": candidate, "signature": base64.b64encode(signature).decode("ascii")}
        validate_envelope(registry, executable, paths["signer-public.pem"])
        request = {
            "schemaId": REGISTRY_OPERATION_SCHEMA_ID,
            "operationId": operation_id,
            "expected": {"registryVersion": current["registryVersion"],
                         "registrySHA256": current["registrySHA256"]},
            "registry": registry,
            "newNodeHostId": new_node_host_id,
        }
        publish_request(output, request)
    finally:
        if snapshots is not None:
            snapshots.cleanup()
    agent_control = agent_control or AgentServiceControl(Path(agent_service_socket),
                                                         candidate["ownerId"], worker_token)
    _, result = coordinate_install(request, control, agent_control, expected_nodes)
    return request, result


def install_published(request_path, router_socket, agent_service_socket, worker_token,
                      control=None, agent_control=None):
    request = read_request(request_path)
    control = control or RouterControl(Path(router_socket))
    owner_id = request["registry"].get("manifest", {}).get("ownerId")
    agent_control = agent_control or AgentServiceControl(Path(agent_service_socket), owner_id, worker_token)
    _, result = coordinate_install(request, control, agent_control)
    return request, result


def parser():
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(dest="command", required=True)
    form = commands.add_parser("form-install", help="form, durably save and install one CAS projection")
    form.add_argument("--registry", required=True, type=Path,
                      help="current signed registry or prior projection request")
    form.add_argument("--signer-public-key", required=True, type=Path)
    form.add_argument("--signer-private-key", required=True, type=Path)
    form.add_argument("--desired-node", type=Path,
                      help="one exact node binding plus explicit compatibility; omit only for legacy upgrade")
    form.add_argument("--legacy-compatibility", choices=("compatible", "legacy_readonly"),
                      help="required explicit gate for every existing legacy node")
    form.add_argument("--operation-id", required=True)
    form.add_argument("--output", required=True, type=Path,
                      help="new private durable request file; retained for idempotent retry")
    form.add_argument("--router-socket", required=True, type=Path)
    form.add_argument("--agent-service-socket", required=True, type=Path)
    form.add_argument("--new-node-host-id",
                      help="existing Agent Service host UUID; required only when adding a node")
    form.add_argument("--openssl", default="openssl")
    install = commands.add_parser("install", help="idempotently install a previously published exact request")
    install.add_argument("--request", required=True, type=Path)
    install.add_argument("--router-socket", required=True, type=Path)
    install.add_argument("--agent-service-socket", required=True, type=Path)
    return root


def main():
    args = parser().parse_args()
    os.umask(0o077)
    worker_token = os.environ.get("AGENT_SERVICE_WORKER_TOKEN")
    require(worker_token is not None, "AGENT_SERVICE_WORKER_TOKEN_REQUIRED")
    if args.command == "form-install":
        request, result = form_install(
            args.registry, args.signer_public_key, args.signer_private_key, args.desired_node,
            args.legacy_compatibility, args.operation_id, args.output, args.router_socket,
            args.agent_service_socket, worker_token, args.new_node_host_id, args.openssl,
        )
    else:
        request, result = install_published(args.request, args.router_socket,
                                            args.agent_service_socket, worker_token)
    print(json.dumps({
        "status": "REGISTRY_PROJECTION_INSTALLED", "operationId": request["operationId"],
        "registryVersion": result["registryVersion"], "registrySHA256": result["registrySHA256"],
    }, separators=(",", ":")))


if __name__ == "__main__":
    try:
        main()
    except ProjectionError as error:
        print(str(error))
        raise SystemExit(1) from None
