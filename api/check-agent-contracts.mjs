#!/usr/bin/env node

import assert from "node:assert/strict";
import fs from "node:fs";

import Ajv2020 from "../web/mobile-workspace/node_modules/ajv/dist/2020.js";
import addFormats from "../web/mobile-workspace/node_modules/ajv-formats/dist/index.js";

const read = (path) => JSON.parse(fs.readFileSync(new URL(path, import.meta.url)));
const openapi = read("./agent-service-v1.openapi.json");
const panelOpenapi = read("./panel-session.openapi.json");
const importSchema = read("./agent-registry-import-v1.schema.json");
const retirementSchema = read("./agent-retirement-v1.schema.json");
const routerRegistrySchema = read("./harness-router-registry-v1.schema.json");
const routerStateSchema = read("./harness-router-state-v2.schema.json");
const adapterJournalSchema = read("./docker-adapter-journal-v1.schema.json");
const hostSchema = read("./agent-host-v1.schema.json");
const tunnelBindingsSchema = read("./harness-tunnel-bindings-v1.schema.json");
const admissionSchema = read("./harness-admission-v1.schema.json");
const configurationSchema = read("./agent-configuration-v2.schema.json");
const configurationFixtures = read("./agent-configuration-v2.fixtures.json");

assert.equal(openapi.openapi, "3.1.0");
assert.ok(openapi.paths["/internal/v1/healthz"]?.get);
assert.ok(openapi.paths["/internal/v1/inventory"]?.get);
assert.ok(openapi.paths["/internal/v1/dialog-bindings"]?.get);
assert.ok(openapi.paths["/internal/v1/hosts"]?.get);
assert.ok(openapi.paths["/internal/v1/hosts"]?.post);
assert.ok(openapi.paths["/internal/v1/hosts/{hostId}"]?.get);
assert.ok(openapi.paths["/internal/v1/host-secrets"]?.post);
assert.ok(openapi.paths["/internal/v1/host-secrets/{operationId}"]?.get);
assert.ok(openapi.paths["/internal/v1/hosts/{hostId}/probe"]?.post);
assert.ok(openapi.paths["/internal/v1/configuration-drafts/validate"]?.post);
assert.ok(openapi.paths["/internal/v1/configuration-drafts/{nodeId}"]?.get);
assert.ok(openapi.paths["/internal/v1/configuration-drafts/{nodeId}"]?.post);
assert.ok(openapi.paths["/internal/v1/operations"]?.post);
assert.ok(openapi.paths["/internal/v1/operations/{operationId}"]?.get);
assert.ok(openapi.paths["/internal/v1/operation-targets/{nodeId}"]?.get);
assert.ok(openapi.paths["/internal/v1/operation-workers/claim"]?.post);
assert.ok(openapi.paths["/internal/v1/operation-workers/authority"]?.post);
assert.ok(openapi.paths["/internal/v1/operation-workers/sent"]?.post);
assert.ok(openapi.paths["/internal/v1/operation-workers/advance"]?.post);
assert.ok(openapi.paths["/internal/v1/registry-operations"]?.post);
assert.ok(openapi.paths["/internal/v1/registry-operations/{operationId}"]?.get);
assert.ok(openapi.paths["/internal/v1/registry-operations/sent"]?.post);
assert.ok(openapi.paths["/internal/v1/registry-operations/unknown"]?.post);
assert.ok(openapi.paths["/internal/v1/registry-operations/finish"]?.post);
assert.ok(openapi.paths["/internal/v1/registry-operations/fail"]?.post);
assert.ok(openapi.paths["/internal/v1/external-enrollments"]?.post);
assert.ok(panelOpenapi.paths["/api/v2/external-enrollment-hosts"]?.get);
assert.ok(panelOpenapi.paths["/api/v2/external-enrollments"]?.post);
assert.deepEqual(panelOpenapi.paths["/api/v2/external-enrollments"].post.security, [{ panelSession: [] }]);
assert.equal(
  panelOpenapi.paths["/api/v2/external-enrollments"].post.parameters.find(
    (parameter) => parameter.name === "X-Panel-CSRF",
  )?.required,
  true,
);
assert.deepEqual(openapi.components.securitySchemes.WorkerToken, {
  type: "apiKey",
  in: "header",
  name: "X-Agent-Service-Worker-Token",
  description: openapi.components.securitySchemes.WorkerToken.description,
});
for (const [path, method] of [
  ["/internal/v1/operation-workers/claim", "post"],
  ["/internal/v1/operation-workers/authority", "post"],
  ["/internal/v1/operation-workers/sent", "post"],
  ["/internal/v1/operation-workers/advance", "post"],
  ["/internal/v1/registry-operations", "post"],
  ["/internal/v1/registry-operations/{operationId}", "get"],
  ["/internal/v1/registry-operations/sent", "post"],
  ["/internal/v1/registry-operations/unknown", "post"],
  ["/internal/v1/registry-operations/finish", "post"],
  ["/internal/v1/registry-operations/fail", "post"],
  ["/internal/v1/external-enrollments", "post"],
]) {
  const operation = openapi.paths[path][method];
  assert.deepEqual(operation.security, [{ WorkerToken: [] }], `${method.toUpperCase()} ${path} worker security`);
  assert.ok(operation.responses["403"], `${method.toUpperCase()} ${path} missing 403`);
}

