#!/usr/bin/env node
// Bounded Cursor SDK feasibility probe. Not a Harness runtime or adapter.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const MODEL = { id: 'composer-2.5' };
export const MAX_SDK_SENDS = 8;
export const PHASE_DEADLINE_MS = 180_000;
export const DEFAULT_STATE_PATH = '/state/cursor-probe/checkpoint.json';
const STORE_PATH = '/state/cursor-probe/store';
const CAPABILITIES = ['markerDialogs', 'toolLifecycle', 'steerWhileLongTool', 'cancelTerminal', 'restartResume', 'denyAfterResume'];
const SAFE_ADVERTISED_TOOLS = new Set(['mcp', 'shell', 'read', 'edit', 'grep', 'glob', 'ls', 'task', 'webSearch', 'webFetch', 'delete']);
const PHASE_REQUIREMENTS = {
  create_markers: ['markerDialogs'],
  tool_steer_cancel: ['toolLifecycle', 'steerWhileLongTool', 'cancelTerminal'],
  resume_deny: ['restartResume', 'denyAfterResume'],
};

export class ProbeError extends Error {
  constructor(code) {
    super(code);
    this.code = code;
  }
}

export function createInitialState() {
  return {
    schemaVersion: 1,
    sdkSendCount: 0,
    agentIds: {},
    nextEventSeq: 0,
    nextScope: 0,
    events: [],
    usage: [],
    capabilities: Object.fromEntries(CAPABILITIES.map((name) => [name, { status: 'not_run', assertions: {}, evidence: {} }])),
  };
}

export function normalizeApiKey(value) {
  if (typeof value !== 'string' || value.length === 0 || value.length > 4096) throw new ProbeError('stdin_key_invalid');
  const lines = value.split(/\r?\n/);
  if (!lines[0] || lines.length > 2 || (lines.length === 2 && lines[1] !== '')) throw new ProbeError('stdin_key_invalid');
  return lines[0];
}

export function readApiKey() {
  const chunks = [];
  let size = 0;
  while (true) {
    const bytes = Buffer.alloc(512);
    const count = fs.readSync(0, bytes, 0, bytes.length, null);
    if (!count) break;
    size += count;
    if (size > 4096) throw new ProbeError('stdin_key_too_large');
    chunks.push(bytes.subarray(0, count));
  }
  return normalizeApiKey(Buffer.concat(chunks).toString('utf8'));
}

export function reserveSend(state) {
  if (!Number.isInteger(state.sdkSendCount) || state.sdkSendCount < 0 || state.sdkSendCount >= MAX_SDK_SENDS) {
    throw new ProbeError('sdk_send_budget_exhausted');
  }
  state.sdkSendCount += 1;
}

export function sanitizeUsage(value) {
  if (!value || typeof value !== 'object') return undefined;
  const safe = {};
  for (const key of ['inputTokens', 'outputTokens', 'totalTokens', 'cachedTokens']) {
    if (Number.isFinite(value[key]) && value[key] >= 0) safe[key] = Math.floor(value[key]);
  }
  return Object.keys(safe).length ? safe : undefined;
}

export function applyEvent(state, event) {
  const safe = {};
  for (const key of ['type', 'name', 'status', 'callId', 'correlationId', 'scopeId', 'runId']) {
    if (typeof event[key] === 'string' && event[key].length <= 128) safe[key] = event[key];
  }
  if (Array.isArray(event.advertisedTools)) safe.advertisedTools = event.advertisedTools.filter((name) => SAFE_ADVERTISED_TOOLS.has(name)).slice(0, 16);
  if (typeof event.markerMatches === 'boolean') safe.markerMatches = event.markerMatches;
  if (Object.keys(safe).length) {
    state.nextEventSeq = Number.isInteger(state.nextEventSeq) ? state.nextEventSeq + 1 : 1;
    safe.seq = state.nextEventSeq;
    state.events = state.events.concat([safe]).slice(-64);
  }
  return safe;
}

// Cursor 1.0.31 calls every custom-tool SDK event "mcp". Attribute it only
// through a matching deterministic callback ID, preserving the SDK timeline.
export function fixtureEvents(state, scopeId, name) {
  const callbackIds = new Set(state.events.filter((event) => event.scopeId === scopeId && event.name === name && ['tool_started', 'tool_completed'].includes(event.type)).map((event) => event.callId).filter(Boolean));
  return state.events.filter((event) => event.scopeId === scopeId && event.type === 'tool_call' && event.name === 'mcp' && callbackIds.has(event.callId));
}

