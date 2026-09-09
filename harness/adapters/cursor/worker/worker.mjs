#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline';
import { fileURLToPath } from 'node:url';

export const SDK_VERSION = '1.0.31';
export const DEFAULT_MAX_FRAME_BYTES = 1024 * 1024;

class WorkerError extends Error {
  constructor(code) {
    super(code);
    this.code = code;
  }
}

function boundedString(value, maximum, allowEmpty = false) {
  return typeof value === 'string' && (allowEmpty || value.length > 0) && Buffer.byteLength(value, 'utf8') <= maximum && !value.includes('\0');
}

export function safeUsage(value) {
  if (!value || typeof value !== 'object') return undefined;
  const read = (name) => Number.isSafeInteger(value[name]) && value[name] >= 0 ? value[name] : 0;
  const usage = { inputTokens: read('inputTokens'), outputTokens: read('outputTokens'), totalTokens: read('totalTokens') };
  return Object.values(usage).some((entry) => entry !== 0) ? usage : undefined;
}

export function agentOptions(config, store) {
  return {
    apiKey: config.apiKey,
    model: { id: config.model },
    tools: [],
    mcpServers: {},
    agents: {},
    local: {
      cwd: config.stateDir,
      store,
      settingSources: [],
      customTools: {},
      enableAgentRetries: false,
    },
  };
}

export function createRuntime(sdk, emit) {
  const active = new Map();
  let config;
  let store;

  function send(frame) {
    const encoded = JSON.stringify(frame);
    if (Buffer.byteLength(encoded, 'utf8') > (config?.maxFrameBytes || DEFAULT_MAX_FRAME_BYTES)) throw new WorkerError('frame_too_large');
    emit(encoded + '\n');
  }

  function response(id, result = {}) {
    send({ type: 'response', id, ok: true, result });
  }

  function rejected(id, code = 'rejected') {
    send({ type: 'response', id, ok: false, code });
  }

  async function consumeTools(attemptKey, run) {
    try {
      for await (const message of run.stream()) {
        if (message?.type !== 'tool_call') continue;
        const callId = typeof message.call_id === 'string' ? message.call_id : typeof message.callId === 'string' ? message.callId : '';
        const name = typeof message.name === 'string' ? message.name : '';
        const status = ['running', 'completed', 'error'].includes(message.status) ? message.status : '';
        if (boundedString(callId, 512) && boundedString(name, 200) && status) {
          send({ type: 'event', attemptKey, event: 'tool', callId, name, status });
        }
      }
    } catch {
      // run.wait remains authoritative for terminal state. Stream failure is
      // represented by missing optional lifecycle events, never invented data.
    }
  }

  async function pump(attemptKey, agent, run) {
    const tools = consumeTools(attemptKey, run);
    let result;
    try {
      result = await run.wait();
      await tools;
      const status = ['finished', 'error', 'cancelled'].includes(result?.status) ? result.status : 'unknown';
      const text = typeof result?.result === 'string' ? result.result : '';
      send({ type: 'event', attemptKey, event: 'terminal', status, text, usage: safeUsage(result?.usage) });
    } catch {
      send({ type: 'event', attemptKey, event: 'terminal', status: 'unknown' });
    } finally {
      active.delete(attemptKey);
      try { agent.close(); } catch {}
    }
  }

  async function initialize(id, payload) {
    if (config || !payload || !boundedString(payload.apiKey, 4096) || !boundedString(payload.model, 200) ||
        !boundedString(payload.stateDir, 4096) || !Number.isInteger(payload.maxFrameBytes) ||
        payload.maxFrameBytes < 4096 || payload.maxFrameBytes > 8 * 1024 * 1024) {
      rejected(id);
      return;
    }
    fs.mkdirSync(payload.stateDir, { recursive: true, mode: 0o700 });
    config = { ...payload };
    store = new sdk.JsonlLocalAgentStore(path.join(config.stateDir, 'sdk-store'));
    response(id, { version: SDK_VERSION });
  }

  async function dispatch(id, payload) {
    if (!config || !payload || !boundedString(payload.attemptKey, 4096) || !boundedString(payload.prompt, 64 * 1024) ||
        !(payload.resumeAgentId === '' || boundedString(payload.resumeAgentId, 512)) || active.has(payload.attemptKey)) {
      rejected(id);
      return;
    }
    const options = agentOptions(config, store);
    const agent = payload.resumeAgentId ? await sdk.Agent.resume(payload.resumeAgentId, options) : await sdk.Agent.create(options);
    const run = await agent.send(payload.prompt, { model: { id: config.model }, onStep: () => {}, onDelta: () => {} });
    if (!boundedString(agent?.agentId, 512) || !boundedString(run?.id, 512)) throw new WorkerError('native_identity_invalid');
    active.set(payload.attemptKey, { agent, run });
    response(id, { agentId: agent.agentId, runId: run.id });
    void pump(payload.attemptKey, agent, run);
  }

  async function steer(id, payload) {
    const current = active.get(payload?.attemptKey);
    if (!current || current.run.id !== payload.runId || !boundedString(payload.text, 64 * 1024) || typeof current.run.steer !== 'function') {
      rejected(id);
      return;
    }
    const status = await current.run.steer(payload.text);
    response(id, { status: typeof status === 'string' && status.length <= 64 ? status : 'unknown' });
  }

  async function cancel(id, payload) {
    const current = active.get(payload?.attemptKey);
    if (!current || current.run.id !== payload.runId || typeof current.run.cancel !== 'function') {
      rejected(id);
      return;
    }
    await current.run.cancel();
    response(id);
  }

  async function handle(frame) {
    const id = frame?.id;
    if (frame?.type !== 'request' || !boundedString(id, 128) || !boundedString(frame.operation, 64)) return;
    try {
      if (frame.operation === 'init') await initialize(id, frame.payload);
      else if (frame.operation === 'dispatch') await dispatch(id, frame.payload);
      else if (frame.operation === 'steer') await steer(id, frame.payload);
      else if (frame.operation === 'cancel') await cancel(id, frame.payload);
      else rejected(id);
    } catch {
      rejected(id, 'unknown');
    }
  }

  return { handle };
}

async function main() {
  const sdk = await import('@cursor/sdk');
  const runtime = createRuntime(sdk, (value) => process.stdout.write(value));
  const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  for await (const line of lines) {
    if (Buffer.byteLength(line, 'utf8') > DEFAULT_MAX_FRAME_BYTES) process.exit(2);
    let frame;
    try { frame = JSON.parse(line); } catch { process.exit(2); }
    void runtime.handle(frame);
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) await main();
