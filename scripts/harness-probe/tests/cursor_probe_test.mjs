import assert from 'node:assert/strict';
import test from 'node:test';
import {
  MAX_SDK_SENDS,
  ProbeError,
  applyEvent,
  createCustomTools,
  createInitialState,
  fixtureEvents,
  makeAgentOptions,
  normalizeApiKey,
  phaseCompletionStatus,
  reserveSend,
  sanitizeUsage,
} from '../cursor_probe.mjs';

test('generic SDK mcp events require matching callback call ID and scope', () => {
  const state = createInitialState();
  for (const event of [
    { type: 'tool_completed', name: 'marker_echo', scopeId: 'scope-1', callId: 'call-1' },
    { type: 'tool_call', name: 'mcp', status: 'running', scopeId: 'scope-1', callId: 'call-1' },
    { type: 'tool_call', name: 'mcp', status: 'completed', scopeId: 'scope-1', callId: 'call-1' },
    { type: 'tool_call', name: 'mcp', status: 'completed', scopeId: 'scope-1', callId: 'unrelated' },
    { type: 'tool_call', name: 'mcp', status: 'completed', scopeId: 'scope-2', callId: 'call-1' },
  ]) applyEvent(state, event);
  assert.deepEqual(fixtureEvents(state, 'scope-1', 'marker_echo').map((event) => event.seq), [2, 3]);
  assert.deepEqual(fixtureEvents(state, 'scope-1', 'bounded_long_wait'), []);
});

test('agent options preserve the deterministic boundary for create and resume', () => {
  const options = makeAgentOptions('test-key', { test: true }, createCustomTools(() => {}));
  assert.deepEqual(options.model, { id: 'composer-2.5' });
  assert.deepEqual(options.tools, ['mcp']);
  assert.deepEqual(options.mcpServers, {});
  assert.deepEqual(options.agents, {});
  assert.deepEqual(options.local.settingSources, []);
  assert.equal(options.local.enableAgentRetries, false);
  assert.deepEqual(Object.keys(options.local.customTools), ['marker_echo', 'bounded_long_wait', 'deterministic_error']);
});

test('send budget is cumulative and fails closed at eight', () => {
  const state = createInitialState();
  for (let index = 0; index < MAX_SDK_SENDS; index += 1) reserveSend(state);
  assert.equal(state.sdkSendCount, MAX_SDK_SENDS);
  assert.throws(() => reserveSend(state), (error) => error instanceof ProbeError && error.code === 'sdk_send_budget_exhausted');
});

test('safe mapping excludes raw results and unknown usage', () => {
  const state = createInitialState();
  applyEvent(state, { type: 'tool_completed', name: 'marker_echo', result: 'secret', headers: 'secret' });
  assert.deepEqual(state.events, [{ type: 'tool_completed', name: 'marker_echo', seq: 1 }]);
  assert.deepEqual(sanitizeUsage({ inputTokens: 10.9, outputTokens: 4, dollarCost: 1 }), { inputTokens: 10, outputTokens: 4 });
  assert.equal(sanitizeUsage({ raw: 'secret' }), undefined);
});

test('unknown or failed mandatory capability cannot become a completed phase', () => {
  const state = createInitialState();
  assert.equal(phaseCompletionStatus('create_markers', state), 'unknown');
  state.capabilities.markerDialogs.status = 'failed';
  assert.equal(phaseCompletionStatus('create_markers', state), 'failed');
  state.capabilities.markerDialogs.status = 'observed';
  assert.equal(phaseCompletionStatus('create_markers', state), 'completed');
  state.capabilities.toolLifecycle.status = 'observed';
  state.capabilities.steerWhileLongTool.status = 'unknown';
  state.capabilities.cancelTerminal.status = 'observed';
  assert.equal(phaseCompletionStatus('tool_steer_cancel', state), 'unknown');
});

test('stdin key parser accepts one bounded line without exposing it', () => {
  assert.equal(normalizeApiKey('test-key\n'), 'test-key');
  assert.throws(() => normalizeApiKey('a\nb'), (error) => error instanceof ProbeError && error.code === 'stdin_key_invalid');
  assert.throws(() => normalizeApiKey('x'.repeat(4097)), (error) => error instanceof ProbeError && error.code === 'stdin_key_invalid');
});
