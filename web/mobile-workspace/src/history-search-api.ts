import { api, APIError } from './panel-api';

const uuid =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const timestamp =
  /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?Z$/;
const encoder = new TextEncoder();

export type HistorySearchQuery = {
  q: string;
  nodeId?: string;
  logicalDialogId?: string;
  archived?: boolean;
};

export type HistorySearchItem = {
  logicalDialogId: string;
  entryId: string;
  nodeId: string;
  nodeDialogId: string;
  title: string;
  role: 'user' | 'assistant';
  kind: string;
  createdAt: string;
  rank: number;
  snippet: string;
};

export type HistorySearchPage = {
  schemaId: 'agent-search-v1';
  query: HistorySearchQuery;
  items: HistorySearchItem[];
  nextCursor: string | null;
  totalCount: number;
  observedAt: string;
  lagMillis: number;
  incomplete: boolean;
};

type InlineContent = {
  kind: 'inline';
  content: string;
  redaction?: 'none' | 'applied';
  truncated?: boolean;
};

type ArtifactContent = {
  kind: 'artifact';
  artifactId: string;
  sizeBytes: number;
  sha256: string;
  redaction: 'none' | 'applied';
  truncated: boolean;
};

type UnavailableContent = {
  kind: 'unavailable';
  reason: 'not_observed' | 'provider_redacted' | 'output_limit' | 'unmapped';
  redaction: 'none' | 'applied' | 'unknown';
  truncated: boolean;
};

export type HistoryEntryContent =
  | InlineContent
  | ArtifactContent
  | UnavailableContent;

export type HistoryEntry = {
  entryId: string;
  origin: {
    nodeId: string;
    nodeDialogId: string;
    authorEventId: string;
  };
  logicalDialogId: string;
  messageId: string;
  requestId?: string;
  attemptId?: string;
  kind: string;
  role: 'user' | 'assistant';
  content: HistoryEntryContent;
  createdAt: string;
  profileHash?: string;
  sourceGeneration: number;
  executionOrdinal: number;
  executionStatus: string;
};

export type HistoryEntryLookup = {
  schemaId: 'agent-history-entry-v1';
  entry: HistoryEntry;
  observedAt: string;
  lagMillis: number;
  incomplete: boolean;
};

const object = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

function exactKeys(
  value: Record<string, unknown>,
  required: readonly string[],
  optional: readonly string[] = [],
): boolean {
  const keys = Object.keys(value);
  return (
    required.every((key) => Object.hasOwn(value, key)) &&
    keys.every((key) => required.includes(key) || optional.includes(key))
  );
}

function bounded(
  value: unknown,
  maximum: number,
  allowEmpty = false,
): value is string {
  return (
    typeof value === 'string' &&
    (allowEmpty || value.length > 0) &&
    !value.includes('\u0000') &&
    encoder.encode(value).byteLength <= maximum
  );
}

const safeInteger = (value: unknown, minimum = 0): value is number =>
  typeof value === 'number' && Number.isSafeInteger(value) && value >= minimum;

const validTimestamp = (value: unknown): value is string =>
  typeof value === 'string' &&
  timestamp.test(value) &&
  !Number.isNaN(Date.parse(value));

function validQuery(value: unknown): value is HistorySearchQuery {
  if (
    !object(value) ||
    !exactKeys(value, ['q'], ['nodeId', 'logicalDialogId', 'archived']) ||
    !bounded(value.q, 1000)
  )
    return false;
  return (
    (!Object.hasOwn(value, 'nodeId') ||
      (typeof value.nodeId === 'string' && uuid.test(value.nodeId))) &&
    (!Object.hasOwn(value, 'logicalDialogId') ||
      (typeof value.logicalDialogId === 'string' &&
        uuid.test(value.logicalDialogId))) &&
    (!Object.hasOwn(value, 'archived') || typeof value.archived === 'boolean')
  );
}

function validItem(value: unknown): value is HistorySearchItem {
  return (
    object(value) &&
    exactKeys(value, [
      'logicalDialogId',
      'entryId',
      'nodeId',
      'nodeDialogId',
      'title',
      'role',
      'kind',
      'createdAt',
      'rank',
      'snippet',
    ]) &&
    typeof value.logicalDialogId === 'string' &&
    uuid.test(value.logicalDialogId) &&
    typeof value.entryId === 'string' &&
    uuid.test(value.entryId) &&
    typeof value.nodeId === 'string' &&
    uuid.test(value.nodeId) &&
    typeof value.nodeDialogId === 'string' &&
    uuid.test(value.nodeDialogId) &&
    bounded(value.title, 1000, true) &&
    (value.role === 'user' || value.role === 'assistant') &&
    bounded(value.kind, 120) &&
    validTimestamp(value.createdAt) &&
    typeof value.rank === 'number' &&
    Number.isFinite(value.rank) &&
    value.rank >= 0 &&
    bounded(value.snippet, 512, true)
  );
}

export function parseHistorySearchPage(value: unknown): HistorySearchPage {
  if (
    !object(value) ||
    !exactKeys(value, [
      'schemaId',
      'query',
      'items',
      'nextCursor',
      'totalCount',
      'observedAt',
      'lagMillis',
      'incomplete',
    ]) ||
    value.schemaId !== 'agent-search-v1' ||
    !validQuery(value.query) ||
    !Array.isArray(value.items) ||
    value.items.length > 50 ||
    !value.items.every(validItem) ||
    (value.nextCursor !== null && !bounded(value.nextCursor, 8192)) ||
    !safeInteger(value.totalCount) ||
    !validTimestamp(value.observedAt) ||
    !safeInteger(value.lagMillis) ||
    typeof value.incomplete !== 'boolean'
  ) {
    throw new APIError(0, 'invalid_history_search_response');
  }
  return value as HistorySearchPage;
}

