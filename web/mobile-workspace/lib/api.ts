const API_ROOT = '/api/v1';
const REQUEST_TIMEOUT_MS = 10_000;
const MAX_RESPONSE_BYTES = 1024 * 1024;

type JsonObject = Record<string, unknown>;

export type Session = {
  role: 'owner';
  capabilityVersion: number;
  expiresAt: string;
  csrfToken: string;
  dialogCreationEnabled?: boolean;
  taskSubmissionEnabled?: boolean;
};

export type SubmissionReceipt = {
  commandId: string;
  dialogId: string;
  admissionId: string;
  messageId: string;
  taskId: string;
  objectVersion: number;
  receivedAt: string;
};

export type DialogLifecycle = 'active' | 'archived';

export type Dialog = {
  id: string;
  lifecycle: DialogLifecycle;
  objectVersion: number;
  createdAt: string;
  updatedAt: string;
  activeTaskId: string | null;
  pendingAdmissionId?: string;
  admissionNextCheckAt?: string;
};

export type DialogPage = {
  dialogs: Dialog[];
  snapshotVersion: number;
  freshnessAt: string;
  nextCursor: string | null;
  eventCheckpoint: string | null;
};

export type DialogSnapshot = {
  dialog: Dialog;
  snapshotVersion: number;
  freshnessAt: string;
};

export type MessageActor = 'owner' | 'controller' | 'worker';
export type MessageKind = 'input' | 'output' | 'system';

export type DialogMessage = {
  id: string;
  dialogId: string;
  sequence: number;
  actor: MessageActor;
  kind: MessageKind;
  content: string;
  supersedesId: string | null;
  createdAt: string;
};

export type MessagePage = {
  messages: DialogMessage[];
  hasMore: boolean;
  nextAfterSequence: number | null;
  freshnessAt: string;
};

export type TaskState =
  | 'ready'
  | 'running'
  | 'waiting'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'needs_review';

export type CancelState = 'none' | 'requested' | 'stop_uncertain' | 'stopped';

