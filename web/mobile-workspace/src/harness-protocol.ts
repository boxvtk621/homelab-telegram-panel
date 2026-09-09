/** Browser-safe validation for the frozen Harness wire contract.
 *
 * The schema is supplied by the route consumer (the Go and API package owns
 * its frozen copy). Keeping the validator schema-driven avoids duplicating a
 * 300 KiB generated schema in the browser bundle.
 */

import { HARNESS_SCHEMA_SHA256 } from './harness-protocol-types.ts';
import type { HarnessWireMap, HarnessWireType, HarnessWireValue } from './harness-protocol-types';

export const HARNESS_WIRE_TYPES = [
  'command', 'receipt', 'error', 'event', 'snapshot', 'dialogPage',
  'historyPage', 'requestPage', 'attemptPage', 'eventPage', 'attemptRead',
  'commandStatus', 'nodeIdentity', 'healthLive', 'healthReady', 'artifactMetadata',
] as const;
export type HarnessSchema = Record<string, unknown>;

const MAX_RAW_BYTES = 8 * 1024 * 1024;
const MAX_DEPTH = 32;

export class HarnessProtocolError extends Error {
  readonly code: string;
  constructor(code: string, message = code) {
    super(message);
    this.code = code;
    this.name = 'HarnessProtocolError';
  }
}

type Reader = { text: string; index: number; depth: number };

export function parseHarnessJson<K extends HarnessWireType>(
  raw: string,
  wireType: K,
  schema: HarnessSchema,
): HarnessWireMap[K] {
  if (typeof raw !== 'string') throw new HarnessProtocolError('raw_not_string');
  if (new TextEncoder().encode(raw).byteLength > MAX_RAW_BYTES)
    throw new HarnessProtocolError('raw_too_large');
  const reader: Reader = { text: raw, index: 0, depth: 0 };
  const value = parseValue(reader);
  skipWhitespace(reader);
  if (reader.index !== raw.length)
    throw new HarnessProtocolError('trailing_data');
  validateHarnessValue(value, wireType, schema);
  return value as HarnessWireMap[K];
}

export function validateHarnessValue(
  value: unknown,
  wireType: HarnessWireType,
  schema: HarnessSchema,
): asserts value is HarnessWireValue {
  if (!HARNESS_WIRE_TYPES.includes(wireType))
    throw new HarnessProtocolError('wire_type_invalid');
  const defs = schema.$defs;
  if (!isObject(defs) || !isObject(defs[wireType]))
    throw new HarnessProtocolError('schema_type_missing');
  validateSchema(value, defs[wireType], schema, 0);
  validateRelations(value, wireType);
}

export function isValidHarnessValue(
  value: unknown,
  wireType: HarnessWireType,
  schema: HarnessSchema,
): value is HarnessWireValue {
  try {
    validateHarnessValue(value, wireType, schema);
    return true;
  } catch (error) {
    if (error instanceof HarnessProtocolError) return false;
    throw error;
  }
}

function parseValue(reader: Reader): unknown {
  skipWhitespace(reader);
  if (reader.index >= reader.text.length) fail('unexpected_eof');
  const char = reader.text[reader.index];
  if (char === '{') return parseObject(reader);
  if (char === '[') return parseArray(reader);
  if (char === '"') return parseString(reader);
  if (char === 't' && reader.text.startsWith('true', reader.index)) { reader.index += 4; return true; }
  if (char === 'f' && reader.text.startsWith('false', reader.index)) { reader.index += 5; return false; }
  if (char === 'n' && reader.text.startsWith('null', reader.index)) { reader.index += 4; return null; }
  if (char >= '0' && char <= '9') return parseNumber(reader);
  fail('invalid_value');
}

function parseObject(reader: Reader): Record<string, unknown> {
  enter(reader);
  reader.index++;
  const result = Object.create(null) as Record<string, unknown>;
  const keys = new Set<string>();
  skipWhitespace(reader);
  if (reader.text[reader.index] === '}') { reader.index++; leave(reader); return result; }
  while (true) {
    skipWhitespace(reader);
    if (reader.text[reader.index] !== '"') fail('object_key_expected');
    const key = parseString(reader);
    if (keys.has(key)) fail('duplicate_key');
    keys.add(key);
    skipWhitespace(reader);
    if (reader.text[reader.index++] !== ':') fail('colon_expected');
    Object.defineProperty(result, key, { value: parseValue(reader), enumerable: true, writable: true, configurable: true });
    skipWhitespace(reader);
    const delimiter = reader.text[reader.index++];
    if (delimiter === '}') { leave(reader); return result; }
    if (delimiter !== ',') fail('object_delimiter_expected');
  }
}

