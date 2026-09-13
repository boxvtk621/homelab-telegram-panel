import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  assertIdentity,
  HARNESS_SCHEMA_SHA256,
  harnessAPI,
  HarnessAPIError,
  newCommandId,
  parseHarnessEvent,
  type HarnessDialogPage,
  type HarnessEvent,
  type HarnessEventPage,
  type HarnessHistoryPage,
  type HarnessNode,
  type HarnessNodeIdentity,
  type HarnessSnapshot,
  type HarnessAttemptPage,
  type HarnessRequestPage,
} from './harness-api';
import {
  acceptsTarget,
  classifyControlFailure,
  classifyEvent,
  controlIntentKey,
  emptyDraft,
  readDraft,
  reconnectDelay,
  targetKey,
  writeDraft,
  type CreateCommand,
  type CreateIntent,
  type ControlCommand,
  type ControlIntent,
  type DeleteCommand,
  type DeleteIntent,
  type HarnessDraft,
  type MessageCommand,
} from './harness-state';
import type { Session } from './panel-api';
import { SafeMarkdown } from './safe-markdown';

type Props = {
  session: Session;
  onExpired: () => void;
  selectedNodeId?: string;
  onBack?: () => void;
};
type StreamBaseline = {
  nodeId: string;
  generation: number;
  epoch: number;
  seq: number;
};
type HistoryState = {
  nodeId: string;
  dialogId: string;
  epoch: number;
  page: HarnessHistoryPage;
};
type ArtifactContent = {
  kind: 'artifact';
  artifactId: string;
  sizeBytes: number;
  sha256: string;
  redaction: 'none' | 'applied';
  truncated: boolean;
};
type RequestState = {
  nodeId: string;
  epoch: number;
  page: HarnessRequestPage;
};
type AttemptState = {
  nodeId: string;
  requestId: string;
  epoch: number;
  page: HarnessAttemptPage;
};
type TimelineState = {
  nodeId: string;
  dialogId: string;
  attemptId: string;
  epoch: number;
  events: HarnessEventPage['items'];
  archiveAfter: number;
  hasMore: boolean;
  loadingMore: boolean;
  error?: string;
};
type DialogItem = HarnessDialogPage['items'][number];

function controlResourceKey(command: ControlCommand): string {
  return `${command.kind}:${JSON.stringify(command.target)}`;
}

function sameControlAction(
  saved: ControlCommand,
  proposed: ControlCommand,
): boolean {
  return (
    controlIntentKey(saved) === controlIntentKey(proposed) &&
    JSON.stringify(saved.payload) === JSON.stringify(proposed.payload)
  );
}

function isRecoverableControl(intent: ControlIntent): boolean {
  return (
    intent.phase === 'sending' ||
    intent.phase === 'unknown' ||
    intent.phase === 'checking' ||
    intent.phase === 'retry-ready'
  );
}

function controlCommandLabel(command: ControlCommand): string {
  switch (command.kind) {
    case 'attempt.stop':
      return 'Остановить работу';
    case 'queue.resume':
      return 'Продолжить очередь';
    case 'request.cancel':
      return 'Отменить поручение';
    case 'message.steer':
      return 'Передать в текущую попытку';
    case 'attempt.retry':
      return 'Повторить попытку';
    case 'approval.respond':
      return command.payload.decision === 'allow_once'
        ? 'Разрешить один раз'
        : 'Отклонить';
    case 'input.respond':
      return 'Ответить агенту';
  }
}
type ApprovalRequestedEvent = Extract<
  HarnessEvent,
  { type: 'approval.requested' }
>;
type InputRequestedEvent = Extract<HarnessEvent, { type: 'input.requested' }>;
type TerminalAttemptEvent = Extract<
  HarnessEvent,
  {
    type:
      | 'attempt.completed'
      | 'attempt.failed'
      | 'attempt.interrupted'
      | 'attempt.unknown';
  }
>;
type SafeContent =
  | {
      kind: 'inline';
      content: string;
      redaction: 'none' | 'applied';
      truncated: boolean;
    }
  | ArtifactContent
  | {
      kind: 'unavailable';
      reason:
        | 'not_observed'
        | 'provider_redacted'
        | 'output_limit'
        | 'unmapped';
      redaction: 'none' | 'applied' | 'unknown';
      truncated: boolean;
    };

function safeError(error: unknown): string {
  if (error instanceof HarnessAPIError) return error.message;
  return 'Не удалось получить подтверждённый ответ агента.';
}

const stateLabels: Record<string, string> = {
  online: 'на связи',
  offline: 'не на связи',
  ready: 'готов',
  blocked: 'нужна проверка',
  active: 'работает',
  idle: 'свободен',
  stale: 'данные устарели',
  unknown: 'неизвестно',
  paused: 'на паузе',
  queued: 'в очереди',
  dispatching: 'запускается',
  running: 'выполняется',
  waiting_input: 'ждёт решения',
  stopping: 'останавливается',
  completed: 'завершено',
  complete: 'завершено',
  failed: 'ошибка',
  interrupted: 'остановлено',
  canceled: 'отменено',
  cancelled: 'отменено',
  applied: 'доставлено',
  accepted: 'принято',
  succeeded: 'успешно',
  stop: 'завершено агентом',
  length: 'достигнут лимит ответа',
  content_filter: 'ответ ограничен политикой',
  tool_calls: 'агент вызвал инструменты',
  none: 'нет',
  known: 'подтверждён',
};

const eventLabels: Record<string, string> = {
  'attempt.dispatching': 'Подготовка запуска',
  'attempt.started': 'Работа началась',
  'attempt.waiting_input': 'Требуется решение',
  'attempt.stop_requested': 'Запрошена остановка',
  'attempt.completed': 'Работа завершена',
  'attempt.failed': 'Ошибка выполнения',
  'attempt.interrupted': 'Работа остановлена',
  'attempt.unknown': 'Результат не подтверждён',
  'tool.started': 'Вызов инструмента',
  'tool.output': 'Ответ инструмента',
  'tool.completed': 'Инструмент завершён',
  'assistant.delta': 'Агент формирует ответ',
  'assistant.message': 'Ответ агента',
  'approval.requested': 'Требуется разрешение',
  'approval.resolved': 'Решение принято',
  'input.requested': 'Агент ждёт ответ',
  'input.resolved': 'Ответ передан агенту',
  'artifact.available': 'Готов файл',
};

const toolLabels: Record<string, string> = {
  'cursor.command': 'Команда в рабочей папке',
  cursor_command: 'Команда в рабочей папке',
  'cursor.file_change': 'Изменение файлов',
  cursor_file_change: 'Изменение файлов',
  'codex.command': 'Команда в рабочей папке',
  codex_command: 'Команда в рабочей папке',
  'codex.file_change': 'Изменение файлов',
  codex_file_change: 'Изменение файлов',
};

function stateLabel(value: string): string {
  return stateLabels[value] ?? 'неизвестное состояние';
}

function eventLabel(value: string): string {
  return eventLabels[value] ?? 'Состояние обновлено';
}

function toolLabel(value: string): string {
  return toolLabels[value] ?? 'Дополнительный инструмент агента';
}

function unavailableReasonLabel(value: string): string {
  switch (value) {
    case 'not_observed':
      return 'результат не был получен';
    case 'provider_redacted':
      return 'содержимое скрыто поставщиком';
    case 'output_limit':
      return 'ответ превысил допустимый размер';
    default:
      return 'формат ответа не распознан';
  }
}

function contentNotice(content: SafeContent): string {
  const notices: string[] = [];
  if (content.redaction === 'applied') notices.push('Часть данных скрыта.');
  if (content.redaction === 'unknown') {
    notices.push('Полнота данных не подтверждена.');
  }
  if (content.truncated) notices.push('Показан сокращённый результат.');
  return notices.join(' ');
}

function effectSummary(value: 'none' | 'known' | 'unknown'): string {
  switch (value) {
    case 'none':
      return 'Внешних изменений нет.';
    case 'known':
      return 'Внешние изменения подтверждены.';
    case 'unknown':
      return 'Состояние внешних изменений не подтверждено.';
  }
}

function usageSourceLabel(value: 'per_attempt' | 'cumulative_process'): string {
  return value === 'per_attempt'
    ? 'за этот запуск'
    : 'накопительно для процесса';
}

function tokenCount(value: number): string {
  const tail = value % 100;
  const digit = value % 10;
  const noun =
    tail >= 11 && tail <= 14
      ? 'токенов'
      : digit === 1
        ? 'токен'
        : digit >= 2 && digit <= 4
          ? 'токена'
          : 'токенов';
  return `${value} ${noun}`;
}

function eventTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return value;
  return date.toLocaleTimeString('ru-RU', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

function dialogDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return 'Время создания неизвестно';
  return date.toLocaleString('ru-RU', {
    day: 'numeric',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
  });
}

function isTerminalAttemptEvent(
  event: HarnessEvent,
): event is TerminalAttemptEvent {
  return (
    event.type === 'attempt.completed' ||
    event.type === 'attempt.failed' ||
    event.type === 'attempt.interrupted' ||
    event.type === 'attempt.unknown'
  );
}

function pageMatches(
  page: HarnessDialogPage,
  snapshot: HarnessSnapshot,
): boolean {
  return (
    page.nodeId === snapshot.nodeId &&
    page.epoch === snapshot.epoch &&
    page.snapshotStateVersion >= snapshot.stateVersion &&
    page.lastEventSeq >= snapshot.lastEventSeq
  );
}

function historyMatches(
  page: HarnessHistoryPage,
  snapshot: HarnessSnapshot,
  nodeId: string,
  dialogId: string,
): boolean {
  return (
    page.nodeId === nodeId &&
    page.dialogId === dialogId &&
    page.epoch === snapshot.epoch &&
    page.snapshotStateVersion >= snapshot.stateVersion &&
    page.lastEventSeq >= snapshot.lastEventSeq
  );
}

function archivePageMatches(
  page: HarnessRequestPage | HarnessAttemptPage | HarnessEventPage,
  snapshot: HarnessSnapshot,
): boolean {
  return (
    page.nodeId === snapshot.nodeId &&
    page.epoch === snapshot.epoch &&
    page.snapshotStateVersion >= snapshot.stateVersion &&
    page.lastEventSeq >= snapshot.lastEventSeq
  );
}

function hex(bytes: ArrayBuffer): string {
  return Array.from(new Uint8Array(bytes), (value) =>
    value.toString(16).padStart(2, '0'),
  ).join('');
}

function ArtifactDownload({
  session,
  nodeId,
  dialogId,
  attemptId,
  content,
  expectedCallId,
  bindCallId = false,
  expectedName,
  expectedMediaType,
  onExpired,
}: {
  session: Session;
  nodeId: string;
  dialogId: string;
  attemptId: string;
  content: ArtifactContent;
  expectedCallId?: string;
  bindCallId?: boolean;
  expectedName?: string;
  expectedMediaType?: string;
  onExpired: () => void;
}) {
  const [state, setState] = useState<'idle' | 'loading' | 'error'>('idle');
  const [error, setError] = useState('');
  const abortRef = useRef<AbortController | null>(null);
  const mounted = useRef(true);
  useEffect(
    () => () => {
      mounted.current = false;
      abortRef.current?.abort();
    },
    [],
  );

  async function download() {
    if (state === 'loading') return;
    const abort = new AbortController();
    const timeout = window.setTimeout(() => abort.abort(), 21_000);
    abortRef.current = abort;
    setState('loading');
    setError('');
    try {
      const metadata = await harnessAPI.artifactMetadata(
        session,
        nodeId,
        content.artifactId,
        abort.signal,
      );
      if (
        metadata.nodeId !== nodeId ||
        metadata.dialogId !== dialogId ||
        metadata.attemptId !== attemptId ||
        metadata.artifactId !== content.artifactId ||
        metadata.sizeBytes !== content.sizeBytes ||
        metadata.sha256 !== content.sha256 ||
        metadata.redaction !== content.redaction ||
        metadata.truncated !== content.truncated ||
        (bindCallId && metadata.callId !== expectedCallId) ||
        (expectedName !== undefined && metadata.name !== expectedName) ||
        (expectedMediaType !== undefined &&
          metadata.mediaType !== expectedMediaType)
      ) {
        throw new HarnessAPIError(
          200,
          'artifact_scope_mismatch',
          'Описание файла не совпадает с сообщением.',
          false,
          'unknown',
        );
      }
      const binary = await harnessAPI.artifact(
        session,
        nodeId,
        content.artifactId,
        abort.signal,
      );
      const digest = hex(await crypto.subtle.digest('SHA-256', binary.bytes));
      if (
        binary.bytes.byteLength !== metadata.sizeBytes ||
        digest !== metadata.sha256 ||
        binary.mediaType !== 'application/octet-stream' ||
        !/^attachment(?:;|$)/i.test(binary.disposition)
      ) {
        throw new HarnessAPIError(
          200,
          'artifact_integrity_mismatch',
          'Размер, SHA-256 или заголовки артефакта не совпали.',
          false,
          'unknown',
        );
      }
      const url = URL.createObjectURL(
        new Blob([binary.bytes], { type: metadata.mediaType }),
      );
      try {
        const link = document.createElement('a');
        link.href = url;
        link.download = metadata.name;
        link.click();
      } finally {
        URL.revokeObjectURL(url);
      }
      setState('idle');
    } catch (cause) {
      if (!mounted.current) return;
      if (cause instanceof HarnessAPIError && cause.status === 401) {
        onExpired();
        return;
      }
      setError(
        abort.signal.aborted
          ? 'Загрузка артефакта превысила 21 секунду.'
          : safeError(cause),
      );
      setState('error');
    } finally {
      window.clearTimeout(timeout);
      if (abortRef.current === abort) abortRef.current = null;
    }
  }

  return (
    <span className="harness-artifact">
      <button onClick={download} disabled={state === 'loading'}>
        {state === 'loading'
          ? 'Проверяем артефакт…'
          : `Скачать артефакт (${content.sizeBytes} байт)`}
      </button>
      {error && <span className="notice error">{error}</span>}
    </span>
  );
}