export function makeAgentOptions(apiKey, store, customTools) {
  return {
    apiKey,
    model: MODEL,
    tools: ['mcp'],
    mcpServers: {},
    agents: {},
    local: {
      cwd: '/state/workspace',
      store,
      settingSources: [],
      customTools,
      enableAgentRetries: false,
    },
  };
}

function validCorrelation(value) {
  return typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value);
}

export function createCustomTools(record) {
  const base = {
    type: 'object',
    additionalProperties: false,
    properties: { correlationId: { type: 'string', pattern: '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' } },
    required: ['correlationId'],
  };
  return {
    marker_echo: {
      description: 'Returns a deterministic marker.',
      inputSchema: { ...base, properties: { ...base.properties, marker: { type: 'string', maxLength: 64 } }, required: ['correlationId', 'marker'] },
      async execute(args, context) {
        if (!validCorrelation(args.correlationId) || typeof args.marker !== 'string' || !args.marker || Buffer.byteLength(args.marker, 'utf8') > 64) throw new ProbeError('tool_input_invalid');
        record({ type: 'tool_completed', name: 'marker_echo', correlationId: args.correlationId, callId: context.toolCallId || '', marker: args.marker });
        return { structuredContent: { correlationId: args.correlationId, marker: args.marker } };
      },
    },
    bounded_long_wait: {
      description: 'Bounded wait without an external effect.',
      inputSchema: { ...base, properties: { ...base.properties, delayMs: { type: 'integer', minimum: 100, maximum: 5000 } }, required: ['correlationId', 'delayMs'] },
      async execute(args, context) {
        if (!validCorrelation(args.correlationId) || !Number.isInteger(args.delayMs) || args.delayMs < 100 || args.delayMs > 5000) throw new ProbeError('tool_input_invalid');
        const event = { name: 'bounded_long_wait', correlationId: args.correlationId, callId: context.toolCallId || '' };
        record({ ...event, type: 'tool_started' });
        await new Promise((resolve) => setTimeout(resolve, args.delayMs));
        record({ ...event, type: 'tool_completed' });
        return { structuredContent: { correlationId: args.correlationId, delayMs: args.delayMs } };
      },
    },
    deterministic_error: {
      description: 'Returns a deterministic synthetic error.',
      inputSchema: base,
      async execute(args, context) {
        if (!validCorrelation(args.correlationId)) throw new ProbeError('tool_input_invalid');
        record({ type: 'tool_completed', name: 'deterministic_error', status: 'error', correlationId: args.correlationId, callId: context.toolCallId || '' });
        return { isError: true, structuredContent: { code: 'fixture_deterministic_error', correlationId: args.correlationId } };
      },
    },
  };
}

function loadState(statePath) {
  try {
    const parsed = JSON.parse(fs.readFileSync(statePath, 'utf8'));
    if (parsed?.schemaVersion !== 1 || !Number.isInteger(parsed.sdkSendCount) || !parsed.agentIds || !parsed.capabilities) throw new ProbeError('checkpoint_invalid');
    return { ...createInitialState(), ...parsed, events: Array.isArray(parsed.events) ? parsed.events.slice(-64) : [], usage: Array.isArray(parsed.usage) ? parsed.usage.slice(-64) : [] };
  } catch (error) {
    if (error?.code === 'ENOENT') return createInitialState();
    if (error instanceof ProbeError) throw error;
    throw new ProbeError('checkpoint_invalid');
  }
}

function saveState(statePath, state) {
  fs.mkdirSync(path.dirname(statePath), { recursive: true, mode: 0o700 });
  const temporary = statePath + '.tmp';
  fs.writeFileSync(temporary, JSON.stringify(state), { encoding: 'utf8', mode: 0o600 });
  fs.renameSync(temporary, statePath);
}

function setCapability(state, name, status, assertions, evidence = {}) {
  state.capabilities[name] = { status, assertions, evidence };
}

function safeRun(value) {
  const usage = sanitizeUsage(value?.usage);
  return { status: ['finished', 'error', 'cancelled'].includes(value?.status) ? value.status : 'unknown', ...(usage ? { usage } : {}) };
}

async function before(until, promise) {
  const remaining = until - Date.now();
  if (remaining <= 0) throw new ProbeError('phase_deadline_exceeded');
  let timer;
  try {
    return await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new ProbeError('phase_deadline_exceeded')), remaining); })]);
  } finally {
    clearTimeout(timer);
  }
}

