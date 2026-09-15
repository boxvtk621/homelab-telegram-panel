import { createHash } from 'node:crypto';
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { harnessAPI } from '../src/harness-api';
import type { HarnessCommand, HarnessSnapshot } from '../src/harness-api';
import { HarnessWorkspace, toolLabel } from '../src/harness-workspace';
import { Panel } from '../src/panel';
import {
  clearPanelSessionState,
  readPanelSessionState,
  updatePanelSessionState,
} from '../src/panel-session-state';
import scenarios from '../../../api/harness-v1.scenarios.json';

const node1 = '20000000-0000-4000-8000-000000000001';
const node2 = '20000000-0000-4000-8000-000000000002';
const dialog1 = '30000000-0000-4000-8000-000000000001';
const dialog2 = '30000000-0000-4000-8000-000000000002';
const dialog3 = '30000000-0000-4000-8000-000000000003';
const logicalDialog1 = '30000000-0000-4000-8000-000000000010';
const commandId = '10000000-0000-4000-8000-000000000001';
const schemaHash =
  '5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9';
const session = {
  user: { id: '1-1', login: 'owner', name: 'Owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
};

it('uses operator-facing names for agent tools', () => {
  expect(toolLabel('cursor_command')).toBe('Команда в рабочей папке');
  expect(toolLabel('cursor.file_change')).toBe('Изменение файлов');
  expect(toolLabel('codex.command')).toBe('Команда в рабочей папке');
  expect(toolLabel('codex_file_change')).toBe('Изменение файлов');
  expect(toolLabel('provider_internal_tool')).toBe(
    'Дополнительный инструмент агента',
  );
});

type FetchExtra = (
  path: string,
  options?: RequestInit,
) => Promise<Response> | Response | undefined;

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((promiseResolve, promiseReject) => {
    resolve = promiseResolve;
    reject = promiseReject;
  });
  return { promise, resolve, reject };
}

function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function canonicalJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(',')}]`;
  if (value !== null && typeof value === 'object') {
    const record = value as Record<string, unknown>;
    return `{${Object.keys(record)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJSON(record[key])}`)
      .join(',')}}`;
  }
  return JSON.stringify(value) ?? 'null';
}

function commandHash(value: unknown): string {
  return createHash('sha256').update(canonicalJSON(value)).digest('hex');
}

function identity(nodeId: string) {
  const codex = nodeId === node2;
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    schemaSHA256: schemaHash,
    nodeId,
    registryVersion: 1,
    identityEpoch: 1,
    adapter: codex
      ? { kind: 'codex', version: '0.153.4' }
      : { kind: 'cursor', version: '1.0.31' },
    capabilities: {
      chat: 'verified',
      events: 'verified',
      tool_results: 'verified',
      cancel: 'verified',
      steer_attached: 'verified',
      session_resume: 'verified',
      policy_enforcement: 'verified',
    },
  };
}

function snapshot(nodeId: string, epoch = 1, lastEventSeq = 23) {
  const queued = nodeId === node1;
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId,
    epoch,
    stateVersion: 4,
    lastEventSeq,
    capturedAt: '2026-09-09T00:00:00Z',
    node: {
      transportAvailability: 'online',
      engineReadiness: 'ready',
      occupancy: 'idle',
      queuePaused: false,
      queueVersion: 1,
      pendingCount: queued ? 1 : 0,
      blockedReasons: [],
      activeAttemptId: null,
    },
    pendingQueue: queued
      ? [
          {
            requestId: '50000000-0000-4000-8000-000000000001',
            dialogId: dialog1,
            inputMessageId: '40000000-0000-4000-8000-000000000001',
            queueSequence: 1,
            version: 1,
            status: 'queued',
          },
        ]
      : [],
    activeAttempt: null,
    completeness: 'complete',
  };
}

function dialogPage(nodeId: string) {
  const ids = nodeId === node1 ? [dialog1, dialog2] : [dialog3];
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId,
    epoch: 1,
    snapshotStateVersion: 4,
    lastEventSeq: 23,
    nextCursor: null,
    pageType: 'dialogs',
    items: ids.map((dialogId, index) => ({
      dialogId,
      version: 1,
      title: `Dialog ${index + 1} ${nodeId === node2 ? 'node two' : 'node one'}`,
      createdAt: '2026-09-09T00:00:00Z',
    })),
  };
}

function bindingPage(
  nodeId: string,
  items: Array<{
    nodeDialogId: string;
    logicalDialogId: string;
    bindingVersion: number;
  }>,
) {
  return {
    schemaId: 'agent-dialog-bindings-v1',
    nodeId,
    items,
    nextCursor: null,
  };
}

function historyPage(nodeId: string, dialogId: string, text = dialogId) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId,
    epoch: 1,
    snapshotStateVersion: 4,
    lastEventSeq: 23,
    nextCursor: null,
    pageType: 'history',
    dialogId,
    items: [
      {
        messageId:
          dialogId === dialog2
            ? '40000000-0000-4000-8000-000000000002'
            : '40000000-0000-4000-8000-000000000001',
        role: 'user',
        dialogId,
        sequence: 1,
        version: 1,
        createdAt: '2026-09-09T00:00:00Z',
        text,
        disposition: 'queued',
        commandId,
        requestId: '50000000-0000-4000-8000-000000000001',
      },
    ],
  };
}

function receipt(command: {
  commandId: string;
  kind: string;
  target: { nodeId: string; dialogId?: string };
}) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    commandId: command.commandId,
    commandKind: command.kind,
    receiptId: '10000000-0000-4000-8000-000000000002',
    acceptedAt: '2026-09-09T00:00:00Z',
    nodeId: command.target.nodeId,
    eventSeq: 24,
    result: command.kind === 'dialog.delete' ? 'deleted' : 'admitted',
    references:
      command.kind === 'dialog.create'
        ? { dialogId: dialog3 }
        : command.kind === 'dialog.delete'
          ? { dialogId: command.target.dialogId }
          : {
              dialogId: command.target.dialogId,
              messageId: '40000000-0000-4000-8000-000000000003',
              requestId: '50000000-0000-4000-8000-000000000003',
            },
  };
}

function error(code: string, retryable = false) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    safeMessage: `safe ${code}`,
    correlationId: '90000000-0000-4000-8000-000000000001',
    code,
    retryable,
  };
}

function installFetch(extra?: FetchExtra) {
  const fetcher = vi.fn(
    async (input: string | URL | Request, options?: RequestInit) => {
      const path =
        typeof input === 'string'
          ? input
          : input instanceof URL
            ? input.pathname + input.search
            : new URL(input.url).pathname + new URL(input.url).search;
      const custom = extra?.(path, options);
      if (custom !== undefined) return await custom;
      if (path === '/api/v2/harness/nodes') {
        return json({
          registryVersion: 1,
          mode: 'fixture',
          nodes: [
            { nodeId: node1, name: 'Node One', adapter: 'cursor' },
            { nodeId: node2, name: 'Node Two', adapter: 'codex' },
          ],
        });
      }
      const node = path.includes(node2) ? node2 : node1;
      if (path.endsWith('/identity')) return json(identity(node));
      if (path.endsWith('/snapshot')) return json(snapshot(node));
      if (path.includes('/dialogs?')) return json(dialogPage(node));
      if (path.includes('/requests?')) {
        return json({
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          nodeId: node,
          epoch: 1,
          snapshotStateVersion: 4,
          lastEventSeq: 23,
          items: [],
          nextCursor: null,
          pageType: 'requests',
        });
      }
      const history = path.match(/\/dialogs\/([^/]+)\/messages/);
      if (history)
        return json(historyPage(node, decodeURIComponent(history[1])));
      if (path.endsWith('/health/ready')) {
        return json({
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          checkedAt: '2026-09-09T00:00:00Z',
          identity: identity(node),
          readiness: 'ready',
          blockedReasons: [],
        });
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        return json(receipt(JSON.parse(options.body)), 202);
      }
      throw new Error(`unexpected endpoint ${path}`);
    },
  );
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  readonly url: string;
  readonly withCredentials: boolean;
  closed = false;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;

  constructor(url: string | URL, init?: EventSourceInit) {
    this.url = String(url);
    this.withCredentials = init?.withCredentials ?? false;
    FakeEventSource.instances.push(this);
  }

  close() {
    this.closed = true;
  }

  emit(value: unknown) {
    this.onmessage?.(
      new MessageEvent('message', { data: JSON.stringify(value) }),
    );
  }

  fail() {
    this.onerror?.(new Event('error'));
  }
}

async function firstEventSource() {
  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
  return FakeEventSource.instances[0];
}

function nodeEvent(seq: number, epoch: number) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId: node1,
    seq,
    epoch,
    type: 'node.state_changed',
    entityId: '70000000-0000-4000-8000-000000000001',
    entityVersion: 1,
    observedAt: '2026-09-09T00:00:00Z',
    completeness: 'complete',
    payload: {
      transportAvailability: 'online',
      engineReadiness: 'ready',
      occupancy: 'idle',
      queuePaused: false,
      queueVersion: 1,
      pendingCount: 1,
      blockedReasons: [],
      activeAttemptId: null,
    },
  };
}

const requestId = '50000000-0000-4000-8000-000000000001';
const attemptId = '60000000-0000-4000-8000-000000000001';
const callId = '80000000-0000-4000-8000-000000000001';
const approvalId = '90000000-0000-4000-8000-000000000001';
const inputRequestId = 'a0000000-0000-4000-8000-000000000001';