export type Task = {
  id: string;
  dialogId: string;
  issueId: string;
  expectedRevision: number;
  state: TaskState;
  cancelState: CancelState;
  nextCheckAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type TaskPage = {
  tasks: Task[];
  freshnessAt: string;
};

export type TaskSnapshot = {
  task: Task;
  freshnessAt: string;
};

export type ManagementEventKind =
  | 'dialog.created'
  | 'admission.pending'
  | 'admission.deferred'
  | 'admission.accepted'
  | 'admission.rejected'
  | 'dialog.task.released';

export type ManagementEvent = {
  eventId: number;
  ownerVersion: number;
  dialogId: string;
  objectVersion: number;
  kind: ManagementEventKind;
  lifecycle: DialogLifecycle;
  occurredAt: string;
};

export type EventPage = {
  events: ManagementEvent[];
  snapshotVersion: number;
  freshnessAt: string;
  nextCursor: string | null;
  hasMore: boolean;
};

export type HealthStatus = 'healthy' | 'degraded' | 'unavailable';

export type ControlComponentName =
  | 'controller'
  | 'persistence'
  | 'scheduler'
  | 'worker_pool'
  | 'delivery';

export type HealthComponent = {
  name: ControlComponentName;
  state: 'ready' | 'degraded' | 'unavailable';
};

export type CapacitySnapshot = {
  class: 'general' | 'reserve';
  inFlight: number;
  limit: number;
};

export type ControlHealth = {
  components: HealthComponent[];
  capacity: CapacitySnapshot[];
  freshnessAt: string;
  liveness: 'ok' | 'unavailable';
  readiness: 'ok' | 'unavailable';
};

export type ApiErrorKind =
  | 'unauthorized'
  | 'forbidden'
  | 'overloaded'
  | 'unavailable'
  | 'offline'
  | 'invalid_response'
  | 'request_failed';

export class ApiError extends Error {
  readonly kind: ApiErrorKind;
  readonly status: number | null;
  readonly code: string | null;
  readonly requestId: string | null;
  readonly retryAfterSeconds: number | null;

  constructor(
    kind: ApiErrorKind,
    message: string,
    options: {
      status?: number;
      code?: string;
      requestId?: string;
      retryAfterSeconds?: number;
      cause?: unknown;
    } = {},
  ) {
    super(message, { cause: options.cause });
    this.name = 'ApiError';
    this.kind = kind;
    this.status = options.status ?? null;
    this.code = options.code ?? null;
    this.requestId = options.requestId ?? null;
    this.retryAfterSeconds = options.retryAfterSeconds ?? null;
  }
}

type Envelope = {
  requestId: string;
  serverTime: string;
  data: JsonObject;
};

type RequestOptions = {
  method?: 'GET' | 'POST' | 'DELETE';
  body?: string;
  headers?: Record<string, string>;
  signal?: AbortSignal;
};

export class MobileApi {
  readonly clientInstanceId: string;

  constructor(clientInstanceId: string) {
    if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$/.test(clientInstanceId)) {
      throw new ApiError(
        'request_failed',
        'client instance identity is invalid',
      );
    }
    this.clientInstanceId = clientInstanceId;
  }

  async bootstrap(signal?: AbortSignal): Promise<number> {
    const envelope = await this.request('/auth/bootstrap', {
      headers: { 'X-Fixik-Client-Instance': this.clientInstanceId },
      signal,
    });
    exactKeys(envelope.data, ['expires_in'], 'bootstrap data');
    return integer(envelope.data.expires_in, 'bootstrap expires_in', 1);
  }

  async exchangeTelegram(
    initData: string,
    signal?: AbortSignal,
  ): Promise<Session> {
    if (
      initData.length < 1 ||
      new TextEncoder().encode(initData).byteLength > 16 * 1024
    ) {
      throw new ApiError(
        'request_failed',
        'Telegram initData is outside the reviewed bound',
      );
    }
    const envelope = await this.request('/auth/telegram', {
      method: 'POST',
      body: initData,
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded; charset=utf-8',
        'X-Fixik-Client-Instance': this.clientInstanceId,
      },
      signal,
    });
    return this.decodeSession(envelope);
  }

  async resumeSession(signal?: AbortSignal): Promise<Session> {
    return this.decodeSession(
      await this.request('/session/resume', {
        method: 'POST',
        headers: { 'X-Fixik-Client-Instance': this.clientInstanceId },
        signal,
      }),
    );
  }

  async createDialog(
    commandId: string,
    expectedCollectionVersion: number,
    csrfToken: string,
  ): Promise<Dialog> {
    canonicalUUID(commandId, 'command_id');
    integer(expectedCollectionVersion, 'expected_collection_version', 0);
    const envelope = await this.request('/dialogs', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Fixik-CSRF': csrfToken,
      },
      body: JSON.stringify({
        command_id: commandId,
        expected_collection_version: expectedCollectionVersion,
      }),
    });
    exactKeys(
      envelope.data,
      ['command_id', 'replayed', 'dialog', 'snapshot_version', 'freshness_at'],
      'dialog receipt',
    );
    const dialog = decodeDialog(object(envelope.data.dialog, 'created dialog'));
    if (
      envelope.data.command_id !== commandId ||
      typeof envelope.data.replayed !== 'boolean' ||
      envelope.data.snapshot_version !== expectedCollectionVersion + 1 ||
      dialog.lifecycle !== 'active' ||
      dialog.objectVersion !== 1 ||
      dialog.activeTaskId !== null ||
      dialog.pendingAdmissionId !== undefined ||
      dialog.admissionNextCheckAt !== undefined
    ) {
      invalid('dialog receipt does not match command');
    }
    assertSnapshotFreshness(
      timestamp(envelope.data.freshness_at, 'freshness_at'),
      envelope.serverTime,
      'dialog receipt',
    );
    return dialog;
  }

  async submitTask(
    dialogId: string,
    commandId: string,
    expectedVersion: number,
    requestedIssueId: string,
    text: string,
    csrfToken: string,
  ): Promise<SubmissionReceipt> {
    canonicalUUID(commandId, 'command_id');
    canonicalUUID(dialogId, 'dialog_id');
    integer(expectedVersion, 'expected_object_version', 1);
    const envelope = await this.request(
      `/dialogs/${encodeURIComponent(dialogId)}/submissions`,
      {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Fixik-CSRF': csrfToken,
        },
        body: JSON.stringify({
          command_id: commandId,
          expected_object_version: expectedVersion,
          requested_issue_id: requestedIssueId,
          text,
        }),
      },
    );
    const data = envelope.data;
    exactKeys(
      data,
      [
        'command_id',
        'acknowledgment',
        'dialog_id',
        'admission_id',
        'message_id',
        'task_id',
        'object_version',
        'received_at',
        'replayed',
      ],
      'submission receipt',
    );
    if (
      data.command_id !== commandId ||
      data.dialog_id !== dialogId ||
      data.acknowledgment !== 'received' ||
      data.object_version !== expectedVersion + 1 ||
      typeof data.replayed !== 'boolean'
    )
      invalid('submission receipt does not match target');
    const receivedAt = timestamp(data.received_at, 'received_at');
    assertSnapshotFreshness(
      receivedAt,
      envelope.serverTime,
      'submission receipt',
    );
    return {
      commandId,
      dialogId,
      admissionId: canonicalUUID(data.admission_id, 'admission_id'),
      messageId: canonicalUUID(data.message_id, 'message_id'),
      taskId: taskIdentifier(data.task_id, 'task_id'),
      objectVersion: integer(data.object_version, 'object_version', 1),
      receivedAt,
    };
  }

  async revokeSession(csrfToken: string, signal?: AbortSignal): Promise<void> {
    const envelope = await this.request('/session', {
      method: 'DELETE',
      headers: { 'X-Fixik-CSRF': csrfToken },
      signal,
    });
    exactKeys(envelope.data, ['revoked'], 'session revoke data');
    if (envelope.data.revoked !== true) {
      invalid('session revoke acknowledgement');
    }
  }

  private decodeSession(envelope: Envelope): Session {
    exactOptionalKeys(
      envelope.data,
      ['role', 'capability_version', 'expires_at', 'csrf_token'],
      ['dialog_creation_enabled', 'task_submission_enabled'],
      'Telegram session data',
    );
    if (
      envelope.data.dialog_creation_enabled !== undefined &&
      typeof envelope.data.dialog_creation_enabled !== 'boolean'
    ) {
      invalid('dialog creation capability must be boolean');
    }
    if (
      envelope.data.task_submission_enabled !== undefined &&
      typeof envelope.data.task_submission_enabled !== 'boolean'
    )
      invalid('submission capability must be boolean');
    const role = literal(
      envelope.data.role,
      ['owner'] as const,
      'session role',
    );
    const expiresAt = timestamp(envelope.data.expires_at, 'session expires_at');
    if (timestampKey(expiresAt) <= timestampKey(envelope.serverTime)) {
      invalid('session expires_at is not after server_time');
    }
    return {
      role,
      capabilityVersion: integer(
        envelope.data.capability_version,
        'capability_version',
        1,
      ),
      expiresAt,
      csrfToken: boundedString(envelope.data.csrf_token, 'csrf_token', 16, 512),
      dialogCreationEnabled: envelope.data.dialog_creation_enabled === true,
      taskSubmissionEnabled: envelope.data.task_submission_enabled === true,
    };
  }

  async listDialogs(signal?: AbortSignal): Promise<DialogPage> {
    const dialogs: Dialog[] = [];
    let cursor: string | null = null;
    let firstPage: DialogPage | null = null;
    let lastPage: DialogPage | null = null;
    for (let pageNumber = 0; pageNumber < 5; pageNumber += 1) {
      const query =
        cursor === null
          ? '?limit=100'
          : `?limit=100&cursor=${encodeURIComponent(cursor)}`;
      const envelope = await this.request(`/dialogs${query}`, { signal });
      const page = decodeDialogPage(envelope.data);
      assertSnapshotFreshness(page.freshnessAt, envelope.serverTime, 'dialogs');
      if (
        page.dialogs.some(
          (dialog) =>
            dialog.objectVersion > page.snapshotVersion ||
            timestampKey(dialog.updatedAt) > timestampKey(page.freshnessAt),
        )
      ) {
        invalid('dialog page contains state newer than its snapshot');
      }
      firstPage ??= page;
      if (firstPage.snapshotVersion !== page.snapshotVersion) {
        invalid('dialog pagination snapshot changed');
      }
      if (page.nextCursor !== null && page.eventCheckpoint !== null) {
        invalid('non-terminal dialog page contains an event checkpoint');
      }
      dialogs.push(...page.dialogs);
      lastPage = page;
      if (page.nextCursor === null) {
        break;
      }
      if (page.nextCursor === cursor) {
        invalid('dialog pagination did not advance');
      }
      cursor = page.nextCursor;
    }
    if (firstPage === null || lastPage === null) {
      invalid('dialog pagination returned no page');
    }
    if (lastPage.nextCursor !== null) {
      invalid('dialog list exceeds client bound');
    }
    if (lastPage.eventCheckpoint === null) {
      invalid('terminal dialog page has no event checkpoint');
    }
    assertUnique(
      dialogs.map((dialog) => dialog.id),
      'dialog IDs',
    );
    assertDialogOrder(dialogs);
    return {
      ...firstPage,
      dialogs,
      freshnessAt: lastPage.freshnessAt,
      nextCursor: null,
      eventCheckpoint: lastPage.eventCheckpoint,
    };
  }

  async dialog(
    dialogId: string,
    signal?: AbortSignal,
  ): Promise<DialogSnapshot> {
    const expectedDialogID = canonicalUUID(dialogId, 'dialog path ID');
    const envelope = await this.request(
      `/dialogs/${encodeURIComponent(expectedDialogID)}`,
      {
        signal,
      },
    );
    exactKeys(
      envelope.data,
      ['dialog', 'snapshot_version', 'freshness_at'],
      'dialog snapshot',
    );
    const dialog = decodeDialog(object(envelope.data.dialog, 'dialog'));
    if (dialog.id !== expectedDialogID) {
      invalid('dialog snapshot does not match its requested identity');
    }
    const snapshotVersion = integer(
      envelope.data.snapshot_version,
      'snapshot_version',
      0,
    );
    const freshnessAt = timestamp(envelope.data.freshness_at, 'freshness_at');
    assertSnapshotFreshness(freshnessAt, envelope.serverTime, 'dialog');
    if (
      dialog.objectVersion > snapshotVersion ||
      timestampKey(dialog.updatedAt) > timestampKey(freshnessAt)
    ) {
      invalid('dialog snapshot contains future state');
    }
    return {
      dialog,
      snapshotVersion,
      freshnessAt,
    };
  }

  async messages(dialogId: string, signal?: AbortSignal): Promise<MessagePage> {
    const expectedDialogID = canonicalUUID(dialogId, 'dialog path ID');
    const messages: DialogMessage[] = [];
    let afterSequence: number | null = null;
    let page: MessagePage | null = null;
    for (let pageNumber = 0; pageNumber < 5; pageNumber += 1) {
      const query =
        afterSequence === null
          ? '?limit=100'
          : `?limit=100&after_sequence=${afterSequence}`;
      const envelope = await this.request(
        `/dialogs/${encodeURIComponent(expectedDialogID)}/messages${query}`,
        { signal },
      );
      page = decodeMessagePage(envelope.data);
      assertSnapshotFreshness(
        page.freshnessAt,
        envelope.serverTime,
        'messages',
      );
      const pageFreshnessAt = page.freshnessAt;
      if (
        page.messages.some(
          (message) =>
            timestampKey(message.createdAt) > timestampKey(pageFreshnessAt),
        )
      ) {
        invalid('message page contains future state');
      }
      messages.push(...page.messages);
      if (!page.hasMore) {
        break;
      }
      if (
        page.nextAfterSequence === null ||
        page.nextAfterSequence === afterSequence
      ) {
        invalid('message pagination did not advance');
      }
      afterSequence = page.nextAfterSequence;
    }
    if (page === null) {
      invalid('message pagination returned no page');
    }
    if (page.hasMore) {
      invalid('message history exceeds client bound');
    }
    assertUnique(
      messages.map((message) => message.id),
      'message IDs',
    );
    if (messages.some((message) => message.dialogId !== expectedDialogID)) {
      invalid('message history contains a foreign dialog identity');
    }
    for (let index = 1; index < messages.length; index += 1) {
      if (messages[index - 1].sequence >= messages[index].sequence) {
        invalid('messages are not ordered by sequence');
      }
    }
    return { ...page, messages };
  }

  async listTasks(signal?: AbortSignal): Promise<TaskPage> {
    const envelope = await this.request('/tasks?limit=100', { signal });
    exactKeys(envelope.data, ['tasks', 'freshness_at'], 'task page');
    const tasks = array(envelope.data.tasks, 'tasks').map((value) =>
      decodeTask(object(value, 'task')),
    );
    if (tasks.length > 100) {
      invalid('task page exceeds its item bound');
    }
    assertUnique(
      tasks.map((task) => task.id),
      'task IDs',
    );
    assertTaskOrder(tasks);
    const freshnessAt = timestamp(envelope.data.freshness_at, 'freshness_at');
    assertSnapshotFreshness(freshnessAt, envelope.serverTime, 'tasks');
    if (
      tasks.some(
        (task) => timestampKey(task.updatedAt) > timestampKey(freshnessAt),
      )
    ) {
      invalid('task page contains future state');
    }
    return { tasks, freshnessAt };
  }

  async task(taskId: string, signal?: AbortSignal): Promise<TaskSnapshot> {
    const expectedTaskID = taskIdentifier(taskId, 'task path ID');
    const envelope = await this.request(
      `/tasks/${encodeURIComponent(expectedTaskID)}`,
      {
        signal,
      },
    );
    exactKeys(envelope.data, ['task', 'freshness_at'], 'task snapshot');
    const task = decodeTask(object(envelope.data.task, 'task'));
    if (task.id !== expectedTaskID) {
      invalid('task snapshot does not match its requested identity');
    }
    const freshnessAt = timestamp(envelope.data.freshness_at, 'freshness_at');
    assertSnapshotFreshness(freshnessAt, envelope.serverTime, 'task');
    if (timestampKey(task.updatedAt) > timestampKey(freshnessAt)) {
      invalid('task snapshot contains future state');
    }
    return {
      task,
      freshnessAt,
    };
  }

  async events(
    cursor: string | null,
    signal?: AbortSignal,
  ): Promise<EventPage> {
    const search =
      cursor === null
        ? '?limit=100'
        : `?limit=100&after=${encodeURIComponent(cursor)}`;
    const envelope = await this.request(`/events${search}`, { signal });
    exactOptionalKeys(
      envelope.data,
      ['events', 'snapshot_version', 'freshness_at', 'has_more'],
      ['next_cursor'],
      'event page',
    );
    const events = array(envelope.data.events, 'events').map((value) =>
      decodeEvent(object(value, 'management event')),
    );
    if (events.length > 100) {
      invalid('event page exceeds its item bound');
    }
    assertUnique(
      events.map((event) => String(event.eventId)),
      'management event IDs',
    );
    for (let index = 1; index < events.length; index += 1) {
      if (
        events[index - 1].eventId >= events[index].eventId ||
        events[index - 1].ownerVersion + 1 !== events[index].ownerVersion
      ) {
        invalid('management events are not a contiguous ordered delta');
      }
    }
    const snapshotVersion = integer(
      envelope.data.snapshot_version,
      'snapshot_version',
      0,
    );
    const nextCursor =
      envelope.data.next_cursor === undefined
        ? null
        : boundedString(envelope.data.next_cursor, 'next_cursor', 1, 2048);
    const hasMore = boolean(envelope.data.has_more, 'has_more');
    const last = events.at(-1);
    if (events.some((event) => event.ownerVersion > snapshotVersion)) {
      invalid('management event exceeds its snapshot version');
    }
    const freshnessAt = timestamp(envelope.data.freshness_at, 'freshness_at');
    assertSnapshotFreshness(freshnessAt, envelope.serverTime, 'events');
    if (
      events.some(
        (event) => timestampKey(event.occurredAt) > timestampKey(freshnessAt),
      )
    ) {
      invalid('management event page contains future state');
    }
    if (
      (last !== undefined && nextCursor === null) ||
      (hasMore &&
        (events.length !== 100 ||
          last === undefined ||
          last.ownerVersion >= snapshotVersion)) ||
      (!hasMore &&
        last !== undefined &&
        last.ownerVersion !== snapshotVersion) ||
      (hasMore && nextCursor === null)
    ) {
      invalid('management event pagination fields are inconsistent');
    }
    return {
      events,
      snapshotVersion,
      freshnessAt,
      nextCursor,
      hasMore,
    };
  }

  async controlHealth(signal?: AbortSignal): Promise<ControlHealth> {
    const [controlResult, livenessResult, readinessResult] =
      await Promise.allSettled([
        this.request('/control', { signal }),
        this.probe('/healthz', signal),
        this.probe('/readyz', signal),
      ]);
    if (controlResult.status === 'rejected') {
      throw controlResult.reason;
    }
    for (const result of [livenessResult, readinessResult]) {
      if (
        result.status === 'rejected' &&
        result.reason instanceof ApiError &&
        (result.reason.kind === 'unauthorized' ||
          result.reason.kind === 'forbidden')
      ) {
        throw result.reason;
      }
    }
    const envelope = controlResult.value;
    exactKeys(
      envelope.data,
      ['components', 'capacity', 'freshness_at'],
      'control',
    );
    const components = array(envelope.data.components, 'health components').map(
      (value) => decodeHealthComponent(object(value, 'health component')),
    );
    if (components.length < 1 || components.length > 5) {
      invalid('health components are outside their item bound');
    }
    assertUnique(
      components.map((component) => component.name),
      'health component names',
    );
    const capacity = array(envelope.data.capacity, 'capacity').map((value) =>
      decodeCapacity(object(value, 'capacity entry')),
    );
    if (capacity.length < 1 || capacity.length > 2) {
      invalid('capacity entries are outside their item bound');
    }
    assertUnique(
      capacity.map((entry) => entry.class),
      'capacity classes',
    );
    const freshnessAt = timestamp(envelope.data.freshness_at, 'freshness_at');
    assertSnapshotFreshness(freshnessAt, envelope.serverTime, 'control');
    return {
      components,
      capacity,
      freshnessAt,
      liveness: livenessResult.status === 'fulfilled' ? 'ok' : 'unavailable',
      readiness: readinessResult.status === 'fulfilled' ? 'ok' : 'unavailable',
    };
  }

  private async probe(path: string, signal?: AbortSignal): Promise<void> {
    const envelope = await this.request(path, { signal });
    exactKeys(envelope.data, ['status'], 'health probe');
    if (envelope.data.status !== 'ok') {
      invalid('health probe status');
    }
  }

  private async request(
    path: string,
    options: RequestOptions = {},
  ): Promise<Envelope> {
    const controller = new AbortController();
    const abortFromCaller = () => controller.abort();
    if (options.signal?.aborted) {
      controller.abort();
    } else {
      options.signal?.addEventListener('abort', abortFromCaller, {
        once: true,
      });
    }
    const timeoutID = window.setTimeout(
      () => controller.abort(),
      REQUEST_TIMEOUT_MS,
    );
    let response: Response;
    try {
      response = await fetch(`${API_ROOT}${path}`, {
        method: options.method ?? 'GET',
        body: options.body,
        headers: {
          Accept: 'application/json',
          ...options.headers,
        },
        credentials: 'same-origin',
        cache: 'no-store',
        redirect: 'error',
        signal: controller.signal,
      });
    } catch (error) {
      if (options.signal?.aborted) {
        throw error;
      }
      const offline = typeof navigator !== 'undefined' && !navigator.onLine;
      throw new ApiError(
        offline ? 'offline' : 'unavailable',
        offline ? 'network is offline' : 'API is unavailable',
        { cause: error },
      );
    } finally {
      window.clearTimeout(timeoutID);
      options.signal?.removeEventListener('abort', abortFromCaller);
    }

    const retryAfterSeconds = retryAfter(response.headers.get('Retry-After'));
    const parsed = await parseEnvelope(response);
    if (!response.ok) {
      const error = parsed.error;
      const options = {
        status: response.status,
        code: error?.code,
        requestId: parsed.requestId,
        retryAfterSeconds: retryAfterSeconds ?? undefined,
      };
      if (response.status === 401) {
        throw new ApiError(
          'unauthorized',
          'session is not authorized',
          options,
        );
      }
      if (response.status === 403) {
        throw new ApiError('forbidden', 'request is not permitted', options);
      }
      if (response.status === 429) {
        throw new ApiError('overloaded', 'API capacity is exhausted', options);
      }
      if (response.status >= 500) {
        throw new ApiError(
          'unavailable',
          'API request is unavailable',
          options,
        );
      }
      throw new ApiError('request_failed', 'API request failed', options);
    }
    if (parsed.error !== null || parsed.data === null) {
      invalid('successful envelope has no data');
    }
    return {
      requestId: parsed.requestId,
      serverTime: parsed.serverTime,
      data: parsed.data,
    };
  }
}