function parseArray(reader: Reader): unknown[] {
  enter(reader);
  reader.index++;
  const result: unknown[] = [];
  skipWhitespace(reader);
    if (reader.text[reader.index] === ']') { reader.index++; leave(reader); return result; }
  while (true) {
    result.push(parseValue(reader));
    skipWhitespace(reader);
    const delimiter = reader.text[reader.index++];
    if (delimiter === ']') { leave(reader); return result; }
    if (delimiter !== ',') fail('array_delimiter_expected');
  }
}

function parseString(reader: Reader): string {
  reader.index++;
  let result = '';
  while (reader.index < reader.text.length) {
    const code = reader.text.charCodeAt(reader.index++);
    if (code === 34) return result;
    if (code < 0x20) fail('control_in_string');
    if (code !== 92) { result += String.fromCharCode(code); continue; }
    if (reader.index >= reader.text.length) fail('unterminated_escape');
    const escape = reader.text[reader.index++];
    const simple: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
    if (escape in simple) { result += simple[escape]; continue; }
    if (escape !== 'u' || reader.index + 4 > reader.text.length) fail('invalid_escape');
    const hex = reader.text.slice(reader.index, reader.index + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) fail('invalid_escape');
    reader.index += 4;
    const unit = Number.parseInt(hex, 16);
    if (unit >= 0xdc00 && unit <= 0xdfff) fail('unpaired_surrogate');
    if (unit >= 0xd800 && unit <= 0xdbff) {
      if (reader.text.slice(reader.index, reader.index + 2) !== '\\u') fail('unpaired_surrogate');
      const lowHex = reader.text.slice(reader.index + 2, reader.index + 6);
      if (!/^[0-9a-fA-F]{4}$/.test(lowHex)) fail('unpaired_surrogate');
      const low = Number.parseInt(lowHex, 16);
      if (low < 0xdc00 || low > 0xdfff) fail('unpaired_surrogate');
      reader.index += 6;
      result += String.fromCodePoint(0x10000 + ((unit - 0xd800) << 10) + low - 0xdc00);
    } else result += String.fromCharCode(unit);
  }
  fail('unterminated_string');
}

function parseNumber(reader: Reader): number {
  const start = reader.index;
  while (reader.index < reader.text.length && /[0-9]/.test(reader.text[reader.index])) reader.index++;
  const lexeme = reader.text.slice(start, reader.index);
  if (!/^0$|^[1-9][0-9]*$/.test(lexeme)) fail('number_lexeme_invalid');
  const value = Number(lexeme);
  if (!Number.isSafeInteger(value)) fail('number_out_of_range');
  return value;
}

