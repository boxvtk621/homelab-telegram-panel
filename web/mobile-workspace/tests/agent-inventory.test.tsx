import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import {
  harnessAPI,
  type HarnessNode,
  type HarnessSnapshot,
} from '../src/harness-api';
import { HarnessManagement } from '../src/harness-management';
import type {
  InventoryItem,
  InventoryStatus,
} from '../src/agent-inventory-api';
import { inventoryAPI } from '../src/agent-inventory-api';

const session = {
  user: { id: 'owner-1', login: 'owner', name: 'owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
  inventory_enabled: true,
};

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function uuid(prefix: string, index: number): string {
  return `${prefix}-0000-4000-8000-${index.toString().padStart(12, '0')}`;
}

function item(index: number, status: InventoryStatus): InventoryItem {
  const observed =
    status === 'unknown' || status === 'readonly'
      ? null
      : '2026-09-14T10:00:00Z';
  const sendAllowed = status === 'online' || status === 'busy';
  return {
    nodeId: uuid('20000000', index),
    name: `Agent ${index.toString().padStart(3, '0')}`,
    engine: index % 2 === 0 ? 'cursor' : 'codex',
    sourceMode: 'fixture',
    host: {
      hostId: uuid('10000000', ((index - 1) % 10) + 1),
      name: `Mac ${(((index - 1) % 10) + 1).toString().padStart(2, '0')}`,
    },
    registrationMode: status === 'readonly' ? 'legacy_readonly' : 'compatible',
    status,
    state: {
      process:
        status === 'stopped'
          ? 'stopped'
          : status === 'unknown'
            ? 'unknown'
            : 'running',
      connection:
        status === 'stopped'
          ? 'offline'
          : status === 'unknown'
            ? 'unknown'
            : 'online',
      readiness:
        status === 'unready'
          ? 'unready'
          : status === 'unknown' || status === 'stopped'
            ? 'unknown'
            : 'ready',
      occupancy:
        status === 'busy'
          ? 'busy'
          : status === 'unknown' || status === 'stopped'
            ? 'unknown'
            : 'idle',
    },
    observedAt: observed,
    source: observed === null ? null : 'registry-import',
    pendingCount:
      index === 3 && observed !== null
        ? { value: 2, observedAt: observed, source: 'registry-import' }
        : null,
    actions: {
      openWorkspace: { allowed: true },
      sendMessage: sendAllowed
        ? { allowed: true }
        : {
            allowed: false,
            reason:
              status === 'readonly'
                ? 'readonly_registration'
                : 'state_unavailable',
            nextAction:
              status === 'readonly'
                ? 'Обновите регистрацию Harness до совместимой версии.'
                : 'Получите подтверждённое состояние Harness.',
          },
      lifecycle: {
        allowed: false,
        reason: 'r01_read_only',
        nextAction: 'Управление lifecycle появится в следующих этапах.',
      },
    },
    dialogCount: 1,
  };
}

function legacySnapshot(nodeId: string): HarnessSnapshot {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    nodeId,
    epoch: 1,
    stateVersion: 1,
    lastEventSeq: 0,
    capturedAt: '2026-09-14T10:00:00Z',
    node: {
      transportAvailability: 'online',
      engineReadiness: 'ready',
      occupancy: 'idle',
      queuePaused: false,
      queueVersion: 1,
      pendingCount: 0,
      blockedReasons: [],
      activeAttemptId: null,
    },
    pendingQueue: [],
    activeAttempt: null,
    completeness: 'complete',
  };
}