const ajv = new Ajv2020({ allErrors: true, strict: true });
addFormats(ajv);
const rewriteRefs = (value) => {
  if (Array.isArray(value)) return value.map(rewriteRefs);
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, rewriteRefs(item)]));
  }
  return typeof value === "string" ? value.replace("#/components/schemas/", "#/$defs/") : value;
};
const inventorySchema = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: "#/$defs/InventoryPage",
  $defs: rewriteRefs(openapi.components.schemas),
};
const validateInventory = ajv.compile(inventorySchema);
const validateBindings = ajv.compile({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: "#/$defs/DialogBindingPage",
  $defs: rewriteRefs(openapi.components.schemas),
});
ajv.addSchema(hostSchema);
const hostValidator = (name) => ajv.compile({ $ref: `${hostSchema.$id}#/$defs/${name}` });
const validateHostUpsert = hostValidator("HostUpsert");
const validateHostDescriptor = hostValidator("HostDescriptor");
const validateHostSecret = hostValidator("HostSecretInput");
const validateHostSecretProvision = hostValidator("HostSecretProvision");
const validateHostProbe = hostValidator("HostProbeRequest");
const validateHostObservation = hostValidator("HostObservation");
const validateHostRecord = hostValidator("HostRecord");
const validateHostPage = hostValidator("HostPage");
const validateImport = ajv.compile(importSchema);
const validateRetirement = ajv.compile(retirementSchema);
const validateOperation = ajv.compile({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: "#/$defs/OperationIntent",
  $defs: rewriteRefs(openapi.components.schemas),
});
const validateReceipt = ajv.compile({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: "#/$defs/OperationReceipt",
  $defs: rewriteRefs(openapi.components.schemas),
});
const validateOperationStatus = ajv.compile({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: "#/$defs/OperationStatus",
  $defs: rewriteRefs(openapi.components.schemas),
});
const operationSchema = (name) => ajv.compile({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: `#/$defs/${name}`,
  $defs: rewriteRefs(openapi.components.schemas),
});
const validateOperationTarget = operationSchema("OperationTargetStatus");
const validateOperationClaim = operationSchema("OperationClaimRequest");
const validateOperationProof = operationSchema("OperationProof");
const validateOperationWork = operationSchema("OperationWork");
const validateOperationAuthority = operationSchema("OperationAuthorityRequest");
const validateOperationSent = operationSchema("OperationSentRequest");
const validateOperationAdvance = operationSchema("OperationAdvanceRequest");
const validateRegistryIntent = operationSchema("RegistryOperationIntent");
const validateRegistryReceipt = operationSchema("RegistryOperationReceipt");
const validateRegistryStatus = operationSchema("RegistryOperationStatus");
const validateRegistryCommand = operationSchema("RegistryOperationCommand");
const validateRegistryFinish = operationSchema("RegistryOperationFinish");
const validateRegistryFailure = operationSchema("RegistryOperationFailure");
const validateExternalEnrollment = operationSchema("ExternalEnrollmentRequest");
const validateExternalEnrollmentPlan = operationSchema("ExternalEnrollmentPlan");
const panelSchema = (name) =>
  ajv.compile({
    $schema: "https://json-schema.org/draft/2020-12/schema",
    ...panelOpenapi.components.schemas[name],
  });
