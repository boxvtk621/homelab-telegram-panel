import assert from 'node:assert/strict';
import test from 'node:test';
import { agentOptions, createCustomTools, createRuntime, installedSDKVersion, safeUsage, MAX_PENDING_EXECUTIONS, SDK_VERSION } from './worker.mjs';

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function fakeSDK(terminal, streamMessages = []) {
  const calls = [];
  const run = {
    id: 'run-1',
    async wait() { return terminal.promise; },
    async *stream() {
      for (const message of streamMessages) yield message;
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

test('chat alpha retains the native prompt and exposes no tools or inherited settings', () => {
  const options = agentOptions({ apiKey: 'key', model: 'model', stateDir: '/state' }, { store: true }, 'selected policy');
  assert.deepEqual(options, {
    apiKey: 'key',
    model: { id: 'model' },
    tools: [],
    disallowedTools: ['shell', 'task'],
    mcpServers: {},
    agents: {},
    local: {
      cwd: 'selected policy', store: { store: true }, settingSources: [], customTools: {}, enableAgentRetries: false,
    },
  });
  assert.equal(Object.hasOwn(options, 'systemPrompt'), false);
});

test('custom tools expose only the MCP family and forward the exact call identity', async () => {
  const calls = [];
  const tools = createCustomTools('attempt-1', async (request) => {
    calls.push(request);
    return { success: true, output: 'ok' };
  });
  const options = agentOptions({ apiKey: 'key', model: 'model', stateDir: '/state' }, { store: true }, '/workspace/dialog', tools);
  assert.deepEqual(options, {
    apiKey: 'key',
    model: { id: 'model' },
    tools: ['mcp'],
    disallowedTools: ['shell', 'task'],
    mcpServers: {},
    agents: {},
    local: {
      cwd: '/workspace/dialog', store: { store: true }, settingSources: [], customTools: tools, enableAgentRetries: false,
    },
  });
  assert.deepEqual(Object.keys(options.local.customTools).sort(), ['cursor_command', 'cursor_file_change']);
  const result = await tools.cursor_command.execute(
    { command: 'pwd', cwd: '.', workspaceAccess: 'read' },
    { toolCallId: 'native-call' },
  );
  assert.deepEqual(result, { success: true, output: 'ok' });
  assert.deepEqual(calls, [{
    attemptKey: 'attempt-1', callId: 'native-call', name: 'cursor_command',
    args: { command: 'pwd', cwd: '.', workspaceAccess: 'read' },
  }]);
  await assert.rejects(() => tools.cursor_command.execute({}, {}), /tool_request_invalid/);
});

test('installed SDK matches the locked native version', () => {
  assert.equal(installedSDKVersion(), SDK_VERSION);
});

test('dispatch acknowledges before terminal and controls remain concurrent', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal);
  const output = [];
  const runtime = createRuntime(sdk, (line) => output.push(JSON.parse(line)));
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-test', maxFrameBytes: 65536 } });
  assert.equal(output[0].result.version, SDK_VERSION);
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'hello', policyContent: 'start policy', workspace: '/workspace/dialog', approvalMode: 'deny', resumeAgentId: '' } });
  assert.deepEqual(output[1], { type: 'response', id: '2', ok: true, result: { agentId: 'agent-1', runId: 'run-1' } });
  assert.equal(output.some((entry) => entry.event === 'terminal'), false);
  assert.equal(Object.hasOwn(calls[0][1], 'systemPrompt'), false);
  assert.equal(calls[0][1].local.cwd, '/workspace/dialog');
  assert.deepEqual(calls[0][1].local.customTools, {});
  assert.deepEqual(calls[1], ['send', 'Chat guidance (user-level):\nstart policy\n\nUser message:\nhello']);
  await runtime.handle({ type: 'request', id: '3', operation: 'steer', payload: { attemptKey: 'attempt', runId: 'run-1', text: 'more' } });
  await runtime.handle({ type: 'request', id: '4', operation: 'cancel', payload: { attemptKey: 'attempt', runId: 'run-1' } });
  assert.deepEqual(calls.slice(-2), [['steer', 'more'], ['cancel']]);
  terminal.resolve({ status: 'cancelled', usage: { inputTokens: 2, outputTokens: 1, totalTokens: 3 }, result: 'excluded-on-cancel' });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(output.at(-1).status, 'cancelled');
  assert.equal(output.at(-1).text, 'excluded-on-cancel');
  assert.deepEqual(output.filter((entry) => entry.event === 'tool'), []);
});

test('resume reapplies the exact private agent id and fail-closed tool boundary', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal);
  const runtime = createRuntime(sdk, () => {});
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-resume-test', maxFrameBytes: 65536 } });
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'next', policyContent: 'resume policy', workspace: '/workspace/dialog', approvalMode: 'explicit_once', resumeAgentId: 'agent-1' } });
  assert.equal(calls[0][0], 'resume');
  assert.equal(calls[0][1], 'agent-1');
  const resumedOptions = calls[0][2];
  assert.equal(Object.hasOwn(resumedOptions, 'systemPrompt'), false);
  assert.deepEqual({
    ...resumedOptions,
    local: { ...resumedOptions.local, customTools: Object.keys(resumedOptions.local.customTools).sort() },
  }, {
    apiKey: 'key',
    model: { id: 'model' },
    tools: ['mcp'],
    disallowedTools: ['shell', 'task'],
    mcpServers: {},
    agents: {},
    local: {
      cwd: '/workspace/dialog',
      store: resumedOptions.local.store,
      settingSources: [],
      customTools: ['cursor_command', 'cursor_file_change'],
      enableAgentRetries: false,
    },
  });
  assert.deepEqual(calls[1], ['send', 'Chat guidance (user-level):\nresume policy\n\nUser message:\nnext']);
  terminal.resolve({ status: 'finished', result: 'done' });
});