type ParsedEnvelope = {
  requestId: string;
  serverTime: string;
  data: JsonObject | null;
  error: { code: string; message: string } | null;
};

async function parseEnvelope(response: Response): Promise<ParsedEnvelope> {
  const contentType = response.headers.get('Content-Type') ?? '';
  if (!/^application\/json(?:\s*;\s*charset=utf-8)?$/i.test(contentType)) {
    invalid('response content type');
  }
  const declaredLength = response.headers.get('Content-Length');
  if (
    declaredLength !== null &&
    (!/^\d+$/.test(declaredLength) ||
      Number(declaredLength) > MAX_RESPONSE_BYTES)
  ) {
    invalid('response content length');
  }
  let value: unknown;
  try {
    const payload = await response.text();
    if (new TextEncoder().encode(payload).byteLength > MAX_RESPONSE_BYTES) {
      invalid('response exceeds its byte bound');
    }
    value = JSON.parse(payload) as unknown;
  } catch (error) {
    if (error instanceof ApiError) {
      throw error;
    }
    throw new ApiError('invalid_response', 'response is not JSON', {
      cause: error,
    });
  }
  const envelope = object(value, 'response envelope');
  const allowed =
    envelope.error === undefined
      ? ['schema_version', 'request_id', 'server_time', 'data']
      : ['schema_version', 'request_id', 'server_time', 'error'];
  exactKeys(envelope, allowed, 'response envelope');
  if (envelope.schema_version !== 1) {
    invalid('unsupported schema_version');
  }
  const parsed: ParsedEnvelope = {
    requestId: requestIdentifier(envelope.request_id),
    serverTime: timestamp(envelope.server_time, 'server_time'),
    data: null,
    error: null,
  };
  if (envelope.error !== undefined) {
    const error = object(envelope.error, 'response error');
    exactKeys(error, ['code', 'message'], 'response error');
    parsed.error = {
      code: errorCode(error.code),
      message: boundedString(error.message, 'error message', 1, 512),
    };
  } else {
    parsed.data = object(envelope.data, 'response data');
  }
  return parsed;
}