function u2Snapshot(
  state: 'idle' | 'active',
  attemptState: 'running' | 'waiting_input' = 'running',
) {
  const value = snapshot(node1) as unknown as HarnessSnapshot;
  value.pendingQueue = [];
  value.node.pendingCount = 0;
  value.node.occupancy = state;
  value.node.activeAttemptId = state === 'active' ? attemptId : null;
  value.activeAttempt =
    state === 'active'
      ? {
          attemptId,
          dialogId: dialog1,
          requestId,
          generation: 2,
          version: 3,
          state: attemptState,
          effectStatus: 'none',
          startedAt: '2026-09-09T00:00:00Z',
        }
      : null;
  return value;
}

function requestPage(status: string) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId: node1,
    epoch: 1,
    snapshotStateVersion: 4,
    lastEventSeq: 23,
    items: [
      {
        requestId,
        dialogId: dialog1,
        inputMessageId: '40000000-0000-4000-8000-000000000001',
        queueSequence: 1,
        version: 3,
        status,
      },
    ],
    nextCursor: null,
    pageType: 'requests',
  };
}

function attemptPage(
  state: string,
  effectStatus: 'none' | 'known' | 'unknown' = 'none',
) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId: node1,
    epoch: 1,
    snapshotStateVersion: 4,
    lastEventSeq: 23,
    items: [
      {
        attemptId,
        dialogId: dialog1,
        requestId,
        generation: 2,
        version: 3,
        state,
        effectStatus,
        startedAt: '2026-09-09T00:00:00Z',
        ...(state === 'failed' || state === 'interrupted'
          ? { finishedAt: '2026-09-09T00:01:00Z' }
          : {}),
      },
    ],
    nextCursor: null,
    pageType: 'attempts',
    dialogId: dialog1,
    requestId,
  };
}

function attemptEvent(
  type: string,
  seq: number,
  payload: Record<string, unknown>,
) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId: node1,
    seq,
    epoch: 1,
    type,
    entityId: callId,
    entityVersion: 1,
    attemptId,
    dialogId: dialog1,
    observedAt: '2026-09-09T00:00:00Z',
    completeness: 'complete',
    payload,
  };
}

function eventPage(items: unknown[], nextCursor: string | null = null) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId: node1,
    epoch: 1,
    snapshotStateVersion: 4,
    lastEventSeq: 23,
    items,
    nextCursor,
    pageType: 'events',
    dialogId: dialog1,
    attemptId,
  };
}

function controlReceipt(command: HarnessCommand) {
  const references =
    command.kind === 'attempt.retry'
      ? { priorAttemptId: command.target.attemptId, requestId }
      : command.kind === 'attempt.stop'
        ? { attemptId: command.target.attemptId }
        : command.kind === 'approval.respond'
          ? {
              approvalId: command.target.approvalId,
              attemptId: command.target.attemptId,
            }
          : command.kind === 'input.respond'
            ? {
                inputRequestId: command.target.inputRequestId,
                attemptId: command.target.attemptId,
                messageId: '40000000-0000-4000-8000-000000000004',
              }
            : command.kind === 'request.cancel'
              ? { requestId: command.target.requestId }
              : command.kind === 'message.steer'
                ? {
                    dialogId: command.target.dialogId,
                    messageId: command.target.messageId,
                    attemptId: command.target.attemptId,
                  }
                : { nodeId: command.target.nodeId };
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    commandId: command.commandId,
    commandKind: command.kind,
    receiptId: '10000000-0000-4000-8000-000000000009',
    acceptedAt: '2026-09-09T00:00:00Z',
    nodeId: command.target.nodeId,
    eventSeq: 24,
    result: 'admitted',
    references,
  };
}

beforeEach(() => {
  sessionStorage.clear();
  document.documentElement.removeAttribute('data-theme');
  document.documentElement.style.removeProperty('color-scheme');
  FakeEventSource.instances = [];
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal('crypto', {
    randomUUID: vi.fn(() => commandId),
    subtle: {
      digest: vi.fn(async (_algorithm: string, data: BufferSource) => {
        const digest = createHash('sha256')
          .update(new Uint8Array(data as ArrayBuffer))
          .digest();
        return digest.buffer.slice(
          digest.byteOffset,
          digest.byteOffset + digest.byteLength,
        );
      }),
    },
  });
});