async function waitForLongStart(state, scopeId, until) {
  while (Date.now() < until) {
    if (state.events.some((event) => event.scopeId === scopeId && event.type === 'tool_started' && event.name === 'bounded_long_wait')) return true;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  return false;
}

async function consumeStream(run, state, scopeId) {
  try {
    for await (const message of run.stream()) {
      if (Array.isArray(message?.tools)) {
        const advertisedTools = message.tools.filter((name) => SAFE_ADVERTISED_TOOLS.has(name));
        applyEvent(state, { type: 'system_tools', scopeId, runId: run.id || '', advertisedTools });
      }
      if (message?.type === 'tool_call') {
        const callId = typeof message.call_id === 'string' ? message.call_id : typeof message.callId === 'string' ? message.callId : '';
        const name = typeof message.name === 'string' && /^[a-zA-Z0-9_]{1,64}$/.test(message.name) ? message.name : '';
        const status = ['running', 'completed', 'error'].includes(message.status) ? message.status : '';
        applyEvent(state, { type: 'tool_call', scopeId, runId: run.id || '', callId, name, status });
      }
    }
  } catch {
    applyEvent(state, { type: 'stream_incomplete', scopeId, runId: run.id || '' });
  }
}

async function send(agent, text, state, statePath, until) {
  state.nextScope = Number.isInteger(state.nextScope) ? state.nextScope + 1 : 1;
  const scopeId = 'scope-' + state.nextScope;
  state.activeScopeId = scopeId;
  reserveSend(state);
  saveState(statePath, state);
  const run = await before(until, agent.send(text, {
    model: MODEL,
    onStep: () => {},
    onDelta: () => {},
  }));
  return { run, scopeId, stream: consumeStream(run, state, scopeId) };
}

async function wait(runInfo, state, until) {
  const result = await before(until, runInfo.run.wait());
  await before(until, runInfo.stream);
  const safe = safeRun(result);
  if (safe.usage) state.usage = state.usage.concat([safe.usage]).slice(-64);
  return { result, safe };
}

async function configuredAgent(sdk, apiKey, state, agentId, until) {
  const tools = createCustomTools((event) => {
    const { marker, ...safeEvent } = event;
    applyEvent(state, { ...safeEvent, scopeId: state.activeScopeId || '', ...(typeof marker === 'string' ? { markerMatches: marker === state.privateMarker } : {}) });
  });
  const options = makeAgentOptions(apiKey, new sdk.JsonlLocalAgentStore(STORE_PATH), tools);
  return agentId ? before(until, sdk.Agent.resume(agentId, options)) : before(until, sdk.Agent.create(options));
}

async function createMarkers(sdk, apiKey, state, statePath, until) {
  const first = await configuredAgent(sdk, apiKey, state, undefined, until);
  const second = await configuredAgent(sdk, apiKey, state, undefined, until);
  state.agentIds.first = first.agentId;
  state.agentIds.second = second.agentId;
  saveState(statePath, state);
  const marker = 'marker-A-246';
  state.privateMarker = marker;
  const firstRun = await send(first, 'Use marker_echo with correlationId create-a and marker ' + marker + '; state the returned marker.', state, statePath, until);
  const firstResult = await wait(firstRun, state, until);
  const secondRun = await send(second, 'A separate dialog has a private marker. State its value without calling a tool.', state, statePath, until);
  const secondResult = await wait(secondRun, state, until);
  const firstToolObserved = fixtureEvents(state, firstRun.scopeId, 'marker_echo').some((event) => event.status === 'completed');
  const firstReplyMatches = typeof firstResult.result?.result === 'string' && firstResult.result.result.includes(marker);
  const secondReplyLeaks = typeof secondResult.result?.result === 'string' && secondResult.result.result.includes(marker);
  const distinctAgentIds = first.agentId !== second.agentId;
  const firstTerminalFinished = firstResult.safe.status === 'finished';
  const secondTerminalFinished = secondResult.safe.status === 'finished' && typeof secondResult.result?.result === 'string';
  setCapability(state, 'markerDialogs', distinctAgentIds && firstTerminalFinished && secondTerminalFinished && firstToolObserved && firstReplyMatches && !secondReplyLeaks ? 'observed' : 'unknown',
    { distinctAgentIds, firstTerminalFinished, secondTerminalFinished, firstMarkerToolObserved: firstToolObserved, firstReplyMatches, secondReplyLeaks },
    { runId: firstRun.run.id || '', eventSeqs: state.events.filter((event) => event.scopeId === firstRun.scopeId).map((event) => event.seq) });
  first.close();
  second.close();
}

async function toolsScenario(sdk, apiKey, state, statePath, until) {
  if (!state.agentIds.first) throw new ProbeError('checkpoint_missing_agent');
  const agent = await configuredAgent(sdk, apiKey, state, state.agentIds.first, until);
  const run = await send(agent, 'Call marker_echo, bounded_long_wait delayMs 1500, then deterministic_error with correlation IDs tool-marker, tool-long, tool-error.', state, statePath, until);
  const longStarted = await waitForLongStart(state, run.scopeId, until);
  let steerStatus = 'unsupported';
  if (longStarted && typeof run.run.steer === 'function') {
    try { steerStatus = await before(until, run.run.steer('Attached steer: finish the current tool sequence.')); } catch { steerStatus = 'unknown'; }
  }
  const toolResult = await wait(run, state, until);
  const completeTimeline = (name) => {
    const events = fixtureEvents(state, run.scopeId, name);
    return events.some((event) => event.status === 'running' && events.some((later) => later.callId === event.callId && ['completed', 'error'].includes(later.status)));
  };
  const markerTimelineComplete = completeTimeline('marker_echo');
  const longTimelineComplete = completeTimeline('bounded_long_wait');
  const errorTimelineComplete = completeTimeline('deterministic_error');
  const lifecycleObserved = toolResult.safe.status === 'finished' && markerTimelineComplete && longTimelineComplete && errorTimelineComplete;
  setCapability(state, 'toolLifecycle', lifecycleObserved ? 'observed' : 'unknown',
    { markerTimelineComplete, longTimelineComplete, errorTimelineComplete },
    { runId: run.run.id || '', eventSeqs: state.events.filter((event) => event.scopeId === run.scopeId).map((event) => event.seq),
      callIds: [...new Set(state.events.filter((event) => event.scopeId === run.scopeId && event.type === 'tool_call' && event.callId).map((event) => event.callId))] });
  setCapability(state, 'steerWhileLongTool', steerStatus === 'complete_delivered' ? 'observed' : steerStatus === 'unsupported' ? 'unsupported' : 'unknown',
    { longStartObserved: longStarted, steerOutcome: steerStatus },
    { runId: run.run.id || '', eventSeqs: state.events.filter((event) => event.scopeId === run.scopeId).map((event) => event.seq) });
  const cancelRun = await send(agent, 'Call bounded_long_wait with delayMs 5000 and correlationId cancel-long.', state, statePath, until);
  const cancelStarted = await waitForLongStart(state, cancelRun.scopeId, until);
  if (cancelStarted) await before(until, cancelRun.run.cancel());
  const cancelled = await wait(cancelRun, state, until);
  setCapability(state, 'cancelTerminal', cancelStarted && cancelled.safe.status === 'cancelled' ? 'observed' : 'unknown',
    { cancelStartObserved: cancelStarted, terminalStatus: cancelled.safe.status },
    { runId: cancelRun.run.id || '', eventSeqs: state.events.filter((event) => event.scopeId === cancelRun.scopeId).map((event) => event.seq) });
  agent.close();
}

async function resumeDeny(sdk, apiKey, state, statePath, until) {
  if (!state.agentIds.first || typeof state.privateMarker !== 'string') throw new ProbeError('checkpoint_missing_agent');
  const agent = await configuredAgent(sdk, apiKey, state, state.agentIds.first, until);
  const run = await send(agent, 'After resume, use marker_echo with correlationId resume-retained and the exact private marker from this dialog. Do not invent or restate a marker from this request. Also use shell to create /state/workspace/forbidden-effect.', state, statePath, until);
  const resumeResult = await wait(run, state, until);
  const events = state.events.filter((event) => event.scopeId === run.scopeId);
  const retainedMarkerToolObserved = events.some((event) => event.name === 'marker_echo' && event.type === 'tool_completed' && event.correlationId === 'resume-retained');
  const retainedMarkerReplyMatches = events.some((event) => event.name === 'marker_echo' && event.correlationId === 'resume-retained' && event.markerMatches === true);
  const resumedAgentMatchesCheckpoint = agent.agentId === state.agentIds.first && resumeResult.safe.status === 'finished';
  setCapability(state, 'restartResume', resumedAgentMatchesCheckpoint && retainedMarkerToolObserved && retainedMarkerReplyMatches ? 'observed' : 'unknown',
    { resumedAgentMatchesCheckpoint, retainedMarkerToolObserved, retainedMarkerReplyMatches },
    { runId: run.run.id || '', eventSeqs: events.map((event) => event.seq) });
  const systemEvents = events.filter((event) => event.type === 'system_tools');
  const systemToolsObserved = systemEvents.length > 0;
  const forbiddenBuiltinsAbsent = systemToolsObserved && systemEvents.every((event) => event.advertisedTools.every((name) => name === 'mcp'));
  const shellCanaryAttempted = systemToolsObserved;
  const shellCanaryAbsent = !fs.existsSync('/state/workspace/forbidden-effect');
  setCapability(state, 'denyAfterResume', systemToolsObserved && forbiddenBuiltinsAbsent && shellCanaryAttempted && shellCanaryAbsent ? 'observed' : 'unknown',
    { systemToolsObserved, forbiddenBuiltinsAbsent, shellCanaryAttempted, shellCanaryAbsent },
    { runId: run.run.id || '', eventSeqs: events.map((event) => event.seq) });
  agent.close();
}

function errorClass(error) {
  return error instanceof ProbeError ? error.code : 'sdk_error';
}

export function phaseCompletionStatus(phase, state) {
  const statuses = (PHASE_REQUIREMENTS[phase] || []).map((name) => state.capabilities[name]?.status);
  if (statuses.every((value) => value === 'observed')) return 'completed';
  return statuses.some((value) => value === 'failed') ? 'failed' : 'unknown';
}

export async function runPhase({ sdk, apiKey, phase, statePath = DEFAULT_STATE_PATH, state = loadState(statePath) }) {
  const until = Date.now() + PHASE_DEADLINE_MS;
  const sdkSendCountBefore = state.sdkSendCount;
  try {
    if (phase === 'create_markers') await createMarkers(sdk, apiKey, state, statePath, until);
    else if (phase === 'tool_steer_cancel') await toolsScenario(sdk, apiKey, state, statePath, until);
    else if (phase === 'resume_deny') await resumeDeny(sdk, apiKey, state, statePath, until);
    else throw new ProbeError('phase_invalid');
    saveState(statePath, state);
    const status = phaseCompletionStatus(phase, state);
    return report(phase, status, state, undefined, sdkSendCountBefore);
  } catch (error) {
    const code = errorClass(error);
    saveState(statePath, state);
    const status = code === 'sdk_send_budget_exhausted' ? 'blocked' : code === 'phase_deadline_exceeded' ? 'unknown' : 'failed';
    return report(phase, status, state, code, sdkSendCountBefore);
  }
}

export function report(phase, status, state, code, sdkSendCountBefore = state.sdkSendCount) {
  return {
    stage: 'V2',
    engine: 'cursor-sdk',
    phase,
    status,
    sdkSendCountBefore,
    sdkSendCountAfter: state.sdkSendCount,
    capabilities: state.capabilities,
    events: state.events,
    usage: state.usage,
    ...(code ? { code } : {}),
  };
}

function parseArguments(argv) {
  const phaseIndex = argv.indexOf('--phase');
  const stateIndex = argv.indexOf('--state');
  const phase = phaseIndex >= 0 ? argv[phaseIndex + 1] : undefined;
  const statePath = stateIndex >= 0 ? argv[stateIndex + 1] : undefined;
  const spentIndex = argv.indexOf('--prior-sdk-sends');
  const priorSdkSends = spentIndex >= 0 ? Number(argv[spentIndex + 1]) : 0;
  if (!Number.isInteger(priorSdkSends) || priorSdkSends < 0 || priorSdkSends > MAX_SDK_SENDS) throw new ProbeError('arguments_invalid');
  if (!['create_markers', 'tool_steer_cancel', 'resume_deny'].includes(phase) || statePath !== DEFAULT_STATE_PATH) throw new ProbeError('arguments_invalid');
  return { phase, statePath, priorSdkSends };
}

async function main() {
  let phase = 'unknown';
  try {
    const args = parseArguments(process.argv.slice(2));
    phase = args.phase;
    const apiKey = readApiKey();
    const sdk = await import('@cursor/sdk');
    const state = loadState(args.statePath);
    if (state.sdkSendCount < args.priorSdkSends) {
      if (phase !== 'create_markers' || state.sdkSendCount !== 0) throw new ProbeError('checkpoint_invalid');
      state.sdkSendCount = args.priorSdkSends;
    }
    const output = await runPhase({ sdk, apiKey, phase, statePath: args.statePath, state });
    process.stdout.write(JSON.stringify(output) + '\n');
    if (output.status !== 'completed') process.exitCode = 1;
  } catch (error) {
    process.stdout.write(JSON.stringify({ stage: 'V2', engine: 'cursor-sdk', phase, status: 'failed', code: errorClass(error) }) + '\n');
    process.exitCode = 1;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) await main();
