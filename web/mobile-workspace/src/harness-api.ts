import schema from '../../../api/harness-v1.schema.json';
import { parseHarnessJson, type HarnessSchema } from './harness-protocol.ts';
import {
  HARNESS_SCHEMA_SHA256,
  type HarnessArtifactMetadata,
  type HarnessAttemptPage,
  type HarnessAttemptRead,
  type HarnessCommand,
  type HarnessCommandStatus,
  type HarnessDialogPage,
  type HarnessError,
  type HarnessEvent,
  type HarnessEventPage,
  type HarnessHealthReady,
  type HarnessHistoryPage,
  type HarnessNodeIdentity,
  type HarnessReceipt,
  type HarnessRequestPage,
  type HarnessSnapshot,
} from './harness-protocol-types.ts';
import type { Session } from './panel-api';

export type HarnessNode = {
  nodeId: string;
  name: string;
  adapter: 'cursor' | 'codex';
};
export type HarnessNodes = {
  registryVersion: number;
  mode: 'live' | 'fixture';
  nodes: HarnessNode[];
};

const contract = schema as HarnessSchema;
const responseLimit = 8 * 1024 * 1024;
const uuidPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class HarnessAPIError extends Error {
  constructor(
    public status: number,
    public code: string,
    safeMessage: string,
    public retryable = false,
    public outcome: 'definite' | 'unknown' = 'definite',
  ) {
    super(safeMessage);
  }
}

function safeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

function invalidResponse(
  message = 'Harness вернул ответ, который не соответствует контракту.',
): HarnessAPIError {
  return new HarnessAPIError(
    200,
    'invalid_response',
    message,
    false,
    'unknown',
  );
}

// C1 command keys are ASCII and numbers are safe integers. Sort every object
// key, preserve array order and encode strings without Unicode normalization.
function canonicalCommandJSON(value: unknown): string {
  if (Array.isArray(value)) {
    return `[${value.map(canonicalCommandJSON).join(',')}]`;
  }
  if (value !== null && typeof value === 'object') {
    const object = value as Record<string, unknown>;
    return `{${Object.keys(object)
      .sort()
      .map(
        (key) => `${JSON.stringify(key)}:${canonicalCommandJSON(object[key])}`,
      )
      .join(',')}}`;
  }
  const json = JSON.stringify(value);
  if (json === undefined)
    throw invalidResponse('Некорректная сохранённая команда.');
  return json;
}

async function commandHash(command: HarnessCommand): Promise<string> {
  const bytes = new TextEncoder().encode(canonicalCommandJSON(command));
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, '0'),
  ).join('');
}

function validateNodes(value: unknown): HarnessNodes {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw invalidResponse();
  }
  const record = value as Record<string, unknown>;
  if (
    Object.keys(record).length !== 3 ||
    !['registryVersion', 'mode', 'nodes'].every((key) => key in record) ||
    !safeInteger(record.registryVersion) ||
    (record.mode !== 'live' && record.mode !== 'fixture') ||
    !Array.isArray(record.nodes) ||
    record.nodes.length > 16
  ) {
    throw invalidResponse();
  }
  const ids = new Set<string>();
  const nodes = record.nodes.map((node) => {
    if (typeof node !== 'object' || node === null || Array.isArray(node)) {
      throw invalidResponse();
    }
    const item = node as Record<string, unknown>;
    if (
      Object.keys(item).length !== 3 ||
      !['nodeId', 'name', 'adapter'].every((key) => key in item) ||
      typeof item.nodeId !== 'string' ||
      !uuidPattern.test(item.nodeId) ||
      ids.has(item.nodeId) ||
      typeof item.name !== 'string' ||
      item.name.length === 0 ||
      Array.from(item.name).some((character) => {
        const codePoint = character.codePointAt(0) ?? 0;
        return codePoint <= 31 || codePoint === 127;
      }) ||
      new TextEncoder().encode(item.name).length > 200 ||
      (item.adapter !== 'cursor' && item.adapter !== 'codex')
    ) {
      throw invalidResponse();
    }
    ids.add(item.nodeId);
    return {
      nodeId: item.nodeId,
      name: item.name,
      adapter: item.adapter as 'cursor' | 'codex',
    };
  });
  return {
    registryVersion: record.registryVersion,
    mode: record.mode,
    nodes,
  };
}

