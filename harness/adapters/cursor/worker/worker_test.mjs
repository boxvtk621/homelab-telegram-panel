import assert from 'node:assert/strict';
import test from 'node:test';
import { agentOptions, createRuntime, safeUsage, SDK_VERSION } from './worker.mjs';

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function fakeSDK(terminal) {
  const calls = [];
  const run = {
    id: 'run-1',
    async wait() { return terminal.promise; },
    async *stream() {
      yield { type: 'tool_call', call_id: 'native-call', name: 'fixture', status: 'running', raw: 'excluded' };
      yield { type: 'tool_call', call_id: 'native-call', name: 'fixture', status: 'completed', result: 'excluded' };
    },
    async steer(text) { calls.push(['steer', text]); return 'complete_delivered'; },
    async cancel() { calls.push(['cancel']); },
  };
  const agent = { agentId: 'agent-1', async send(text) { calls.push(['send', text]); return run; }, close() { calls.push(['close']); } };
  return {
    calls,
    sdk: {
      JsonlLocalAgentStore: class { constructor(location) { this.location = location; } },
      Agent: {
        async create(options) { calls.push(['create', options]); return agent; },
        async resume(id, options) { calls.push(['resume', id, options]); return agent; },
      },
    },
  };
}

test('deny options expose no tools or inherited settings', () => {
  const options = agentOptions({ apiKey: 'key', model: 'model', stateDir: '/state' }, { store: true });
  assert.deepEqual(options.tools, []);
  assert.deepEqual(options.mcpServers, {});
  assert.deepEqual(options.agents, {});
  assert.deepEqual(options.local.settingSources, []);
  assert.deepEqual(options.local.customTools, {});
  assert.equal(options.local.enableAgentRetries, false);
});

test('dispatch acknowledges before terminal and controls remain concurrent', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal);
  const output = [];
  const runtime = createRuntime(sdk, (line) => output.push(JSON.parse(line)));
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-test', maxFrameBytes: 65536 } });
  assert.equal(output[0].result.version, SDK_VERSION);
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'hello', resumeAgentId: '' } });
  assert.deepEqual(output[1], { type: 'response', id: '2', ok: true, result: { agentId: 'agent-1', runId: 'run-1' } });
  assert.equal(output.some((entry) => entry.event === 'terminal'), false);
  await runtime.handle({ type: 'request', id: '3', operation: 'steer', payload: { attemptKey: 'attempt', runId: 'run-1', text: 'more' } });
  await runtime.handle({ type: 'request', id: '4', operation: 'cancel', payload: { attemptKey: 'attempt', runId: 'run-1' } });
  assert.deepEqual(calls.slice(-2), [['steer', 'more'], ['cancel']]);
  terminal.resolve({ status: 'cancelled', usage: { inputTokens: 2, outputTokens: 1, totalTokens: 3 }, result: 'excluded-on-cancel' });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(output.at(-1).status, 'cancelled');
  assert.equal(output.at(-1).text, 'excluded-on-cancel');
  assert.deepEqual(output.filter((entry) => entry.event === 'tool').map((entry) => entry.status), ['running', 'completed']);
});

test('resume uses the exact private agent id', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal);
  const runtime = createRuntime(sdk, () => {});
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-resume-test', maxFrameBytes: 65536 } });
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'next', resumeAgentId: 'agent-1' } });
  assert.equal(calls[0][0], 'resume');
  assert.equal(calls[0][1], 'agent-1');
  terminal.resolve({ status: 'finished', result: 'done' });
});

test('usage excludes invalid provider fields', () => {
  assert.deepEqual(safeUsage({ inputTokens: 2, outputTokens: 1, totalTokens: 3, cost: 99 }), { inputTokens: 2, outputTokens: 1, totalTokens: 3 });
  assert.equal(safeUsage({ inputTokens: -1, raw: 'secret' }), undefined);
});