const validatePanelSession = panelSchema("Session");
const validateEnrollmentHostPage = panelSchema("EnrollmentHostPage");
const validatePanelExternalEnrollment = panelSchema("ExternalEnrollmentRequest");
const validatePanelExternalEnrollmentResult = panelSchema("ExternalEnrollmentResult");
ajv.addSchema(routerRegistrySchema);
const validateRouterRegistry = ajv.getSchema(routerRegistrySchema.$id);
const validateRouterState = ajv.compile(routerStateSchema);
const validateAdapterJournal = ajv.compile(adapterJournalSchema);
const validateTunnelBindings = ajv.compile(tunnelBindingsSchema);
const validateAdmission = ajv.compile(admissionSchema);
const validateConfiguration = ajv.compile(configurationSchema);
for (const fixture of configurationFixtures.valid) assert.equal(validateConfiguration(fixture), true, ajv.errorsText(validateConfiguration.errors));
for (const fixture of configurationFixtures.invalidRaw) assert.equal(typeof fixture.raw, "string");
const validateBuildContextManifest = ajv.compile({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: "#/$defs/buildContextManifest",
  $defs: configurationSchema.$defs,
});
assert.equal(validateBuildContextManifest(configurationFixtures.buildContextManifest), true, ajv.errorsText(validateBuildContextManifest.errors));
const validateBuildContextAsset = ajv.compile({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $ref: "#/$defs/asset",
  $defs: configurationSchema.$defs,
});
const pathAsset = {
  assetId: "44444444-4444-4444-8444-444444444444",
  sha256: "b".repeat(64),
};
for (const path of configurationFixtures.buildContextPathCases.valid)
  assert.equal(validateBuildContextAsset({ ...pathAsset, path }), true, `${path}: ${ajv.errorsText(validateBuildContextAsset.errors)}`);
for (const path of configurationFixtures.buildContextPathCases.invalid)
  assert.equal(validateBuildContextAsset({ ...pathAsset, path }), false, `${path} must be rejected`);