function endpoint(path: string): string {
  const url = new URL(`api/v2/harness${path}`, window.location.href);
  return url.pathname + url.search;
}

async function request<T>(
  session: Session,
  path: string,
  wireType?:
    | 'nodeIdentity'
    | 'snapshot'
    | 'dialogPage'
    | 'historyPage'
    | 'requestPage'
    | 'attemptPage'
    | 'eventPage'
    | 'attemptRead'
    | 'healthReady'
    | 'commandStatus'
    | 'artifactMetadata'
    | 'receipt',
  options?: RequestInit,
  allowedStatuses = [200],
): Promise<T> {
  let response: Response;
  try {
    const headers = new Headers(options?.headers);
    if (options?.body !== undefined) {
      headers.set('Content-Type', 'application/json');
      headers.set('X-Panel-CSRF', session.csrf);
    }
    response = await fetch(endpoint(path), {
      credentials: 'same-origin',
      cache: 'no-store',
      redirect: 'error',
      ...options,
      headers,
    });
  } catch (cause) {
    if (options?.signal?.aborted) throw cause;
    throw new HarnessAPIError(
      0,
      'network_unavailable',
      'Связь с Harness потеряна; результат команды может быть неизвестен.',
      true,
      options?.method === 'POST' ? 'unknown' : 'definite',
    );
  }

  let text: string;
  try {
    text = await response.text();
  } catch {
    throw new HarnessAPIError(
      response.status,
      'response_unavailable',
      'Ответ Harness потерян; результат команды может быть неизвестен.',
      true,
      options?.method === 'POST' ? 'unknown' : 'definite',
    );
  }
  if (new TextEncoder().encode(text).length > responseLimit) {
    throw invalidResponse('Ответ Harness превышает допустимый размер.');
  }
  if (response.status === 401) {
    throw new HarnessAPIError(
      401,
      'no_session',
      'Сессия истекла. Войдите заново.',
    );
  }
  if (!allowedStatuses.includes(response.status)) {
    try {
      const error = parseHarnessJson(text, 'error', contract) as HarnessError;
      throw new HarnessAPIError(
        response.status,
        error.code,
        error.safeMessage,
        error.retryable,
        options?.method === 'POST' && response.status >= 500
          ? 'unknown'
          : 'definite',
      );
    } catch (error) {
      if (error instanceof HarnessAPIError) throw error;
      if (response.status >= 500) {
        throw new HarnessAPIError(
          response.status,
          'harness_unavailable',
          'Harness недоступен; результат команды может быть неизвестен.',
          true,
          options?.method === 'POST' ? 'unknown' : 'definite',
        );
      }
      throw invalidResponse('Harness вернул некорректное описание ошибки.');
    }
  }
  if (!wireType) {
    try {
      return JSON.parse(text) as T;
    } catch {
      throw invalidResponse();
    }
  }
  try {
    return parseHarnessJson(text, wireType, contract) as T;
  } catch {
    throw invalidResponse();
  }
}

function assertNode(expectedNodeId: string, actualNodeId: string): void {
  if (actualNodeId !== expectedNodeId) {
    throw invalidResponse('Harness подтвердил другую ноду.');
  }
}

function assertReceipt(command: HarnessCommand, receipt: HarnessReceipt): void {
  assertNode(command.target.nodeId, receipt.nodeId);
  if (
    receipt.commandId !== command.commandId ||
    receipt.commandKind !== command.kind
  ) {
    throw invalidResponse('Harness подтвердил другую команду.');
  }
  const references = receipt.references as unknown as Record<string, string>;
  const target = command.target as unknown as Record<string, string>;
  for (const key of [
    'dialogId',
    'messageId',
    'requestId',
    'attemptId',
    'approvalId',
    'inputRequestId',
  ]) {
    const referenceKey =
      command.kind === 'attempt.retry' && key === 'attemptId'
        ? 'priorAttemptId'
        : key;
    if (key in target && references[referenceKey] !== target[key]) {
      throw invalidResponse('Harness подтвердил другую цель команды.');
    }
  }
}

