import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import {
  HarnessProtocolError,
  HARNESS_WIRE_TYPES,
  isValidHarnessValue,
  parseHarnessJson,
  validateHarnessValue,
} from '../src/harness-protocol.ts';

const apiDir = new URL('../../../api/', import.meta.url);
const schema = JSON.parse(await readFile(new URL('harness-v1.schema.json', apiDir), 'utf8'));
const corpus = JSON.parse(await readFile(new URL('harness-v1.fixtures.json', apiDir), 'utf8'));

for (const fixture of corpus.fixtures) {
  test(`frozen corpus: ${fixture.name}`, () => {
    if (fixture.shapeValid) {
      validateHarnessValue(fixture.value, fixture.wireType, schema);
      assert.doesNotThrow(() => parseHarnessJson(JSON.stringify(fixture.value), fixture.wireType, schema));
    } else {
      assert.equal(isValidHarnessValue(fixture.value, fixture.wireType, schema), false);
      if (fixture.raw) assert.throws(() => parseHarnessJson(fixture.raw, fixture.wireType, schema), HarnessProtocolError);
      else assert.throws(() => parseHarnessJson(JSON.stringify(fixture.value), fixture.wireType, schema), HarnessProtocolError);
    }
    // Shape validity never claims the authoritative state/CAS outcome.
    if (fixture.authoritativeOutcome) assert.equal(fixture.schemaValid ?? fixture.shapeValid, true);
  });
}

test('wire type allowlist is frozen', () => {
  assert.deepEqual([...HARNESS_WIRE_TYPES], [
    'command', 'receipt', 'error', 'event', 'snapshot', 'dialogPage',
    'historyPage', 'requestPage', 'attemptPage', 'eventPage', 'attemptRead', 'commandStatus',
    'nodeIdentity', 'healthLive', 'healthReady', 'artifactMetadata',
  ]);
});

test('raw parser rejects duplicate and escaped duplicate keys', () => {
  for (const raw of ['{"a":1,"a":2}', '{"a":1,"\\u0061":2}']) {
    assert.throws(() => parseHarnessJson(raw, 'error', schema), /duplicate_key/);
  }
});

test('raw parser and validator reject prototype keys and inherited required fields', () => {
  const validCommand = corpus.fixtures.find((fixture) => fixture.wireType === 'command' && fixture.shapeValid).value;
  assert.throws(() => parseHarnessJson(`{"__proto__":${JSON.stringify(validCommand)}}`, 'error', schema), /required_missing|additional_property|schema_union_mismatch/);
  for (const key of ['__proto__', 'constructor', 'toString']) {
    assert.throws(() => parseHarnessJson(`{"${key}":1}`, 'healthLive', schema), HarnessProtocolError);
  }
  const inherited = Object.create({ protocolVersion: 1, schemaId: 'harness-wire-v1', status: 'live', processStartedAt: '2026-01-01T00:00:00Z' });
  assert.equal(isValidHarnessValue(inherited, 'healthLive', schema), false);
});

test('raw parser accepts only JSON whitespace', () => {
  assert.throws(() => parseHarnessJson('{\u00a0"protocolVersion":1}', 'healthLive', schema), /invalid_value|object_key_expected/);
});

test('raw parser rejects fractional, exponent, unsafe and leading-zero numbers', () => {
  for (const raw of ['{"protocolVersion":1.0}', '{"protocolVersion":1e0}', '{"protocolVersion":01}', '{"protocolVersion":9007199254740992}']) {
    assert.throws(() => parseHarnessJson(raw, 'healthLive', schema), HarnessProtocolError);
  }
});

test('raw parser rejects unpaired UTF-16 surrogates', () => {
  assert.throws(() => parseHarnessJson('{"safeMessage":"\\ud800"}', 'error', schema), /unpaired_surrogate/);
  assert.throws(() => parseHarnessJson('{"safeMessage":"\\udc00"}', 'error', schema), /unpaired_surrogate/);
});

test('validator rejects unpaired UTF-16 surrogates in existing values', () => {
  const value = { protocolVersion: 1, schemaId: 'harness-wire-v1', status: 'live', processStartedAt: '2026-01-01T00:00:00Z' + '\ud800' };
  assert.equal(isValidHarnessValue(value, 'healthLive', schema), false);
});

test('message payload rejects every unpaired surrogate and accepts a pair', () => {
  const base = structuredClone(corpus.fixtures.find((fixture) => fixture.name === 'command.2.message.enqueue').value);
  for (const text of ['hello\ud800', '\ud800hello', 'hello\udc00']) {
    const value = structuredClone(base);
    value.payload.text = text;
    assert.equal(isValidHarnessValue(value, 'command', schema), false);
    const escaped = JSON.stringify(value);
    const rawLiteral = escaped.replace('\\\\ud800', '\ud800').replace('\\\\udc00', '\udc00');
    assert.equal(isValidHarnessValue(JSON.parse(escaped), 'command', schema), false);
    assert.throws(() => parseHarnessJson(rawLiteral, 'command', schema), /unpaired_surrogate/);
  }
  const pair = structuredClone(base);
  pair.payload.text = 'hello\ud83d\ude00';
  assert.equal(isValidHarnessValue(pair, 'command', schema), true);
  assert.doesNotThrow(() => parseHarnessJson(JSON.stringify(pair), 'command', schema));
});

test('date-time validation rejects normalized invalid calendar values', () => {
  const value = { protocolVersion: 1, schemaId: 'harness-wire-v1', status: 'live', processStartedAt: '2026-02-30T12:00:00Z' };
  assert.equal(isValidHarnessValue(value, 'healthLive', schema), false);
});

test('date-time validation accepts Gregorian leap day in year zero', () => {
  const value = { protocolVersion: 1, schemaId: 'harness-wire-v1', status: 'live', processStartedAt: '0000-02-29T00:00:00Z' };
  assert.equal(isValidHarnessValue(value, 'healthLive', schema), true);
});

test('snapshot queue requires unique strictly increasing identifiers', () => {
  const base = structuredClone(corpus.fixtures.find((fixture) => fixture.name === 'snapshot.atomic').value);
  for (const mutate of [
    (items) => { items[1] = structuredClone(items[0]); items[1].queueSequence = 2; },
    (items) => { items[1].inputMessageId = items[0].inputMessageId; },
    (items) => { items[1].queueSequence = items[0].queueSequence; },
    (items) => { items[1].queueSequence = 0; },
  ]) {
    const value = structuredClone(base);
    value.pendingQueue.push(structuredClone(value.pendingQueue[0]));
    value.node.pendingCount = value.pendingQueue.length;
    mutate(value.pendingQueue);
    assert.equal(isValidHarnessValue(value, 'snapshot', schema), false);
  }
});