const observedAt = "2026-09-14T10:00:00Z";
const item = {
  nodeId: "20000000-0000-4000-8000-000000000001",
  name: "Agent",
  engine: "codex",
  sourceMode: "fixture",
  host: {
    hostId: "10000000-0000-4000-8000-000000000001",
    name: "Local Mac",
  },
  registrationMode: "compatible",
  status: "busy",
  state: {
    process: "running",
    connection: "online",
    readiness: "ready",
    occupancy: "busy",
  },
  observedAt,
  source: "registry-import",
  pendingCount: { value: 1, observedAt, source: "registry-import" },
  actions: {
    openWorkspace: { allowed: true },
    sendMessage: { allowed: true },
    lifecycle: {
      allowed: false,
      reason: "r01_read_only",
      nextAction: "Available in a later stage.",
    },
  },
  dialogCount: 20_000,
};
const binding = {
  nodeDialogId: "30000000-0000-4000-8000-000000000001",
  logicalDialogId: "40000000-0000-4000-8000-000000000001",
  bindingVersion: 1,
};
assert.equal(
  validateInventory({
    schemaId: "agent-management-v1",
    items: [item],
    nextCursor: null,
  }),
  true,
  ajv.errorsText(validateInventory.errors),
);
assert.equal(
  validateBindings({
    schemaId: "agent-dialog-bindings-v1",
    nodeId: item.nodeId,
    items: [binding],
    nextCursor: "next-page",
  }),
  true,
  ajv.errorsText(validateBindings.errors),
);
const hostUpsert = {
  schemaId: "agent-host-upsert-v1",
  hostId: item.host.hostId,
  expectedHostVersion: 0,
  displayName: "Local Desktop",
  transport: "local",
  targetRef: "desktop-local",
  credentialRef: "",
  registryCredentialRef: "",
  expectedHostKey: "",
  dockerContextRef: "desktop-linux",
  expectedIdentitySHA256: "",
  hostPlatform: "darwin",
  hostArchitecture: "arm64",
};
assert.equal(validateHostUpsert(hostUpsert), true, ajv.errorsText(validateHostUpsert.errors));
const hostDescriptor = { ...hostUpsert, schemaId: "docker-host-descriptor-v1", hostVersion: 1 };
delete hostDescriptor.expectedHostVersion;
assert.equal(validateHostDescriptor(hostDescriptor), true, ajv.errorsText(validateHostDescriptor.errors));
const hostSecret = {
  schemaId: "docker-secret-input-v1",
  operationId: "30000000-0000-4000-8000-000000000001",
  kind: "ssh",
  privateKey: "cHJpdmF0ZS1rZXk=",
  passphrase: "",
  payload: "",
};
assert.equal(validateHostSecret(hostSecret), true, ajv.errorsText(validateHostSecret.errors));
assert.equal(validateHostSecret({ ...hostSecret, payload: "cmVnaXN0cnk=" }), false);
assert.equal(validateHostSecretProvision({
  schemaId: "docker-secret-provision-v1",
  operationId: hostSecret.operationId,
  kind: "ssh",
  status: "provisioned",
  credentialRef: "cred_1",
}), true, ajv.errorsText(validateHostSecretProvision.errors));
assert.equal(validateHostProbe({ schemaId: "agent-host-probe-v1", expectedHostVersion: 1 }), true, ajv.errorsText(validateHostProbe.errors));
const unavailableObservation = {
  schemaId: "docker-host-observation-v1",
  hostId: hostUpsert.hostId,
  hostVersion: 1,
  observedAt,
  availability: "unavailable",
  failureStage: "daemon_ping",
  failureCode: "docker_permission_denied",
  nextAction: "Provision daemon access.",
  hostKeySHA256: "",
  daemonId: "",
  dockerContextRef: hostUpsert.dockerContextRef,
  contextEndpoint: "",
  engineOS: "",
  architecture: "",
  apiVersion: "",
  engineVersion: "",
  capabilities: [],
  identitySHA256: "",
  registryAvailability: "not_checked",
};
assert.equal(validateHostObservation(unavailableObservation), true, ajv.errorsText(validateHostObservation.errors));
const incompatiblePlatformObservation = {
  ...unavailableObservation,
  failureStage: "target_platform",
  failureCode: "platform_incompatible",
  nextAction: "Select a supported Linux containers target.",
  daemonId: "windows-daemon",
  engineOS: "windows",
  architecture: "unknown64",
  apiVersion: "1.56",
  engineVersion: "29.8.0",
};
assert.equal(validateHostObservation(incompatiblePlatformObservation), true, ajv.errorsText(validateHostObservation.errors));
const hostRecord = {
  ...hostUpsert,
  schemaId: "agent-host-v1",
  hostVersion: 1,
  observedAt: null,
  availability: "unverified",
  failureStage: "",
  failureCode: "",
  nextAction: "",
  hostKeySHA256: "",
  daemonId: "",
  contextEndpoint: "",
  engineOS: "",
  architecture: "",
  apiVersion: "",
  engineVersion: "",
  capabilities: [],
  identitySHA256: "",
  registryAvailability: "not_configured",
};
delete hostRecord.expectedHostVersion;
assert.equal(validateHostRecord(hostRecord), true, ajv.errorsText(validateHostRecord.errors));
assert.equal(validateHostPage({ schemaId: "agent-host-page-v1", items: [hostRecord], nextCursor: null }), true, ajv.errorsText(validateHostPage.errors));
const invalidAction = structuredClone(item);
invalidAction.actions.openWorkspace.reason = "must_not_be_present";
assert.equal(
  validateInventory({
    schemaId: "agent-management-v1",
    items: [invalidAction],
    nextCursor: null,
  }),
  false,
);

