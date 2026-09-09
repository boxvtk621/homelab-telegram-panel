#!/usr/bin/env node
import { readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
const schemaPath = path.join(root, 'api', 'harness-v1.schema.json');
const outputPath = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'src', 'harness-protocol-types.ts');
const check = process.argv.includes('--check');
const schemaBytes = await readFile(schemaPath);
const schemaSHA256 = createHash('sha256').update(schemaBytes).digest('hex');
const schema = JSON.parse(schemaBytes.toString('utf8'));
const defs = schema.$defs;
const names = new Set(Object.keys(defs));

function quote(value) { return JSON.stringify(value); }
function typeOf(rule) {
  if (!rule || typeof rule !== 'object') return 'never';
  if (typeof rule.$ref === 'string') return rule.$ref.startsWith('#/$defs/') ? `Harness${capitalize(rule.$ref.slice(8))}` : 'never';
  if (Array.isArray(rule.oneOf)) return union(rule.oneOf.map(typeOf));
  if (Array.isArray(rule.anyOf)) return union(rule.anyOf.map(typeOf));
  if ('const' in rule) return literal(rule.const);
  if (Array.isArray(rule.enum)) return union(rule.enum.map(literal));
  if (rule.type === 'null') return 'null';
  if (rule.type === 'string') return 'string';
  if (rule.type === 'integer') return 'number';
  if (rule.type === 'boolean') return 'boolean';
  if (rule.type === 'array') return `ReadonlyArray<${typeOf(rule.items)}>`;
  if (rule.type === 'object') return objectType(rule);
  return 'never';
}
function objectType(rule) {
  const properties = rule.properties && typeof rule.properties === 'object' ? rule.properties : {};
  const required = new Set(Array.isArray(rule.required) ? rule.required : []);
  const entries = Object.entries(properties).map(([key, value]) => `${quote(key)}${required.has(key) ? '' : '?'}: ${typeOf(value)}`);
  if (rule.additionalProperties !== false) entries.push('[key: string]: JsonValue');
  return entries.length ? `{ ${entries.join('; ')} }` : (rule.additionalProperties === false ? 'EmptyObject' : 'JsonObject');
}
function union(values) {
  const unique = [...new Set(values)];
  return unique.length === 1 ? unique[0] : unique.join(' | ');
}
function literal(value) {
  if (typeof value === 'string') return quote(value);
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  if (value === null) return 'null';
  return 'never';
}
function capitalize(value) { return value[0].toUpperCase() + value.slice(1); }

const lines = [
  '// Generated from api/harness-v1.schema.json. Do not edit by hand.',
  'export type JsonPrimitive = string | number | boolean | null;',
  'export type JsonValue = JsonPrimitive | JsonObject | ReadonlyArray<JsonValue>;',
  'export type JsonObject = { readonly [key: string]: JsonValue };',
  'export type EmptyObject = { readonly [key: string]: never };',
  `export const HARNESS_SCHEMA_SHA256 = ${quote(schemaSHA256)} as const;`,
  '',
];
for (const name of names) lines.push(`export type Harness${capitalize(name)} = ${typeOf(defs[name])};`);
lines.push('', `export type HarnessWireType = ${[...names].map(quote).join(' | ')};`);
lines.push('export interface HarnessWireMap {');
for (const name of names) lines.push(`  ${quote(name)}: Harness${capitalize(name)};`);
lines.push('}', 'export type HarnessWireValue = HarnessWireMap[HarnessWireType];', '');
const output = lines.join('\n');
if (check) {
  const existing = await readFile(outputPath, 'utf8').catch(() => '');
  if (existing !== output) { console.error('harness_types_stale'); process.exit(1); }
} else await writeFile(outputPath, output);
