#!/usr/bin/env node

import assert from "node:assert/strict";
import fs from "node:fs";

import Ajv2020 from "../web/mobile-workspace/node_modules/ajv/dist/2020.js";
import addFormats from "../web/mobile-workspace/node_modules/ajv-formats/dist/index.js";

const read = (path) => JSON.parse(fs.readFileSync(new URL(path, import.meta.url)));
const openapi = read("./agent-service-v1.openapi.json");
const importSchema = read("./agent-registry-import-v1.schema.json");
const retirementSchema = read("./agent-retirement-v1.schema.json");

assert.equal(openapi.openapi, "3.1.0");
assert.ok(openapi.paths["/internal/v1/healthz"]?.get);
assert.ok(openapi.paths["/internal/v1/inventory"]?.get);
assert.ok(openapi.paths["/internal/v1/dialog-bindings"]?.get);

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

console.log("agent contracts: OK");