assert.equal(
  validateImport({
    schemaId: "agent-registry-import-v1",
    complete: true,
    hosts: [item.host],
    nodes: [
      {
        nodeId: item.nodeId,
        hostId: item.host.hostId,
        registrationMode: "compatible",
        observation: {
          ...item.state,
          observedAt,
          source: "registry-import",
          pendingCount: 1,
        },
        dialogs: [{ nodeDialogId: binding.nodeDialogId }],
      },
    ],
  }),
  true,
  ajv.errorsText(validateImport.errors),
);
assert.equal(
  validateRetirement({
    schemaId: "agent-retirement-v1",
    descriptorVersion: 1,
    kind: "external_detached",
    state: "verified",
    observedAt,
    processExitObserved: null,
    ownershipReleaseProof: "operator-release-1",
  }),
  true,
  ajv.errorsText(validateRetirement.errors),
);
assert.equal(
  validateRetirement({
    schemaId: "agent-retirement-v1",
    descriptorVersion: 1,
    kind: "external_detached",
    state: "verified",
    observedAt,
    processExitObserved: false,
    ownershipReleaseProof: null,
  }),
  false,
);

const operation = {
  schemaId: "agent-operation-v1",
  operationId: "op-1",
  kind: "adapter.fixture",
  target: {
    nodeId: item.nodeId,
    hostId: item.host.hostId,
    registrationRevision: 1,
    registrationEpoch: 1,
    generation: 0,
  },
  step: {
    stepId: "apply-1",
    action: "adapter.fixture.apply",
    resourceIds: ["container:agent-1"],
  },
};
assert.equal(validateOperation(operation), true, ajv.errorsText(validateOperation.errors));
const wrongAction = structuredClone(operation);
wrongAction.step.action = "router.registry.install";
assert.equal(validateOperation(wrongAction), false);
const receipt = {
  schemaId: "agent-operation-receipt-v1",
  operationId: operation.operationId,
  requestHash: "a".repeat(64),
  target: operation.target,
  acceptedGeneration: 1,
  acceptedAt: observedAt,
};
assert.equal(validateReceipt(receipt), true, ajv.errorsText(validateReceipt.errors));
const operationStatus = {
  schemaId: "agent-operation-status-v1",
  receipt,
  phase: "reconciling",
  effectState: "unknown",
  operationVersion: 4,
  updatedAt: observedAt,
  resultCode: null,
};
assert.equal(validateOperationStatus(operationStatus), true, ajv.errorsText(validateOperationStatus.errors));
const unsafeStatus = structuredClone(operationStatus);
unsafeStatus.phase = "succeeded";
assert.equal(validateOperationStatus(unsafeStatus), false);