function errorCode(value: unknown): string {
  const result = boundedString(value, 'error code', 1, 128);
  if (!/^[a-z][a-z0-9_]*$/.test(result)) {
    invalid('error code is invalid');
  }
  return result;
}

function decodeDialog(value: JsonObject): Dialog {
  exactOptionalKeys(
    value,
    ['id', 'lifecycle', 'object_version', 'created_at', 'updated_at'],
    ['active_task_id', 'pending_admission_id', 'admission_next_check_at'],
    'dialog',
  );
  const createdAt = timestamp(value.created_at, 'dialog created_at');
  const updatedAt = timestamp(value.updated_at, 'dialog updated_at');
  if (timestampKey(updatedAt) < timestampKey(createdAt)) {
    invalid('dialog updated_at precedes created_at');
  }
  if (
    (value.pending_admission_id !== undefined &&
      value.active_task_id !== undefined) ||
    (value.admission_next_check_at !== undefined &&
      value.pending_admission_id === undefined)
  )
    invalid('conflicting dialog work projection');
  return {
    id: canonicalUUID(value.id, 'dialog id'),
    lifecycle: literal(
      value.lifecycle,
      ['active', 'archived'] as const,
      'dialog lifecycle',
    ),
    objectVersion: integer(value.object_version, 'dialog object_version', 1),
    createdAt,
    updatedAt,
    activeTaskId:
      value.active_task_id === undefined
        ? null
        : taskIdentifier(value.active_task_id, 'active_task_id'),
    ...(value.pending_admission_id === undefined
      ? {}
      : {
          pendingAdmissionId: canonicalUUID(
            value.pending_admission_id,
            'pending_admission_id',
          ),
        }),
    ...(value.admission_next_check_at === undefined
      ? {}
      : {
          admissionNextCheckAt: timestamp(
            value.admission_next_check_at,
            'admission_next_check_at',
          ),
        }),
  };
}