export function assertIdentity(
  nodeId: string,
  registryVersion: number,
  identity: HarnessNodeIdentity,
): void {
  assertNode(nodeId, identity.nodeId);
  if (
    identity.registryVersion !== registryVersion ||
    identity.schemaSHA256 !== HARNESS_SCHEMA_SHA256
  ) {
    throw invalidResponse(
      'Идентичность Harness не совпадает с registry/схемой.',
    );
  }
}

export const harnessAPI = {
  nodes: (session: Session, signal?: AbortSignal) =>
    request<HarnessNodes>(session, '/nodes', undefined, { signal }).then(
      validateNodes,
    ),
  identity: (session: Session, nodeId: string, signal?: AbortSignal) =>
    request<HarnessNodeIdentity>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/identity`,
      'nodeIdentity',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      return value;
    }),
  snapshot: (session: Session, nodeId: string, signal?: AbortSignal) =>
    request<HarnessSnapshot>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/snapshot`,
      'snapshot',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      return value;
    }),
  dialogs: (
    session: Session,
    nodeId: string,
    cursor = '',
    limit = 50,
    signal?: AbortSignal,
  ) =>
    request<HarnessDialogPage>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/dialogs?limit=${limit}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
      'dialogPage',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      return value;
    }),
  history: (
    session: Session,
    nodeId: string,
    dialogId: string,
    cursor = '',
    limit = 100,
    signal?: AbortSignal,
  ) =>
    request<HarnessHistoryPage>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/dialogs/${encodeURIComponent(dialogId)}/messages?limit=${limit}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
      'historyPage',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      if (value.dialogId !== dialogId) {
        throw invalidResponse('Harness вернул историю другого диалога.');
      }
      return value;
    }),
  requests: (
    session: Session,
    nodeId: string,
    state = '',
    cursor = '',
    limit = 100,
    signal?: AbortSignal,
  ) =>
    request<HarnessRequestPage>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/requests?limit=${limit}${state ? `&state=${encodeURIComponent(state)}` : ''}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
      'requestPage',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      return value;
    }),
  attempts: (
    session: Session,
    nodeId: string,
    requestId: string,
    cursor = '',
    limit = 100,
    signal?: AbortSignal,
  ) =>
    request<HarnessAttemptPage>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/requests/${encodeURIComponent(requestId)}/attempts?limit=${limit}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
      'attemptPage',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      if (
        value.requestId !== requestId ||
        value.items.some(
          (attempt) =>
            attempt.requestId !== requestId ||
            attempt.dialogId !== value.dialogId,
        )
      ) {
        throw invalidResponse('Harness вернул попытки другого поручения.');
      }
      return value;
    }),
  attempt: (
    session: Session,
    nodeId: string,
    attemptId: string,
    signal?: AbortSignal,
  ) =>
    request<HarnessAttemptRead>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/attempts/${encodeURIComponent(attemptId)}`,
      'attemptRead',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      if (value.attempt.attemptId !== attemptId) {
        throw invalidResponse('Harness вернул другую попытку.');
      }
      return value;
    }),
  attemptEvents: (
    session: Session,
    nodeId: string,
    attemptId: string,
    after = 0,
    limit = 100,
    signal?: AbortSignal,
  ) =>
    request<HarnessEventPage>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/attempts/${encodeURIComponent(attemptId)}/events?after=${after}&limit=${limit}`,
      'eventPage',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      if (
        value.attemptId !== attemptId ||
        value.items.some(
          (event) =>
            event.nodeId !== nodeId ||
            event.attemptId !== attemptId ||
            event.dialogId !== value.dialogId,
        )
      ) {
        throw invalidResponse('Harness вернул события другой попытки.');
      }
      return value;
    }),
  ready: (session: Session, nodeId: string, signal?: AbortSignal) =>
    request<HarnessHealthReady>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/health/ready`,
      'healthReady',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.identity.nodeId);
      return value;
    }),
  artifactMetadata: (
    session: Session,
    nodeId: string,
    artifactId: string,
    signal?: AbortSignal,
  ) =>
    request<HarnessArtifactMetadata>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/artifacts/${encodeURIComponent(artifactId)}/metadata`,
      'artifactMetadata',
      { signal },
    ).then((value) => {
      assertNode(nodeId, value.nodeId);
      if (value.artifactId !== artifactId) {
        throw invalidResponse('Harness вернул metadata другого артефакта.');
      }
      return value;
    }),
  artifact: async (
    session: Session,
    nodeId: string,
    artifactId: string,
    signal?: AbortSignal,
  ) => {
    let response: Response;
    try {
      response = await fetch(
        endpoint(
          `/nodes/${encodeURIComponent(nodeId)}/artifacts/${encodeURIComponent(artifactId)}`,
        ),
        {
          credentials: 'same-origin',
          cache: 'no-store',
          redirect: 'error',
          signal,
        },
      );
    } catch (cause) {
      if (signal?.aborted) throw cause;
      throw new HarnessAPIError(
        0,
        'network_unavailable',
        'Не удалось загрузить артефакт.',
      );
    }
    if (response.status === 401) {
      throw new HarnessAPIError(
        401,
        'no_session',
        'Сессия истекла. Войдите заново.',
      );
    }
    if (!response.ok) {
      const text = await response.text();
      try {
        const error = parseHarnessJson(text, 'error', contract) as HarnessError;
        throw new HarnessAPIError(
          response.status,
          error.code,
          error.safeMessage,
          error.retryable,
        );
      } catch (error) {
        if (error instanceof HarnessAPIError) throw error;
        throw invalidResponse('Harness вернул некорректное описание ошибки.');
      }
    }
    const bytes = await response.arrayBuffer();
    if (bytes.byteLength > 16 * 1024 * 1024) {
      throw invalidResponse('Артефакт превышает допустимый размер.');
    }
    return {
      bytes,
      mediaType: response.headers.get('Content-Type') ?? '',
      disposition: response.headers.get('Content-Disposition') ?? '',
    };
  },
  status: (
    session: Session,
    nodeId: string,
    command: HarnessCommand,
    signal?: AbortSignal,
  ) =>
    request<HarnessCommandStatus>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/commands/${encodeURIComponent(command.commandId)}`,
      'commandStatus',
      { signal },
    ).then(async (value) => {
      assertNode(nodeId, value.nodeId);
      if (value.commandId !== command.commandId) {
        throw invalidResponse('Harness вернул status другой команды.');
      }
      if (value.canonicalPayloadHash !== (await commandHash(command))) {
        throw invalidResponse(
          'Сохранённая команда отличается от отправленной.',
        );
      }
      assertReceipt(command, value.receipt);
      return value;
    }),
  command: async (
    session: Session,
    nodeId: string,
    command: HarnessCommand,
    signal?: AbortSignal,
  ) => {
    assertNode(nodeId, command.target.nodeId);
    const receipt = await request<HarnessReceipt>(
      session,
      `/nodes/${encodeURIComponent(nodeId)}/commands`,
      'receipt',
      { method: 'POST', body: JSON.stringify(command), signal },
      [200, 202],
    );
    assertReceipt(command, receipt);
    return receipt;
  },
  eventsURL: (nodeId: string, after: number) =>
    endpoint(
      `/nodes/${encodeURIComponent(nodeId)}/events?after=${encodeURIComponent(String(after))}`,
    ),
};

export function newCommandId(): string {
  return crypto.randomUUID();
}

export function parseHarnessEvent(raw: string): HarnessEvent {
  return parseHarnessJson(raw, 'event', contract);
}

export type {
  HarnessCommand,
  HarnessCommandStatus,
  HarnessDialogPage,
  HarnessEvent,
  HarnessHistoryPage,
  HarnessNodeIdentity,
  HarnessReceipt,
  HarnessSnapshot,
  HarnessArtifactMetadata,
  HarnessAttemptPage,
  HarnessAttemptRead,
  HarnessEventPage,
  HarnessRequestPage,
};