const operationTarget = {
  schemaId: "agent-operation-target-v1",
  target: operation.target,
  registryMode: "fixture",
  registrationMode: "compatible",
};
assert.equal(validateOperationTarget(operationTarget), true, ajv.errorsText(validateOperationTarget.errors));
const claimRequest = {
  schemaId: "agent-operation-claim-request-v1",
  nodeId: operation.target.nodeId,
  workerId: "worker-1",
  leaseMilliseconds: 30_000,
};
assert.equal(validateOperationClaim(claimRequest), true, ajv.errorsText(validateOperationClaim.errors));
const proof = {
  schemaId: "agent-operation-proof-v1",
  operationId: operation.operationId,
  requestHash: receipt.requestHash,
  nodeId: operation.target.nodeId,
  generation: 1,
  workerId: claimRequest.workerId,
  workerToken: "50000000-0000-4000-8000-000000000001",
  operationVersion: 2,
  leaseExpiresAt: observedAt,
};
assert.equal(validateOperationProof(proof), true, ajv.errorsText(validateOperationProof.errors));
assert.equal(
  validateOperationWork({ schemaId: "agent-operation-work-v1", intent: operation, proof, effectState: "not_sent" }),
  true,
  ajv.errorsText(validateOperationWork.errors),
);
assert.equal(
  validateOperationAuthority({ schemaId: "agent-operation-authority-v1", proof }),
  true,
  ajv.errorsText(validateOperationAuthority.errors),
);
assert.equal(
  validateOperationSent({ schemaId: "agent-operation-sent-v1", proof }),
  true,
  ajv.errorsText(validateOperationSent.errors),
);
assert.equal(
  validateOperationAdvance({
    schemaId: "agent-operation-advance-v1",
    proof,
    phase: "reconciling",
    effectState: "unknown",
    resultCode: "effect_unknown",
  }),
  true,
  ajv.errorsText(validateOperationAdvance.errors),
);
const extraProof = structuredClone(proof);
extraProof.expiresAt = extraProof.leaseExpiresAt;
assert.equal(validateOperationProof(extraProof), false);