function validContent(
  value: unknown,
  role: unknown,
): value is HistoryEntryContent {
  if (!object(value) || typeof value.kind !== 'string') return false;
  if (value.kind === 'inline') {
    if (role === 'user') {
      return (
        exactKeys(value, ['kind', 'content']) &&
        bounded(value.content, 64 * 1024, true)
      );
    }
    return (
      role === 'assistant' &&
      exactKeys(value, ['kind', 'content', 'redaction', 'truncated']) &&
      bounded(value.content, 64 * 1024, true) &&
      (value.redaction === 'none' || value.redaction === 'applied') &&
      typeof value.truncated === 'boolean'
    );
  }
  if (value.kind === 'artifact') {
    return (
      role === 'assistant' &&
      exactKeys(value, [
        'kind',
        'artifactId',
        'sizeBytes',
        'sha256',
        'redaction',
        'truncated',
      ]) &&
      typeof value.artifactId === 'string' &&
      uuid.test(value.artifactId) &&
      safeInteger(value.sizeBytes) &&
      typeof value.sha256 === 'string' &&
      /^[0-9a-f]{64}$/.test(value.sha256) &&
      (value.redaction === 'none' || value.redaction === 'applied') &&
      typeof value.truncated === 'boolean'
    );
  }
  return (
    role === 'assistant' &&
    value.kind === 'unavailable' &&
    exactKeys(value, ['kind', 'reason', 'redaction', 'truncated']) &&
    ['not_observed', 'provider_redacted', 'output_limit', 'unmapped'].includes(
      String(value.reason),
    ) &&
    ['none', 'applied', 'unknown'].includes(String(value.redaction)) &&
    typeof value.truncated === 'boolean'
  );
}

function validEntry(value: unknown): value is HistoryEntry {
  if (
    !object(value) ||
    !exactKeys(
      value,
      [
        'entryId',
        'origin',
        'logicalDialogId',
        'messageId',
        'kind',
        'role',
        'content',
        'createdAt',
        'sourceGeneration',
        'executionOrdinal',
        'executionStatus',
      ],
      ['requestId', 'attemptId', 'profileHash'],
    ) ||
    typeof value.entryId !== 'string' ||
    !uuid.test(value.entryId) ||
    typeof value.logicalDialogId !== 'string' ||
    !uuid.test(value.logicalDialogId) ||
    typeof value.messageId !== 'string' ||
    !uuid.test(value.messageId) ||
    !object(value.origin) ||
    !exactKeys(value.origin, ['nodeId', 'nodeDialogId', 'authorEventId']) ||
    typeof value.origin.nodeId !== 'string' ||
    !uuid.test(value.origin.nodeId) ||
    typeof value.origin.nodeDialogId !== 'string' ||
    !uuid.test(value.origin.nodeDialogId) ||
    !bounded(value.origin.authorEventId, 256) ||
    !bounded(value.kind, 120) ||
    (value.role !== 'user' && value.role !== 'assistant') ||
    !validContent(value.content, value.role) ||
    !validTimestamp(value.createdAt) ||
    !safeInteger(value.sourceGeneration) ||
    !safeInteger(value.executionOrdinal, 1) ||
    !bounded(value.executionStatus, 120)
  )
    return false;
  for (const key of ['requestId', 'attemptId'] as const) {
    if (
      Object.hasOwn(value, key) &&
      (typeof value[key] !== 'string' || !uuid.test(value[key]))
    )
      return false;
  }
  return (
    !Object.hasOwn(value, 'profileHash') ||
    (typeof value.profileHash === 'string' &&
      /^[0-9a-f]{64}$/.test(value.profileHash))
  );
}

export function parseHistoryEntryLookup(value: unknown): HistoryEntryLookup {
  if (
    !object(value) ||
    !exactKeys(value, [
      'schemaId',
      'entry',
      'observedAt',
      'lagMillis',
      'incomplete',
    ]) ||
    value.schemaId !== 'agent-history-entry-v1' ||
    !validEntry(value.entry) ||
    !validTimestamp(value.observedAt) ||
    !safeInteger(value.lagMillis) ||
    typeof value.incomplete !== 'boolean'
  ) {
    throw new APIError(0, 'invalid_history_entry_response');
  }
  return value as HistoryEntryLookup;
}

export async function searchHistory(
  query: HistorySearchQuery,
  cursor: string | null,
  signal?: AbortSignal,
): Promise<HistorySearchPage> {
  const values = new URLSearchParams({ q: query.q, limit: '50' });
  if (query.nodeId) values.set('nodeId', query.nodeId);
  if (query.logicalDialogId)
    values.set('logicalDialogId', query.logicalDialogId);
  if (query.archived !== undefined)
    values.set('archived', String(query.archived));
  if (cursor) values.set('cursor', cursor);
  return parseHistorySearchPage(
    await api<unknown>(`history/search?${values.toString()}`, { signal }),
  );
}

export async function readHistoryEntry(
  logicalDialogId: string,
  entryId: string,
  signal?: AbortSignal,
): Promise<HistoryEntryLookup> {
  if (!uuid.test(logicalDialogId) || !uuid.test(entryId))
    throw new APIError(400, 'invalid_history_link');
  return parseHistoryEntryLookup(
    await api<unknown>(
      `history/dialogs/${encodeURIComponent(logicalDialogId)}/entries/${encodeURIComponent(entryId)}`,
      { signal },
    ),
  );
}

export function validHistoryLink(logicalDialogId: string, entryId: string) {
  return uuid.test(logicalDialogId) && uuid.test(entryId);
}