function ControlAction({
  label,
  intent,
  disabled,
  onRun,
  onReconcile,
}: {
  label: string;
  intent?: ControlIntent;
  disabled: boolean;
  onRun: () => void;
  onReconcile: () => void;
}) {
  const busy = intent?.phase === 'sending' || intent?.phase === 'checking';
  const blocked =
    disabled ||
    busy ||
    intent?.phase === 'unknown' ||
    intent?.phase === 'accepted';
  return (
    <span className="harness-control">
      <button onClick={onRun} disabled={blocked}>
        {intent?.phase === 'sending'
          ? `${label}…`
          : intent?.phase === 'retry-ready'
            ? 'Повторить запрос'
            : intent?.phase === 'accepted'
              ? 'Запрос принят'
              : label}
      </button>
      {intent?.phase === 'unknown' && (
        <>
          <span className="notice error">
            Результат запроса пока не подтверждён. Его можно безопасно
            проверить.
          </span>
          <details className="technical-details">
            <summary>Технические детали</summary>
            <code>commandId: {intent.command.commandId}</code>
          </details>
          <button onClick={onReconcile}>Проверить запрос</button>
        </>
      )}
      {intent?.phase === 'accepted' && (
        <span className="muted">
          Агент принял запрос. Итог появится в состоянии или журнале работы.
        </span>
      )}
      {intent?.error && intent.phase !== 'unknown' && (
        <span className="notice error">{intent.error}</span>
      )}
    </span>
  );
}

function ContentView({
  label,
  content,
  session,
  nodeId,
  dialogId,
  attemptId,
  callId,
  renderMarkdown = false,
  onExpired,
}: {
  label: string;
  content: SafeContent;
  session: Session;
  nodeId: string;
  dialogId: string;
  attemptId: string;
  callId?: string;
  renderMarkdown?: boolean;
  onExpired: () => void;
}) {
  return (
    <div className="harness-safe-content">
      <strong>{label}</strong>
      {content.kind === 'inline' ? (
        renderMarkdown ? (
          <SafeMarkdown markdown={content.content} />
        ) : (
          <pre>{content.content}</pre>
        )
      ) : content.kind === 'artifact' ? (
        <ArtifactDownload
          session={session}
          nodeId={nodeId}
          dialogId={dialogId}
          attemptId={attemptId}
          content={content}
          expectedCallId={callId}
          onExpired={onExpired}
        />
      ) : (
        <span className="notice">
          Недоступно: {unavailableReasonLabel(content.reason)}.
        </span>
      )}
      {contentNotice(content) && (
        <p className="muted content-notice">{contentNotice(content)}</p>
      )}
      <details className="technical-details">
        <summary>Технические детали</summary>
        <span>
          Фильтрация: {content.redaction}; сокращено:{' '}
          {content.truncated ? 'да' : 'нет'}
        </span>
      </details>
    </div>
  );
}