test('custom tool waits for the harness response on the bidirectional bridge', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal, [
    { type: 'tool_call', call_id: 'native-call', name: 'mcp', status: 'running', args: { providerIdentifier: 'custom-user-tools', toolName: 'cursor_file_change' } },
    { type: 'tool_call', call_id: 'native-call', name: 'mcp', status: 'completed', args: { providerIdentifier: 'custom-user-tools', toolName: 'cursor_file_change' }, result: 'excluded' },
  ]);
  const output = [];
  const runtime = createRuntime(sdk, (line) => output.push(JSON.parse(line)));
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-tool-test', maxFrameBytes: 65536 } });
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'tool', policyContent: 'tool policy', workspace: '/workspace/dialog', approvalMode: 'explicit_once', resumeAgentId: '' } });
  const tool = calls[0][1].local.customTools.cursor_file_change;
  const execution = tool.execute({ changes: [{ path: 'note.txt', operation: 'write', content: 'hello' }] }, { toolCallId: 'native-call' });
  const request = output.at(-1);
  assert.equal(request.type, 'request');
  assert.equal(request.operation, 'execute_tool');
  assert.equal(request.payload.callId, 'native-call');
  await runtime.handle({ type: 'response', id: request.id, ok: true, result: { success: true, output: 'changed' } });
  assert.deepEqual(await execution, { success: true, output: 'changed' });
  terminal.resolve({ status: 'finished', result: 'done' });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(output.filter((entry) => entry.event === 'tool'), []);
});

test('unexpected tool stream event fails the run closed', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal, [
    { type: 'tool_call', call_id: 'unexpected', name: 'mcp', status: 'running', args: { providerIdentifier: 'other-server', toolName: 'other_tool' } },
  ]);
  const output = [];
  const runtime = createRuntime(sdk, (line) => output.push(JSON.parse(line)));
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-unexpected-tool-test', maxFrameBytes: 65536 } });
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'tool', policyContent: 'tool policy', workspace: '/workspace/dialog', approvalMode: 'explicit_once', resumeAgentId: '' } });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(output.at(-1).event, 'terminal');
  assert.equal(output.at(-1).status, 'unknown');
  assert.equal(calls.some((entry) => entry[0] === 'cancel'), true);
});

test('pending custom executions are capped and cleared on worker close', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal);
  const output = [];
  const runtime = createRuntime(sdk, (line) => output.push(JSON.parse(line)));
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-cap-test', maxFrameBytes: 65536 } });
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'tool', policyContent: 'tool policy', workspace: '/workspace/dialog', approvalMode: 'explicit_once', resumeAgentId: '' } });
  const tool = calls[0][1].local.customTools.cursor_command;
  const pending = [];
  for (let index = 0; index < MAX_PENDING_EXECUTIONS; index += 1) {
    pending.push(tool.execute({ command: 'pwd', cwd: '.', workspaceAccess: 'read' }, { toolCallId: `call-${index}` }).catch((error) => error));
  }
  await assert.rejects(
    () => tool.execute({ command: 'pwd', cwd: '.', workspaceAccess: 'read' }, { toolCallId: 'overflow' }),
    /tool_execution_capacity/,
  );
  assert.equal(output.filter((entry) => entry.operation === 'execute_tool').length, MAX_PENDING_EXECUTIONS);
  runtime.close();
  const results = await Promise.all(pending);
  assert.equal(results.every((error) => error.message === 'tool_worker_closed'), true);
});

test('terminal state rejects pending custom executions for its attempt', async () => {
  const terminal = deferred();
  const { sdk, calls } = fakeSDK(terminal);
  const runtime = createRuntime(sdk, () => {});
  await runtime.handle({ type: 'request', id: '1', operation: 'init', payload: { apiKey: 'key', model: 'model', stateDir: '/tmp/cursor-worker-terminal-test', maxFrameBytes: 65536 } });
  await runtime.handle({ type: 'request', id: '2', operation: 'dispatch', payload: { attemptKey: 'attempt', prompt: 'tool', policyContent: 'tool policy', workspace: '/workspace/dialog', approvalMode: 'explicit_once', resumeAgentId: '' } });
	const tool = calls[0][1].local.customTools.cursor_command;
  const execution = tool.execute(
    { command: 'pwd', cwd: '.', workspaceAccess: 'read' },
    { toolCallId: 'native-call' },
  );
  terminal.resolve({ status: 'finished', result: 'done' });
  await assert.rejects(() => execution, /tool_attempt_closed/);
  await new Promise((resolve) => setImmediate(resolve));
  await assert.rejects(
    () => tool.execute({ command: 'pwd', cwd: '.', workspaceAccess: 'read' }, { toolCallId: 'late-call' }),
    /tool_attempt_closed/,
  );
});

test('usage excludes invalid provider fields', () => {
  assert.deepEqual(safeUsage({ inputTokens: 2, outputTokens: 1, totalTokens: 3, cost: 99 }), { inputTokens: 2, outputTokens: 1, totalTokens: 3 });
  assert.equal(safeUsage({ inputTokens: -1, raw: 'secret' }), undefined);
});