function validateSchema(value: unknown, schema: unknown, root: HarnessSchema, depth: number): void {
  if (depth > MAX_DEPTH) fail('schema_depth_exceeded');
  if (!isObject(schema)) fail('schema_invalid');
  if ('$ref' in schema) {
    const ref = schema.$ref;
    if (typeof ref !== 'string' || !ref.startsWith('#/$defs/')) fail('schema_ref_invalid');
    const target = root.$defs;
    if (!isObject(target) || !isObject(target[ref.slice(8)])) fail('schema_ref_missing');
    validateSchema(value, target[ref.slice(8)], root, depth + 1); return;
  }
  for (const branchKey of ['oneOf', 'anyOf']) {
    if (Array.isArray(schema[branchKey])) {
      let matches = 0;
      for (const branch of schema[branchKey]) { try { validateSchema(value, branch, root, depth + 1); matches++; } catch {} }
      if ((branchKey === 'oneOf' && matches !== 1) || (branchKey === 'anyOf' && matches < 1)) fail('schema_union_mismatch');
      return;
    }
  }
  if ('const' in schema && !Object.is(value, schema.const)) fail('const_mismatch');
  if (Array.isArray(schema.enum) && !schema.enum.some((item) => Object.is(item, value))) fail('enum_mismatch');
  if (schema.type === 'object') {
    if (!isObject(value)) fail('object_expected');
    const properties = isObject(schema.properties) ? schema.properties : {};
    const required = Array.isArray(schema.required) ? schema.required : [];
    for (const key of required) if (typeof key !== 'string' || !Object.prototype.hasOwnProperty.call(value, key)) fail('required_missing');
    if (schema.additionalProperties === false) for (const key of Object.keys(value)) if (!Object.prototype.hasOwnProperty.call(properties, key)) fail('additional_property');
    for (const [key, rule] of Object.entries(properties)) if (Object.prototype.hasOwnProperty.call(value, key)) validateSchema(value[key], rule, root, depth + 1);
    return;
  }
  if (schema.type === 'array') {
    if (!Array.isArray(value)) fail('array_expected');
    if (typeof schema.maxItems === 'number' && value.length > schema.maxItems) fail('too_many_items');
    if ('items' in schema) for (const item of value) validateSchema(item, schema.items, root, depth + 1);
    return;
  }
  if (schema.type === 'string') {
    if (typeof value !== 'string') fail('string_expected');
    if (hasUnpairedSurrogate(value)) fail('unpaired_surrogate');
    if (typeof schema.minLength === 'number' && Array.from(value).length < schema.minLength) fail('string_too_short');
    if (typeof schema.maxLength === 'number' && Array.from(value).length > schema.maxLength) fail('string_too_long');
    if (typeof schema['x-utf8MaxBytes'] === 'number' && new TextEncoder().encode(value).byteLength > schema['x-utf8MaxBytes']) fail('string_bytes_too_large');
    if (typeof schema.pattern === 'string' && !new RegExp(schema.pattern).test(value)) fail('pattern_mismatch');
    if (schema.format === 'date-time' && !validDateTime(value)) fail('date_time_invalid');
    return;
  }
  if (schema.type === 'integer') {
    if (typeof value !== 'number' || !Number.isSafeInteger(value)) fail('integer_expected');
    if (typeof schema.minimum === 'number' && value < schema.minimum) fail('integer_below_minimum');
    if (typeof schema.maximum === 'number' && value > schema.maximum) fail('integer_above_maximum');
    return;
  }
  if (schema.type === 'boolean' && typeof value !== 'boolean') fail('boolean_expected');
  if (schema.type === 'null' && value !== null) fail('null_expected');
  if ('type' in schema && !['object', 'array', 'string', 'integer', 'boolean', 'null'].includes(String(schema.type))) fail('schema_type_unknown');
}