function decodeDialogPage(value: JsonObject): DialogPage {
  exactOptionalKeys(
    value,
    ['dialogs', 'snapshot_version', 'freshness_at'],
    ['next_cursor', 'event_checkpoint'],
    'dialog page',
  );
  const dialogs = array(value.dialogs, 'dialogs').map((dialog) =>
    decodeDialog(object(dialog, 'dialog')),
  );
  if (dialogs.length > 100) {
    invalid('dialog page exceeds its item bound');
  }
  assertUnique(
    dialogs.map((dialog) => dialog.id),
    'dialog IDs',
  );
  return {
    dialogs,
    snapshotVersion: integer(value.snapshot_version, 'snapshot_version', 0),
    freshnessAt: timestamp(value.freshness_at, 'freshness_at'),
    nextCursor:
      value.next_cursor === undefined
        ? null
        : boundedString(value.next_cursor, 'next_cursor', 1, 2048),
    eventCheckpoint:
      value.event_checkpoint === undefined
        ? null
        : boundedString(value.event_checkpoint, 'event_checkpoint', 1, 2048),
  };
}

function decodeMessage(value: JsonObject): DialogMessage {
  exactOptionalKeys(
    value,
    ['id', 'dialog_id', 'sequence', 'actor', 'kind', 'content', 'created_at'],
    ['supersedes_id'],
    'dialog message',
  );
  const id = canonicalUUID(value.id, 'message id');
  const supersedesId =
    value.supersedes_id === undefined
      ? null
      : canonicalUUID(value.supersedes_id, 'message supersedes_id');
  if (supersedesId === id) {
    invalid('message supersedes itself');
  }
  return {
    id,
    dialogId: canonicalUUID(value.dialog_id, 'message dialog_id'),
    sequence: integer(value.sequence, 'message sequence', 1),
    actor: literal(
      value.actor,
      ['owner', 'controller', 'worker'] as const,
      'message actor',
    ),
    kind: literal(
      value.kind,
      ['input', 'output', 'system'] as const,
      'message kind',
    ),
    content: protectedPlaintext(value.content, 'message content', 65_536),
    supersedesId,
    createdAt: timestamp(value.created_at, 'message created_at'),
  };
}

