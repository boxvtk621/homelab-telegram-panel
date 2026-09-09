import { createHash } from 'node:crypto';
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { harnessAPI } from '../src/harness-api';
import type { HarnessCommand, HarnessSnapshot } from '../src/harness-api';
import { HarnessWorkspace } from '../src/harness-workspace';
import { Panel } from '../src/panel';
import scenarios from '../../../api/harness-v1.scenarios.json';

const node1 = '20000000-0000-4000-8000-000000000001';
const node2 = '20000000-0000-4000-8000-000000000002';
const dialog1 = '30000000-0000-4000-8000-000000000001';
const dialog2 = '30000000-0000-4000-8000-000000000002';
const dialog3 = '30000000-0000-4000-8000-000000000003';
const commandId = '10000000-0000-4000-8000-000000000001';
const schemaHash =
  'a482f087231d1991e140f074cbea35db675fb204fea443808ee253c58bdd5236';
const session = {
  user: { id: '1-1', login: 'owner', name: 'Owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
  youtrack_url: 'https://youtrack.example.test',
  project: 'HL',
};

type FetchExtra = (
  path: string,
  options?: RequestInit,
) => Promise<Response> | Response | undefined;

function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function identity(nodeId: string) {
  const codex = nodeId === node2;
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
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

function historyPage(nodeId: string, dialogId: string, text = dialogId) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
    commandId: command.commandId,
    commandKind: command.kind,
    receiptId: '10000000-0000-4000-8000-000000000002',
    acceptedAt: '2026-09-09T00:00:00Z',
    nodeId: command.target.nodeId,
    eventSeq: 24,
    result: 'admitted',
    references:
      command.kind === 'dialog.create'
        ? { dialogId: dialog3 }
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
    schemaId: 'harness-wire-v1',
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
          schemaId: 'harness-wire-v1',
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
          schemaId: 'harness-wire-v1',
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

function nodeEvent(seq: number, epoch: number) {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
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
    schemaId: 'harness-wire-v1',
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
            schemaId: 'harness-wire-v1',
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
      'd9786de4e07f2cfc48793dc310cff12902d18e15c9615b06134bfd5a99f71a67',
    ],
    ['é', '927b48e83d129abc2f4d38b886fc340f9219cb62cc1c2082387399b279980496'],
    [
      'e\u0301',
      'bf027062592cb056b2edf7294597bb1ba7829433ab0a39096a3345c75fb538d5',
    ],
  ])('preserves Unicode command bytes for hash %s', async (text, hash) => {
    const command = {
      protocolVersion: 1 as const,
      schemaId: 'harness-wire-v1' as const,
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
            schemaId: 'harness-wire-v1',
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
            schemaId: 'harness-wire-v1',
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
        schemaId: 'harness-wire-v1',
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
    expect(screen.getByText(/данные синтетические/)).toBeDefined();
    expect(screen.getByText('Доступность')).toBeDefined();
    expect(screen.getByText(/позиция 1 · диалог/)).toBeDefined();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].url).toBe(
      `/api/v2/harness/nodes/${node1}/events?after=23`,
    );
    expect(FakeEventSource.instances[0].withCredentials).toBe(true);
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
    const selector = await screen.findByLabelText('Нода');
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
          schemaId: 'harness-wire-v1',
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
    vi.useFakeTimers();
    try {
      await act(async () =>
        FakeEventSource.instances[0].emit(nodeEvent(24, 1)),
      );
      await act(async () => {
        fireEvent.change(screen.getByLabelText('Нода'), {
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
    await screen.findByText(/Исход неизвестен; commandId/);
    fireEvent.change(screen.getByLabelText('Нода'), {
      target: { value: node2 },
    });
    await screen.findByRole('heading', { name: 'Dialog 1 node two' });
    fireEvent.change(screen.getByLabelText('Нода'), {
      target: { value: node1 },
    });
    await screen.findByText(/Исход неизвестен; commandId/);
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
          schemaId: 'harness-wire-v1',
          nodeId: node1,
          commandId,
          canonicalPayloadHash: wrongHash
            ? '0'.repeat(64)
            : 'deb49f2c3c0a93eeae13e26014a87eae8919a4c8f63a0d05acd9416a7a3f6a69',
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
    await screen.findByText(/Исход неизвестен; commandId/);
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
    const first = FakeEventSource.instances[0];
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
    FakeEventSource.instances[0].fail();
    await waitFor(() => expect(expired).toHaveBeenCalledOnce());
    expect(FakeEventSource.instances[0].closed).toBe(true);
    expect(
      fetcher.mock.calls.some(([, options]) => options?.method === 'POST'),
    ).toBe(false);
  });

  it('aborts an old session probe before its late 401 reaches a new session', async () => {
    let resolveReady!: (response: Response) => void;
    const delayedReady = new Promise<Response>((resolve) => {
      resolveReady = resolve;
    });
    installFetch((path) =>
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
    FakeEventSource.instances[0].fail();
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

  it('keeps a Harness draft mounted while Panel shows YouTrack', async () => {
    installFetch((path) => {
      if (path === '/api/v2/session') return json(session);
      if (path === '/api/v2/issues?skip=0') {
        return json({ data: [], observed_at: '2026-09-09T00:00:00Z' });
      }
      if (path === '/api/v2/agent/runs') {
        return json({ runs: [], durable: false, model: 'synthetic' });
      }
      return undefined;
    });
    render(<Panel />);
    fireEvent.click(await screen.findByRole('button', { name: 'Harness' }));
    const field = await screen.findByLabelText('Сообщение агенту');
    fireEvent.change(field, { target: { value: 'in-memory only' } });
    fireEvent.click(screen.getByRole('button', { name: 'Задачи' }));
    fireEvent.click(screen.getByRole('button', { name: 'Harness' }));
    expect(
      (screen.getByLabelText('Сообщение агенту') as HTMLTextAreaElement).value,
    ).toBe('in-memory only');
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
          schemaId: 'harness-wire-v1',
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
    await screen.findByText('Metadata артефакта не совпадает с сообщением.');
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
      await screen.findByRole('button', { name: 'Остановить попытку' }),
    );
    await screen.findByText(/Исход не подтверждён; commandId/);
    fireEvent.change(screen.getByLabelText('Нода'), {
      target: { value: node2 },
    });
    await screen.findByRole('heading', { name: 'Dialog 1 node two' });
    fireEvent.change(screen.getByLabelText('Нода'), {
      target: { value: node1 },
    });
    await screen.findByText(/Исход не подтверждён; commandId/);
    await screen.findByRole('heading', { name: 'Неподтверждённые запросы' });
    expect(
      screen.getByRole('button', { name: 'Остановить попытку' }),
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
    await screen.findByText(/Terminal: failed/);
    expect(screen.queryByText(/^Агент$/)).toBeNull();
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
    await screen.findByText(/Harness принял запрос/);
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
    await act(async () => {
      FakeEventSource.instances[0].emit(
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
    const input = await screen.findByLabelText(
      `Ответ агенту ${inputRequestId}`,
    );
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
    await screen.findByText(/Исход не подтверждён; commandId/);
    expect(screen.queryByRole('button', { name: 'Отклонить' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Проверить запрос' }));
    await screen.findByText(/Запрос не найден/);
    expect(
      screen.getAllByRole('button', { name: 'Повторить запрос' }),
    ).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: 'Повторить запрос' }));
    await screen.findByText(/Harness принял запрос/);
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
    await act(async () => {
      FakeEventSource.instances[0].emit(
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
          schemaId: 'harness-wire-v1',
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
    await screen.findByText('redaction: applied; truncated: да');
    fireEvent.click(
      screen.getByRole('button', { name: 'Загрузить ещё события' }),
    );
    await screen.findByText('done');
    expect(eventReads).toEqual([
      `/api/v2/harness/nodes/${node1}/attempts/${attemptId}/events?after=0&limit=100`,
      `/api/v2/harness/nodes/${node1}/attempts/${attemptId}/events?after=22&limit=100`,
    ]);
    fireEvent.click(screen.getByRole('button', { name: /Скачать артефакт/ }));
    await screen.findByText('Metadata артефакта не совпадает с сообщением.');
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
    await screen.findByRole('option', { name: new RegExp(archivedRequestId) });
    fireEvent.click(
      screen.getByRole('button', { name: 'Загрузить ещё попытки' }),
    );
    await screen.findByRole('option', { name: /generation 1 · failed/ });
    expect(requestReads).toEqual([
      `/api/v2/harness/nodes/${node1}/requests?limit=100`,
      `/api/v2/harness/nodes/${node1}/requests?limit=100&cursor=request-next`,
    ]);
    expect(attemptReads).toEqual([
      `/api/v2/harness/nodes/${node1}/requests/${requestId}/attempts?limit=100`,
      `/api/v2/harness/nodes/${node1}/requests/${requestId}/attempts?limit=100&cursor=attempt-next`,
    ]);
  });
});
