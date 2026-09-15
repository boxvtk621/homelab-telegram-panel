#!/usr/bin/env node

import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '..',
  '..',
  '..',
);
const schemaPath = path.join(root, 'api', 'transcript-view-v1.schema.json');
const outputPath = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  '..',
  'src',
  'transcript-view-types.ts',
);
const check = process.argv.includes('--check');
const schemaBytes = await readFile(schemaPath);
const schemaSHA256 = createHash('sha256').update(schemaBytes).digest('hex');
const schema = JSON.parse(schemaBytes.toString('utf8'));
const defs = schema.$defs;

function quote(value) {
  return JSON.stringify(value);
}
function capitalize(value) {
  return value[0].toUpperCase() + value.slice(1);
}
function union(values) {
  const unique = [...new Set(values)];
  return unique.length === 1 ? unique[0] : unique.join(' | ');
}
function literal(value) {
  if (typeof value === 'string') return quote(value);
  if (typeof value === 'number' || typeof value === 'boolean')
    return String(value);
  if (value === null) return 'null';
  return 'never';
}
function objectType(rule) {
  const properties =
    rule.properties && typeof rule.properties === 'object'
      ? rule.properties
      : {};
  const required = new Set(Array.isArray(rule.required) ? rule.required : []);
  const entries = Object.entries(properties).map(
    ([key, value]) =>
      `${quote(key)}${required.has(key) ? '' : '?'}: ${typeOf(value)}`,
  );
  if (rule.additionalProperties !== false)
    entries.push('[key: string]: TranscriptJsonValue');
  return entries.length
    ? `{ ${entries.join('; ')} }`
    : rule.additionalProperties === false
      ? 'TranscriptEmptyObject'
      : 'TranscriptJsonObject';
}
function typeOf(rule) {
  if (!rule || typeof rule !== 'object') return 'never';
  if (typeof rule.$ref === 'string')
    return rule.$ref.startsWith('#/$defs/')
      ? `Transcript${capitalize(rule.$ref.slice(8))}`
      : 'never';
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

const lines = [
  '// Generated from api/transcript-view-v1.schema.json. Do not edit by hand.',
  'export type TranscriptJsonPrimitive = string | number | boolean | null;',
  'export type TranscriptJsonValue = TranscriptJsonPrimitive | TranscriptJsonObject | ReadonlyArray<TranscriptJsonValue>;',
  'export type TranscriptJsonObject = { readonly [key: string]: TranscriptJsonValue };',
  'export type TranscriptEmptyObject = { readonly [key: string]: never };',
  `export const TRANSCRIPT_VIEW_SCHEMA_SHA256 = ${quote(schemaSHA256)} as const;`,
  '',
];
for (const [name, definition] of Object.entries(defs))
  lines.push(
    `export type Transcript${capitalize(name)} = ${typeOf(definition)};`,
  );
lines.push('');
const output = lines.join('\n');
if (check) {
  const existing = await readFile(outputPath, 'utf8').catch(() => '');
  if (existing !== output) {
    console.error('transcript_types_stale');
    process.exit(1);
  }
} else {
  await writeFile(outputPath, output);
}