function decodeTask(value: JsonObject): Task {
  exactOptionalKeys(
    value,
    [
      'id',
      'dialog_id',
      'issue_id',
      'expected_revision',
      'state',
      'cancel_state',
      'created_at',
      'updated_at',
    ],
    ['next_check_at'],
    'task',
  );
  const createdAt = timestamp(value.created_at, 'task created_at');
  const updatedAt = timestamp(value.updated_at, 'task updated_at');
  if (timestampKey(updatedAt) < timestampKey(createdAt)) {
    invalid('task updated_at precedes created_at');
  }
  return {
    id: taskIdentifier(value.id, 'task id'),
    dialogId: canonicalUUID(value.dialog_id, 'task dialog_id'),
    issueId: identifier(value.issue_id, 'task issue_id'),
    expectedRevision: integer(
      value.expected_revision,
      'task expected_revision',
      1,
    ),
    state: literal(
      value.state,
      [
        'ready',
        'running',
        'waiting',
        'completed',
        'failed',
        'cancelled',
        'needs_review',
      ] as const,
      'task state',
    ),
    cancelState: literal(
      value.cancel_state,
      ['none', 'requested', 'stop_uncertain', 'stopped'] as const,
      'task cancel_state',
    ),
    nextCheckAt:
      value.next_check_at === undefined
        ? null
        : timestamp(value.next_check_at, 'task next_check_at'),
    createdAt,
    updatedAt,
  };
}