function validateRelations(value: unknown, wireType: HarnessWireType): void {
  if (!isObject(value)) return;
  if (wireType === 'historyPage') {
    const dialogId = value.dialogId;
    if (Array.isArray(value.items)) {
      for (const item of value.items) {
        if (isObject(item) && item.dialogId !== dialogId) fail('history_scope_mismatch');
      }
    }
  } else if (wireType === 'attemptPage') {
    const dialogId = value.dialogId;
    const requestId = value.requestId;
    if (Array.isArray(value.items)) {
      for (const item of value.items) {
        if (isObject(item) && (item.dialogId !== dialogId || item.requestId !== requestId))
          fail('attempt_scope_mismatch');
      }
    }
  } else if (wireType === 'eventPage') {
    const nodeId = value.nodeId;
    const epoch = value.epoch;
    const dialogId = value.dialogId;
    const attemptId = value.attemptId;
    if (Array.isArray(value.items)) {
      for (const item of value.items) {
        if (!isObject(item)) continue;
        if (item.nodeId !== nodeId || item.epoch !== epoch) fail('event_scope_mismatch');
        if (item.dialogId !== dialogId || item.attemptId !== attemptId) fail('event_scope_mismatch');
      }
    }
  } else if (wireType === 'healthReady' && value.readiness === 'ready') {
    const blockedReasons = value.blockedReasons;
    const identity = value.identity;
    const capabilities = isObject(identity) ? identity.capabilities : undefined;
    const required = ['chat', 'events', 'tool_results', 'cancel', 'steer_attached', 'session_resume', 'policy_enforcement'];
    if (!Array.isArray(blockedReasons) || blockedReasons.length !== 0 || !isObject(capabilities))
      fail('readiness_unverified');
    const keys = Object.keys(capabilities);
    if (keys.length !== required.length || required.some((key) => !Object.prototype.hasOwnProperty.call(capabilities, key)))
      fail('readiness_unverified');
    if (required.some((key) => capabilities[key] !== 'verified')) fail('readiness_unverified');
  }
  if (wireType === 'nodeIdentity' && value.schemaSHA256 !== HARNESS_SCHEMA_SHA256)
    fail('schema_digest_mismatch');
  if (wireType === 'healthReady' && isObject(value.identity) && value.identity.schemaSHA256 !== HARNESS_SCHEMA_SHA256)
    fail('schema_digest_mismatch');
  if (wireType === 'snapshot') {
    if (!isObject(value.node)) fail('snapshot_node_invalid');
    const node = value.node;
    if (Array.isArray(value.pendingQueue) && node.pendingCount !== value.pendingQueue.length)
      fail('snapshot_pending_count_mismatch');
    if (value.activeAttempt === null && node.activeAttemptId !== null)
      fail('snapshot_active_attempt_mismatch');
    if (value.activeAttempt !== null && isObject(value.activeAttempt) && node.activeAttemptId !== value.activeAttempt.attemptId)
      fail('snapshot_active_attempt_mismatch');
    if (Array.isArray(value.pendingQueue)) {
      const requestIds = new Set<string>();
      const inputMessageIds = new Set<string>();
      const queueSequences = new Set<number>();
      let previousSequence = -1;
      for (const item of value.pendingQueue) {
        if (!isObject(item) || typeof item.requestId !== 'string' || typeof item.inputMessageId !== 'string' || typeof item.queueSequence !== 'number')
          fail('snapshot_queue_invalid');
        if (requestIds.has(item.requestId) || inputMessageIds.has(item.inputMessageId) || queueSequences.has(item.queueSequence))
          fail('snapshot_queue_duplicate');
        if (item.queueSequence <= previousSequence) fail('snapshot_queue_order');
        requestIds.add(item.requestId);
        inputMessageIds.add(item.inputMessageId);
        queueSequences.add(item.queueSequence);
        previousSequence = item.queueSequence;
      }
    }
  }
  if (wireType === 'commandStatus' && isObject(value.receipt)) {
    if (value.receipt.commandId !== value.commandId || value.receipt.nodeId !== value.nodeId)
      fail('command_status_scope_mismatch');
  }
}

function validDateTime(value: string): boolean {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  if (!match) return false;
  const [, y, mo, d, h, mi, s, , zone] = match;
  const month = Number(mo), day = Number(d), hour = Number(h), minute = Number(mi), second = Number(s);
  if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59) return false;
  const year = Number(y);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const monthLengths = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  const maxDay = monthLengths[month - 1];
  if (day < 1 || day > maxDay) return false;
  if (zone !== 'Z' && (Number(zone.slice(1, 3)) > 23 || Number(zone.slice(4)) > 59)) return false;
  return true;
}

function skipWhitespace(reader: Reader): void {
  while (reader.index < reader.text.length && ' \t\r\n'.includes(reader.text[reader.index])) reader.index++;
}
function enter(reader: Reader): void { if (++reader.depth > MAX_DEPTH) fail('depth_exceeded'); }
function leave(reader: Reader): void { reader.depth--; }
function isObject(value: unknown): value is Record<string, unknown> { return typeof value === 'object' && value !== null && !Array.isArray(value); }
function hasUnpairedSurrogate(value: string): boolean {
  for (let index = 0; index < value.length; index++) {
    const unit = value.charCodeAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      if (index + 1 >= value.length) return true;
      const low = value.charCodeAt(index + 1);
      if (low < 0xdc00 || low > 0xdfff) return true;
      index++;
    } else if (unit >= 0xdc00 && unit <= 0xdfff) return true;
  }
  return false;
}
function fail(code: string): never { throw new HarnessProtocolError(code); }