afterEach(() => {
  clearPanelSessionState(session.user.id);
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('Harness U1 workspace', () => {
  it('matches the frozen C1 command-status hash independently of property order', async () => {
    const seed = scenarios.scenarios[0].given.commands[0];
    installFetch((path) =>
      path.endsWith(`/commands/${seed.commandId}`)
        ? json({
            protocolVersion: 1,
            schemaId: 'harness-wire-v2',
            nodeId: node1,
            commandId: seed.commandId,
            canonicalPayloadHash: seed.canonicalPayloadHash,
            status: 'accepted',
            receipt: seed.receipt,
          })
        : undefined,
    );
    await expect(
      harnessAPI.status(
        session,
        node1,
        seed.canonicalValue as Parameters<typeof harnessAPI.status>[2],
      ),
    ).resolves.toMatchObject({
      canonicalPayloadHash: seed.canonicalPayloadHash,
    });
  });

  // Expected hashes independently generated with Python json.dumps:
  // ensure_ascii=False, sort_keys=True, separators=(',', ':') over this domain.
  it.each([
    [
      '\u2028\u2029',
      '7b9694921577959fcdbb95cbff1e7472d8bd3be42ce78fba148f18284058ffed',
    ],
    ['é', '8fd87d77b7001f88c84e1d56d076b103967e8a842489ec991c72b81352606d02'],
    [
      'e\u0301',
      '97c7a4e4ae1fb569884970249db878f339eb9a3928e2c33e3e52ef91e44b0548',
    ],
  ])('preserves Unicode command bytes for hash %s', async (text, hash) => {
    const command = {
      protocolVersion: 1 as const,
      schemaId: 'harness-wire-v2' as const,
      commandId,
      kind: 'message.enqueue' as const,
      target: { nodeId: node1, dialogId: dialog1 },
      expected: { dialogVersion: 1 },
      payload: { text },
    };
    installFetch((path) =>
      path.endsWith(`/commands/${commandId}`)
        ? json({
            protocolVersion: 1,
            schemaId: 'harness-wire-v2',
            nodeId: node1,
            commandId,
            canonicalPayloadHash: hash,
            status: 'accepted',
            receipt: receipt(command),
          })
        : undefined,
    );
    await expect(
      harnessAPI.status(session, node1, command),
    ).resolves.toMatchObject({ canonicalPayloadHash: hash });
  });

  it('rejects registry extensions and accepts attempt.retry priorAttemptId binding', async () => {
    installFetch((path, options) => {
      if (path === '/api/v2/harness/nodes') {
        return json({
          registryVersion: 1,
          mode: 'live',
          nodes: [],
          unexpected: true,
        });
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        const command = JSON.parse(options.body);
        return json(
          {
            protocolVersion: 1,
            schemaId: 'harness-wire-v2',
            commandId: command.commandId,
            commandKind: 'attempt.retry',
            receiptId: '10000000-0000-4000-8000-000000000002',
            acceptedAt: '2026-09-09T00:00:00Z',
            nodeId: node1,
            eventSeq: 24,
            result: 'admitted',
            references: {
              priorAttemptId: '60000000-0000-4000-8000-000000000001',
              requestId: '50000000-0000-4000-8000-000000000001',
            },
          },
          202,
        );
      }
      return undefined;
    });
    await expect(harnessAPI.nodes(session)).rejects.toMatchObject({
      code: 'invalid_response',
    });
    await expect(
      harnessAPI.command(session, node1, {
        protocolVersion: 1,
        schemaId: 'harness-wire-v2',
        commandId,
        kind: 'attempt.retry',
        target: {
          nodeId: node1,
          attemptId: '60000000-0000-4000-8000-000000000001',
        },
        expected: { attemptGeneration: 1 },
        payload: { acknowledgeKnownEffects: true },
      }),
    ).resolves.toMatchObject({ commandKind: 'attempt.retry' });
  });

  it('bootstraps snapshot before SSE and renders fixture, exact state axes, and FIFO', async () => {
    installFetch();
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const workspace = screen.getByRole('region', {
      name: 'Рабочее место агента',
    });
    const navigation = within(workspace).getByRole('navigation', {
      name: 'Разделы рабочего места',
    });
    expect(
      within(navigation)
        .getAllByRole('link')
        .map((link) => link.textContent),
    ).toEqual(['Чат', 'Ход работы', 'Диалоги', 'Агент']);
    expect(
      within(navigation).getByRole('link', { name: 'Чат' }),
    ).toHaveProperty('hash', '#agent-conversation');
    expect(
      within(navigation).getByRole('link', { name: 'Агент' }),
    ).toHaveProperty('hash', '#agent-state-details');
    expect(
      screen.getByText(
        /Учебные данные · команды не управляют реальным Harness/,
      ),
    ).toBeDefined();
    expect(screen.getByText('Доступность')).toBeDefined();
    expect(screen.getByText('Позиция 1')).toBeDefined();
    expect(screen.getAllByText('свободен')).toHaveLength(1);
    expect(screen.queryByText('idle')).toBeNull();
    expect(screen.queryByText('INTERACTION')).toBeNull();
    expect(
      screen.getByText(`nodeId: ${node1}`).closest('details'),
    ).toBeDefined();
    const source = await firstEventSource();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(source.url).toBe(`/api/v2/harness/nodes/${node1}/events?after=23`);
    expect(source.withCredentials).toBe(true);
  });

  it('fails closed when an explicitly selected agent left the current registry', async () => {
    const retiredNode = '20000000-0000-4000-8000-000000000099';
    const fetcher = installFetch();
    render(
      <HarnessWorkspace
        session={session}
        onExpired={vi.fn()}
        selectedNodeId={retiredNode}
        onBack={vi.fn()}
      />,
    );

    expect(
      await screen.findByText(
        'Выбранный агент больше не зарегистрирован. Вернитесь к списку и обновите реестр.',
      ),
    ).toBeDefined();
    expect(screen.queryByText('Dialog 1 node one')).toBeNull();
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(fetcher.mock.calls[0]?.[0]).toBe('/api/v2/harness/nodes');
  });

  it('clears a prior exact target before a changed selection is reverified', async () => {
    const missingNode = '20000000-0000-4000-8000-000000000099';
    let registryReads = 0;
    let resolveRegistry!: (response: Response) => void;
    const delayedRegistry = new Promise<Response>((resolve) => {
      resolveRegistry = resolve;
    });
    const fetcher = installFetch((path) => {
      if (path !== '/api/v2/harness/nodes') return undefined;
      registryReads += 1;
      return registryReads === 1 ? undefined : delayedRegistry;
    });
    const view = render(
      <HarnessWorkspace
        session={session}
        onExpired={vi.fn()}
        selectedNodeId={node1}
        onBack={vi.fn()}
      />,
    );
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const priorSource = await firstEventSource();

    view.rerender(
      <HarnessWorkspace
        session={session}
        onExpired={vi.fn()}
        selectedNodeId={missingNode}
        onBack={vi.fn()}
      />,
    );

    await waitFor(() => expect(registryReads).toBe(2));
    expect(priorSource.closed).toBe(true);
    expect(screen.queryByLabelText('Сообщение агенту')).toBeNull();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(
      fetcher.mock.calls.some(([, options]) => options?.method === 'POST'),
    ).toBe(false);

    await act(async () => {
      resolveRegistry(
        json({
          registryVersion: 1,
          mode: 'fixture',
          nodes: [
            { nodeId: node1, name: 'Node One', adapter: 'cursor' },
            { nodeId: node2, name: 'Node Two', adapter: 'codex' },
          ],
        }),
      );
      await delayedRegistry;
    });
    expect(
      await screen.findByText(
        'Выбранный агент больше не зарегистрирован. Вернитесь к списку и обновите реестр.',
      ),
    ).toBeDefined();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(
      fetcher.mock.calls.some(([, options]) => options?.method === 'POST'),
    ).toBe(false);
  });

  it('confirms the exact dialog, sends one delete command, and selects the deterministic fallback', async () => {
    let deleted = false;
    const posted: HarnessCommand[] = [];
    installFetch((path, options) => {
      if (path === `/api/v2/harness/nodes/${node1}/snapshot`) {
        return json(u2Snapshot('idle'));
      }
      if (path.startsWith(`/api/v2/harness/nodes/${node1}/dialogs?`)) {
        const page = dialogPage(node1);
        if (deleted) {
          page.items = page.items.filter((item) => item.dialogId !== dialog1);
        }
        return json(page);
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        const command = JSON.parse(options.body) as HarnessCommand;
        if (command.kind !== 'dialog.delete') return undefined;
        posted.push(command);
        deleted = true;
        return json(receipt(command), 202);
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const draft = screen.getByLabelText('Сообщение агенту');
    fireEvent.change(draft, { target: { value: 'не отправленный черновик' } });
    const deleteButton = screen.getByRole('button', {
      name: `Удалить диалог ${dialog1}`,
    });
    await waitFor(() => expect(deleteButton).toHaveProperty('disabled', false));
    fireEvent.click(deleteButton);
    const confirmation = await screen.findByRole('alertdialog');
    expect(confirmation.textContent).toContain('Dialog 1 node one');
    expect(confirmation.textContent).toContain(dialog1);
    expect(confirmation.textContent).toContain(
      'данные сохраняются для аварийного восстановления',
    );
    expect(confirmation.textContent).toContain(
      'вернуть диалог через Panel нельзя',
    );
    const cancel = screen.getByRole('button', { name: 'Отмена' });
    await waitFor(() => expect(document.activeElement).toBe(cancel));
    fireEvent.keyDown(confirmation, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull());
    expect(posted).toHaveLength(0);

    fireEvent.click(deleteButton);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Удалить диалог' }),
    );
    await screen.findByRole('heading', { name: 'Dialog 2 node one' });
    expect(
      screen.queryByRole('button', {
        name: `Удалить диалог ${dialog1}`,
      }),
    ).toBeNull();
    expect(posted).toHaveLength(1);
    expect(posted[0]).toMatchObject({
      kind: 'dialog.delete',
      target: { nodeId: node1, dialogId: dialog1 },
      expected: { dialogVersion: 1 },
      payload: {},
    });
  });

  it('renders the empty state after deleting the only dialog', async () => {
    let deleted = false;
    installFetch((path, options) => {
      if (path.startsWith(`/api/v2/harness/nodes/${node2}/dialogs?`)) {
        const page = dialogPage(node2);
        if (deleted) page.items = [];
        return json(page);
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        const command = JSON.parse(options.body) as HarnessCommand;
        if (command.kind !== 'dialog.delete') return undefined;
        deleted = true;
        return json(receipt(command), 202);
      }
      return undefined;
    });
    render(
      <HarnessWorkspace
        session={session}
        onExpired={vi.fn()}
        selectedNodeId={node2}
      />,
    );
    await screen.findByRole('heading', { name: 'Dialog 1 node two' });
    const deleteButton = screen.getByRole('button', {
      name: `Удалить диалог ${dialog3}`,
    });
    await waitFor(() => expect(deleteButton).toHaveProperty('disabled', false));
    fireEvent.click(deleteButton);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Удалить диалог' }),
    );
    await screen.findByText('Диалогов пока нет. Создайте первый.');
    expect(screen.queryByText(dialog3)).toBeNull();
  });

  it('blocks deletion while exact work is pending and reports a stale server refusal', async () => {
    let posts = 0;
    installFetch((path, options) => {
      if (path.endsWith('/commands') && options?.method === 'POST') {
        posts += 1;
        return json(error('stale'), 409);
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const busyDelete = screen.getByRole('button', {
      name: `Удалить диалог ${dialog1}`,
    });
    expect(busyDelete).toHaveProperty('disabled', true);
    await screen.findByText(/В диалоге есть поручение в очереди/);
    const idleDelete = screen.getByRole('button', {
      name: `Удалить диалог ${dialog2}`,
    });
    await waitFor(() => expect(idleDelete).toHaveProperty('disabled', false));
    fireEvent.click(idleDelete);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Удалить диалог' }),
    );
    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'safe stale',
    );
    expect(posts).toBe(1);
  });

  it('reconciles a lost delete ACK by command status without posting twice', async () => {
    let deleted = false;
    let posted: HarnessCommand | null = null;
    let posts = 0;
    installFetch((path, options) => {
      if (path === `/api/v2/harness/nodes/${node1}/snapshot`) {
        return json(u2Snapshot('idle'));
      }
      if (path.startsWith(`/api/v2/harness/nodes/${node1}/dialogs?`)) {
        const page = dialogPage(node1);
        if (deleted) {
          page.items = page.items.filter((item) => item.dialogId !== dialog1);
        }
        return json(page);
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        posted = JSON.parse(options.body) as HarnessCommand;
        posts += 1;
        return Promise.reject(new TypeError('lost ACK'));
      }
      if (path.endsWith(`/commands/${commandId}`)) {
        if (!posted) throw new Error('delete was not posted');
        deleted = true;
        return json({
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          nodeId: node1,
          commandId,
          canonicalPayloadHash: commandHash(posted),
          status: 'accepted',
          receipt: receipt(posted),
        });
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const deleteButton = screen.getByRole('button', {
      name: `Удалить диалог ${dialog1}`,
    });
    await waitFor(() => expect(deleteButton).toHaveProperty('disabled', false));
    fireEvent.click(deleteButton);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Удалить диалог' }),
    );
    await screen.findByText(/Ответ потерян\. Не повторяйте удаление/);
    expect(screen.getByText('Технические сведения')).toBeDefined();
    expect(posts).toBe(1);
    fireEvent.click(screen.getByRole('button', { name: 'Проверить удаление' }));
    await screen.findByRole('heading', { name: 'Dialog 2 node one' });
    expect(posts).toBe(1);
  });

  it('renders assistant Markdown safely and keeps user Markdown literal', async () => {
    const base = historyPage(node1, dialog1, '# Команда пользователя');
    const page = {
      ...base,
      items: [
        ...base.items,
        {
          messageId: '40000000-0000-4000-8000-000000000009',
          role: 'assistant',
          dialogId: dialog1,
          attemptId,
          sequence: 2,
          version: 1,
          createdAt: '2026-09-09T00:00:01Z',
          finishReason: 'complete',
          content: {
            kind: 'inline',
            content: [
              '## Форматированный ответ',
              '',
              '- понятный пункт',
              '- `config.yaml`',
              '',
              '<script>window.compromised = true</script>',
              '',
              '[опасно](javascript:alert(1))',
            ].join('\n'),
            redaction: 'none',
            truncated: false,
          },
        },
      ],
    };
    installFetch((path) =>
      path.includes(`/dialogs/${dialog1}/messages`) ? json(page) : undefined,
    );
    const { container } = render(
      <HarnessWorkspace session={session} onExpired={vi.fn()} />,
    );

    expect(await screen.findByText('# Команда пользователя')).toHaveProperty(
      'tagName',
      'P',
    );
    expect(
      await screen.findByRole('heading', { name: 'Форматированный ответ' }),
    ).toBeDefined();
    expect(screen.getByText('понятный пункт').tagName).toBe('LI');
    expect(screen.getByText('config.yaml').tagName).toBe('CODE');
    expect(container.querySelector('script')).toBeNull();
    expect(container.innerHTML).not.toContain('javascript:');
  });

  it('fences a stale node bootstrap response after selection changes', async () => {
    let resolveIdentity!: (response: Response) => void;
    const delayedIdentity = new Promise<Response>((resolve) => {
      resolveIdentity = resolve;
    });
    installFetch((path) =>
      path === `/api/v2/harness/nodes/${node1}/identity`
        ? delayedIdentity
        : undefined,
    );
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    const selector = await screen.findByLabelText('Агент');
    fireEvent.change(selector, { target: { value: node2 } });
    await screen.findByRole('heading', { name: 'Dialog 1 node two' });
    resolveIdentity(json(identity(node1)));
    await waitFor(() =>
      expect((selector as HTMLSelectElement).value).toBe(node2),
    );
    expect(screen.queryByText('Dialog 1 node one')).toBeNull();
  });

  it('refreshes the new node when switching while an old event refresh is pending', async () => {
    let updated = false;
    const fetcher = installFetch((path) => {
      if (!path.includes(node2)) return undefined;
      if (path.endsWith('/snapshot'))
        return json(snapshot(node2, 1, updated ? 24 : 23));
      if (path.includes('/requests?')) {
        return json({
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          nodeId: node2,
          epoch: 1,
          snapshotStateVersion: 4,
          lastEventSeq: updated ? 24 : 23,
          items: [],
          nextCursor: null,
          pageType: 'requests',
        });
      }
      if (path.includes('/dialogs?')) {
        const page = dialogPage(node2);
        page.lastEventSeq = updated ? 24 : 23;
        return json(page);
      }
      if (path.includes(`/dialogs/${dialog3}/messages`)) {
        const page = historyPage(
          node2,
          dialog3,
          updated ? 'Node two refreshed' : 'Node two baseline',
        );
        page.lastEventSeq = updated ? 24 : 23;
        return json(page);
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const first = await firstEventSource();
    vi.useFakeTimers();
    try {
      await act(async () => first.emit(nodeEvent(24, 1)));
      await act(async () => {
        fireEvent.change(screen.getByLabelText('Агент'), {
          target: { value: node2 },
        });
      });
      expect(screen.getByText('Node two baseline')).toBeDefined();
      const snapshotReads = () =>
        fetcher.mock.calls.filter(
          ([path]) => path === `/api/v2/harness/nodes/${node2}/snapshot`,
        ).length;
      const before = snapshotReads();
      updated = true;
      await act(async () => {
        FakeEventSource.instances
          .at(-1)!
          .emit({ ...nodeEvent(24, 1), nodeId: node2 });
        await vi.advanceTimersByTimeAsync(25);
      });
      expect(snapshotReads()).toBe(before + 1);
      expect(screen.getByText('Node two refreshed')).toBeDefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('fences a late history reply after switching the exact dialog', async () => {
    let resolveHistory!: (response: Response) => void;
    const delayedHistory = new Promise<Response>((resolve) => {
      resolveHistory = resolve;
    });
    installFetch((path) => {
      if (path.includes(`/dialogs/${dialog1}/messages`)) return delayedHistory;
      if (path.includes(`/dialogs/${dialog2}/messages`)) {
        return json(historyPage(node1, dialog2, 'current dialog message'));
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    fireEvent.click(
      await screen.findByRole('button', { name: /Dialog 2 node one/ }),
    );
    await screen.findByText('current dialog message');
    resolveHistory(json(historyPage(node1, dialog1, 'stale dialog message')));
    await waitFor(() =>
      expect(screen.queryByText('stale dialog message')).toBeNull(),
    );
    expect(screen.getByText('current dialog message')).toBeDefined();
  });

  it('re-baselines instead of committing history older than its snapshot', async () => {
    let historyReads = 0;
    installFetch((path) => {
      if (!path.includes(`/dialogs/${dialog1}/messages`)) return undefined;
      historyReads += 1;
      const page = historyPage(node1, dialog1, 'fresh history');
      if (historyReads === 1) {
        page.snapshotStateVersion = 3;
        page.lastEventSeq = 22;
      }
      return json(page);
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await waitFor(() => expect(historyReads).toBeGreaterThanOrEqual(2));
    await screen.findByText('fresh history');
    expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(2);
  });

  it('retains lost-ACK intent across targets, reconciles 404, and retries the same command', async () => {
    let posts = 0;
    const posted: unknown[] = [];
    installFetch((path, options) => {
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        const command = JSON.parse(options.body);
        posted.push(command);
        posts += 1;
        return posts === 1
          ? Promise.reject(new TypeError('lost ACK'))
          : json(receipt(command), 200);
      }
      if (path.endsWith(`/commands/${commandId}`)) {
        return json(error('not_found'), 404);
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    const field = await screen.findByLabelText('Сообщение агенту');
    fireEvent.change(field, { target: { value: 'same exact intent' } });
    fireEvent.click(screen.getByRole('button', { name: 'Отправить' }));
    await screen.findByText(/Результат отправки не подтверждён/);
    fireEvent.change(screen.getByLabelText('Агент'), {
      target: { value: node2 },
    });
    await screen.findByRole('heading', { name: 'Dialog 1 node two' });
    fireEvent.change(screen.getByLabelText('Агент'), {
      target: { value: node1 },
    });
    await screen.findByText(/Результат отправки не подтверждён/);
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('same exact intent');
    fireEvent.click(screen.getByRole('button', { name: 'Проверить отправку' }));
    await screen.findByText(/Команда не найдена/);
    fireEvent.click(screen.getByRole('button', { name: 'Повторить отправку' }));
    await screen.findByText('Команда принята в очередь.');
    expect(posted).toHaveLength(2);
    expect(posted[1]).toEqual(posted[0]);
  });

  it('treats a wrong successful receipt as unknown and clears only after exact status', async () => {
    let postedCommand: Parameters<typeof receipt>[0] | undefined;
    let wrongHash = true;
    installFetch((path, options) => {
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        const parsed = JSON.parse(options.body) as Parameters<
          typeof receipt
        >[0];
        postedCommand = parsed;
        const wrong = receipt(parsed);
        wrong.references.dialogId = dialog2;
        return json(wrong, 202);
      }
      if (path.endsWith(`/commands/${commandId}`)) {
        if (!postedCommand) throw new Error('command was not posted');
        const exact = receipt(postedCommand);
        return json({
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          nodeId: node1,
          commandId,
          canonicalPayloadHash: wrongHash
            ? '0'.repeat(64)
            : '909b5c1483f589b882dfdc42119e46d976a869301126c46be013e1b87fa13e15',
          status: 'accepted',
          receipt: exact,
        });
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    const field = await screen.findByLabelText('Сообщение агенту');
    const text = 'Проверка <>&\n😀e\u0301';
    fireEvent.change(field, { target: { value: text } });
    fireEvent.click(screen.getByRole('button', { name: 'Отправить' }));
    await screen.findByText(/Результат отправки не подтверждён/);
    expect((field as HTMLTextAreaElement).value).toBe(text);
    fireEvent.click(screen.getByRole('button', { name: 'Проверить отправку' }));
    await screen.findByText('Сохранённая команда отличается от отправленной.');
    expect((field as HTMLTextAreaElement).value).toBe(text);
    expect(screen.queryByText('Команда принята в очередь.')).toBeNull();
    wrongHash = false;
    fireEvent.click(screen.getByRole('button', { name: 'Проверить отправку' }));
    await screen.findByText('Команда принята в очередь.');
    expect((field as HTMLTextAreaElement).value).toBe('');
  });

  it.each([
    ['gap', 25, 1],
    ['epoch change', 24, 2],
  ])('closes and re-baselines SSE on %s', async (_case, seq, epoch) => {
    installFetch();
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const first = await firstEventSource();
    expect(FakeEventSource.instances).toHaveLength(1);
    first.emit(nodeEvent(seq, epoch));
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    expect(first.closed).toBe(true);
    expect(FakeEventSource.instances[1].url).toBe(
      `/api/v2/harness/nodes/${node1}/events?after=23`,
    );
  });

  it('bounds repeated SSE reconnects when connections never become stable', async () => {
    installFetch();
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    await firstEventSource();
    vi.useFakeTimers();
    try {
      for (const delay of [1_000, 2_000, 3_000]) {
        await act(async () => {
          FakeEventSource.instances.at(-1)?.fail();
          await Promise.resolve();
          await vi.advanceTimersByTimeAsync(delay);
        });
      }
      await act(async () => {
        FakeEventSource.instances.at(-1)?.fail();
        await Promise.resolve();
        await vi.advanceTimersByTimeAsync(10_000);
      });
      expect(FakeEventSource.instances).toHaveLength(4);
      expect(
        screen.getByText(/автоматические повторы исчерпаны/),
      ).toBeDefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('expires the session after SSE auth loss without sending a command', async () => {
    const expired = vi.fn();
    const fetcher = installFetch((path) =>
      path.endsWith('/health/ready')
        ? json(error('no_session'), 401)
        : undefined,
    );
    render(<HarnessWorkspace session={session} onExpired={expired} />);
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const first = await firstEventSource();
    first.fail();
    await waitFor(() => expect(expired).toHaveBeenCalledOnce());
    expect(first.closed).toBe(true);
    expect(
      fetcher.mock.calls.some(([, options]) => options?.method === 'POST'),
    ).toBe(false);
  });

  it('aborts an old session probe before its late 401 reaches a new session', async () => {
    let resolveReady!: (response: Response) => void;
    const delayedReady = new Promise<Response>((resolve) => {
      resolveReady = resolve;
    });
    const fetcher = installFetch((path) =>
      path.endsWith('/health/ready') ? delayedReady : undefined,
    );
    const oldExpired = vi.fn();
    const newExpired = vi.fn();
    const { rerender } = render(
      <HarnessWorkspace
        key={session.csrf}
        session={session}
        onExpired={oldExpired}
      />,
    );
    await screen.findByRole('heading', { name: 'Dialog 1 node one' });
    const first = await firstEventSource();
    first.fail();
    await waitFor(() =>
      expect(
        fetcher.mock.calls.some(
          ([path]) =>
            typeof path === 'string' && path.endsWith('/health/ready'),
        ),
      ).toBe(true),
    );
    rerender(
      <HarnessWorkspace
        key={'t'.repeat(43)}
        session={{ ...session, csrf: 't'.repeat(43) }}
        onExpired={newExpired}
      />,
    );
    resolveReady(json(error('no_session'), 401));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(oldExpired).not.toHaveBeenCalled();
    expect(newExpired).not.toHaveBeenCalled();
  });

  it('separates agent management from interaction and keeps an accepted run mounted', async () => {
    const fetcher = installFetch((path) => {
      if (path === '/api/v2/session') return json(session);
      if (path === `/api/v2/harness/nodes/${node2}/snapshot`) {
        return json(error('engine_unavailable'), 503);
      }
      return undefined;
    });
    render(<Panel />);
    expect(
      await screen.findByRole('heading', {
        name: 'Harness / Инстансы',
      }),
    ).toBeDefined();
    const selectedRow = await screen.findByRole('button', {
      name: 'Выбрать Harness Node One для управления, свободен',
    });
    fireEvent.click(selectedRow);
    expect(
      within(screen.getByLabelText('Инспектор выбранного Harness')).getByRole(
        'heading',
        { name: 'Node One' },
      ),
    ).toBeDefined();
    expect(
      screen.getByText(
        'Harness недоступен; результат команды может быть неизвестен.',
      ),
    ).toBeDefined();
    expect(screen.getAllByText('Доступность').length).toBeGreaterThan(0);
    expect(
      within(screen.getByLabelText('Инспектор выбранного Harness')).getByText(
        'Очередь',
      ).parentElement?.textContent,
    ).toContain('1');
    fireEvent.click(
      within(screen.getByLabelText('Инспектор выбранного Harness')).getByRole(
        'button',
        { name: 'Открыть диалог Harness Node One' },
      ),
    );
    const field = await screen.findByLabelText('Сообщение агенту');
    fireEvent.change(field, { target: { value: 'accepted in background' } });
    fireEvent.click(screen.getByRole('button', { name: 'Отправить' }));
    await screen.findByText('Команда принята в очередь.');
    fireEvent.click(
      screen.getByRole('button', { name: 'Вернуться к управлению Harness' }),
    );
    expect(
      screen.getByRole('heading', { name: 'Harness / Инстансы' }),
    ).toBeDefined();
    fireEvent.click(
      within(screen.getByLabelText('Инспектор выбранного Harness')).getByRole(
        'button',
        { name: 'Открыть диалог Harness Node One' },
      ),
    );
    expect(screen.getByText('Команда принята в очередь.')).toBeDefined();
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('');
    expect(
      fetcher.mock.calls.filter(
        ([path, options]) =>
          typeof path === 'string' &&
          path.endsWith(`/nodes/${node1}/commands`) &&
          options?.method === 'POST',
      ),
    ).toHaveLength(1);
  });

  it('checks artifact metadata before download and verifies binary SHA-256', async () => {
    const artifactId = 'b0000000-0000-4000-8000-000000000001';
    const attemptId = '60000000-0000-4000-8000-000000000001';
    const bytes = new Uint8Array([1, 2, 3]);
    const corruptBytes = new Uint8Array([3, 2, 1]);
    const sha256 = createHash('sha256').update(bytes).digest('hex');
    let binaryReads = 0;
    let metadataMismatch = true;
    let corruptBinary = true;
    const NativeURL = URL;
    const createObjectURL = vi.fn(() => 'blob:verified-artifact');
    const revokeObjectURL = vi.fn();
    class TestURL extends NativeURL {
      static createObjectURL = createObjectURL;
      static revokeObjectURL = revokeObjectURL;
    }
    vi.stubGlobal('URL', TestURL);
    const downloadClick = vi
      .spyOn(HTMLAnchorElement.prototype, 'click')
      .mockImplementation(() => undefined);
    installFetch((path) => {
      if (path.includes(`/dialogs/${dialog1}/messages`)) {
        return json({
          ...historyPage(node1, dialog1),
          items: [
            {
              messageId: '40000000-0000-4000-8000-000000000003',
              role: 'assistant',
              dialogId: dialog1,
              sequence: 3,
              version: 1,
              createdAt: '2026-09-09T00:00:00Z',
              attemptId,
              content: {
                kind: 'artifact',
                artifactId,
                sizeBytes: bytes.byteLength,
                sha256,
                redaction: 'none',
                truncated: false,
              },
              finishReason: 'complete',
            },
          ],
        });
      }
      if (path.endsWith(`/${artifactId}/metadata`)) {
        return json({
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          nodeId: node1,
          dialogId: dialog1,
          attemptId,
          artifactId,
          name: 'answer.bin',
          mediaType: 'application/octet-stream',
          sizeBytes: bytes.byteLength,
          sha256: metadataMismatch ? 'f'.repeat(64) : sha256,
          redaction: 'none',
          truncated: false,
          disposition: 'attachment',
        });
      }
      if (path.endsWith(`/${artifactId}`)) {
        binaryReads += 1;
        return new Response(corruptBinary ? corruptBytes : bytes, {
          headers: {
            'Content-Type': 'application/octet-stream',
            'Content-Disposition': 'attachment; filename="answer.bin"',
          },
        });
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    fireEvent.click(
      await screen.findByRole('button', { name: /Скачать артефакт/ }),
    );
    await screen.findByText('Описание файла не совпадает с сообщением.');
    expect(binaryReads).toBe(0);
    metadataMismatch = false;
    fireEvent.click(screen.getByRole('button', { name: /Скачать артефакт/ }));
    await screen.findByText(/Размер, SHA-256 или заголовки/);
    expect(binaryReads).toBe(1);
    corruptBinary = false;
    fireEvent.click(screen.getByRole('button', { name: /Скачать артефакт/ }));
    await waitFor(() => expect(downloadClick).toHaveBeenCalledOnce());
    expect(createObjectURL).toHaveBeenCalledOnce();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:verified-artifact');
    expect(binaryReads).toBe(2);
  });

  it('retains an unknown stop command across target switches and retries only after reconcile', async () => {
    const posted: string[] = [];
    let first = true;
    let projectionActive = true;
    installFetch((path, options) => {
      if (path.endsWith(`/${node1}/snapshot`)) {
        return json(u2Snapshot(projectionActive ? 'active' : 'idle'));
      }
      if (path.includes(`/${node1}/requests?`)) {
        return json(requestPage('active'));
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        return json(attemptPage('running'));
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        return json(eventPage([]));
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        posted.push(options.body);
        if (first) {
          first = false;
          projectionActive = false;
          return Promise.reject(new TypeError('lost ACK'));
        }
        return json(controlReceipt(JSON.parse(options.body)), 202);
      }
      if (path.includes('/commands/')) return json(error('not_found'), 404);
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Остановить работу' }),
    );
    await screen.findByText(/Результат запроса пока не подтверждён/);
    fireEvent.change(screen.getByLabelText('Агент'), {
      target: { value: node2 },
    });
    await screen.findByRole('heading', { name: 'Dialog 1 node two' });
    fireEvent.change(screen.getByLabelText('Агент'), {
      target: { value: node1 },
    });
    await screen.findByText(/Результат запроса пока не подтверждён/);
    await screen.findByRole('heading', { name: 'Неподтверждённые запросы' });
    expect(
      screen.getByRole('button', { name: 'Остановить работу' }),
    ).toBeDefined();
    expect(posted).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: 'Проверить запрос' }));
    await screen.findByText(/Запрос не найден/);
    fireEvent.click(screen.getByRole('button', { name: 'Повторить запрос' }));
    await waitFor(() => expect(posted).toHaveLength(2));
    expect(posted[1]).toBe(posted[0]);
  });

  it('discovers a terminal attempt without assistant text and requires known-effects acknowledgement', async () => {
    const posted: HarnessCommand[] = [];
    installFetch((path, options) => {
      if (path.endsWith(`/${node1}/snapshot`)) return json(u2Snapshot('idle'));
      if (path.includes(`/${node1}/requests?`)) {
        return json(requestPage('failed'));
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        return json(attemptPage('failed', 'known'));
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        return json(
          eventPage([
            attemptEvent('approval.requested', 15, {
              approvalId,
              callId,
              actionHash: '5'.repeat(64),
              safePrompt: 'This approval is already terminal.',
              approvalVersion: 1,
            }),
            attemptEvent('input.requested', 16, {
              inputRequestId,
              prompt: {
                kind: 'inline',
                content: 'This input is already terminal.',
                redaction: 'none',
                truncated: false,
              },
              inputVersion: 1,
            }),
            attemptEvent('tool.completed', 17, {
              callId,
              status: 'succeeded',
              result: {
                kind: 'inline',
                content: 'external write completed',
                redaction: 'applied',
                truncated: false,
              },
              effectStatus: 'known',
              effectRef: 'effect-42',
            }),
            attemptEvent('attempt.failed', 18, {
              generation: 2,
              failureClass: 'task',
              errorCode: 'synthetic_failure',
              safeMessage: 'Synthetic task failed.',
              retryable: true,
              effectStatus: 'known',
            }),
          ]),
        );
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        const command = JSON.parse(options.body);
        posted.push(command);
        return json(controlReceipt(command), 202);
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('heading', {
      name: 'Повторить завершённый запуск',
    });
    expect(
      await screen.findByText('Внешние изменения подтверждены.'),
    ).toBeDefined();
    expect(
      document.querySelector('.message[data-role="assistant"]'),
    ).toBeNull();
    expect(
      screen.queryByRole('button', { name: 'Разрешить один раз' }),
    ).toBeNull();
    expect(
      screen.queryByRole('button', { name: 'Ответить агенту' }),
    ).toBeNull();
    const retry = screen.getByRole('button', { name: 'Повторить попытку' });
    expect((retry as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(
      screen.getByLabelText(/Я проверил известные эффекты в ленте/),
    );
    fireEvent.click(retry);
    await screen.findByText(/Агент принял запрос/);
    expect(posted).toHaveLength(1);
    expect(posted[0]).toMatchObject({
      kind: 'attempt.retry',
      target: { nodeId: node1, attemptId },
      expected: { attemptGeneration: 2 },
      payload: { acknowledgeKnownEffects: true },
    });
  });

  it('removes pending approval and input controls as soon as a matching terminal SSE event arrives', async () => {
    installFetch((path) => {
      if (path.endsWith(`/${node1}/snapshot`)) {
        return json(u2Snapshot('active', 'waiting_input'));
      }
      if (path.includes(`/${node1}/requests?`)) {
        return json(requestPage('active'));
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        return json(attemptPage('waiting_input'));
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        return json(
          eventPage([
            attemptEvent('approval.requested', 18, {
              approvalId,
              callId,
              actionHash: '6'.repeat(64),
              safePrompt: 'Allow?',
              approvalVersion: 1,
            }),
            attemptEvent('input.requested', 19, {
              inputRequestId,
              prompt: {
                kind: 'inline',
                content: 'Answer?',
                redaction: 'none',
                truncated: false,
              },
              inputVersion: 1,
            }),
          ]),
        );
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByRole('button', { name: 'Разрешить один раз' });
    await screen.findByRole('button', { name: 'Ответить агенту' });
    const source = await firstEventSource();
    await act(async () => {
      source.emit(
        attemptEvent('attempt.failed', 24, {
          generation: 2,
          failureClass: 'task',
          errorCode: 'terminal_before_refresh',
          safeMessage: 'Terminal before refreshed projection.',
          retryable: true,
          effectStatus: 'none',
        }),
      );
    });
    expect(
      screen.queryByRole('button', { name: 'Разрешить один раз' }),
    ).toBeNull();
    expect(
      screen.queryByRole('button', { name: 'Ответить агенту' }),
    ).toBeNull();
  });

  it('binds approval and input responses to the exact attempt, version and action hash', async () => {
    const actionHash = '3'.repeat(64);
    const posted: HarnessCommand[] = [];
    installFetch((path, options) => {
      if (path.endsWith(`/${node1}/snapshot`)) {
        return json(u2Snapshot('active', 'waiting_input'));
      }
      if (path.includes(`/${node1}/requests?`)) {
        return json(requestPage('active'));
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        return json(attemptPage('waiting_input'));
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        return json(
          eventPage([
            attemptEvent('approval.resolved', 17, {
              approvalId,
              decision: 'deny',
              actorId: 'owner',
              approvalVersion: 3,
            }),
            attemptEvent('approval.requested', 18, {
              approvalId,
              callId,
              actionHash,
              safePrompt: 'Allow synthetic effect?',
              approvalVersion: 4,
            }),
            attemptEvent('input.resolved', 19, {
              inputRequestId,
              messageId: '40000000-0000-4000-8000-000000000003',
              inputVersion: 4,
            }),
            attemptEvent('input.requested', 20, {
              inputRequestId,
              prompt: {
                kind: 'inline',
                content: 'Provide value',
                redaction: 'none',
                truncated: false,
              },
              inputVersion: 5,
            }),
          ]),
        );
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        const command = JSON.parse(options.body);
        posted.push(command);
        return json(controlReceipt(command), 202);
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Разрешить один раз' }),
    );
    const input = await screen.findByLabelText('Ответ агенту');
    fireEvent.change(input, { target: { value: 'exact answer' } });
    fireEvent.click(screen.getByRole('button', { name: 'Ответить агенту' }));
    await waitFor(() => expect(posted).toHaveLength(2));
    expect(posted[0]).toMatchObject({
      kind: 'approval.respond',
      target: { nodeId: node1, approvalId, attemptId },
      expected: { approvalVersion: 4, attemptGeneration: 2 },
      payload: { decision: 'allow_once', actionHash },
    });
    expect(posted[1]).toMatchObject({
      kind: 'input.respond',
      target: { nodeId: node1, inputRequestId, attemptId },
      expected: { inputVersion: 5, attemptGeneration: 2 },
      payload: { text: 'exact answer' },
    });
    expect((input as HTMLTextAreaElement).disabled).toBe(true);
  });

  it('shows one immutable approval recovery action and retries its saved decision', async () => {
    const posted: string[] = [];
    let first = true;
    installFetch((path, options) => {
      if (path.endsWith(`/${node1}/snapshot`)) {
        return json(u2Snapshot('active', 'waiting_input'));
      }
      if (path.includes(`/${node1}/requests?`)) {
        return json(requestPage('active'));
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        return json(attemptPage('waiting_input'));
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        return json(
          eventPage([
            attemptEvent('approval.requested', 18, {
              approvalId,
              callId,
              actionHash: '7'.repeat(64),
              safePrompt: 'Allow immutable action?',
              approvalVersion: 4,
            }),
          ]),
        );
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        posted.push(options.body);
        if (first) {
          first = false;
          return Promise.reject(new TypeError('lost ACK'));
        }
        return json(controlReceipt(JSON.parse(options.body)), 202);
      }
      if (path.includes('/commands/')) return json(error('not_found'), 404);
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Разрешить один раз' }),
    );
    await screen.findByText(/Результат запроса пока не подтверждён/);
    expect(screen.queryByRole('button', { name: 'Отклонить' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Проверить запрос' }));
    await screen.findByText(/Запрос не найден/);
    expect(
      screen.getAllByRole('button', { name: 'Повторить запрос' }),
    ).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: 'Повторить запрос' }));
    await screen.findByText(/Агент принял запрос/);
    expect(posted).toHaveLength(2);
    expect(posted[1]).toBe(posted[0]);
    expect(JSON.parse(posted[1])).toMatchObject({
      kind: 'approval.respond',
      payload: { decision: 'allow_once', actionHash: '7'.repeat(64) },
    });
  });

  it('keeps the archive watermark behind a newer SSE event', async () => {
    const eventReads: string[] = [];
    let snapshotReads = 0;
    const baseline = u2Snapshot('active');
    baseline.lastEventSeq = 199;
    installFetch((path) => {
      if (path.endsWith(`/${node1}/snapshot`)) {
        snapshotReads += 1;
        return snapshotReads === 1
          ? json(baseline)
          : new Promise<Response>(() => undefined);
      }
      if (path.includes(`/${node1}/dialogs?`)) {
        return json({ ...dialogPage(node1), lastEventSeq: 199 });
      }
      if (path.includes(`/dialogs/${dialog1}/messages`)) {
        return json({ ...historyPage(node1, dialog1), lastEventSeq: 199 });
      }
      if (path.includes(`/${node1}/requests?`)) {
        return json({ ...requestPage('active'), lastEventSeq: 199 });
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        return json({ ...attemptPage('running'), lastEventSeq: 199 });
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        eventReads.push(path);
        return path.includes('after=0')
          ? json({
              ...eventPage(
                [
                  attemptEvent('tool.started', 100, {
                    callId,
                    toolName: 'archive-page-one',
                    actionHash: '8'.repeat(64),
                    input: {
                      kind: 'inline',
                      content: 'archive 100',
                      redaction: 'none',
                      truncated: false,
                    },
                  }),
                ],
                'more',
              ),
              lastEventSeq: 199,
            })
          : json({
              ...eventPage([
                attemptEvent('tool.output', 101, {
                  callId,
                  chunkIndex: 1,
                  stream: 'result',
                  output: {
                    kind: 'inline',
                    content: 'archive 101',
                    redaction: 'none',
                    truncated: false,
                  },
                }),
              ]),
              lastEventSeq: 199,
            });
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByText('archive 100');
    const source = await firstEventSource();
    await act(async () => {
      source.emit(
        attemptEvent('tool.completed', 200, {
          callId,
          status: 'succeeded',
          result: {
            kind: 'inline',
            content: 'live 200',
            redaction: 'none',
            truncated: false,
          },
          effectStatus: 'none',
        }),
      );
    });
    expect(screen.getByText('live 200')).toBeDefined();
    fireEvent.click(
      screen.getByRole('button', { name: 'Загрузить ещё события' }),
    );
    await screen.findByText('archive 101');
    expect(screen.getByText('live 200')).toBeDefined();
    expect(eventReads).toEqual([
      `/api/v2/harness/nodes/${node1}/attempts/${attemptId}/events?after=0&limit=100`,
      `/api/v2/harness/nodes/${node1}/attempts/${attemptId}/events?after=100&limit=100`,
    ]);
  });

  it('pages the tool timeline explicitly and verifies event-bound artifact metadata', async () => {
    const bytes = new TextEncoder().encode('ok');
    const sha256 = createHash('sha256').update(bytes).digest('hex');
    const eventReads: string[] = [];
    let unexpectedCallId = true;
    let binaryReads = 0;
    const NativeURL = URL;
    const createObjectURL = vi.fn(() => 'blob:u2-artifact');
    const revokeObjectURL = vi.fn();
    class TestURL extends NativeURL {
      static createObjectURL = createObjectURL;
      static revokeObjectURL = revokeObjectURL;
    }
    vi.stubGlobal('URL', TestURL);
    const downloadClick = vi
      .spyOn(HTMLAnchorElement.prototype, 'click')
      .mockImplementation(() => undefined);
    installFetch((path) => {
      if (path.endsWith(`/${node1}/snapshot`)) {
        return json(u2Snapshot('active'));
      }
      if (path.includes(`/${node1}/requests?`)) {
        return json(requestPage('active'));
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        return json(attemptPage('running'));
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        eventReads.push(path);
        return path.includes('after=0')
          ? json(
              eventPage(
                [
                  attemptEvent('tool.started', 15, {
                    callId,
                    toolName: 'synthetic',
                    actionHash: '4'.repeat(64),
                    input: {
                      kind: 'inline',
                      content: 'marker',
                      redaction: 'applied',
                      truncated: true,
                    },
                  }),
                  attemptEvent('artifact.available', 22, {
                    artifactId: 'b0000000-0000-4000-8000-000000000001',
                    name: 'result.txt',
                    mediaType: 'text/plain',
                    sizeBytes: bytes.byteLength,
                    sha256,
                    redaction: 'applied',
                    truncated: false,
                  }),
                ],
                'more',
              ),
            )
          : json(
              eventPage([
                attemptEvent('tool.completed', 23, {
                  callId,
                  status: 'succeeded',
                  result: {
                    kind: 'inline',
                    content: 'done',
                    redaction: 'none',
                    truncated: false,
                  },
                  effectStatus: 'none',
                }),
              ]),
            );
      }
      if (
        path.endsWith(
          '/artifacts/b0000000-0000-4000-8000-000000000001/metadata',
        )
      ) {
        return json({
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          nodeId: node1,
          dialogId: dialog1,
          attemptId,
          artifactId: 'b0000000-0000-4000-8000-000000000001',
          ...(unexpectedCallId ? { callId } : {}),
          name: 'result.txt',
          mediaType: 'text/plain',
          sizeBytes: bytes.byteLength,
          sha256,
          redaction: 'applied',
          truncated: false,
          disposition: 'attachment',
        });
      }
      if (path.endsWith('/artifacts/b0000000-0000-4000-8000-000000000001')) {
        binaryReads += 1;
        return new Response(bytes, {
          headers: {
            'Content-Type': 'application/octet-stream',
            'Content-Disposition': 'attachment; filename="result.txt"',
          },
        });
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    await screen.findByText('Фильтрация: applied; сокращено: да');
    expect(await screen.findAllByText(/Часть данных скрыта/)).not.toHaveLength(
      0,
    );
    fireEvent.click(
      screen.getByRole('button', { name: 'Загрузить ещё события' }),
    );
    await screen.findByText('done');
    expect(eventReads).toEqual([
      `/api/v2/harness/nodes/${node1}/attempts/${attemptId}/events?after=0&limit=100`,
      `/api/v2/harness/nodes/${node1}/attempts/${attemptId}/events?after=22&limit=100`,
    ]);
    fireEvent.click(screen.getByRole('button', { name: /Скачать артефакт/ }));
    await screen.findByText('Описание файла не совпадает с сообщением.');
    expect(binaryReads).toBe(0);
    unexpectedCallId = false;
    fireEvent.click(screen.getByRole('button', { name: /Скачать артефакт/ }));
    await waitFor(() => expect(downloadClick).toHaveBeenCalledOnce());
    expect(createObjectURL).toHaveBeenCalledOnce();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:u2-artifact');
    expect(binaryReads).toBe(1);
  });

  it('loads request and attempt archives through their exact next cursors', async () => {
    const archivedRequestId = '50000000-0000-4000-8000-000000000002';
    const archivedAttemptId = '60000000-0000-4000-8000-000000000002';
    const requestReads: string[] = [];
    const attemptReads: string[] = [];
    installFetch((path) => {
      if (path.endsWith(`/${node1}/snapshot`)) {
        return json(u2Snapshot('active'));
      }
      if (path.includes(`/${node1}/requests?`)) {
        requestReads.push(path);
        if (path.includes('cursor=request-next')) {
          return json({
            ...requestPage('active'),
            items: [
              {
                requestId: archivedRequestId,
                dialogId: dialog1,
                inputMessageId: '40000000-0000-4000-8000-000000000002',
                queueSequence: 2,
                version: 1,
                status: 'completed',
              },
            ],
            nextCursor: null,
          });
        }
        return json({ ...requestPage('active'), nextCursor: 'request-next' });
      }
      if (path.includes(`/requests/${requestId}/attempts?`)) {
        attemptReads.push(path);
        if (path.includes('cursor=attempt-next')) {
          return json({
            ...attemptPage('running'),
            items: [
              {
                attemptId: archivedAttemptId,
                dialogId: dialog1,
                requestId,
                generation: 1,
                version: 1,
                state: 'failed',
                effectStatus: 'none',
                startedAt: '2026-09-09T00:00:00Z',
                finishedAt: '2026-09-09T00:01:00Z',
              },
            ],
            nextCursor: null,
          });
        }
        return json({ ...attemptPage('running'), nextCursor: 'attempt-next' });
      }
      if (path.includes(`/attempts/${attemptId}/events?`)) {
        return json(eventPage([]));
      }
      if (path.includes(`/attempts/${archivedAttemptId}/events?`)) {
        return json({ ...eventPage([]), attemptId: archivedAttemptId });
      }
      return undefined;
    });
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    fireEvent.click(
      await screen.findByRole('button', { name: 'Загрузить ещё поручения' }),
    );
    await screen.findByRole('option', { name: 'Поручение 2 · завершено' });
    fireEvent.click(
      screen.getByRole('button', { name: 'Загрузить ещё попытки' }),
    );
    await screen.findByRole('option', { name: /Запуск 1 · ошибка/ });
    expect(requestReads).toEqual([
      `/api/v2/harness/nodes/${node1}/requests?limit=100`,
      `/api/v2/harness/nodes/${node1}/requests?limit=100&cursor=request-next`,
    ]);
    expect(attemptReads).toEqual([
      `/api/v2/harness/nodes/${node1}/requests/${requestId}/attempts?limit=100`,
      `/api/v2/harness/nodes/${node1}/requests/${requestId}/attempts?limit=100&cursor=attempt-next`,
    ]);
  });

  it('preserves two drafts and the exact chat target through 20 section switches and refresh', async () => {
    const fetcher = installFetch((path) =>
      path === '/api/v2/session' ? json(session) : undefined,
    );
    const first = render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', {
        name: 'Открыть диалог Harness Node One',
      }),
    );
    const field = await screen.findByLabelText('Сообщение агенту');
    fireEvent.change(field, { target: { value: 'черновик первого диалога' } });
    fireEvent.click(
      screen.getByRole('button', { name: /Открыть диалог Dialog 2 node one/ }),
    );
    fireEvent.change(screen.getByLabelText('Сообщение агенту'), {
      target: { value: 'черновик второго диалога' },
    });

    for (let index = 0; index < 20; index += 1) {
      fireEvent.click(
        screen.getByRole('button', { name: 'Открыть управление Harness' }),
      );
      fireEvent.click(
        screen.getByRole('button', { name: 'Открыть раздел общения' }),
      );
    }
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('черновик второго диалога');

    fireEvent.click(
      screen.getByRole('button', { name: 'Открыть управление Harness' }),
    );
    fireEvent.click(
      await screen.findByRole('button', {
        name: 'Выбрать Harness Node Two для управления, свободен',
      }),
    );
    fireEvent.click(
      screen.getByRole('button', { name: 'Открыть раздел общения' }),
    );
    expect(
      screen.getByRole('heading', {
        name: /Node One.*Dialog 2 node one/,
      }),
    ).toBeDefined();
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('черновик второго диалога');

    fireEvent.click(
      screen.getByRole('button', { name: 'Включить светлую тему' }),
    );
    expect(document.documentElement.dataset.theme).toBe('light');
    first.unmount();
    render(<Panel />);

    await screen.findByRole('heading', { name: 'Dialog 2 node one' });
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('черновик второго диалога');
    expect(document.documentElement.dataset.theme).toBe('light');
    fireEvent.click(
      screen.getByRole('button', { name: /Открыть диалог Dialog 1 node one/ }),
    );
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('черновик первого диалога');
    expect(
      fetcher.mock.calls.filter(([, options]) => options?.method === 'POST'),
    ).toHaveLength(0);
  });

  it('checks a restored unknown send at its origin without another POST', async () => {
    const original = {
      protocolVersion: 1 as const,
      schemaId: 'harness-wire-v2' as const,
      commandId,
      kind: 'message.enqueue' as const,
      target: { nodeId: node1, dialogId: dialog1 },
      expected: { dialogVersion: 1 },
      payload: { text: 'пережить обновление' },
    };
    updatePanelSessionState(session.user.id, (current) => ({
      ...current,
      interactionNodeId: node1,
      interaction: {
        nodeId: node1,
        nodeDialogId: dialog1,
        logicalDialogId: dialog1,
        bindingVersion: 1,
      },
      drafts: {
        [dialog1]: {
          text: original.payload.text,
          phase: 'unknown',
          command: original,
        },
      },
    }));
    const fetcher = installFetch((path) =>
      path.endsWith(`/commands/${commandId}`)
        ? json({
            protocolVersion: 1,
            schemaId: 'harness-wire-v2',
            nodeId: node1,
            commandId,
            canonicalPayloadHash: commandHash(original),
            status: 'accepted',
            receipt: receipt(original),
          })
        : undefined,
    );

    render(
      <HarnessWorkspace
        session={session}
        selectedNodeId={node1}
        selectedDialog={{
          nodeId: node1,
          nodeDialogId: dialog1,
          logicalDialogId: dialog1,
          bindingVersion: 1,
        }}
        onExpired={vi.fn()}
      />,
    );

    await screen.findByText('Команда принята в очередь.');
    expect(
      fetcher.mock.calls.filter(
        ([path]) =>
          typeof path === 'string' && path.endsWith(`/commands/${commandId}`),
      ),
    ).toHaveLength(1);
    expect(
      fetcher.mock.calls.filter(([, options]) => options?.method === 'POST'),
    ).toHaveLength(0);
  });

  it('uses Enter once, keeps Shift+Enter local, and ignores Enter during IME composition', async () => {
    const fetcher = installFetch();
    render(<HarnessWorkspace session={session} onExpired={vi.fn()} />);
    const field = await screen.findByLabelText('Сообщение агенту');

    fireEvent.change(field, { target: { value: 'первая команда' } });
    fireEvent.keyDown(field, { key: 'Enter', code: 'Enter' });
    await screen.findByText('Команда принята в очередь.');

    fireEvent.change(field, { target: { value: 'вторая команда' } });
    fireEvent.keyDown(field, { key: 'Enter', code: 'Enter', shiftKey: true });
    fireEvent.compositionStart(field);
    fireEvent.keyDown(field, {
      key: 'Enter',
      code: 'Enter',
      isComposing: true,
    });
    expect(
      fetcher.mock.calls.filter(([, options]) => options?.method === 'POST'),
    ).toHaveLength(1);
    fireEvent.compositionEnd(field);
    fireEvent.keyDown(field, { key: 'Enter', code: 'Enter' });
    await waitFor(() =>
      expect(
        fetcher.mock.calls.filter(([, options]) => options?.method === 'POST'),
      ).toHaveLength(2),
    );
  });

  it('migrates a provisional dialog draft to its canonical logical binding', async () => {
    const provisional = {
      nodeId: node1,
      nodeDialogId: dialog1,
      logicalDialogId: dialog1,
      bindingVersion: 1,
    };
    updatePanelSessionState(session.user.id, (current) => ({
      ...current,
      interactionNodeId: node1,
      interaction: provisional,
      drafts: {
        [dialog1]: { text: 'черновик до появления привязки', phase: 'draft' },
      },
    }));
    installFetch((path) =>
      path === `/api/v2/agents/${node1}/dialogs?limit=100`
        ? json(
            bindingPage(node1, [
              {
                nodeDialogId: dialog1,
                logicalDialogId: logicalDialog1,
                bindingVersion: 1,
              },
            ]),
          )
        : undefined,
    );
    const changed = vi.fn();
    render(
      <HarnessWorkspace
        session={{ ...session, inventory_enabled: true }}
        selectedNodeId={node1}
        selectedDialog={provisional}
        onSelectionChange={changed}
        onExpired={vi.fn()}
      />,
    );

    const field = await screen.findByLabelText('Сообщение агенту');
    await waitFor(() =>
      expect((field as HTMLTextAreaElement).value).toBe(
        'черновик до появления привязки',
      ),
    );
    const saved = readPanelSessionState(session.user.id).state.drafts;
    expect(saved[logicalDialog1]?.text).toBe('черновик до появления привязки');
    expect(saved[dialog1]).toBeUndefined();
    expect(changed).toHaveBeenLastCalledWith({
      nodeId: node1,
      nodeDialogId: dialog1,
      logicalDialogId: logicalDialog1,
      bindingVersion: 1,
    });
  });

  it('moves to a same-node rebound after stale 409 without posting twice', async () => {
    let bindingReads = 0;
    const posted: HarnessCommand[] = [];
    const fetcher = installFetch((path, options) => {
      if (path === `/api/v2/agents/${node1}/dialogs?limit=100`) {
        bindingReads += 1;
        return json(
          bindingPage(node1, [
            {
              nodeDialogId: bindingReads === 1 ? dialog1 : dialog2,
              logicalDialogId: logicalDialog1,
              bindingVersion: bindingReads === 1 ? 1 : 2,
            },
          ]),
        );
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        posted.push(JSON.parse(options.body));
        return json(error('stale'), 409);
      }
      return undefined;
    });
    const changed = vi.fn();
    render(
      <HarnessWorkspace
        session={{ ...session, inventory_enabled: true }}
        selectedNodeId={node1}
        selectedDialog={{
          nodeId: node1,
          nodeDialogId: dialog1,
          logicalDialogId: logicalDialog1,
          bindingVersion: 1,
        }}
        onSelectionChange={changed}
        onExpired={vi.fn()}
      />,
    );
    const field = await screen.findByLabelText('Сообщение агенту');
    fireEvent.change(field, { target: { value: 'проверить новую привязку' } });
    fireEvent.click(screen.getByRole('button', { name: 'Отправить' }));

    await screen.findByText(/Адресат или версия диалога изменились/);
    await screen.findByRole('heading', { name: 'Dialog 2 node one' });
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('проверить новую привязку');
    expect(changed).toHaveBeenLastCalledWith({
      nodeId: node1,
      nodeDialogId: dialog2,
      logicalDialogId: logicalDialog1,
      bindingVersion: 2,
    });
    expect(posted).toHaveLength(1);
    expect(posted[0]).toMatchObject({
      kind: 'message.enqueue',
      target: { nodeId: node1, dialogId: dialog1 },
    });
    expect(bindingReads).toBe(2);
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(
      fetcher.mock.calls.filter(([, options]) => options?.method === 'POST'),
    ).toHaveLength(1);
  });

  it('expires and clears the session when binding recovery gets 401 after 409', async () => {
    let bindingReads = 0;
    const expired = vi.fn(() => clearPanelSessionState(session.user.id));
    const fetcher = installFetch((path, options) => {
      if (path === `/api/v2/agents/${node1}/dialogs?limit=100`) {
        bindingReads += 1;
        return bindingReads === 1
          ? json(
              bindingPage(node1, [
                {
                  nodeDialogId: dialog1,
                  logicalDialogId: logicalDialog1,
                  bindingVersion: 1,
                },
              ]),
            )
          : json({ error: 'edge_authentication_required' }, 401);
      }
      if (path.endsWith('/commands') && options?.method === 'POST') {
        return json(error('stale'), 409);
      }
      return undefined;
    });
    render(
      <HarnessWorkspace
        session={{ ...session, inventory_enabled: true }}
        selectedNodeId={node1}
        selectedDialog={{
          nodeId: node1,
          nodeDialogId: dialog1,
          logicalDialogId: logicalDialog1,
          bindingVersion: 1,
        }}
        onExpired={expired}
      />,
    );
    const field = await screen.findByLabelText('Сообщение агенту');
    fireEvent.change(field, { target: { value: 'не сохранять после expiry' } });
    fireEvent.click(screen.getByRole('button', { name: 'Отправить' }));

    await waitFor(() => expect(expired).toHaveBeenCalledOnce());
    expect(readPanelSessionState(session.user.id).state.drafts).toEqual({});
    expect(bindingReads).toBe(2);
    expect(
      fetcher.mock.calls.filter(([, options]) => options?.method === 'POST'),
    ).toHaveLength(1);
  });

  it('does not restore a pending send after logout clears the session', async () => {
    const pendingSend = deferred<Response>();
    const pendingLogout = deferred<Response>();
    let posted: HarnessCommand | null = null;
    installFetch((path, options) => {
      if (path === '/api/v2/session') return json(session);
      if (path.endsWith('/commands') && options?.method === 'POST') {
        if (typeof options.body !== 'string') throw new Error('missing body');
        posted = JSON.parse(options.body) as HarnessCommand;
        return pendingSend.promise;
      }
      if (path === '/api/v2/logout' && options?.method === 'POST') {
        return pendingLogout.promise;
      }
      return undefined;
    });
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', {
        name: 'Открыть диалог Harness Node One',
      }),
    );
    const field = await screen.findByLabelText('Сообщение агенту');
    fireEvent.change(field, { target: { value: 'секретный черновик' } });
    fireEvent.click(screen.getByRole('button', { name: 'Отправить' }));
    await waitFor(() => expect(posted).not.toBeNull());
    expect(
      Object.keys(readPanelSessionState(session.user.id).state.drafts),
    ).toHaveLength(1);

    fireEvent.click(screen.getByRole('button', { name: 'Выйти из Panel' }));
    expect(readPanelSessionState(session.user.id).state.drafts).toEqual({});

    await act(async () => {
      if (!posted) throw new Error('command was not posted');
      pendingSend.resolve(json(receipt(posted), 202));
      await pendingSend.promise;
      await Promise.resolve();
    });
    expect(readPanelSessionState(session.user.id).state.drafts).toEqual({});
    expect(sessionStorage.getItem('homelab-panel:r03:1-1')).toBeNull();

    await act(async () => {
      pendingLogout.resolve(json({ logged_out: true }));
      await pendingLogout.promise;
      await Promise.resolve();
    });
  });
});
