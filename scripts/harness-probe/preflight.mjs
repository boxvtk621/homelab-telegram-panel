// Package inspection only. No Agent.create/send/resume or provider connection.
import fs from 'node:fs';
import { Agent } from '@cursor/sdk';
const expected = { '@cursor/sdk': '1.0.31', '@openai/codex': '0.153.4' };
for (const [name, version] of Object.entries(expected)) {
  const info = JSON.parse(fs.readFileSync(new URL(`node_modules/${name}/package.json`, import.meta.url)));
  if (info.version !== version) throw new Error('package_version_mismatch');
}
if (typeof Agent.create !== 'function' || typeof Agent.resume !== 'function') throw new Error('cursor_api_missing');
console.log(JSON.stringify({packages: expected, node: process.version, cursorImport: 'passed', providerCalls: 0}));