export function HarnessWorkspace({
  session,
  onExpired,
  selectedNodeId,
  onBack,
}: Props) {
  const [nodes, setNodes] = useState<HarnessNode[]>([]);
  const [mode, setMode] = useState<'live' | 'fixture'>('live');
  const [registryVersion, setRegistryVersion] = useState(0);
  const [nodeId, setNodeId] = useState('');
  const [dialogId, setDialogId] = useState('');
  const [dialogs, setDialogs] = useState<HarnessDialogPage['items']>([]);
  const [snapshot, setSnapshot] = useState<HarnessSnapshot | null>(null);
  const [history, setHistory] = useState<HistoryState | null>(null);
  const [requests, setRequests] = useState<RequestState | null>(null);
  const [attempts, setAttempts] = useState<AttemptState | null>(null);
  const [timeline, setTimeline] = useState<TimelineState | null>(null);
  const [selectedRequestId, setSelectedRequestId] = useState('');
  const [selectedAttemptId, setSelectedAttemptId] = useState('');
  const [loadingRequestsMore, setLoadingRequestsMore] = useState(false);
  const [loadingAttemptsMore, setLoadingAttemptsMore] = useState(false);
  const [identity, setIdentity] = useState<HarnessNodeIdentity | null>(null);
  const [drafts, setDrafts] = useState<Record<string, HarnessDraft>>({});
  const [creates, setCreates] = useState<Record<string, CreateIntent>>({});
  const [deletes, setDeletes] = useState<Record<string, DeleteIntent>>({});
  const [deleteTarget, setDeleteTarget] = useState<DialogItem | null>(null);
  const [controls, setControls] = useState<Record<string, ControlIntent>>({});
  const [inputDrafts, setInputDrafts] = useState<Record<string, string>>({});
  const [retryAcknowledgements, setRetryAcknowledgements] = useState<
    Record<string, boolean>
  >({});
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [nodeLoading, setNodeLoading] = useState(false);
  const [health, setHealth] = useState<'fresh' | 'stale' | 'unknown'>(
    'unknown',
  );
  const [streamBaseline, setStreamBaseline] = useState<StreamBaseline | null>(
    null,
  );
  const [bootstrapVersion, setBootstrapVersion] = useState(0);

  const eventSource = useRef<EventSource | null>(null);
  const deleteDialogElement = useRef<HTMLDialogElement | null>(null);
  const deleteCancelButton = useRef<HTMLButtonElement | null>(null);
  const deleteLocks = useRef(new Set<string>());
  const dialogsRef = useRef<HarnessDialogPage['items']>([]);
  const selectedDialogsRef = useRef<Record<string, string>>({});
  const nodeRef = useRef('');
  const dialogRef = useRef('');
  const generationRef = useRef(0);
  const lastEvent = useRef<StreamBaseline | null>(null);
  const lastHealthyAt = useRef(0);
  const refreshTicket = useRef(0);
  const historyTicket = useRef(0);
  const requestsTicket = useRef(0);
  const attemptsTicket = useRef(0);
  const timelineTicket = useRef(0);
  const selectedRequestRef = useRef('');
  const selectedAttemptRef = useRef('');
  const sessionRef = useRef(session);
  const controlsRef = useRef<Record<string, ControlIntent>>({});
  const refreshTimer = useRef<number | undefined>(undefined);
  const resyncCount = useRef(0);
  const resyncNode = useRef('');
  const readControllers = useRef(new Set<AbortController>());
  const mutationControllers = useRef(new Set<AbortController>());

  useEffect(() => {
    nodeRef.current = nodeId;
    dialogRef.current = dialogId;
  }, [dialogId, nodeId]);

  useEffect(() => {
    dialogsRef.current = dialogs;
  }, [dialogs]);

  useEffect(() => {
    const element = deleteDialogElement.current;
    if (!element) return;
    if (deleteTarget) {
      if (!element.open) {
        if (typeof element.showModal === 'function') element.showModal();
        else element.setAttribute('open', '');
      }
      const frame = window.requestAnimationFrame(() =>
        deleteCancelButton.current?.focus(),
      );
      return () => window.cancelAnimationFrame(frame);
    }
    if (!element.open) return;
    if (typeof element.close === 'function') element.close();
    else element.removeAttribute('open');
  }, [deleteTarget]);

  useEffect(() => {
    sessionRef.current = session;
    const controllers = mutationControllers.current;
    return () => {
      for (const controller of controllers) controller.abort();
      controllers.clear();
    };
  }, [session]);

  const fail = useCallback(
    (cause: unknown, visible = true) => {
      if (cause instanceof DOMException && cause.name === 'AbortError') return;
      if (cause instanceof HarnessAPIError && cause.status === 401) {
        eventSource.current?.close();
        onExpired();
        return;
      }
      if (visible) setError(safeError(cause));
    },
    [onExpired],
  );

  const selectDialog = useCallback(
    (nextNodeId: string, nextDialogId: string) => {
      selectedDialogsRef.current = {
        ...selectedDialogsRef.current,
        [nextNodeId]: nextDialogId,
      };
      if (nodeRef.current === nextNodeId) {
        dialogRef.current = nextDialogId;
        setDialogId(nextDialogId);
      }
    },
    [],
  );

  const clearDeletedDialog = useCallback(
    (targetNodeId: string, targetDialogId: string) => {
      const selectedDialogWasDeleted =
        nodeRef.current === targetNodeId &&
        dialogRef.current === targetDialogId;
      const currentDialogs = dialogsRef.current;
      const deletedIndex = currentDialogs.findIndex(
        (item) => item.dialogId === targetDialogId,
      );
      const remaining = currentDialogs.filter(
        (item) => item.dialogId !== targetDialogId,
      );
      const fallbackDialogId =
        remaining[Math.max(0, deletedIndex)]?.dialogId ??
        remaining.at(-1)?.dialogId ??
        '';
      dialogsRef.current = remaining;
      setDialogs(remaining);
      const storedDialogId = selectedDialogsRef.current[targetNodeId];
      selectedDialogsRef.current = {
        ...selectedDialogsRef.current,
        [targetNodeId]:
          storedDialogId === targetDialogId || selectedDialogWasDeleted
            ? fallbackDialogId
            : (storedDialogId ?? fallbackDialogId),
      };
      if (selectedDialogWasDeleted) {
        dialogRef.current = fallbackDialogId;
        setDialogId(fallbackDialogId);
      }
      setHistory((current) =>
        current?.nodeId === targetNodeId && current.dialogId === targetDialogId
          ? null
          : current,
      );
      setRequests((current) =>
        current?.nodeId === targetNodeId
          ? {
              ...current,
              page: {
                ...current.page,
                items: current.page.items.filter(
                  (item) => item.dialogId !== targetDialogId,
                ),
              },
            }
          : current,
      );
      setAttempts((current) =>
        current?.nodeId === targetNodeId &&
        current.page.dialogId === targetDialogId
          ? null
          : current,
      );
      setTimeline((current) =>
        current?.nodeId === targetNodeId && current.dialogId === targetDialogId
          ? null
          : current,
      );
      if (selectedDialogWasDeleted) {
        selectedRequestRef.current = '';
        selectedAttemptRef.current = '';
        setSelectedRequestId('');
        setSelectedAttemptId('');
      }
      setDrafts((current) => {
        const key = targetKey(targetNodeId, targetDialogId);
        if (!(key in current)) return current;
        const next = { ...current };
        delete next[key];
        return next;
      });
      const dialogStatePrefix = `${targetNodeId}:${targetDialogId}:`;
      setInputDrafts((current) =>
        Object.fromEntries(
          Object.entries(current).filter(
            ([key]) => !key.startsWith(dialogStatePrefix),
          ),
        ),
      );
      setRetryAcknowledgements((current) =>
        Object.fromEntries(
          Object.entries(current).filter(
            ([key]) => !key.startsWith(dialogStatePrefix),
          ),
        ),
      );
      setDeletes((current) => {
        const key = targetKey(targetNodeId, targetDialogId);
        if (!(key in current)) return current;
        const next = { ...current };
        delete next[key];
        return next;
      });
      setDeleteTarget((current) =>
        current?.dialogId === targetDialogId ? null : current,
      );
    },
    [],
  );

  useEffect(() => {
    const abort = new AbortController();
    harnessAPI
      .nodes(session, abort.signal)
      .then((result) => {
        if (abort.signal.aborted) return;
        setNodes(result.nodes);
        setMode(result.mode);
        setRegistryVersion(result.registryVersion);
        setNodeId((current) => {
          const next = result.nodes.some(
            (node) => node.nodeId === selectedNodeId,
          )
            ? (selectedNodeId ?? '')
            : result.nodes.some((node) => node.nodeId === current)
              ? current
              : (result.nodes[0]?.nodeId ?? '');
          nodeRef.current = next;
          if (!next) {
            dialogRef.current = '';
            setDialogId('');
          }
          return next;
        });
      })
      .catch((cause) => {
        if (!abort.signal.aborted) fail(cause);
      })
      .finally(() => {
        if (!abort.signal.aborted) setLoading(false);
      });
    return () => abort.abort();
  }, [session, fail, selectedNodeId]);

  useEffect(() => {
    if (
      !selectedNodeId ||
      selectedNodeId === nodeRef.current ||
      !nodes.some((node) => node.nodeId === selectedNodeId)
    ) {
      return;
    }
    const nextDialogId = selectedDialogsRef.current[selectedNodeId] ?? '';
    nodeRef.current = selectedNodeId;
    dialogRef.current = nextDialogId;
    setNodeId(selectedNodeId);
    setDialogId(nextDialogId);
  }, [nodes, selectedNodeId]);

  const triggerResync = useCallback(
    (targetNodeId: string, generation: number, reason: string) => {
      if (
        nodeRef.current !== targetNodeId ||
        generationRef.current !== generation
      ) {
        return;
      }
      eventSource.current?.close();
      setStreamBaseline(null);
      setHealth('stale');
      if (resyncNode.current !== targetNodeId) {
        resyncNode.current = targetNodeId;
        resyncCount.current = 0;
      }
      resyncCount.current += 1;
      if (resyncCount.current > 3) {
        setError(`${reason} Автоматическое восстановление остановлено.`);
        return;
      }
      setError(`${reason} Перечитываем состояние.`);
      setBootstrapVersion((value) => value + 1);
    },
    [],
  );
  const triggerResyncRef = useRef(triggerResync);
  useEffect(() => {
    triggerResyncRef.current = triggerResync;
  }, [triggerResync]);

  const refreshView = useCallback(
    async (
      targetNodeId: string,
      generation: number,
      epoch: number,
      targetDialogId: string,
    ) => {
      const ticket = ++refreshTicket.current;
      const abort = new AbortController();
      readControllers.current.add(abort);
      let nextSnapshot: HarnessSnapshot;
      let nextDialogs: HarnessDialogPage;
      let nextHistory: HarnessHistoryPage | null;
      try {
        [nextSnapshot, nextDialogs, nextHistory] = await Promise.all([
          harnessAPI.snapshot(session, targetNodeId, abort.signal),
          harnessAPI.dialogs(session, targetNodeId, '', 50, abort.signal),
          targetDialogId
            ? harnessAPI.history(
                session,
                targetNodeId,
                targetDialogId,
                '',
                100,
                abort.signal,
              )
            : Promise.resolve(null),
        ]);
      } finally {
        readControllers.current.delete(abort);
      }
      if (
        generation !== generationRef.current ||
        targetNodeId !== nodeRef.current ||
        ticket !== refreshTicket.current
      ) {
        return;
      }
      if (
        nextSnapshot.epoch !== epoch ||
        !pageMatches(nextDialogs, nextSnapshot) ||
        (nextHistory &&
          !historyMatches(
            nextHistory,
            nextSnapshot,
            targetNodeId,
            targetDialogId,
          ))
      ) {
        triggerResyncRef.current(
          targetNodeId,
          generation,
          'Состояние ноды сменило epoch или scope.',
        );
        return;
      }
      setSnapshot(nextSnapshot);
      setDialogs(nextDialogs.items);
      const cursor = lastEvent.current;
      if (
        cursor?.nodeId === targetNodeId &&
        cursor.epoch === nextSnapshot.epoch &&
        nextSnapshot.lastEventSeq > cursor.seq
      ) {
        lastEvent.current = {
          ...cursor,
          seq: nextSnapshot.lastEventSeq,
        };
      }
      if (nextHistory && dialogRef.current === targetDialogId) {
        setHistory({
          nodeId: targetNodeId,
          dialogId: targetDialogId,
          epoch,
          page: nextHistory,
        });
      }
      const preferred = selectedDialogsRef.current[targetNodeId];
      const nextDialogId = nextDialogs.items.some(
        (item) => item.dialogId === preferred,
      )
        ? preferred
        : (nextDialogs.items[0]?.dialogId ?? '');
      if (
        !targetDialogId ||
        !nextDialogs.items.some((item) => item.dialogId === targetDialogId)
      ) {
        selectedDialogsRef.current = {
          ...selectedDialogsRef.current,
          [targetNodeId]: nextDialogId,
        };
        if (nodeRef.current === targetNodeId) {
          dialogRef.current = nextDialogId;
          setDialogId(nextDialogId);
        }
      }
      lastHealthyAt.current = Date.now();
      setHealth('fresh');
    },
    // oxlint-disable-next-line react/react-compiler -- fetches use this session
    [session],
  );

  const scheduleRefresh = useCallback(
    (targetNodeId: string, generation: number, epoch: number) => {
      if (refreshTimer.current !== undefined) return;
      refreshTimer.current = window.setTimeout(() => {
        refreshTimer.current = undefined;
        void refreshView(
          targetNodeId,
          generation,
          epoch,
          dialogRef.current,
        ).catch((cause) => fail(cause));
      }, 25);
    },
    [fail, refreshView],
  );

  useEffect(() => {
    if (!nodeId) {
      return;
    }
    const abort = new AbortController();
    const generation = ++generationRef.current;
    const bootstrapTicket = ++refreshTicket.current;
    if (refreshTimer.current !== undefined) {
      window.clearTimeout(refreshTimer.current);
      refreshTimer.current = undefined;
    }
    for (const controller of readControllers.current) controller.abort();
    readControllers.current.clear();
    eventSource.current?.close();
    queueMicrotask(() => {
      if (abort.signal.aborted || generation !== generationRef.current) return;
      setNodeLoading(true);
      setError('');
      setIdentity(null);
      setSnapshot(null);
      setDialogs([]);
      setHistory(null);
      setRequests(null);
      setAttempts(null);
      setTimeline(null);
      selectedRequestRef.current = '';
      selectedAttemptRef.current = '';
      setSelectedRequestId('');
      setSelectedAttemptId('');
      setStreamBaseline(null);
      setHealth('unknown');
    });
    if (resyncNode.current !== nodeId) {
      resyncNode.current = nodeId;
      resyncCount.current = 0;
    }

    void (async () => {
      try {
        const [nextIdentity, nextSnapshot] = await Promise.all([
          harnessAPI.identity(session, nodeId, abort.signal),
          harnessAPI.snapshot(session, nodeId, abort.signal),
        ]);
        if (
          abort.signal.aborted ||
          generation !== generationRef.current ||
          bootstrapTicket !== refreshTicket.current
        ) {
          return;
        }
        assertIdentity(nodeId, registryVersion, nextIdentity);
        if (nextIdentity.identityEpoch !== nextSnapshot.epoch) {
          throw new HarnessAPIError(
            200,
            'identity_epoch_mismatch',
            'Данные об агенте относятся к разным поколениям состояния.',
            false,
            'unknown',
          );
        }
        setIdentity(nextIdentity);
        setSnapshot(nextSnapshot);
        lastEvent.current = {
          nodeId,
          generation,
          epoch: nextSnapshot.epoch,
          seq: nextSnapshot.lastEventSeq,
        };
        setStreamBaseline(lastEvent.current);
        lastHealthyAt.current = Date.now();
        setHealth('fresh');

        const page = await harnessAPI.dialogs(
          session,
          nodeId,
          '',
          50,
          abort.signal,
        );
        if (
          abort.signal.aborted ||
          generation !== generationRef.current ||
          bootstrapTicket !== refreshTicket.current
        ) {
          return;
        }
        if (!pageMatches(page, nextSnapshot)) {
          triggerResync(nodeId, generation, 'Страница диалогов устарела.');
          return;
        }
        setDialogs(page.items);
        const preferred = selectedDialogsRef.current[nodeId];
        const nextDialogId = page.items.some(
          (item) => item.dialogId === preferred,
        )
          ? preferred
          : (page.items[0]?.dialogId ?? '');
        selectDialog(nodeId, nextDialogId);
      } catch (cause) {
        if (!abort.signal.aborted) fail(cause);
      } finally {
        if (!abort.signal.aborted && generation === generationRef.current) {
          setNodeLoading(false);
        }
      }
    })();
    return () => abort.abort();
  }, [
    bootstrapVersion,
    fail,
    nodeId,
    registryVersion,
    selectDialog,
    session,
    triggerResync,
  ]);

  useEffect(() => {
    if (!nodeId || !dialogId || !snapshot) {
      return;
    }
    const abort = new AbortController();
    const generation = generationRef.current;
    const epoch = snapshot.epoch;
    const ticket = ++historyTicket.current;
    harnessAPI
      .history(session, nodeId, dialogId, '', 100, abort.signal)
      .then((page) => {
        if (
          abort.signal.aborted ||
          ticket !== historyTicket.current ||
          generation !== generationRef.current ||
          !acceptsTarget(nodeRef.current, dialogRef.current, nodeId, dialogId)
        ) {
          return;
        }
        if (!historyMatches(page, snapshot, nodeId, dialogId)) {
          triggerResync(
            nodeId,
            generation,
            'История относится к другому epoch.',
          );
          return;
        }
        setHistory({ nodeId, dialogId, epoch, page });
      })
      .catch((cause) => {
        if (!abort.signal.aborted) fail(cause);
      });
    return () => abort.abort();
  }, [dialogId, fail, nodeId, session, snapshot, triggerResync]);

  useEffect(() => {
    if (!nodeId || !snapshot) return;
    const abort = new AbortController();
    const generation = generationRef.current;
    const ticket = ++requestsTicket.current;
    harnessAPI
      .requests(session, nodeId, '', '', 100, abort.signal)
      .then((page) => {
        if (
          abort.signal.aborted ||
          ticket !== requestsTicket.current ||
          generation !== generationRef.current ||
          nodeId !== nodeRef.current
        ) {
          return;
        }
        if (!archivePageMatches(page, snapshot)) {
          triggerResync(nodeId, generation, 'Список поручений устарел.');
          return;
        }
        setRequests({ nodeId, epoch: snapshot.epoch, page });
      })
      .catch((cause) => {
        if (!abort.signal.aborted) fail(cause);
      });
    return () => abort.abort();
  }, [fail, nodeId, session, snapshot, triggerResync]);

  useEffect(() => {
    const items =
      requests?.nodeId === nodeId && requests.epoch === snapshot?.epoch
        ? requests.page.items.filter((item) => item.dialogId === dialogId)
        : [];
    const preferred = items.some(
      (item) => item.requestId === selectedRequestRef.current,
    )
      ? selectedRequestRef.current
      : snapshot?.activeAttempt?.dialogId === dialogId &&
          items.some(
            (item) => item.requestId === snapshot.activeAttempt?.requestId,
          )
        ? snapshot.activeAttempt.requestId
        : (items.at(-1)?.requestId ?? '');
    if (preferred === selectedRequestRef.current) return;
    selectedRequestRef.current = preferred;
    selectedAttemptRef.current = '';
    setSelectedRequestId(preferred);
    setSelectedAttemptId('');
    setAttempts(null);
    setTimeline(null);
  }, [dialogId, nodeId, requests, snapshot?.activeAttempt, snapshot?.epoch]);

  useEffect(() => {
    if (!nodeId || !dialogId || !selectedRequestId || !snapshot) return;
    const abort = new AbortController();
    const generation = generationRef.current;
    const ticket = ++attemptsTicket.current;
    harnessAPI
      .attempts(session, nodeId, selectedRequestId, '', 100, abort.signal)
      .then((page) => {
        if (
          abort.signal.aborted ||
          ticket !== attemptsTicket.current ||
          generation !== generationRef.current ||
          nodeRef.current !== nodeId ||
          dialogRef.current !== dialogId ||
          selectedRequestRef.current !== selectedRequestId
        ) {
          return;
        }
        if (page.dialogId !== dialogId || !archivePageMatches(page, snapshot)) {
          triggerResync(nodeId, generation, 'Список попыток устарел.');
          return;
        }
        setAttempts({
          nodeId,
          requestId: selectedRequestId,
          epoch: snapshot.epoch,
          page,
        });
        const preferred = page.items.some(
          (item) => item.attemptId === selectedAttemptRef.current,
        )
          ? selectedAttemptRef.current
          : (page.items.at(-1)?.attemptId ?? '');
        selectedAttemptRef.current = preferred;
        setSelectedAttemptId(preferred);
      })
      .catch((cause) => {
        if (!abort.signal.aborted) fail(cause);
      });
    return () => abort.abort();
  }, [
    dialogId,
    fail,
    nodeId,
    selectedRequestId,
    session,
    snapshot,
    triggerResync,
  ]);

  useEffect(() => {
    if (!nodeId || !dialogId || !selectedAttemptId || !snapshot) return;
    const abort = new AbortController();
    const generation = generationRef.current;
    const ticket = ++timelineTicket.current;
    harnessAPI
      .attemptEvents(session, nodeId, selectedAttemptId, 0, 100, abort.signal)
      .then((page) => {
        if (
          abort.signal.aborted ||
          ticket !== timelineTicket.current ||
          generation !== generationRef.current ||
          nodeRef.current !== nodeId ||
          dialogRef.current !== dialogId ||
          selectedAttemptRef.current !== selectedAttemptId
        ) {
          return;
        }
        if (page.dialogId !== dialogId || !archivePageMatches(page, snapshot)) {
          triggerResync(nodeId, generation, 'Лента попытки устарела.');
          return;
        }
        const archiveEvents = [...page.items].sort((a, b) => a.seq - b.seq);
        setTimeline({
          nodeId,
          dialogId,
          attemptId: selectedAttemptId,
          epoch: snapshot.epoch,
          events: archiveEvents,
          archiveAfter: archiveEvents.at(-1)?.seq ?? 0,
          hasMore: page.nextCursor !== null,
          loadingMore: false,
        });
      })
      .catch((cause) => {
        if (!abort.signal.aborted) fail(cause);
      });
    return () => abort.abort();
  }, [
    dialogId,
    fail,
    nodeId,
    selectedAttemptId,
    session,
    snapshot,
    triggerResync,
  ]);

  useEffect(() => {
    if (
      !streamBaseline ||
      streamBaseline.nodeId !== nodeId ||
      streamBaseline.generation !== generationRef.current
    ) {
      return;
    }
    const { generation, epoch } = streamBaseline;
    let source: EventSource | null = null;
    let retry = 0;
    let retryTimer: number | undefined;
    let stableTimer: number | undefined;
    let stopped = false;

    const connect = () => {
      if (stopped || generation !== generationRef.current) return;
      const after = lastEvent.current?.seq ?? streamBaseline.seq;
      source = new EventSource(harnessAPI.eventsURL(nodeId, after), {
        withCredentials: true,
      });
      eventSource.current = source;
      source.onopen = () => {
        lastHealthyAt.current = Date.now();
        setHealth('fresh');
        if (stableTimer !== undefined) window.clearTimeout(stableTimer);
        stableTimer = window.setTimeout(() => {
          if (!stopped && generation === generationRef.current) {
            retry = 0;
            resyncCount.current = 0;
          }
        }, 15_000);
      };
      source.onmessage = (event) => {
        if (stopped || generation !== generationRef.current) return;
        let parsed;
        try {
          parsed = parseHarnessEvent(event.data);
        } catch {
          triggerResync(nodeId, generation, 'Получено некорректное событие.');
          return;
        }
        const decision = classifyEvent(lastEvent.current, {
          nodeId: parsed.nodeId,
          epoch: parsed.epoch,
          seq: parsed.seq,
        });
        if (decision === 'duplicate') return;
        if (decision !== 'accept') {
          triggerResync(
            nodeId,
            generation,
            decision === 'epoch-change'
              ? 'Событие относится к новому epoch.'
              : decision === 'gap'
                ? 'В потоке событий обнаружен разрыв.'
                : 'Поток вернул событие другой ноды.',
          );
          return;
        }
        lastEvent.current = {
          nodeId,
          generation,
          epoch: parsed.epoch,
          seq: parsed.seq,
        };
        lastHealthyAt.current = Date.now();
        setHealth('fresh');
        if (parsed.type === 'dialog.deleted') {
          clearDeletedDialog(nodeId, parsed.dialogId);
        }
        if (
          'attemptId' in parsed &&
          parsed.attemptId === selectedAttemptRef.current &&
          parsed.dialogId === dialogRef.current
        ) {
          setTimeline((current) => {
            if (
              !current ||
              current.nodeId !== nodeId ||
              current.dialogId !== parsed.dialogId ||
              current.attemptId !== parsed.attemptId ||
              current.epoch !== parsed.epoch ||
              current.events.some((item) => item.seq === parsed.seq)
            ) {
              return current;
            }
            return {
              ...current,
              events: [...current.events, parsed].sort((a, b) => a.seq - b.seq),
            };
          });
        }
        scheduleRefresh(nodeId, generation, epoch);
      };
      source.onerror = () => {
        source?.close();
        if (stableTimer !== undefined) window.clearTimeout(stableTimer);
        if (stopped || generation !== generationRef.current) return;
        const readyAbort = new AbortController();
        readControllers.current.add(readyAbort);
        void harnessAPI
          .ready(session, nodeId, readyAbort.signal)
          .then((ready) => {
            if (stopped || generation !== generationRef.current) return;
            assertIdentity(nodeId, registryVersion, ready.identity);
            if (ready.identity.identityEpoch !== epoch) {
              triggerResync(
                nodeId,
                generation,
                'Поколение состояния агента изменилось.',
              );
            }
          })
          .catch((cause) => {
            if (
              !readyAbort.signal.aborted &&
              generation === generationRef.current
            ) {
              fail(cause, false);
            }
          })
          .finally(() => readControllers.current.delete(readyAbort));
        const delay = reconnectDelay(retry);
        if (delay === null) {
          setHealth('stale');
          setError('Поток событий закрыт; автоматические повторы исчерпаны.');
          return;
        }
        retry += 1;
        retryTimer = window.setTimeout(connect, delay);
      };
    };
    connect();
    return () => {
      stopped = true;
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
      if (stableTimer !== undefined) window.clearTimeout(stableTimer);
      source?.close();
      if (eventSource.current === source) eventSource.current = null;
    };
  }, [
    clearDeletedDialog,
    fail,
    nodeId,
    registryVersion,
    scheduleRefresh,
    session,
    streamBaseline,
    triggerResync,
  ]);

  useEffect(() => {
    if (!nodeId || !identity) return;
    const generation = generationRef.current;
    const timer = window.setInterval(() => {
      if (Date.now() - lastHealthyAt.current > 15_000) setHealth('stale');
      const readyAbort = new AbortController();
      readControllers.current.add(readyAbort);
      void harnessAPI
        .ready(session, nodeId, readyAbort.signal)
        .then((ready) => {
          if (
            generation !== generationRef.current ||
            nodeId !== nodeRef.current
          ) {
            return;
          }
          assertIdentity(nodeId, registryVersion, ready.identity);
          if (ready.identity.identityEpoch !== identity.identityEpoch) {
            triggerResync(
              nodeId,
              generation,
              'Поколение состояния агента изменилось.',
            );
            return;
          }
          lastHealthyAt.current = Date.now();
          setHealth('fresh');
        })
        .catch((cause) => {
          if (
            !readyAbort.signal.aborted &&
            generation === generationRef.current
          ) {
            fail(cause, false);
          }
        })
        .finally(() => readControllers.current.delete(readyAbort));
    }, 5_000);
    return () => window.clearInterval(timer);
  }, [fail, identity, nodeId, registryVersion, session, triggerResync]);

  useEffect(
    () => () => {
      eventSource.current?.close();
      for (const controller of readControllers.current) controller.abort();
      readControllers.current.clear();
      for (const controller of mutationControllers.current) controller.abort();
      mutationControllers.current.clear();
      if (refreshTimer.current !== undefined) {
        window.clearTimeout(refreshTimer.current);
      }
    },
    [],
  );

  const selectedDialog = useMemo(
    () => dialogs.find((item) => item.dialogId === dialogId),
    [dialogs, dialogId],
  );
  const draft = readDraft(drafts, nodeId, dialogId);
  const createIntent = creates[nodeId];
  const visibleHistory =
    history?.nodeId === nodeId &&
    history.dialogId === dialogId &&
    history.epoch === snapshot?.epoch
      ? history.page
      : null;

  const updateDraft = useCallback(
    (
      targetNodeId: string,
      targetDialogId: string,
      update: (old: HarnessDraft) => HarnessDraft,
    ) => {
      setDrafts((old) =>
        writeDraft(
          old,
          targetNodeId,
          targetDialogId,
          update(readDraft(old, targetNodeId, targetDialogId)),
        ),
      );
    },
    [],
  );

  const refreshAfterCommand = useCallback(
    (targetNodeId: string, targetDialogId: string) => {
      const cursor = lastEvent.current;
      if (targetNodeId === nodeRef.current && cursor?.nodeId === targetNodeId) {
        void refreshView(
          targetNodeId,
          generationRef.current,
          cursor.epoch,
          targetDialogId,
        ).catch((cause) => fail(cause));
      }
    },
    [fail, refreshView],
  );

  function classifyCommandFailure(
    cause: unknown,
    command: MessageCommand,
  ): HarnessDraft {
    if (cause instanceof HarnessAPIError && cause.outcome === 'unknown') {
      return {
        text: command.payload.text,
        phase: 'unknown',
        command,
        error: cause.message,
      };
    }
    if (cause instanceof HarnessAPIError && cause.retryable) {
      return {
        text: command.payload.text,
        phase: 'retry-ready',
        command,
        error: cause.message,
      };
    }
    return {
      text: command.payload.text,
      phase: 'rejected',
      error: safeError(cause),
    };
  }

  async function sendMessage() {
    if (!nodeId || !dialogId || !selectedDialog || !draft.text.trim()) return;
    if (draft.phase === 'sending' || draft.phase === 'checking') return;
    const targetNodeId = nodeId;
    const targetDialogId = dialogId;
    const command: MessageCommand =
      draft.phase === 'retry-ready' && draft.command
        ? draft.command
        : {
            protocolVersion: 1,
            schemaId: 'harness-wire-v2',
            commandId: newCommandId(),
            kind: 'message.enqueue',
            target: { nodeId: targetNodeId, dialogId: targetDialogId },
            expected: { dialogVersion: selectedDialog.version },
            payload: { text: draft.text },
          };
    updateDraft(targetNodeId, targetDialogId, () => ({
      text: command.payload.text,
      phase: 'sending',
      command,
    }));
    try {
      await harnessAPI.command(session, targetNodeId, command);
      updateDraft(targetNodeId, targetDialogId, () => ({
        text: '',
        phase: 'queued',
      }));
      refreshAfterCommand(targetNodeId, targetDialogId);
    } catch (cause) {
      updateDraft(targetNodeId, targetDialogId, () =>
        classifyCommandFailure(cause, command),
      );
      fail(cause, false);
    }
  }

  async function reconcileMessage() {
    if (!draft.command || draft.phase !== 'unknown') return;
    const command = draft.command;
    const targetNodeId = command.target.nodeId;
    const targetDialogId = command.target.dialogId;
    updateDraft(targetNodeId, targetDialogId, (old) => ({
      ...old,
      phase: 'checking',
    }));
    try {
      await harnessAPI.status(session, targetNodeId, command);
      updateDraft(targetNodeId, targetDialogId, () => ({
        text: '',
        phase: 'queued',
      }));
      refreshAfterCommand(targetNodeId, targetDialogId);
    } catch (cause) {
      if (cause instanceof HarnessAPIError && cause.status === 404) {
        updateDraft(targetNodeId, targetDialogId, () => ({
          text: command.payload.text,
          phase: 'retry-ready',
          command,
          error: 'Команда не найдена. Можно повторить отправку.',
        }));
      } else {
        updateDraft(targetNodeId, targetDialogId, () => ({
          text: command.payload.text,
          phase: 'unknown',
          command,
          error: safeError(cause),
        }));
        fail(cause, false);
      }
    }
  }

  function classifyCreateFailure(
    cause: unknown,
    command: CreateCommand,
  ): CreateIntent {
    if (cause instanceof HarnessAPIError && cause.outcome === 'unknown') {
      return {
        phase: 'unknown',
        command,
        error: cause.message,
      };
    }
    if (cause instanceof HarnessAPIError && cause.retryable) {
      return {
        phase: 'retry-ready',
        command,
        error: cause.message,
      };
    }
    return { phase: 'rejected', command, error: safeError(cause) };
  }

  async function createDialog() {
    if (!nodeId || !snapshot || !identity) return;
    if (
      createIntent?.phase === 'sending' ||
      createIntent?.phase === 'checking' ||
      createIntent?.phase === 'unknown'
    ) {
      return;
    }
    const targetNodeId = nodeId;
    const generation = generationRef.current;
    const command: CreateCommand =
      createIntent?.phase === 'retry-ready'
        ? createIntent.command
        : {
            protocolVersion: 1,
            schemaId: 'harness-wire-v2',
            commandId: newCommandId(),
            kind: 'dialog.create',
            target: { nodeId: targetNodeId },
            expected: { registryVersion },
            payload: {},
          };
    setCreates((old) => ({
      ...old,
      [targetNodeId]: { phase: 'sending', command },
    }));
    try {
      const receipt = await harnessAPI.command(session, targetNodeId, command);
      if (receipt.commandKind !== 'dialog.create') {
        throw new HarnessAPIError(
          200,
          'receipt_kind_mismatch',
          'Harness подтвердил другую команду.',
          false,
          'unknown',
        );
      }
      const newDialogId = receipt.references.dialogId;
      setCreates((old) => {
        const next = { ...old };
        delete next[targetNodeId];
        return next;
      });
      if (
        targetNodeId === nodeRef.current &&
        generation === generationRef.current
      ) {
        selectDialog(targetNodeId, newDialogId);
        refreshAfterCommand(targetNodeId, newDialogId);
      }
    } catch (cause) {
      setCreates((old) => ({
        ...old,
        [targetNodeId]: classifyCreateFailure(cause, command),
      }));
      fail(cause, false);
    }
  }

  async function reconcileCreate() {
    if (!createIntent || createIntent.phase !== 'unknown') return;
    const { command } = createIntent;
    const targetNodeId = command.target.nodeId;
    setCreates((old) => ({
      ...old,
      [targetNodeId]: { phase: 'checking', command },
    }));
    try {
      const status = await harnessAPI.status(session, targetNodeId, command);
      if (status.receipt.commandKind !== 'dialog.create') {
        throw new HarnessAPIError(
          200,
          'receipt_kind_mismatch',
          'Harness вернул status другой команды.',
          false,
          'unknown',
        );
      }
      const newDialogId = status.receipt.references.dialogId;
      setCreates((old) => {
        const next = { ...old };
        delete next[targetNodeId];
        return next;
      });
      if (targetNodeId === nodeRef.current) {
        selectDialog(targetNodeId, newDialogId);
        refreshAfterCommand(targetNodeId, newDialogId);
      }
    } catch (cause) {
      setCreates((old) => ({
        ...old,
        [targetNodeId]:
          cause instanceof HarnessAPIError && cause.status === 404
            ? {
                phase: 'retry-ready',
                command,
                error: 'Команда не найдена. Можно повторить создание.',
              }
            : {
                phase: 'unknown',
                command,
                error: safeError(cause),
              },
      }));
      fail(cause, false);
    }
  }

  function deleteReceiptMatches(
    command: DeleteCommand,
    receipt: Awaited<ReturnType<typeof harnessAPI.command>>,
  ): boolean {
    return (
      receipt.commandKind === 'dialog.delete' &&
      receipt.result === 'deleted' &&
      receipt.references.dialogId === command.target.dialogId
    );
  }

  function finishDelete(command: DeleteCommand) {
    const { nodeId: targetNodeId, dialogId: targetDialogId } = command.target;
    clearDeletedDialog(targetNodeId, targetDialogId);
    refreshAfterCommand(targetNodeId, '');
  }

  async function deleteDialog(target: DialogItem) {
    if (!nodeId || target.dialogId !== deleteTarget?.dialogId) return;
    const targetNodeId = nodeId;
    const targetDialogId = target.dialogId;
    const key = targetKey(targetNodeId, targetDialogId);
    const current = deletes[key];
    if (
      deleteLocks.current.has(key) ||
      current?.phase === 'sending' ||
      current?.phase === 'checking' ||
      current?.phase === 'unknown'
    ) {
      return;
    }
    const command: DeleteCommand = {
      protocolVersion: 1,
      schemaId: 'harness-wire-v2',
      commandId: newCommandId(),
      kind: 'dialog.delete',
      target: { nodeId: targetNodeId, dialogId: targetDialogId },
      expected: { dialogVersion: target.version },
      payload: {},
    };
    const targetSession = session;
    const abort = new AbortController();
    deleteLocks.current.add(key);
    mutationControllers.current.add(abort);
    setDeletes((old) => ({
      ...old,
      [key]: { phase: 'sending', command },
    }));
    try {
      const receipt = await harnessAPI.command(
        targetSession,
        targetNodeId,
        command,
        abort.signal,
      );
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      if (!deleteReceiptMatches(command, receipt)) {
        throw new HarnessAPIError(
          200,
          'delete_receipt_mismatch',
          'Harness подтвердил удаление другого диалога.',
          false,
          'unknown',
        );
      }
      finishDelete(command);
    } catch (cause) {
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      if (cause instanceof HarnessAPIError && cause.status === 404) {
        finishDelete(command);
        return;
      }
      setDeletes((old) => ({
        ...old,
        [key]:
          cause instanceof HarnessAPIError && cause.outcome === 'unknown'
            ? { phase: 'unknown', command, error: cause.message }
            : { phase: 'rejected', command, error: safeError(cause) },
      }));
      fail(cause, false);
    } finally {
      deleteLocks.current.delete(key);
      mutationControllers.current.delete(abort);
    }
  }

  async function reconcileDelete(intent: DeleteIntent) {
    if (intent.phase !== 'unknown') return;
    const { command } = intent;
    const { nodeId: targetNodeId, dialogId: targetDialogId } = command.target;
    const key = targetKey(targetNodeId, targetDialogId);
    if (deleteLocks.current.has(key)) return;
    const targetSession = session;
    const abort = new AbortController();
    deleteLocks.current.add(key);
    readControllers.current.add(abort);
    setDeletes((old) => ({
      ...old,
      [key]: { phase: 'checking', command },
    }));
    try {
      const status = await harnessAPI.status(
        targetSession,
        targetNodeId,
        command,
        abort.signal,
      );
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      if (!deleteReceiptMatches(command, status.receipt)) {
        throw new HarnessAPIError(
          200,
          'delete_status_mismatch',
          'Harness вернул статус удаления другого диалога.',
          false,
          'unknown',
        );
      }
      finishDelete(command);
    } catch (cause) {
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      setDeletes((old) => ({
        ...old,
        [key]:
          cause instanceof HarnessAPIError && cause.status === 404
            ? {
                phase: 'rejected',
                command,
                error:
                  'Команда удаления не найдена. Список обновлён; при необходимости подтвердите удаление заново.',
              }
            : { phase: 'unknown', command, error: safeError(cause) },
      }));
      if (cause instanceof HarnessAPIError && cause.status === 404) {
        refreshAfterCommand(targetNodeId, '');
      } else {
        fail(cause, false);
      }
    } finally {
      deleteLocks.current.delete(key);
      readControllers.current.delete(abort);
    }
  }

  async function runControl(proposed: ControlCommand, targetDialogId: string) {
    const key = controlIntentKey(proposed);
    const existing = controlsRef.current[key];
    const outstanding = Object.values(controlsRef.current).find(
      (intent) =>
        isRecoverableControl(intent) &&
        controlResourceKey(intent.command) === controlResourceKey(proposed),
    );
    if (outstanding && !sameControlAction(outstanding.command, proposed)) {
      return;
    }
    if (
      existing &&
      existing.phase !== 'retry-ready' &&
      existing.phase !== 'rejected'
    ) {
      return;
    }
    const command =
      existing?.phase === 'retry-ready' ? existing.command : proposed;
    const targetSession = session;
    const abort = new AbortController();
    mutationControllers.current.add(abort);
    const sending: ControlIntent = { command, phase: 'sending' };
    controlsRef.current = { ...controlsRef.current, [key]: sending };
    setControls(controlsRef.current);
    try {
      await harnessAPI.command(
        targetSession,
        command.target.nodeId,
        command,
        abort.signal,
      );
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      const accepted: ControlIntent = { command, phase: 'accepted' };
      controlsRef.current = { ...controlsRef.current, [key]: accepted };
      setControls(controlsRef.current);
      refreshAfterCommand(command.target.nodeId, targetDialogId);
    } catch (cause) {
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      const failure =
        cause instanceof HarnessAPIError
          ? classifyControlFailure(command, cause)
          : classifyControlFailure(command, { message: safeError(cause) });
      controlsRef.current = { ...controlsRef.current, [key]: failure };
      setControls(controlsRef.current);
      fail(cause, false);
    } finally {
      mutationControllers.current.delete(abort);
    }
  }

  async function reconcileControl(
    intent: ControlIntent,
    targetDialogId: string,
  ) {
    if (intent.phase !== 'unknown') return;
    const { command } = intent;
    const key = controlIntentKey(command);
    const targetSession = session;
    const abort = new AbortController();
    readControllers.current.add(abort);
    const checking: ControlIntent = { command, phase: 'checking' };
    controlsRef.current = { ...controlsRef.current, [key]: checking };
    setControls(controlsRef.current);
    try {
      await harnessAPI.status(
        targetSession,
        command.target.nodeId,
        command,
        abort.signal,
      );
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      const accepted: ControlIntent = { command, phase: 'accepted' };
      controlsRef.current = { ...controlsRef.current, [key]: accepted };
      setControls(controlsRef.current);
      refreshAfterCommand(command.target.nodeId, targetDialogId);
    } catch (cause) {
      if (sessionRef.current !== targetSession || abort.signal.aborted) return;
      const next: ControlIntent =
        cause instanceof HarnessAPIError && cause.status === 404
          ? {
              command,
              phase: 'retry-ready',
              error: 'Запрос не найден. Можно повторить тот же запрос.',
            }
          : { command, phase: 'unknown', error: safeError(cause) };
      controlsRef.current = { ...controlsRef.current, [key]: next };
      setControls(controlsRef.current);
      fail(cause, false);
    } finally {
      readControllers.current.delete(abort);
    }
  }

  async function loadMoreRequests() {
    const current = visibleRequests;
    const cursor = current?.nextCursor;
    if (!current || !cursor || !snapshot || loadingRequestsMore) return;
    const generation = generationRef.current;
    const ticket = ++requestsTicket.current;
    const abort = new AbortController();
    readControllers.current.add(abort);
    setLoadingRequestsMore(true);
    try {
      const page = await harnessAPI.requests(
        session,
        nodeId,
        '',
        cursor,
        100,
        abort.signal,
      );
      if (
        abort.signal.aborted ||
        ticket !== requestsTicket.current ||
        generation !== generationRef.current ||
        nodeRef.current !== nodeId
      ) {
        return;
      }
      if (!archivePageMatches(page, snapshot)) {
        triggerResync(
          nodeId,
          generation,
          'Следующая страница поручений устарела.',
        );
        return;
      }
      const known = new Set(current.items.map((item) => item.requestId));
      const additions = page.items.filter((item) => !known.has(item.requestId));
      setRequests({
        nodeId,
        epoch: snapshot.epoch,
        page: {
          ...page,
          items: [...current.items, ...additions],
          nextCursor: additions.length > 0 ? page.nextCursor : null,
        },
      });
      if (additions.length === 0 && page.nextCursor !== null) {
        setError(
          'Страница поручений не продвинула курсор; загрузка остановлена.',
        );
      }
    } catch (cause) {
      if (!abort.signal.aborted) fail(cause);
    } finally {
      readControllers.current.delete(abort);
      if (generation === generationRef.current) setLoadingRequestsMore(false);
    }
  }

  async function loadMoreAttempts() {
    const current = visibleAttempts;
    const cursor = current?.nextCursor;
    if (!current || !cursor || !snapshot || loadingAttemptsMore) return;
    const targetRequestId = selectedRequestId;
    const generation = generationRef.current;
    const ticket = ++attemptsTicket.current;
    const abort = new AbortController();
    readControllers.current.add(abort);
    setLoadingAttemptsMore(true);
    try {
      const page = await harnessAPI.attempts(
        session,
        nodeId,
        targetRequestId,
        cursor,
        100,
        abort.signal,
      );
      if (
        abort.signal.aborted ||
        ticket !== attemptsTicket.current ||
        generation !== generationRef.current ||
        nodeRef.current !== nodeId ||
        dialogRef.current !== dialogId ||
        selectedRequestRef.current !== targetRequestId
      ) {
        return;
      }
      if (page.dialogId !== dialogId || !archivePageMatches(page, snapshot)) {
        triggerResync(
          nodeId,
          generation,
          'Следующая страница попыток устарела.',
        );
        return;
      }
      const known = new Set(current.items.map((item) => item.attemptId));
      const additions = page.items.filter((item) => !known.has(item.attemptId));
      setAttempts({
        nodeId,
        requestId: targetRequestId,
        epoch: snapshot.epoch,
        page: {
          ...page,
          items: [...current.items, ...additions],
          nextCursor: additions.length > 0 ? page.nextCursor : null,
        },
      });
      if (additions.length === 0 && page.nextCursor !== null) {
        setError(
          'Страница попыток не продвинула курсор; загрузка остановлена.',
        );
      }
    } catch (cause) {
      if (!abort.signal.aborted) fail(cause);
    } finally {
      readControllers.current.delete(abort);
      if (generation === generationRef.current) setLoadingAttemptsMore(false);
    }
  }

  async function loadMoreTimeline() {
    if (!timeline || !snapshot || timeline.loadingMore || !timeline.hasMore) {
      return;
    }
    const target = timeline;
    const after = target.archiveAfter;
    const generation = generationRef.current;
    const ticket = ++timelineTicket.current;
    const abort = new AbortController();
    readControllers.current.add(abort);
    setTimeline((current) =>
      current === target ? { ...current, loadingMore: true } : current,
    );
    try {
      const page = await harnessAPI.attemptEvents(
        session,
        target.nodeId,
        target.attemptId,
        after,
        100,
        abort.signal,
      );
      if (
        abort.signal.aborted ||
        ticket !== timelineTicket.current ||
        generation !== generationRef.current ||
        nodeRef.current !== target.nodeId ||
        dialogRef.current !== target.dialogId ||
        selectedAttemptRef.current !== target.attemptId
      ) {
        return;
      }
      if (
        page.dialogId !== target.dialogId ||
        !archivePageMatches(page, snapshot)
      ) {
        triggerResync(
          target.nodeId,
          generation,
          'Следующая страница ленты устарела.',
        );
        return;
      }
      const archiveItems = page.items
        .filter((event) => event.seq > after)
        .sort((a, b) => a.seq - b.seq);
      setTimeline((current) => {
        if (
          !current ||
          current.nodeId !== target.nodeId ||
          current.dialogId !== target.dialogId ||
          current.attemptId !== target.attemptId ||
          current.epoch !== target.epoch
        ) {
          return current;
        }
        const seen = new Set(current.events.map((event) => event.seq));
        const merged = archiveItems.filter((event) => !seen.has(event.seq));
        return {
          ...current,
          events: [...current.events, ...merged].sort((a, b) => a.seq - b.seq),
          archiveAfter: archiveItems.at(-1)?.seq ?? current.archiveAfter,
          hasMore: archiveItems.length > 0 && page.nextCursor !== null,
          loadingMore: false,
          error:
            archiveItems.length === 0 && page.nextCursor !== null
              ? 'Лента не продвинула курсор; продолжение остановлено.'
              : undefined,
        };
      });
    } catch (cause) {
      if (!abort.signal.aborted) {
        setTimeline((current) =>
          current?.attemptId === target.attemptId
            ? { ...current, loadingMore: false, error: safeError(cause) }
            : current,
        );
        fail(cause, false);
      }
    } finally {
      readControllers.current.delete(abort);
    }
  }

  const visibleRequests =
    requests?.nodeId === nodeId && requests.epoch === snapshot?.epoch
      ? requests.page
      : null;
  function dialogDeletionBlockReason(targetDialogId: string): string {
    if (!identity) return 'Проверяем поддержку удаления агентом.';
    if (identity.schemaSHA256 !== HARNESS_SCHEMA_SHA256) {
      return 'Удаление станет доступно после обновления агента.';
    }
    if (!snapshot || !visibleRequests) {
      return 'Состояние работы диалога ещё загружается.';
    }
    if (snapshot.activeAttempt?.dialogId === targetDialogId) {
      return 'У диалога есть активная или останавливаемая попытка.';
    }
    if (
      snapshot.pendingQueue.some((item) => item.dialogId === targetDialogId)
    ) {
      return 'В диалоге есть поручение в очереди.';
    }
    if (visibleRequests.nextCursor !== null) {
      return 'Список поручений загружен не полностью.';
    }
    const outstanding = visibleRequests.items.find(
      (item) =>
        item.dialogId === targetDialogId &&
        ['queued', 'dispatching', 'active', 'unknown'].includes(item.status),
    );
    if (!outstanding) return '';
    return outstanding.status === 'unknown'
      ? 'Исход работы в диалоге неизвестен.'
      : 'В диалоге есть незавершённая работа.';
  }
  const deleteIntent = deleteTarget
    ? deletes[targetKey(nodeId, deleteTarget.dialogId)]
    : undefined;
  const deleteBlockedReason = deleteTarget
    ? dialogDeletionBlockReason(deleteTarget.dialogId)
    : '';
  const dialogRequests =
    visibleRequests?.items.filter((item) => item.dialogId === dialogId) ?? [];
  const visibleAttempts =
    attempts?.nodeId === nodeId &&
    attempts.requestId === selectedRequestId &&
    attempts.epoch === snapshot?.epoch
      ? attempts.page
      : null;
  const selectedAttempt = visibleAttempts?.items.find(
    (item) => item.attemptId === selectedAttemptId,
  );
  const visibleTimeline =
    timeline?.nodeId === nodeId &&
    timeline.dialogId === dialogId &&
    timeline.attemptId === selectedAttemptId &&
    timeline.epoch === snapshot?.epoch
      ? timeline
      : null;
  const resolvedApprovals = new Map<string, number>();
  for (const event of visibleTimeline?.events ?? []) {
    if (event.type !== 'approval.resolved') continue;
    resolvedApprovals.set(
      event.payload.approvalId,
      Math.max(
        resolvedApprovals.get(event.payload.approvalId) ?? -1,
        event.payload.approvalVersion,
      ),
    );
  }
  const terminalEventObserved =
    selectedAttempt !== undefined &&
    (visibleTimeline?.events.some(
      (event) =>
        isTerminalAttemptEvent(event) &&
        event.payload.generation === selectedAttempt.generation,
    ) ??
      false);
  const canAnswerPending =
    selectedAttempt?.state === 'waiting_input' && !terminalEventObserved;
  const pendingApprovals = canAnswerPending
    ? (visibleTimeline?.events
        .filter(
          (event): event is ApprovalRequestedEvent =>
            event.type === 'approval.requested' &&
            event.payload.approvalVersion >
              (resolvedApprovals.get(event.payload.approvalId) ?? -1),
        )
        .filter(
          (event, index, all) =>
            !all.some(
              (later, laterIndex) =>
                laterIndex > index &&
                later.payload.approvalId === event.payload.approvalId &&
                later.payload.approvalVersion >= event.payload.approvalVersion,
            ),
        ) ?? [])
    : [];
  const resolvedInputs = new Map<string, number>();
  for (const event of visibleTimeline?.events ?? []) {
    if (event.type !== 'input.resolved') continue;
    resolvedInputs.set(
      event.payload.inputRequestId,
      Math.max(
        resolvedInputs.get(event.payload.inputRequestId) ?? -1,
        event.payload.inputVersion,
      ),
    );
  }
  const pendingInputs = canAnswerPending
    ? (visibleTimeline?.events
        .filter(
          (event): event is InputRequestedEvent =>
            event.type === 'input.requested' &&
            event.payload.inputVersion >
              (resolvedInputs.get(event.payload.inputRequestId) ?? -1),
        )
        .filter(
          (event, index, all) =>
            !all.some(
              (later, laterIndex) =>
                laterIndex > index &&
                later.payload.inputRequestId === event.payload.inputRequestId &&
                later.payload.inputVersion >= event.payload.inputVersion,
            ),
        ) ?? [])
    : [];
  const outstandingControls = Object.values(controls).filter(
    (intent) =>
      intent.command.target.nodeId === nodeId && isRecoverableControl(intent),
  );

  function controlAction(
    proposal: ControlCommand,
    label: string,
    targetDialogId: string,
    disabled = false,
  ) {
    const exactIntent = controls[controlIntentKey(proposal)];
    const resourceIntent = Object.values(controls).find(
      (candidate) =>
        isRecoverableControl(candidate) &&
        controlResourceKey(candidate.command) === controlResourceKey(proposal),
    );
    const intent = resourceIntent ?? exactIntent;
    if (intent && !sameControlAction(intent.command, proposal)) return null;
    if (intent && isRecoverableControl(intent)) return null;
    return (
      <ControlAction
        label={intent ? controlCommandLabel(intent.command) : label}
        intent={intent}
        disabled={!session.writes_enabled || disabled}
        onRun={() => {
          // oxlint-disable-next-line react/react-compiler -- ref-backed mutation runs only in click handler
          void runControl(
            { ...proposal, commandId: newCommandId() } as ControlCommand,
            targetDialogId,
          );
        }}
        onReconcile={() => {
          // oxlint-disable-next-line react/react-compiler -- ref-backed reconciliation runs only in click handler
          if (intent) void reconcileControl(intent, targetDialogId);
        }}
      />
    );
  }

  function timelineEvent(event: HarnessEvent) {
    let detail;
    switch (event.type) {
      case 'tool.started':
        detail = (
          <>
            <p className="event-summary">
              Агент запустил: <strong>{toolLabel(event.payload.toolName)}</strong>
            </p>
            <ContentView
              label="Вход"
              content={event.payload.input}
              session={session}
              nodeId={nodeId}
              dialogId={event.dialogId}
              attemptId={event.attemptId}
              callId={event.payload.callId}
              onExpired={onExpired}
            />
            <details className="technical-details">
              <summary>Технические детали вызова</summary>
              <code>tool: {event.payload.toolName}</code>
              <code>actionHash: {event.payload.actionHash}</code>
            </details>
          </>
        );
        break;
      case 'tool.output':
        detail = (
          <ContentView
            label="Полученные данные"
            content={event.payload.output}
            session={session}
            nodeId={nodeId}
            dialogId={event.dialogId}
            attemptId={event.attemptId}
            callId={event.payload.callId}
            onExpired={onExpired}
          />
        );
        break;
      case 'tool.completed':
        detail = (
          <>
            <p className="event-summary">
              Инструмент завершил работу: {stateLabel(event.payload.status)}.{' '}
              {effectSummary(event.payload.effectStatus)}
            </p>
            <ContentView
              label="Результат"
              content={event.payload.result}
              session={session}
              nodeId={nodeId}
              dialogId={event.dialogId}
              attemptId={event.attemptId}
              callId={event.payload.callId}
              onExpired={onExpired}
            />
            <details className="technical-details">
              <summary>Технические детали результата</summary>
              <span>Эффект: {event.payload.effectStatus}</span>
              {event.payload.effectRef && (
                <code>reference: {event.payload.effectRef}</code>
              )}
            </details>
          </>
        );
        break;
      case 'assistant.delta':
      case 'assistant.message':
        detail = (
          <ContentView
            label={
              event.type === 'assistant.delta'
                ? 'Фрагмент ответа'
                : 'Сообщение агента'
            }
            content={event.payload.content}
            session={session}
            nodeId={nodeId}
            dialogId={event.dialogId}
            attemptId={event.attemptId}
            renderMarkdown
            onExpired={onExpired}
          />
        );
        break;
      case 'approval.requested':
        detail = (
          <>
            <p>{event.payload.safePrompt}</p>
            <details className="technical-details">
              <summary>Технические детали решения</summary>
              <code>actionHash: {event.payload.actionHash}</code>
            </details>
          </>
        );
        break;
      case 'approval.resolved':
        detail = (
          <p>
            {event.payload.decision === 'allow_once'
              ? 'Разрешено один раз.'
              : 'Запрос отклонён.'}
          </p>
        );
        break;
      case 'input.requested':
        detail = (
          <ContentView
            label="Вопрос агента"
            content={event.payload.prompt}
            session={session}
            nodeId={nodeId}
            dialogId={event.dialogId}
            attemptId={event.attemptId}
            onExpired={onExpired}
          />
        );
        break;
      case 'input.resolved':
        detail = (
          <>
            <p>Ответ добавлен в текущий диалог.</p>
            <details className="technical-details">
              <summary>Технические детали ответа</summary>
              <code>messageId: {event.payload.messageId}</code>
            </details>
          </>
        );
        break;
      case 'artifact.available': {
        const artifact: ArtifactContent = {
          kind: 'artifact',
          artifactId: event.payload.artifactId,
          sizeBytes: event.payload.sizeBytes,
          sha256: event.payload.sha256,
          redaction: event.payload.redaction,
          truncated: event.payload.truncated,
        };
        detail = (
          <>
            <p>
              {event.payload.name} · {event.payload.mediaType}
            </p>
            <ArtifactDownload
              session={session}
              nodeId={nodeId}
              dialogId={event.dialogId}
              attemptId={event.attemptId}
              content={artifact}
              expectedCallId={event.payload.callId}
              bindCallId
              expectedName={event.payload.name}
              expectedMediaType={event.payload.mediaType}
              onExpired={onExpired}
            />
          </>
        );
        break;
      }
      case 'attempt.completed':
        detail = (
          <>
            <p className="event-summary">
              Агент завершил поручение
              {event.payload.usage
                ? ` · ${tokenCount(event.payload.usage.totalTokens)}, ${usageSourceLabel(event.payload.usage.source)}`
                : ''}
              .
            </p>
            <details className="technical-details">
              <summary>Технические детали результата</summary>
              <span>Формат результата: {event.payload.output.kind}</span>
              <span>
                Учёт токенов: {event.payload.usage?.source ?? 'нет данных'}
              </span>
            </details>
          </>
        );
        break;
      case 'attempt.failed':
        detail = (
          <>
            <p className="notice error">{event.payload.safeMessage}</p>
            <p className="event-summary">
              {effectSummary(event.payload.effectStatus)}
            </p>
            <details className="technical-details">
              <summary>Технические детали ошибки</summary>
              <span>Эффект: {event.payload.effectStatus}</span>
            </details>
          </>
        );
        break;
      case 'attempt.interrupted':
        detail = (
          <>
            <p>
              Работа агента остановлена.{' '}
              {effectSummary(event.payload.effectStatus)}
            </p>
            <details className="technical-details">
              <summary>Технические детали остановки</summary>
              <span>Причина: {event.payload.reason}</span>
              <span>Эффект: {event.payload.effectStatus}</span>
            </details>
          </>
        );
        break;
      case 'attempt.unknown':
        detail = (
          <>
            <p className="notice error">
              Результат работы не подтверждён. Требуется проверка состояния.
            </p>
            <p className="event-summary">
              {effectSummary(event.payload.effectStatus)}
            </p>
            <details className="technical-details">
              <summary>Технические детали проверки</summary>
              <span>Причина: {event.payload.reason}</span>
              <span>Эффект: {event.payload.effectStatus}</span>
            </details>
          </>
        );
        break;
      case 'attempt.dispatching':
      case 'attempt.started':
      case 'attempt.waiting_input':
      case 'attempt.stop_requested':
        detail = <p>Запуск №{event.payload.generation}</p>;
        break;
      default:
        detail = <p className="muted">Состояние агента обновлено.</p>;
    }
    return (
      <article
        className="harness-event"
        data-event-type={event.type}
        key={`${event.nodeId}:${event.seq}`}
      >
        <div className="toolbar event-heading">
          <strong>{eventLabel(event.type)}</strong>
          <time className="muted" dateTime={event.observedAt}>
            {eventTime(event.observedAt)}
          </time>
        </div>
        {detail}
        <details className="technical-details event-technical-details">
          <summary>Код события</summary>
          <code>{event.type}</code>
          <span>seq {event.seq}</span>
        </details>
      </article>
    );
  }

  if (loading) {
    return (
      <section className="card" aria-busy="true">
        <output className="state-panel" aria-live="polite">
          <span className="loading-indicator" aria-hidden="true" />
          Подключаемся к агентам…
        </output>
      </section>
    );
  }
  if (error && nodes.length === 0) {
    return (
      <section className="card" aria-labelledby="workspace-error-title">
        <span className="eyebrow">Подключение</span>
        <h2 id="workspace-error-title">Агент недоступен</h2>
        <p className="notice error" role="alert">
          {error}
        </p>
      </section>
    );
  }

  const draftBusy = draft.phase === 'sending' || draft.phase === 'checking';
  const draftLocked = draftBusy || draft.phase === 'unknown';
  return (
    <section className="harness-workspace" aria-label="Рабочее место агента">
      {mode === 'fixture' && (
        <output className="notice" aria-live="polite">
          Учебный режим: данные синтетические, команды не управляют реальным
          агентом.
        </output>
      )}
      <div className="card workspace-overview" id="agent-status">
        <div className="toolbar section-heading">
          <div className="heading-copy">
            <span className="eyebrow">Выбранный агент</span>
            <h2>
              {nodes.find((node) => node.nodeId === nodeId)?.name ??
                'Рабочее место агента'}
            </h2>
          </div>
          <div className="harness-controls">
            {onBack && (
              <button className="secondary" onClick={onBack}>
                ← Ко всем агентам
              </button>
            )}
            <span
              className="tag status-pill"
              data-state={mode === 'fixture' ? 'fixture' : 'online'}
            >
              <span className="status-dot" aria-hidden="true" />
              {mode === 'fixture' ? 'Учебный режим' : 'Подключён'}
            </span>
            {identity &&
              snapshot?.nodeId === nodeId &&
              snapshot.activeAttempt &&
              snapshot.activeAttempt.state !== 'stopping' &&
              controlAction(
                {
                  protocolVersion: 1,
                  schemaId: 'harness-wire-v2',
                  commandId: '',
                  kind: 'attempt.stop',
                  target: {
                    nodeId,
                    attemptId: snapshot.activeAttempt.attemptId,
                  },
                  expected: {
                    attemptGeneration: snapshot.activeAttempt.generation,
                  },
                  payload: {},
                },
                'Остановить работу',
                snapshot.activeAttempt.dialogId,
              )}
            {identity &&
              snapshot?.nodeId === nodeId &&
              snapshot.node.queuePaused &&
              controlAction(
                {
                  protocolVersion: 1,
                  schemaId: 'harness-wire-v2',
                  commandId: '',
                  kind: 'queue.resume',
                  target: { nodeId },
                  expected: { queueVersion: snapshot.node.queueVersion },
                  payload: {},
                },
                'Продолжить очередь',
                dialogId,
              )}
          </div>
        </div>
        <nav className="workspace-nav" aria-label="Разделы рабочего места">
          {dialogId && <a href="#agent-conversation">Чат</a>}
          {dialogId && <a href="#agent-operations">Ход работы</a>}
          {nodeId && <a href="#agent-dialogs">Диалоги</a>}
          <a href="#agent-state-details">Агент</a>
        </nav>
        {error && (
          <p role="alert" className="notice error">
            {error}
          </p>
        )}
        {nodes.length === 0 ? (
          <p>Доступных агентов нет.</p>
        ) : (
          <>
            {onBack ? (
              <div className="agent-title-row">
                <span className="adapter-label">
                  {nodes.find((node) => node.nodeId === nodeId)?.adapter}
                </span>
              </div>
            ) : (
              <>
                <label htmlFor="harness-node">Агент</label>
                <select
                  id="harness-node"
                  value={nodeId}
                  onChange={(event) => {
                    const nextNodeId = event.target.value;
                    const nextDialogId =
                      selectedDialogsRef.current[nextNodeId] ?? '';
                    nodeRef.current = nextNodeId;
                    dialogRef.current = nextDialogId;
                    setNodeId(nextNodeId);
                    setDialogId(nextDialogId);
                  }}
                >
                  <option value="">Выберите агента</option>
                  {nodes.map((node) => (
                    <option key={node.nodeId} value={node.nodeId}>
                      {node.name} · {node.adapter}
                    </option>
                  ))}
                </select>
              </>
            )}
            {nodeLoading && (
              <output className="state-panel compact" aria-live="polite">
                <span className="loading-indicator" aria-hidden="true" />
                Загружаем состояние агента…
              </output>
            )}
            {identity && snapshot?.nodeId === nodeId && (
              <details className="agent-state-details" id="agent-state-details">
                <summary>
                  <strong>Состояние и очередь агента</strong>
                  <span
                    className="tag status-pill"
                    data-state={snapshot.node.occupancy}
                  >
                    <span className="status-dot" aria-hidden="true" />
                    {stateLabel(snapshot.node.occupancy)} ·{' '}
                    {snapshot.pendingQueue.length} в очереди
                  </span>
                </summary>
                <div className="agent-status-strip">
                  <div className="agent-summary">
                    <p className="muted agent-technical-state">
                      {identity.adapter.kind} {identity.adapter.version} ·
                      данные{' '}
                      {health === 'fresh'
                        ? 'актуальны'
                        : health === 'stale'
                          ? 'устарели'
                          : 'проверяются'}
                    </p>
                    <details className="technical-details agent-details">
                      <summary>Технические детали агента</summary>
                      <code>nodeId: {nodeId}</code>
                      <span>epoch: {identity.identityEpoch}</span>
                    </details>
                  </div>
                  <dl className="harness-state">
                    <div data-state={snapshot.node.transportAvailability}>
                      <dt>Доступность</dt>
                      <dd>
                        <span className="status-dot" aria-hidden="true" />
                        {stateLabel(snapshot.node.transportAvailability)}
                      </dd>
                    </div>
                    <div data-state={snapshot.node.engineReadiness}>
                      <dt>Готовность</dt>
                      <dd>
                        <span className="status-dot" aria-hidden="true" />
                        {stateLabel(snapshot.node.engineReadiness)}
                      </dd>
                    </div>
                    <div data-state={snapshot.node.occupancy}>
                      <dt>Занятость</dt>
                      <dd>
                        <span className="status-dot" aria-hidden="true" />
                        {stateLabel(snapshot.node.occupancy)}
                      </dd>
                    </div>
                    <div
                      data-state={
                        snapshot.node.queuePaused ? 'paused' : 'ready'
                      }
                    >
                      <dt>Ручная пауза</dt>
                      <dd>{snapshot.node.queuePaused ? 'да' : 'нет'}</dd>
                    </div>
                  </dl>
                </div>
                <details className="queue-panel">
                  <summary className="queue-heading">
                    <strong>Очередь агента</strong>
                    <span className="tag">
                      {snapshot.pendingQueue.length} в ожидании
                    </span>
                  </summary>
                  {snapshot.pendingQueue.length === 0 ? (
                    <p className="muted">Очередь пуста.</p>
                  ) : (
                    <ol className="harness-queue">
                      {snapshot.pendingQueue.map((item) => {
                        const message = visibleHistory?.items.find(
                          (candidate) =>
                            candidate.role === 'user' &&
                            candidate.messageId === item.inputMessageId,
                        );
                        const active = snapshot.activeAttempt;
                        return (
                          <li key={item.requestId}>
                            <strong>Поручение в очереди</strong>
                            <span className="muted">
                              Позиция {item.queueSequence}
                            </span>
                            <details className="technical-details">
                              <summary>Технические детали</summary>
                              <code>requestId: {item.requestId}</code>
                              <code>dialogId: {item.dialogId}</code>
                            </details>
                            <div className="harness-controls">
                              {controlAction(
                                {
                                  protocolVersion: 1,
                                  schemaId: 'harness-wire-v2',
                                  commandId: '',
                                  kind: 'request.cancel',
                                  target: { nodeId, requestId: item.requestId },
                                  expected: { requestVersion: item.version },
                                  payload: {},
                                },
                                'Отменить поручение',
                                item.dialogId,
                              )}
                              {active &&
                                active.dialogId === item.dialogId &&
                                message?.role === 'user' &&
                                message.disposition === 'queued' &&
                                controlAction(
                                  {
                                    protocolVersion: 1,
                                    schemaId: 'harness-wire-v2',
                                    commandId: '',
                                    kind: 'message.steer',
                                    target: {
                                      nodeId,
                                      dialogId: item.dialogId,
                                      attemptId: active.attemptId,
                                      messageId: message.messageId,
                                    },
                                    expected: {
                                      attemptGeneration: active.generation,
                                      messageVersion: message.version,
                                    },
                                    payload: {},
                                  },
                                  'Передать в текущую попытку',
                                  item.dialogId,
                                )}
                            </div>
                          </li>
                        );
                      })}
                    </ol>
                  )}
                </details>
              </details>
            )}
          </>
        )}
      </div>

      {outstandingControls.length > 0 && (
        <div className="card harness-outstanding-controls">
          <span className="eyebrow">Требуется проверка</span>
          <h3>Неподтверждённые запросы</h3>
          <p className="muted">
            Проверка покажет, был ли запрос принят агентом. Повторная отправка
            не выполняется автоматически.
          </p>
          {outstandingControls.map((intent) => (
            <div
              className="harness-control-panel"
              key={intent.command.commandId}
            >
              <details className="technical-details">
                <summary>Технические детали запроса</summary>
                <code>{intent.command.kind}</code>
                <code>commandId: {intent.command.commandId}</code>
              </details>
              <ControlAction
                label={controlCommandLabel(intent.command)}
                intent={intent}
                disabled={!session.writes_enabled}
                onRun={() => void runControl(intent.command, dialogRef.current)}
                onReconcile={() =>
                  void reconcileControl(intent, dialogRef.current)
                }
              />
            </div>
          ))}
        </div>
      )}

      {nodeId && (
        <div
          className={`workspace-workbench${dialogId ? '' : ' workspace-workbench--single'}`}
        >
          <div className="card dialog-list-card" id="agent-dialogs">
            <div className="toolbar">
              <div>
                <span className="eyebrow">Сессии агента</span>
                <h3>Диалоги</h3>
              </div>
              <button
                className="secondary"
                onClick={createDialog}
                disabled={
                  !session.writes_enabled ||
                  !snapshot ||
                  !identity ||
                  createIntent?.phase === 'sending' ||
                  createIntent?.phase === 'checking' ||
                  createIntent?.phase === 'unknown'
                }
              >
                {createIntent?.phase === 'sending'
                  ? 'Создаём…'
                  : createIntent?.phase === 'retry-ready'
                    ? 'Повторить создание'
                    : 'Новый диалог'}
              </button>
            </div>
            {createIntent?.error && (
              <p className="notice error" role="alert">
                {createIntent.error}
              </p>
            )}
            {createIntent?.phase === 'unknown' && (
              <button onClick={reconcileCreate}>Проверить создание</button>
            )}
            {dialogs.length === 0 ? (
              <div className="empty-state compact">
                <span className="empty-state-mark" aria-hidden="true">
                  +
                </span>
                <p>Диалогов пока нет. Создайте первый.</p>
              </div>
            ) : (
              <div className="record-list" aria-label="Список диалогов">
                {dialogs.map((dialog) => {
                  const blockReason = dialogDeletionBlockReason(
                    dialog.dialogId,
                  );
                  const descriptionId = `delete-dialog-${dialog.dialogId}-reason`;
                  return (
                    <div className="dialog-record" key={dialog.dialogId}>
                      <button
                        className="record"
                        aria-label={`Открыть диалог ${dialog.title || 'Без названия'}, ${dialog.dialogId}`}
                        aria-current={
                          dialog.dialogId === dialogId ? 'true' : undefined
                        }
                        onClick={() => selectDialog(nodeId, dialog.dialogId)}
                      >
                        <strong>{dialog.title || 'Без названия'}</strong>
                        <span className="muted">
                          {dialogDate(dialog.createdAt)}
                        </span>
                      </button>
                      <button
                        className="danger dialog-delete-trigger"
                        aria-label={`Удалить диалог ${dialog.dialogId}`}
                        aria-describedby={
                          blockReason ? descriptionId : undefined
                        }
                        disabled={
                          !session.writes_enabled || Boolean(blockReason)
                        }
                        onClick={() => setDeleteTarget(dialog)}
                      >
                        Удалить
                      </button>
                      {blockReason && (
                        <span
                          id={descriptionId}
                          className="muted dialog-delete-reason"
                        >
                          Удаление недоступно: {blockReason}
                        </span>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </div>

          {dialogId && (
            <div className="card conversation-card" id="agent-conversation">
              <div className="toolbar">
                <div>
                  <span className="eyebrow">Текущий диалог</span>
                  <h3>{selectedDialog?.title || 'Диалог'}</h3>
                  <details className="technical-details">
                    <summary>Технические детали диалога</summary>
                    <code>dialogId: {dialogId}</code>
                    <span>Версия {selectedDialog?.version ?? '—'}</span>
                  </details>
                </div>
                <span
                  className="tag status-pill"
                  data-state={snapshot?.node.occupancy ?? 'unknown'}
                >
                  <span className="status-dot" aria-hidden="true" />
                  {stateLabel(snapshot?.node.occupancy ?? 'unknown')}
                </span>
              </div>
              <div className="harness-history" aria-label="История сообщений">
                {!visibleHistory && (
                  <output className="state-panel compact" aria-live="polite">
                    <span className="loading-indicator" aria-hidden="true" />
                    Загружаем историю…
                  </output>
                )}
                {visibleHistory?.items.length === 0 && (
                  <div className="empty-state compact">
                    <span className="empty-state-mark" aria-hidden="true">
                      ···
                    </span>
                    <p>Сообщений пока нет.</p>
                  </div>
                )}
                {visibleHistory?.items.map((item) => (
                  <article
                    className="comment message"
                    data-role={item.role}
                    key={item.messageId}
                  >
                    <div className="message-meta">
                      <strong>{item.role === 'user' ? 'Вы' : 'Агент'}</strong>
                      <span className="muted">
                        {stateLabel(
                          item.role === 'user'
                            ? item.disposition
                            : item.finishReason,
                        )}
                      </span>
                    </div>
                    {item.role === 'user' ? (
                      <p className="content">{item.text}</p>
                    ) : item.content.kind === 'inline' ? (
                      <SafeMarkdown markdown={item.content.content} />
                    ) : item.content.kind === 'artifact' ? (
                      <p className="content">Агент подготовил файл.</p>
                    ) : (
                      <p className="content">Ответ агента недоступен.</p>
                    )}
                    {item.role === 'assistant' &&
                      contentNotice(item.content) && (
                        <p className="muted content-notice">
                          {contentNotice(item.content)}
                        </p>
                      )}
                    {item.role === 'assistant' &&
                      item.content.kind === 'artifact' && (
                        <ArtifactDownload
                          session={session}
                          nodeId={nodeId}
                          dialogId={dialogId}
                          attemptId={item.attemptId}
                          content={item.content}
                          onExpired={onExpired}
                        />
                      )}
                    {item.role === 'assistant' && (
                      <details className="technical-details message-details">
                        <summary>Технические детали сообщения</summary>
                        <code>messageId: {item.messageId}</code>
                        <code>attemptId: {item.attemptId}</code>
                        <span>finishReason: {item.finishReason}</span>
                        <span>redaction: {item.content.redaction}</span>
                        <span>
                          truncated: {item.content.truncated ? 'true' : 'false'}
                        </span>
                      </details>
                    )}
                  </article>
                ))}
              </div>
              <div className="composer" aria-label="Новое сообщение">
                <label htmlFor="harness-message">Сообщение агенту</label>
                <textarea
                  id="harness-message"
                  aria-label="Сообщение агенту"
                  value={draft.text}
                  onChange={(event) => {
                    const text = event.target.value;
                    updateDraft(nodeId, dialogId, () => ({
                      text,
                      phase: 'draft',
                    }));
                  }}
                  placeholder="Напишите продолжение для выбранного диалога"
                  disabled={draftLocked}
                />
                {draft.error && (
                  <p className="notice error" role="alert">
                    {draft.error}
                  </p>
                )}
                <div className="toolbar composer-actions">
                  <span className="muted" aria-live="polite">
                    {draft.phase === 'unknown'
                      ? 'Результат отправки не подтверждён. Проверьте запрос.'
                      : draft.phase === 'queued'
                        ? 'Команда принята в очередь.'
                        : draft.phase === 'retry-ready'
                          ? 'Проверка завершена. Можно повторить отправку.'
                          : draft.phase === 'rejected'
                            ? 'Команда отклонена; текст сохранён как черновик.'
                            : 'Черновик ещё не сохранён.'}
                  </span>
                  <button
                    className="primary"
                    onClick={sendMessage}
                    disabled={
                      !session.writes_enabled ||
                      !selectedDialog ||
                      !draft.text.trim() ||
                      draftLocked
                    }
                  >
                    {draft.phase === 'sending'
                      ? 'Отправляем…'
                      : draft.phase === 'retry-ready'
                        ? 'Повторить отправку'
                        : 'Отправить'}
                  </button>
                </div>
                {draft.phase === 'unknown' && (
                  <>
                    <details className="technical-details">
                      <summary>Технические детали отправки</summary>
                      <code>commandId: {draft.command?.commandId ?? ''}</code>
                    </details>
                    <button onClick={reconcileMessage}>
                      Проверить отправку
                    </button>
                  </>
                )}
                {!session.writes_enabled && (
                  <p className="muted">Команды отключены для этой сессии.</p>
                )}
              </div>
            </div>
          )}

          {dialogId && (
            <aside className="card harness-operations" id="agent-operations">
              <div className="toolbar">
                <div>
                  <span className="eyebrow">Активность агента</span>
                  <h3>Ход работы</h3>
                </div>
                <span className="tag">{dialogRequests.length} поручений</span>
              </div>
              <p className="muted operations-copy">
                Здесь видны запуски, вызовы инструментов и решения, которых ждёт
                агент.
              </p>
              {visibleRequests?.nextCursor && (
                <button
                  onClick={() => void loadMoreRequests()}
                  disabled={loadingRequestsMore}
                >
                  {loadingRequestsMore
                    ? 'Загружаем поручения…'
                    : 'Загрузить ещё поручения'}
                </button>
              )}
              {dialogRequests.length === 0 ? (
                <p className="muted">У диалога ещё нет поручений.</p>
              ) : (
                <>
                  <label htmlFor="harness-request">Поручение</label>
                  <select
                    id="harness-request"
                    value={selectedRequestId}
                    onChange={(event) => {
                      const value = event.target.value;
                      selectedRequestRef.current = value;
                      selectedAttemptRef.current = '';
                      setSelectedRequestId(value);
                      setSelectedAttemptId('');
                      setAttempts(null);
                      setTimeline(null);
                    }}
                  >
                    {dialogRequests.map((request, index) => (
                      <option key={request.requestId} value={request.requestId}>
                        Поручение {index + 1} · {stateLabel(request.status)}
                      </option>
                    ))}
                  </select>
                  {visibleAttempts?.nextCursor && (
                    <button
                      onClick={() => void loadMoreAttempts()}
                      disabled={loadingAttemptsMore}
                    >
                      {loadingAttemptsMore
                        ? 'Загружаем попытки…'
                        : 'Загрузить ещё попытки'}
                    </button>
                  )}
                  {visibleAttempts && visibleAttempts.items.length === 0 ? (
                    <p className="muted">Агент ещё не запускался.</p>
                  ) : visibleAttempts ? (
                    <>
                      <label htmlFor="harness-attempt">Запуск агента</label>
                      <select
                        id="harness-attempt"
                        value={selectedAttemptId}
                        onChange={(event) => {
                          const value = event.target.value;
                          selectedAttemptRef.current = value;
                          setSelectedAttemptId(value);
                          setTimeline(null);
                        }}
                      >
                        {visibleAttempts.items.map((attempt) => (
                          <option
                            key={attempt.attemptId}
                            value={attempt.attemptId}
                          >
                            Запуск {attempt.generation} ·{' '}
                            {stateLabel(attempt.state)}
                          </option>
                        ))}
                      </select>
                      {selectedAttempt && (
                        <details className="technical-details attempt-details">
                          <summary>Технические детали выполнения</summary>
                          <code>requestId: {selectedAttempt.requestId}</code>
                          <code>attemptId: {selectedAttempt.attemptId}</code>
                          <span>
                            Эффект: {stateLabel(selectedAttempt.effectStatus)}
                          </span>
                        </details>
                      )}
                    </>
                  ) : (
                    <output>Загружаем попытки…</output>
                  )}
                </>
              )}

              {selectedAttempt &&
                (selectedAttempt.state === 'failed' ||
                  selectedAttempt.state === 'interrupted') && (
                  <div className="harness-control-panel">
                    <h4>Повторить завершённый запуск</h4>
                    <p className="muted">
                      Новый запуск встанет в конец очереди после подтверждения
                      агента.
                    </p>
                    {selectedAttempt.effectStatus === 'known' && (
                      <label className="harness-check">
                        <input
                          type="checkbox"
                          checked={
                            retryAcknowledgements[
                              `${nodeId}:${selectedAttempt.dialogId}:${selectedAttempt.attemptId}`
                            ] ?? false
                          }
                          onChange={(event) =>
                            setRetryAcknowledgements((old) => ({
                              ...old,
                              [`${nodeId}:${selectedAttempt.dialogId}:${selectedAttempt.attemptId}`]:
                                event.target.checked,
                            }))
                          }
                        />
                        Я проверил известные эффекты в ленте и подтверждаю
                        повтор.
                      </label>
                    )}
                    {selectedAttempt.effectStatus === 'unknown' ? (
                      <p className="notice error">
                        Эффекты неизвестны. Повтор недоступен до серверной
                        сверки.
                      </p>
                    ) : (
                      controlAction(
                        {
                          protocolVersion: 1,
                          schemaId: 'harness-wire-v2',
                          commandId: '',
                          kind: 'attempt.retry',
                          target: {
                            nodeId,
                            attemptId: selectedAttempt.attemptId,
                          },
                          expected: {
                            attemptGeneration: selectedAttempt.generation,
                          },
                          payload: {
                            acknowledgeKnownEffects:
                              selectedAttempt.effectStatus === 'known',
                          },
                        },
                        'Повторить попытку',
                        selectedAttempt.dialogId,
                        selectedAttempt.effectStatus === 'known' &&
                          !retryAcknowledgements[
                            `${nodeId}:${selectedAttempt.dialogId}:${selectedAttempt.attemptId}`
                          ],
                      )
                    )}
                  </div>
                )}

              {visibleTimeline?.hasMore && (
                <p className="notice">
                  Журнал загружен не полностью. Ответы и разрешения станут
                  доступны после загрузки следующих событий.
                </p>
              )}
              {pendingApprovals.map((event) => (
                <div
                  className="harness-control-panel"
                  key={`approval:${event.payload.approvalId}:${event.payload.approvalVersion}`}
                >
                  <h4>Требуется решение</h4>
                  <p>{event.payload.safePrompt}</p>
                  <details className="technical-details">
                    <summary>Технические детали решения</summary>
                    <code>actionHash: {event.payload.actionHash}</code>
                    <code>approvalId: {event.payload.approvalId}</code>
                    <code>attemptId: {event.attemptId}</code>
                  </details>
                  <div className="harness-controls">
                    {controlAction(
                      {
                        protocolVersion: 1,
                        schemaId: 'harness-wire-v2',
                        commandId: '',
                        kind: 'approval.respond',
                        target: {
                          nodeId,
                          approvalId: event.payload.approvalId,
                          attemptId: event.attemptId,
                        },
                        expected: {
                          approvalVersion: event.payload.approvalVersion,
                          attemptGeneration: selectedAttempt?.generation ?? 0,
                        },
                        payload: {
                          decision: 'allow_once',
                          actionHash: event.payload.actionHash,
                        },
                      },
                      'Разрешить один раз',
                      event.dialogId,
                      visibleTimeline?.hasMore || !selectedAttempt,
                    )}
                    {controlAction(
                      {
                        protocolVersion: 1,
                        schemaId: 'harness-wire-v2',
                        commandId: '',
                        kind: 'approval.respond',
                        target: {
                          nodeId,
                          approvalId: event.payload.approvalId,
                          attemptId: event.attemptId,
                        },
                        expected: {
                          approvalVersion: event.payload.approvalVersion,
                          attemptGeneration: selectedAttempt?.generation ?? 0,
                        },
                        payload: {
                          decision: 'deny',
                          actionHash: event.payload.actionHash,
                        },
                      },
                      'Отклонить',
                      event.dialogId,
                      visibleTimeline?.hasMore || !selectedAttempt,
                    )}
                  </div>
                </div>
              ))}

              {pendingInputs.map((event) => {
                const key = `${nodeId}:${event.dialogId}:${event.attemptId}:${event.payload.inputRequestId}:${event.payload.inputVersion}:${selectedAttempt?.generation ?? 0}`;
                const text = inputDrafts[key] ?? '';
                const proposal: ControlCommand = {
                  protocolVersion: 1,
                  schemaId: 'harness-wire-v2',
                  commandId: '',
                  kind: 'input.respond',
                  target: {
                    nodeId,
                    inputRequestId: event.payload.inputRequestId,
                    attemptId: event.attemptId,
                  },
                  expected: {
                    inputVersion: event.payload.inputVersion,
                    attemptGeneration: selectedAttempt?.generation ?? 0,
                  },
                  payload: { text },
                };
                const intent = controls[controlIntentKey(proposal)];
                return (
                  <div className="harness-control-panel" key={key}>
                    <h4>Агент ждёт ответ</h4>
                    <ContentView
                      label="Вопрос"
                      content={event.payload.prompt}
                      session={session}
                      nodeId={nodeId}
                      dialogId={event.dialogId}
                      attemptId={event.attemptId}
                      onExpired={onExpired}
                    />
                    <textarea
                        aria-label="Ответ агенту"
                      value={text}
                      onChange={(change) =>
                        setInputDrafts((old) => ({
                          ...old,
                          [key]: change.target.value,
                        }))
                      }
                      disabled={
                        visibleTimeline?.hasMore ||
                        (intent !== undefined && intent.phase !== 'rejected')
                      }
                    />
                    <details className="technical-details">
                      <summary>Технические детали вопроса</summary>
                      <code>
                        inputRequestId: {event.payload.inputRequestId}
                      </code>
                      <code>attemptId: {event.attemptId}</code>
                    </details>
                    {controlAction(
                      proposal,
                      'Ответить агенту',
                      event.dialogId,
                      visibleTimeline?.hasMore ||
                        !selectedAttempt ||
                        !text.trim(),
                    )}
                  </div>
                );
              })}

              <div className="toolbar">
                <h4>Вызовы инструментов и события</h4>
                {visibleTimeline?.hasMore && (
                  <button
                    onClick={() => void loadMoreTimeline()}
                    disabled={visibleTimeline.loadingMore}
                  >
                    {visibleTimeline.loadingMore
                      ? 'Загружаем…'
                      : 'Загрузить ещё события'}
                  </button>
                )}
              </div>
              {visibleTimeline?.error && (
                <p className="notice error" role="alert">
                  {visibleTimeline.error}
                </p>
              )}
              {selectedAttemptId && !visibleTimeline ? (
                <output className="state-panel compact" aria-live="polite">
                  <span className="loading-indicator" aria-hidden="true" />
                  Загружаем ленту…
                </output>
              ) : visibleTimeline?.events.length === 0 ? (
                <div className="empty-state compact">
                  <span className="empty-state-mark" aria-hidden="true">
                    0
                  </span>
                  <p>Событий попытки пока нет.</p>
                </div>
              ) : (
                <div
                  className="harness-timeline"
                  aria-label="Ход работы агента"
                >
                  {visibleTimeline?.events.map(timelineEvent)}
                </div>
              )}
            </aside>
          )}
        </div>
      )}

      <dialog
        ref={deleteDialogElement}
        className="delete-dialog"
        role="alertdialog"
        aria-labelledby="delete-dialog-title"
        aria-describedby="delete-dialog-description"
        aria-busy={
          deleteIntent?.phase === 'sending' ||
          deleteIntent?.phase === 'checking'
        }
        onCancel={(event) => {
          if (
            deleteIntent?.phase === 'sending' ||
            deleteIntent?.phase === 'checking'
          ) {
            event.preventDefault();
            return;
          }
          setDeleteTarget(null);
        }}
        onKeyDown={(event) => {
          if (event.key !== 'Escape') return;
          event.preventDefault();
          if (
            deleteIntent?.phase !== 'sending' &&
            deleteIntent?.phase !== 'checking'
          ) {
            setDeleteTarget(null);
          }
        }}
        onClose={() => setDeleteTarget(null)}
      >
        {deleteTarget && (
          <div className="delete-dialog-content">
            <span className="eyebrow">БЕЗОПАСНОЕ УДАЛЕНИЕ</span>
            <h3 id="delete-dialog-title">
              Удалить «{deleteTarget.title || 'Без названия'}»?
            </h3>
            <p id="delete-dialog-description">
              Диалог <span className="record-id">{deleteTarget.dialogId}</span>{' '}
              будет скрыт из Panel и станет недоступен через API. История не
              удаляется физически: данные сохраняются для аварийного
              восстановления, но вернуть диалог через Panel нельзя.
            </p>
            {deleteBlockedReason && (
              <output className="notice">
                Удаление недоступно: {deleteBlockedReason}
              </output>
            )}
            {deleteIntent?.error && (
              <p className="notice error" role="alert">
                {deleteIntent.error}
              </p>
            )}
            {deleteIntent?.phase === 'unknown' && (
              <div className="notice error">
                <p>
                  Ответ потерян. Не повторяйте удаление до проверки результата.
                </p>
                <details>
                  <summary>Технические сведения</summary>
                  <span className="record-id">
                    commandId {deleteIntent.command.commandId}
                  </span>
                </details>
              </div>
            )}
            <div className="delete-dialog-actions">
              <button
                ref={deleteCancelButton}
                className="secondary"
                autoFocus
                disabled={
                  deleteIntent?.phase === 'sending' ||
                  deleteIntent?.phase === 'checking'
                }
                onClick={() => setDeleteTarget(null)}
              >
                Отмена
              </button>
              {deleteIntent?.phase === 'unknown' ? (
                <button onClick={() => void reconcileDelete(deleteIntent)}>
                  Проверить удаление
                </button>
              ) : (
                <button
                  className="danger"
                  disabled={
                    !session.writes_enabled ||
                    Boolean(deleteBlockedReason) ||
                    deleteIntent?.phase === 'sending' ||
                    deleteIntent?.phase === 'checking'
                  }
                  onClick={() => void deleteDialog(deleteTarget)}
                >
                  {deleteIntent?.phase === 'sending'
                    ? 'Удаляем…'
                    : deleteIntent?.phase === 'checking'
                      ? 'Проверяем…'
                      : deleteIntent?.phase === 'rejected'
                        ? 'Подтвердить заново'
                        : 'Удалить диалог'}
                </button>
              )}
            </div>
          </div>
        )}
      </dialog>
    </section>
  );
}

export { targetKey, emptyDraft, toolLabel };
