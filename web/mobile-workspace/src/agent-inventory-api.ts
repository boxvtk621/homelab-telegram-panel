import { api, APIError } from './panel-api';

export type InventoryStatus =
  | 'online'
  | 'unready'
  | 'busy'
  | 'stale'
  | 'stopped'
  | 'unknown'
  | 'readonly';

export type InventoryAction = {
  allowed: boolean;
  reason?: string;
  nextAction?: string;
};

export type InventoryItem = {
  nodeId: string;
  name: string;
  engine: 'cursor' | 'codex';
  sourceMode: 'live' | 'fixture';
  host: { hostId: string; name: string };
  registrationMode: 'legacy_readonly' | 'compatible';
  status: InventoryStatus;
  state: {
    process: 'running' | 'stopped' | 'unknown';
    connection: 'online' | 'offline' | 'unknown';
    readiness: 'ready' | 'unready' | 'unknown';
    occupancy: 'idle' | 'busy' | 'unknown';
  };
  observedAt: string | null;
  source: string | null;
  pendingCount: {
    value: number;
    observedAt: string;
    source: string;
  } | null;
  actions: {
    openWorkspace: InventoryAction;
    sendMessage: InventoryAction;
    lifecycle: InventoryAction;
  };
  dialogCount: number;
};

type InventoryPage = {
  schemaId: 'agent-management-v1';
  items: InventoryItem[];
  nextCursor: string | null;
};

const uuid =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const object = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);
const text = (value: unknown, maximum: number): value is string =>
  typeof value === 'string' &&
  value.length > 0 &&
  new TextEncoder().encode(value).length <= maximum &&
  !value.includes('\u0000') &&
  !value.includes('\r') &&
  !value.includes('\n');
const exactKeys = (value: Record<string, unknown>, keys: string[]) =>
  Object.keys(value).length === keys.length &&
  keys.every((key) => key in value);
const oneOf = <T extends string>(
  value: unknown,
  values: readonly T[],
): value is T => typeof value === 'string' && values.includes(value as T);
const safeInteger = (value: unknown): value is number =>
  typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;

function validAction(value: unknown): value is InventoryAction {
  if (!object(value) || typeof value.allowed !== 'boolean') return false;
  if (value.allowed) return exactKeys(value, ['allowed']);
  return (
    exactKeys(value, ['allowed', 'reason', 'nextAction']) &&
    text(value.reason, 100) &&
    text(value.nextAction, 500)
  );
}

function validItem(value: unknown): value is InventoryItem {
  if (
    !object(value) ||
    !exactKeys(value, [
      'nodeId',
      'name',
      'engine',
      'sourceMode',
      'host',
      'registrationMode',
      'status',
      'state',
      'observedAt',
      'source',
      'pendingCount',
      'actions',
      'dialogCount',
    ]) ||
    typeof value.nodeId !== 'string' ||
    !uuid.test(value.nodeId) ||
    !text(value.name, 200) ||
    !oneOf(value.engine, ['cursor', 'codex'] as const) ||
    !oneOf(value.sourceMode, ['live', 'fixture'] as const) ||
    !oneOf(value.registrationMode, [
      'legacy_readonly',
      'compatible',
    ] as const) ||
    !oneOf(value.status, [
      'online',
      'unready',
      'busy',
      'stale',
      'stopped',
      'unknown',
      'readonly',
    ] as const) ||
    !object(value.host) ||
    !exactKeys(value.host, ['hostId', 'name']) ||
    typeof value.host.hostId !== 'string' ||
    !uuid.test(value.host.hostId) ||
    !text(value.host.name, 200) ||
    !object(value.state) ||
    !exactKeys(value.state, [
      'process',
      'connection',
      'readiness',
      'occupancy',
    ]) ||
    !oneOf(value.state.process, ['running', 'stopped', 'unknown'] as const) ||
    !oneOf(value.state.connection, ['online', 'offline', 'unknown'] as const) ||
    !oneOf(value.state.readiness, ['ready', 'unready', 'unknown'] as const) ||
    !oneOf(value.state.occupancy, ['idle', 'busy', 'unknown'] as const) ||
    !object(value.actions) ||
    !exactKeys(value.actions, ['openWorkspace', 'sendMessage', 'lifecycle']) ||
    !validAction(value.actions.openWorkspace) ||
    !validAction(value.actions.sendMessage) ||
    !validAction(value.actions.lifecycle) ||
    !safeInteger(value.dialogCount)
  )
    return false;

  const observed =
    typeof value.observedAt === 'string' &&
    !Number.isNaN(Date.parse(value.observedAt)) &&
    text(value.source, 100);
  if (!observed && (value.observedAt !== null || value.source !== null))
    return false;
  if (value.pendingCount !== null) {
    if (
      !object(value.pendingCount) ||
      !exactKeys(value.pendingCount, ['value', 'observedAt', 'source']) ||
      !safeInteger(value.pendingCount.value) ||
      value.pendingCount.observedAt !== value.observedAt ||
      value.pendingCount.source !== value.source
    )
      return false;
  }
  return true;
}

function validatePage(value: unknown): InventoryPage {
  if (
    !object(value) ||
    !exactKeys(value, ['schemaId', 'items', 'nextCursor']) ||
    value.schemaId !== 'agent-management-v1' ||
    !Array.isArray(value.items) ||
    value.items.length > 100 ||
    !value.items.every(validItem) ||
    (value.nextCursor !== null && !text(value.nextCursor, 512))
  )
    throw new APIError(200, 'invalid_inventory_response');
  return value as InventoryPage;
}

export const inventoryAPI = {
  async all(signal?: AbortSignal): Promise<InventoryItem[]> {
    const result: InventoryItem[] = [];
    const nodes = new Set<string>();
    const cursors = new Set<string>();
    let cursor: string | null = null;
    do {
      const suffix = cursor
        ? `?limit=100&cursor=${encodeURIComponent(cursor)}`
        : '?limit=100';
      const page = validatePage(
        await api<unknown>(`agents${suffix}`, { signal }),
      );
      for (const item of page.items) {
        if (nodes.has(item.nodeId))
          throw new APIError(200, 'invalid_inventory_response');
        nodes.add(item.nodeId);
        result.push(item);
      }
      cursor = page.nextCursor;
      if (cursor !== null) {
        if (cursors.has(cursor))
          throw new APIError(200, 'invalid_inventory_response');
        cursors.add(cursor);
      }
    } while (cursor !== null);
    return result;
  },
};

export function inventoryMessage(cause: unknown): string {
  if (!(cause instanceof APIError))
    return 'Не удалось получить реестр агентов.';
  switch (cause.code) {
    case 'inventory_unavailable':
    case 'database_unavailable':
      return 'Реестр временно недоступен. Повторите обновление.';
    case 'invalid_cursor':
      return 'Список изменился. Обновите реестр с начала.';
    case 'invalid_inventory_response':
      return 'Реестр вернул ответ неизвестного формата.';
    default:
      return 'Не удалось получить реестр агентов.';
  }
}
