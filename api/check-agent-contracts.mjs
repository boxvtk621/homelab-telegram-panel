#!/usr/bin/env node

import assert from "node:assert/strict";
import fs from "node:fs";

import Ajv2020 from "../web/mobile-workspace/node_modules/ajv/dist/2020.js";
import addFormats from "../web/mobile-workspace/node_modules/ajv-formats/dist/index.js";

const read = (path) => JSON.parse(fs.readFileSync(new URL(path, import.meta.url)));
const openapi = read("./agent-service-v1.openapi.json");
const importSchema = read("./agent-registry-import-v1.schema.json");
const retirementSchema = read("./agent-retirement-v1.schema.json");
const routerRegistrySchema = read("./harness-router-registry-v1.schema.json");
const routerStateSchema = read("./harness-router-state-v2.schema.json");
const adapterJournalSchema = read("./docker-adapter-journal-v1.schema.json");

assert.equal(openapi.openapi, "3.1.0");
assert.ok(openapi.paths["/internal/v1/healthz"]?.get);
assert.ok(openapi.paths["/internal/v1/inventory"]?.get);
assert.ok(openapi.paths["/internal/v1/dialog-bindings"]?.get);
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
ajv.addSchema(routerRegistrySchema);
const validateRouterRegistry = ajv.getSchema(routerRegistrySchema.$id);
const validateRouterState = ajv.compile(routerStateSchema);
const validateAdapterJournal = ajv.compile(adapterJournalSchema);

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

console.log("agent contracts: OK");
