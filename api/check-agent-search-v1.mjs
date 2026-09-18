import fs from "node:fs";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import Ajv2020 from "../web/mobile-workspace/node_modules/ajv/dist/2020.js";
import addFormats from "../web/mobile-workspace/node_modules/ajv-formats/dist/index.js";

const schemaBytes = fs.readFileSync(new URL("./agent-search-v1.schema.json", import.meta.url));
const fixtureBytes = fs.readFileSync(new URL("./agent-search-v1.fixtures.json", import.meta.url));
const schema = JSON.parse(schemaBytes);
const fixtures = JSON.parse(fixtureBytes);
const manifest = JSON.parse(fs.readFileSync(new URL("./agent-search-v1.manifest.json", import.meta.url)));
const sha256 = (value) => createHash("sha256").update(value).digest("hex");
assert.equal(manifest.schemaId, "agent-search-v1");
assert.equal(manifest.schemaSHA256, sha256(schemaBytes));
assert.equal(manifest.fixturesSHA256, sha256(fixtureBytes));
const ajv = new Ajv2020({ allErrors: true, strict: true });
addFormats(ajv);
const validate = ajv.compile(schema);
for (const [name, fixture] of Object.entries(fixtures)) {
  assert.equal(validate(fixture), true, `${name}: ${ajv.errorsText(validate.errors)}`);
}
console.log("agent-search-v1 fixtures: PASS");