it('renders 100 agents across 10 hosts without per-Harness fan-out', async () => {
  const statuses: InventoryStatus[] = [
    'online',
    'unready',
    'busy',
    'stale',
    'stopped',
    'unknown',
    'readonly',
  ];
  const items = Array.from({ length: 100 }, (_, offset) =>
    item(offset + 1, statuses[offset] ?? 'online'),
  );
  items[0].dialogCount = 20_000;
  const fetcher = vi.fn((url: string) => {
    if (url === '/api/v2/agents?limit=100') {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            schemaId: 'agent-management-v1',
            items,
            nextCursor: null,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      );
    }
    throw new Error(`unexpected endpoint ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  const onOpen = vi.fn();
  const view = render(
    <HarnessManagement
      session={session}
      selectedNodeId=""
      onOpen={onOpen}
      onExpired={vi.fn()}
    />,
  );

  await waitFor(() =>
    expect(view.container.querySelectorAll('.agent-card')).toHaveLength(100),
  );
  expect(new Set(items.map((agent) => agent.host.hostId)).size).toBe(10);
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(
    fetcher.mock.calls.some(([url]) => String(url).includes('/harness/')),
  ).toBe(false);
  for (const label of [
    'Agent 001, на связи',
    'Agent 002, не готов',
    'Agent 003, занят',
    'Agent 004, данные устарели',
    'Agent 005, остановлен',
    'Agent 006, неизвестно',
    'Agent 007, только чтение',
  ]) {
    expect(screen.getByLabelText(label)).toBeDefined();
  }
  expect(screen.getAllByText('неизвестно').length).toBeGreaterThan(0);
  expect(screen.getByText('20000')).toBeDefined();
  expect(
    screen.getByText('Обновите регистрацию Harness до совместимой версии.'),
  ).toBeDefined();

  fireEvent.click(
    screen.getByRole('button', { name: 'Перейти к агенту Agent 001' }),
  );
  expect(onOpen).toHaveBeenCalledWith(uuid('20000000', 1));
});

it('polls every five seconds and ages observations locally after failures', async () => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date('2026-09-14T10:00:05Z'));
  const online = item(1, 'online');
  let reads = 0;
  const fetcher = vi.fn(() => {
    reads += 1;
    if (reads > 1) return Promise.reject(new Error('offline'));
    return Promise.resolve(
      new Response(
        JSON.stringify({
          schemaId: 'agent-management-v1',
          items: [online],
          nextCursor: null,
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
  });
  vi.stubGlobal('fetch', fetcher);
  render(
    <HarnessManagement
      session={session}
      selectedNodeId=""
      onOpen={vi.fn()}
      onExpired={vi.fn()}
    />,
  );

  await act(async () => {
    for (let index = 0; index < 8; index += 1) await Promise.resolve();
  });
  expect(screen.getByLabelText('Agent 001, на связи')).toBeDefined();

  await act(async () => {
    await vi.advanceTimersByTimeAsync(16_000);
  });
  expect(fetcher.mock.calls.length).toBeGreaterThan(1);
  expect(screen.getByLabelText('Agent 001, данные устарели')).toBeDefined();
  expect(
    screen.getByText('Обновите состояние перед отправкой сообщения.'),
  ).toBeDefined();
});

it('loads inventory beyond the 100-agent test scale through bounded pages', async () => {
  const items = Array.from({ length: 101 }, (_, offset) =>
    item(offset + 1, 'online'),
  );
  const fetcher = vi.fn((url: string) => {
    const response =
      url === '/api/v2/agents?limit=100'
        ? { items: items.slice(0, 100), nextCursor: 'page-2' }
        : url === '/api/v2/agents?limit=100&cursor=page-2'
          ? { items: items.slice(100), nextCursor: null }
          : null;
    if (!response) throw new Error(`unexpected endpoint ${url}`);
    return Promise.resolve(
      new Response(
        JSON.stringify({ schemaId: 'agent-management-v1', ...response }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
  });
  vi.stubGlobal('fetch', fetcher);
  const view = render(
    <HarnessManagement
      session={session}
      selectedNodeId=""
      onOpen={vi.fn()}
      onExpired={vi.fn()}
    />,
  );

  await waitFor(() =>
    expect(view.container.querySelectorAll('.agent-card')).toHaveLength(101),
  );
  expect(fetcher).toHaveBeenCalledTimes(2);
});

it('bounds the legacy per-Harness fallback below the Panel concurrency gate', async () => {
  const nodes: HarnessNode[] = Array.from({ length: 100 }, (_, offset) => ({
    nodeId: uuid('20000000', offset + 1),
    name: `Legacy ${offset + 1}`,
    adapter: offset % 2 === 0 ? 'cursor' : 'codex',
  }));
  vi.spyOn(harnessAPI, 'nodes').mockResolvedValue({
    registryVersion: 1,
    mode: 'fixture',
    nodes,
  });
  let active = 0;
  let peak = 0;
  const snapshot = vi
    .spyOn(harnessAPI, 'snapshot')
    .mockImplementation(async (_session, nodeId) => {
      active += 1;
      peak = Math.max(peak, active);
      await new Promise((resolve) => setTimeout(resolve, 1));
      active -= 1;
      return legacySnapshot(nodeId);
    });

  const view = render(
    <HarnessManagement
      session={{ ...session, inventory_enabled: false }}
      selectedNodeId=""
      onOpen={vi.fn()}
      onExpired={vi.fn()}
    />,
  );

  await waitFor(
    () =>
      expect(view.container.querySelectorAll('.agent-card')).toHaveLength(100),
    { timeout: 5_000 },
  );
  expect(snapshot).toHaveBeenCalledTimes(100);
  expect(peak).toBeGreaterThan(1);
  expect(peak).toBeLessThanOrEqual(4);
});

it('rejects inventory metrics without matching provenance', async () => {
  const invalid = item(1, 'online');
  invalid.pendingCount = {
    value: 0,
    observedAt: invalid.observedAt!,
    source: 'different-source',
  };
  vi.stubGlobal(
    'fetch',
    vi.fn(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            schemaId: 'agent-management-v1',
            items: [invalid],
            nextCursor: null,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      ),
    ),
  );
  render(
    <HarnessManagement
      session={session}
      selectedNodeId=""
      onOpen={vi.fn()}
      onExpired={vi.fn()}
    />,
  );
  expect(
    await screen.findByText('Реестр вернул ответ неизвестного формата.'),
  ).toBeDefined();
});

it('reads exact paginated logical dialog bindings for one node', async () => {
  const nodeId = uuid('20000000', 1);
  const firstDialog = uuid('30000000', 1);
  const secondDialog = uuid('30000000', 2);
  const fetcher = vi.fn((url: string) => {
    const page =
      url === `/api/v2/agents/${nodeId}/dialogs?limit=100`
        ? {
            items: [
              {
                nodeDialogId: firstDialog,
                logicalDialogId: uuid('40000000', 1),
                bindingVersion: 1,
              },
            ],
            nextCursor: 'second',
          }
        : url === `/api/v2/agents/${nodeId}/dialogs?limit=100&cursor=second`
          ? {
              items: [
                {
                  nodeDialogId: secondDialog,
                  logicalDialogId: uuid('40000000', 2),
                  bindingVersion: 2,
                },
              ],
              nextCursor: null,
            }
          : null;
    if (!page) throw new Error(`unexpected endpoint ${url}`);
    return Promise.resolve(
      new Response(
        JSON.stringify({
          schemaId: 'agent-dialog-bindings-v1',
          nodeId,
          ...page,
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
  });
  vi.stubGlobal('fetch', fetcher);

  await expect(inventoryAPI.dialogBindings(nodeId)).resolves.toEqual([
    {
      nodeId,
      nodeDialogId: firstDialog,
      logicalDialogId: uuid('40000000', 1),
      bindingVersion: 1,
    },
    {
      nodeId,
      nodeDialogId: secondDialog,
      logicalDialogId: uuid('40000000', 2),
      bindingVersion: 2,
    },
  ]);
  expect(fetcher).toHaveBeenCalledTimes(2);
});