function decodeEvent(value: JsonObject): ManagementEvent {
  exactKeys(
    value,
    [
      'event_id',
      'owner_version',
      'dialog_id',
      'object_version',
      'kind',
      'lifecycle',
      'occurred_at',
    ],
    'management event',
  );
  const ownerVersion = integer(value.owner_version, 'owner_version', 1);
  const objectVersion = integer(
    value.object_version,
    'event object_version',
    1,
  );
  if (objectVersion > ownerVersion) {
    invalid('event object_version exceeds owner_version');
  }
  return {
    eventId: integer(value.event_id, 'event_id', 1),
    ownerVersion,
    dialogId: canonicalUUID(value.dialog_id, 'event dialog_id'),
    objectVersion,
    kind: literal(
      value.kind,
      [
        'dialog.created',
        'admission.pending',
        'admission.deferred',
        'admission.accepted',
        'admission.rejected',
        'dialog.task.released',
      ] as const,
      'event kind',
    ),
    lifecycle: literal(value.lifecycle, ['active'] as const, 'event lifecycle'),
    occurredAt: timestamp(value.occurred_at, 'event occurred_at'),
  };
}

function decodeHealthComponent(value: JsonObject): HealthComponent {
  exactKeys(value, ['name', 'state'], 'health component');
  return {
    name: literal(
      value.name,
      [
        'controller',
        'persistence',
        'scheduler',
        'worker_pool',
        'delivery',
      ] as const,
      'health component name',
    ),
    state: literal(
      value.state,
      ['ready', 'degraded', 'unavailable'] as const,
      'health component state',
    ),
  };
}

function decodeCapacity(value: JsonObject): CapacitySnapshot {
  exactKeys(value, ['class', 'in_flight', 'limit'], 'capacity entry');
  const limit = integer(value.limit, 'capacity limit', 1);
  const inFlight = integer(value.in_flight, 'capacity in_flight', 0);
  if (limit > 1_000_000 || inFlight > limit) {
    invalid('capacity values are outside their bounds');
  }
  return {
    class: literal(
      value.class,
      ['general', 'reserve'] as const,
      'capacity class',
    ),
    inFlight,
    limit,
  };
}

function decodeMessagePage(value: JsonObject): MessagePage {
  exactOptionalKeys(
    value,
    ['messages', 'has_more', 'freshness_at'],
    ['next_after_sequence'],
    'message page',
  );
  const messages = array(value.messages, 'messages').map((entry) =>
    decodeMessage(object(entry, 'message')),
  );
  if (messages.length > 100) {
    invalid('message page exceeds its item bound');
  }
  const pageBytes = messages.reduce(
    (total, message) =>
      total + new TextEncoder().encode(message.content).byteLength,
    0,
  );
  if (pageBytes > 128 * 1024) {
    invalid('message page exceeds its plaintext byte bound');
  }
  const hasMore = boolean(value.has_more, 'has_more');
  const nextAfterSequence =
    value.next_after_sequence === undefined
      ? null
      : integer(value.next_after_sequence, 'next_after_sequence', 1);
  if (hasMore !== (nextAfterSequence !== null)) {
    invalid('message pagination fields are inconsistent');
  }
  if (
    hasMore &&
    (messages.length === 0 ||
      nextAfterSequence !== messages[messages.length - 1].sequence)
  ) {
    invalid('message pagination cursor does not match the last message');
  }
  return {
    messages,
    hasMore,
    nextAfterSequence,
    freshnessAt: timestamp(value.freshness_at, 'freshness_at'),
  };
}

function object(value: unknown, name: string): JsonObject {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    invalid(`${name} must be an object`);
  }
  return value as JsonObject;
}

function array(value: unknown, name: string): unknown[] {
  if (!Array.isArray(value) || value.length > 500) {
    invalid(`${name} must be a bounded array`);
  }
  return value;
}

function exactKeys(
  value: JsonObject,
  keys: readonly string[],
  name: string,
): void {
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (
    actual.length !== expected.length ||
    actual.some((key, index) => key !== expected[index])
  ) {
    invalid(`${name} fields do not match schema`);
  }
}

function exactOptionalKeys(
  value: JsonObject,
  required: readonly string[],
  optional: readonly string[],
  name: string,
): void {
  const actual = Object.keys(value);
  const allowed = new Set([...required, ...optional]);
  if (
    required.some((key) => !Object.prototype.hasOwnProperty.call(value, key)) ||
    actual.some((key) => !allowed.has(key))
  ) {
    invalid(`${name} fields do not match schema`);
  }
}

function boundedString(
  value: unknown,
  name: string,
  minimum: number,
  maximum: number,
): string {
  if (
    typeof value !== 'string' ||
    value.length < minimum ||
    value.length > maximum ||
    value.includes('\u0000') ||
    !isWellFormedUnicode(value)
  ) {
    invalid(`${name} is invalid`);
  }
  return value;
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1);
      if (next < 0xdc00 || next > 0xdfff) {
        return false;
      }
      index += 1;
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false;
    }
  }
  return true;
}