const signedProjection = {
  manifest: {
    schemaId: "harness-router-registry-v1",
    registryVersion: 2,
    ownerId: "owner-1",
    mode: "fixture",
    wireSchemaSHA256: "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9",
    nodes: [
      {
        nodeId: item.nodeId,
        name: item.name,
        adapter: item.engine,
        url: "https://agent.invalid:9443",
        certificateSHA256: "b".repeat(64),
        registrationRevision: 1,
        registrationEpoch: 7,
        compatibility: "compatible",
      },
    ],
  },
  signature: `${"A".repeat(86)}==`,
};
assert.equal(validateRouterRegistry(signedProjection), true, ajv.errorsText(validateRouterRegistry.errors));
const registryIntent = {
  schemaId: "agent-registry-operation-v1",
  operationId: "registry-1",
  expected: { registryVersion: 1, registrySHA256: "a".repeat(64) },
  registry: signedProjection,
  newNodeHostId: null,
};
const registryReceipt = {
  schemaId: "agent-registry-operation-receipt-v1",
  operationId: registryIntent.operationId,
  requestHash: "b".repeat(64),
  expectedRegistryVersion: 1,
  expectedRegistrySHA256: registryIntent.expected.registrySHA256,
  candidateRegistryVersion: 2,
  candidateRegistrySHA256: "c".repeat(64),
  affectedNodeIds: [item.nodeId],
  acceptedAt: observedAt,
};
const registryStatus = {
  schemaId: "agent-registry-operation-status-v1",
  receipt: registryReceipt,
  phase: "reconciling",
  effectState: "unknown",
  operationVersion: 3,
  updatedAt: observedAt,
  resultCode: null,
};
const registryCommand = {
  schemaId: "agent-registry-operation-command-v1",
  operationId: registryIntent.operationId,
  requestHash: registryReceipt.requestHash,
  operationVersion: registryStatus.operationVersion,
};
assert.equal(validateRegistryIntent(registryIntent), true, ajv.errorsText(validateRegistryIntent.errors));
assert.equal(validateRegistryReceipt(registryReceipt), true, ajv.errorsText(validateRegistryReceipt.errors));
assert.equal(validateRegistryStatus(registryStatus), true, ajv.errorsText(validateRegistryStatus.errors));
assert.equal(validateRegistryCommand(registryCommand), true, ajv.errorsText(validateRegistryCommand.errors));
assert.equal(validateRegistryFinish({
  ...registryCommand,
  schemaId: "agent-registry-operation-finish-v1",
  registryVersion: 2,
  registrySHA256: registryReceipt.candidateRegistrySHA256,
  effectState: "reconciled",
}), true, ajv.errorsText(validateRegistryFinish.errors));
assert.equal(validateRegistryFailure({
  operationId: registryCommand.operationId,
  schemaId: "agent-registry-operation-failure-v1",
  requestHash: registryCommand.requestHash,
  operationVersion: registryCommand.operationVersion,
  resultCode: "router.node_not_sealed",
}), true, ajv.errorsText(validateRegistryFailure.errors));
const impossibleRegistryStatus = structuredClone(registryStatus);
impossibleRegistryStatus.phase = "succeeded";
assert.equal(validateRegistryStatus(impossibleRegistryStatus), false);
const externalBinding = {
  kind: "external",
  nodeId: "20000000-0000-4000-8000-000000000002",
  registrationRevision: 1,
  registrationEpoch: 1,
  endpointRevision: 1,
  hostId: "10000000-0000-4000-8000-000000000002",
  hostVersion: 3,
  transport: "ssh",
  targetRef: "host-two",
  credentialRef: "ssh-two",
  dockerContextRef: "",
  expectedHostKey: `SHA256:${"B".repeat(43)}`,
  expectedHostIdentitySHA256: "",
  hostPlatform: "",
  hostArchitecture: "",
  containerId: "",
  runtimeGeneration: 0,
  address: "127.0.0.1:9443",
};
const externalEnrollment = {
  schemaId: "external-harness-enrollment-v1",
  operationId: "enroll-1",
  hostId: externalBinding.hostId,
  expectedHostVersion: externalBinding.hostVersion,
  nodeId: externalBinding.nodeId,
  name: "External Codex",
  adapter: "codex",
  endpointUri: "https://127.0.0.1:9443",
  certificateSHA256: "e".repeat(64),
};
const externalProjection = structuredClone(signedProjection);
externalProjection.manifest.registryVersion = 3;
externalProjection.manifest.nodes.push({
  nodeId: externalEnrollment.nodeId,
  name: externalEnrollment.name,
  adapter: externalEnrollment.adapter,
  url: externalEnrollment.endpointUri,
  certificateSHA256: externalEnrollment.certificateSHA256,
  registrationRevision: 1,
  registrationEpoch: 1,
  compatibility: "compatible",
  endpointBindingSHA256: "f".repeat(64),
});
const externalPlan = {
  schemaId: "external-harness-enrollment-plan-v1",
  operationId: externalEnrollment.operationId,
  requestHash: "a".repeat(64),
  nodeId: externalEnrollment.nodeId,
  registry: externalProjection,
  binding: externalBinding,
  status: registryStatus,
};
assert.equal(validateExternalEnrollment(externalEnrollment), true, ajv.errorsText(validateExternalEnrollment.errors));
assert.equal(validateExternalEnrollmentPlan(externalPlan), true, ajv.errorsText(validateExternalEnrollmentPlan.errors));
assert.equal(
  validatePanelSession({
    user: { id: "owner-1", login: "owner", name: "Owner" },
    csrf: "s".repeat(43),
    writes_enabled: true,
    inventory_enabled: true,
    enrollment_enabled: true,
  }),
  true,
  ajv.errorsText(validatePanelSession.errors),
);
assert.equal(
  validateEnrollmentHostPage({
    schemaId: "external-harness-enrollment-host-page-v1",
    items: [
      {
        hostId: externalBinding.hostId,
        hostVersion: externalBinding.hostVersion,
        displayName: "Remote host",
        transport: externalBinding.transport,
        availability: "ready",
      },
    ],
    nextCursor: null,
  }),
  true,
  ajv.errorsText(validateEnrollmentHostPage.errors),
);
assert.equal(validatePanelExternalEnrollment(externalEnrollment), true, ajv.errorsText(validatePanelExternalEnrollment.errors));
assert.equal(
  validatePanelExternalEnrollmentResult({
    schemaId: "external-harness-enrollment-result-v1",
    operationId: externalEnrollment.operationId,
    nodeId: externalEnrollment.nodeId,
    status: "ready",
    registrationRevision: 1,
    identityEpoch: 1,
  }),
  true,
  ajv.errorsText(validatePanelExternalEnrollmentResult.errors),
);
const admission = {
  schemaId: "harness-admission-v1",
  ownerId: "owner-1",
  nodeId: externalEnrollment.nodeId,
  registrationRevision: 1,
  identityEpoch: 1,
  wireSchemaSHA256: signedProjection.manifest.wireSchemaSHA256,
  adapter: { kind: "codex", version: "0.153.4" },
  readiness: "ready",
  capabilities: {
    profile: true,
    native_epoch: true,
    policy_enforcement: true,
    history: true,
    facts: true,
    durable_receipts: true,
    replica_export: true,
    replica_import: true,
    asset_export: true,
    asset_import: true,
    scoped_quiesce: true,
    ownership_release: true,
    target_reservation: true,
  },
};
assert.equal(validateAdmission(admission), true, ajv.errorsText(validateAdmission.errors));
const incompleteAdmission = structuredClone(admission);
delete incompleteAdmission.capabilities.target_reservation;
assert.equal(validateAdmission(incompleteAdmission), false);
const routerState = {
  schema: 2,
  ownerId: "owner-1",
  registryVersion: 2,
  registrySHA256: "c".repeat(64),
  registryOperationId: "registry-1",
  registryRequestSHA256: "d".repeat(64),
  registryEnvelope: signedProjection,
  nodes: {
    [item.nodeId]: {
      mode: "eligible",
      stateVersion: 2,
      generation: 1,
      registrationRevision: 1,
      identityEpoch: 7,
      compatibility: "compatible",
      adapterKind: item.engine,
      adapterVersion: "0.153.4",
    },
  },
};
assert.equal(validateRouterState(routerState), true, ajv.errorsText(validateRouterState.errors));
const journal = {
  schemaId: "docker-adapter-journal-v1",
  daemonId: "fixture-daemon",
  instanceId: "fixture-instance",
  entries: [
    {
      operationId: operation.operationId,
      stepId: operation.step.stepId,
      generation: 1,
      requestHash: receipt.requestHash,
      resourceIds: operation.step.resourceIds,
      state: "sent",
      updatedAt: observedAt,
    },
  ],
};
assert.equal(validateAdapterJournal(journal), true, ajv.errorsText(validateAdapterJournal.errors));
const invalidJournal = structuredClone(journal);
invalidJournal.entries[0].receiptId = "invented-receipt";
assert.equal(validateAdapterJournal(invalidJournal), false);

const tunnelBindings = {
  schemaId: "harness-tunnel-bindings-v1",
  ownerId: "owner-1",
  registrySHA256: "c".repeat(64),
  nodes: [{
    nodeId: item.nodeId,
    registrationRevision: 1,
    registrationEpoch: 7,
    endpointRevision: 2,
    hostId: "10000000-0000-4000-8000-000000000001",
    hostVersion: 3,
    transport: "ssh",
    targetRef: "host-one",
    credentialRef: "ssh-one",
    dockerContextRef: "default",
    expectedHostKey: `SHA256:${"A".repeat(43)}`,
    expectedHostIdentitySHA256: "d".repeat(64),
    hostPlatform: "linux",
    hostArchitecture: "amd64",
    containerId: "e".repeat(64),
    runtimeGeneration: 4,
    address: "127.0.0.1:9443",
  }],
};
assert.equal(validateTunnelBindings(tunnelBindings), true, ajv.errorsText(validateTunnelBindings.errors));
const publicTunnel = structuredClone(tunnelBindings);
publicTunnel.nodes[0].address = "192.0.2.1:9443";
assert.equal(validateTunnelBindings(publicTunnel), false);
const stagedTunnelBindings = structuredClone(tunnelBindings);
stagedTunnelBindings.acceptedRegistrySHA256s = ["c".repeat(64), "d".repeat(64)];
stagedTunnelBindings.nodes.push(externalBinding);
assert.equal(validateTunnelBindings(stagedTunnelBindings), true, ajv.errorsText(validateTunnelBindings.errors));

console.log("agent contracts: OK");