function requestIdentifier(value: unknown): string {
  const result = boundedString(value, 'request_id', 1, 128);
  if (!/^[A-Za-z0-9_.:-]+$/.test(result)) {
    invalid('request_id contains unsupported characters');
  }
  return result;
}

function identifier(value: unknown, name: string): string {
  const result = boundedString(value, name, 1, 256);
  if (
    result.trim() !== result ||
    /\p{Cc}/u.test(result) ||
    new TextEncoder().encode(result).byteLength > 256
  ) {
    invalid(`${name} has unsupported whitespace or control characters`);
  }
  return result;
}

function taskIdentifier(value: unknown, name: string): string {
  const result = identifier(value, name);
  if (/[/?#]/.test(result)) {
    invalid(`${name} is not a bounded path-segment identity`);
  }
  return result;
}

function canonicalUUID(value: unknown, name: string): string {
  const result = boundedString(value, name, 36, 36);
  if (
    !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
      result,
    )
  ) {
    invalid(`${name} is not a canonical UUID`);
  }
  return result;
}

function integer(value: unknown, name: string, minimum: number): number {
  if (!Number.isSafeInteger(value) || (value as number) < minimum) {
    invalid(`${name} is invalid`);
  }
  return value as number;
}

function boolean(value: unknown, name: string): boolean {
  if (typeof value !== 'boolean') {
    invalid(`${name} is invalid`);
  }
  return value;
}

function literal<const T extends readonly string[]>(
  value: unknown,
  allowed: T,
  name: string,
): T[number] {
  if (typeof value !== 'string' || !allowed.includes(value)) {
    invalid(`${name} is invalid`);
  }
  return value as T[number];
}

function timestamp(value: unknown, name: string): string {
  const result = boundedString(value, name, 20, 40);
  const match =
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,6}))?Z$/.exec(
      result,
    );
  if (match === null) {
    invalid(`${name} is not UTC RFC3339`);
  }
  const expected = match.slice(1, 7).map(Number);
  const parsed = new Date(0);
  parsed.setUTCFullYear(expected[0], expected[1] - 1, expected[2]);
  parsed.setUTCHours(expected[3], expected[4], expected[5], 0);
  if (
    !Number.isFinite(parsed.getTime()) ||
    parsed.getUTCFullYear() !== expected[0] ||
    parsed.getUTCMonth() !== expected[1] - 1 ||
    parsed.getUTCDate() !== expected[2] ||
    parsed.getUTCHours() !== expected[3] ||
    parsed.getUTCMinutes() !== expected[4] ||
    parsed.getUTCSeconds() !== expected[5] ||
    timestampKey(result) <= '1970-01-01T00:00:00.000000000Z'
  ) {
    invalid(`${name} is not a timestamp`);
  }
  return result;
}

function timestampKey(value: string): string {
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,6}))?Z$/.exec(
    value,
  );
  if (match === null) {
    invalid('validated timestamp changed representation');
  }
  return `${match[1]}.${(match[2] ?? '').padEnd(9, '0')}Z`;
}

function assertSnapshotFreshness(
  freshnessAt: string,
  serverTime: string,
  name: string,
): void {
  if (timestampKey(freshnessAt) > timestampKey(serverTime)) {
    invalid(`${name} freshness_at is after server_time`);
  }
}

function assertUnique(values: readonly string[], name: string): void {
  if (new Set(values).size !== values.length) {
    invalid(`${name} are not unique`);
  }
}

function assertDialogOrder(dialogs: readonly Dialog[]): void {
  for (let index = 1; index < dialogs.length; index += 1) {
    const previous = dialogs[index - 1];
    const current = dialogs[index];
    if (
      timestampKey(previous.createdAt) < timestampKey(current.createdAt) ||
      (timestampKey(previous.createdAt) === timestampKey(current.createdAt) &&
        previous.id <= current.id)
    ) {
      invalid('dialogs are not ordered by created_at DESC, id DESC');
    }
  }
}

function assertTaskOrder(tasks: readonly Task[]): void {
  for (let index = 1; index < tasks.length; index += 1) {
    const previous = tasks[index - 1];
    const current = tasks[index];
    if (
      timestampKey(previous.updatedAt) < timestampKey(current.updatedAt) ||
      (timestampKey(previous.updatedAt) === timestampKey(current.updatedAt) &&
        previous.id <= current.id)
    ) {
      invalid('tasks are not ordered by updated_at DESC, id DESC');
    }
  }
}

function protectedPlaintext(
  value: unknown,
  name: string,
  maximumBytes: number,
): string {
  const result = boundedString(value, name, 0, maximumBytes);
  if (new TextEncoder().encode(result).byteLength > maximumBytes) {
    invalid(`${name} exceeds its UTF-8 byte bound`);
  }
  for (const character of result) {
    const point = character.codePointAt(0) ?? 0;
    if (
      ((point >= 0 && point <= 31) || (point >= 127 && point <= 159)) &&
      character !== '\n' &&
      character !== '\r' &&
      character !== '\t'
    ) {
      invalid(`${name} contains an unsupported control character`);
    }
  }
  return result;
}

function retryAfter(value: string | null): number | null {
  if (value === null || !/^\d{1,5}$/.test(value)) {
    return null;
  }
  const seconds = Number(value);
  return Number.isSafeInteger(seconds) ? seconds : null;
}

function invalid(detail: string): never {
  throw new ApiError('invalid_response', `invalid API response: ${detail}`);
}
