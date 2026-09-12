import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  assertIdentity,
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
  type HarnessDraft,
  type MessageCommand,
} from './harness-state';
import type { Session } from './panel-api';

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
      return 'Остановить попытку';
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
  return 'Не удалось получить подтверждённый ответ Harness.';
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
          'Metadata артефакта не совпадает с сообщением.',
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
            Исход не подтверждён; commandId {intent.command.commandId} сохранён.
          </span>
          <button onClick={onReconcile}>Проверить запрос</button>
        </>
      )}
      {intent?.phase === 'accepted' && (
        <span className="muted">
          Harness принял запрос. Итог появится в состоянии или ленте.
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
  onExpired,
}: {
  label: string;
  content: SafeContent;
  session: Session;
  nodeId: string;
  dialogId: string;
  attemptId: string;
  callId?: string;
  onExpired: () => void;
}) {
  return (
    <div className="harness-safe-content">
      <strong>{label}</strong>
      {content.kind === 'inline' ? (
        <pre>{content.content}</pre>
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
        <span className="notice">Недоступно: {content.reason}</span>
      )}
      <span className="muted">
        redaction: {content.redaction}; truncated:{' '}
        {content.truncated ? 'да' : 'нет'}
      </span>
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
            'Identity и snapshot Harness относятся к разным epoch.',
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
                'Identity epoch ноды изменился.',
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
            triggerResync(nodeId, generation, 'Identity epoch ноды изменился.');
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
            schemaId: 'harness-wire-v1',
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
            schemaId: 'harness-wire-v1',
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
            <p>
              <strong>{event.payload.toolName}</strong> · actionHash{' '}
              {event.payload.actionHash}
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
          </>
        );
        break;
      case 'tool.output':
        detail = (
          <ContentView
            label={`${event.payload.stream} · chunk ${event.payload.chunkIndex}`}
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
            <p>
              status: {event.payload.status}; effect:{' '}
              {event.payload.effectStatus}
              {event.payload.effectRef
                ? `; reference: ${event.payload.effectRef}`
                : ''}
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
          </>
        );
        break;
      case 'assistant.delta':
      case 'assistant.message':
        detail = (
          <ContentView
            label={
              event.type === 'assistant.delta'
                ? `Дельта ${event.payload.deltaIndex}`
                : `Сообщение · ${event.payload.finishReason}`
            }
            content={event.payload.content}
            session={session}
            nodeId={nodeId}
            dialogId={event.dialogId}
            attemptId={event.attemptId}
            onExpired={onExpired}
          />
        );
        break;
      case 'approval.requested':
        detail = (
          <p>
            {event.payload.safePrompt} · actionHash {event.payload.actionHash}
          </p>
        );
        break;
      case 'approval.resolved':
        detail = <p>Решение: {event.payload.decision}</p>;
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
        detail = <p>Ответ сохранён как сообщение {event.payload.messageId}.</p>;
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
          <p>
            Terminal: completed; output {event.payload.output.kind}
            {event.payload.usage
              ? `; tokens ${event.payload.usage.totalTokens} (${event.payload.usage.source})`
              : '; usage unknown'}
          </p>
        );
        break;
      case 'attempt.failed':
        detail = (
          <p>
            Terminal: failed · {event.payload.safeMessage} · effect{' '}
            {event.payload.effectStatus}
          </p>
        );
        break;
      case 'attempt.interrupted':
        detail = (
          <p>
            Terminal: interrupted · {event.payload.reason} · effect{' '}
            {event.payload.effectStatus}
          </p>
        );
        break;
      case 'attempt.unknown':
        detail = (
          <p className="notice error">
            Исход исполнения неизвестен: {event.payload.reason}; effect{' '}
            {event.payload.effectStatus}.
          </p>
        );
        break;
      case 'attempt.dispatching':
      case 'attempt.started':
      case 'attempt.waiting_input':
      case 'attempt.stop_requested':
        detail = <p>generation {event.payload.generation}</p>;
        break;
      default:
        detail = <p className="muted">Состояние записано Harness.</p>;
    }
    return (
      <article
        className="harness-event"
        data-event-type={event.type}
        key={`${event.nodeId}:${event.seq}`}
      >
        <div className="toolbar">
          <strong>{event.type}</strong>
          <span className="muted">
            seq {event.seq} · {event.observedAt}
          </span>
        </div>
        {detail}
      </article>
    );
  }

  if (loading) {
    return (
      <section className="card" aria-busy="true">
        <output className="state-panel" aria-live="polite">
          <span className="loading-indicator" aria-hidden="true" />
          Загружаем ноды Harness…
        </output>
      </section>
    );
  }
  if (error && nodes.length === 0) {
    return (
      <section className="card" aria-labelledby="workspace-error-title">
        <span className="eyebrow">CONNECTION</span>
        <h2 id="workspace-error-title">Harness недоступен</h2>
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
          Fixture режим: данные синтетические, команды не управляют реальным
          агентом.
        </output>
      )}
      <div className="card workspace-overview" id="agent-status">
        <div className="toolbar section-heading">
          <div className="heading-copy">
            <span className="eyebrow">INTERACTION</span>
            <h2>Рабочее место агента</h2>
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
              {mode === 'fixture' ? 'Fixture режим' : 'Live режим'}
            </span>
          </div>
        </div>
        <p className="muted section-intro">
          Диалоги, чат, текущая работа, попытки и инструменты выбранного агента.
          Принятие команды и завершение работы показываются отдельно.
        </p>
        <nav className="workspace-nav" aria-label="Разделы рабочего места">
          <a href="#agent-status">Состояние</a>
          {nodeId && <a href="#agent-dialogs">Диалоги</a>}
          {dialogId && <a href="#agent-conversation">Сообщения</a>}
          {dialogId && <a href="#agent-operations">Выполнение</a>}
        </nav>
        {error && (
          <p role="alert" className="notice error">
            {error}
          </p>
        )}
        {nodes.length === 0 ? (
          <p>Доступных нод нет.</p>
        ) : (
          <>
            {onBack ? (
              <div className="agent-title-row">
                <div>
                  <span className="eyebrow">SELECTED AGENT</span>
                  <h3>{nodes.find((node) => node.nodeId === nodeId)?.name}</h3>
                </div>
                <span className="adapter-label">
                  {nodes.find((node) => node.nodeId === nodeId)?.adapter}
                </span>
              </div>
            ) : (
              <>
                <label htmlFor="harness-node">Нода</label>
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
                  <option value="">Выберите ноду</option>
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
                Загружаем состояние ноды…
              </output>
            )}
            {identity && snapshot?.nodeId === nodeId && (
              <>
                <p className="muted agent-technical-state">
                  {nodes.find((node) => node.nodeId === nodeId)?.name} ·{' '}
                  {identity.adapter.kind} {identity.adapter.version} · health{' '}
                  {health === 'fresh'
                    ? 'свежий'
                    : health === 'stale'
                      ? 'устарел'
                      : 'неизвестен'}
                </p>
                <dl className="harness-state">
                  <div data-state={snapshot.node.transportAvailability}>
                    <dt>Доступность</dt>
                    <dd>
                      <span className="status-dot" aria-hidden="true" />
                      {snapshot.node.transportAvailability}
                    </dd>
                  </div>
                  <div data-state={snapshot.node.engineReadiness}>
                    <dt>Готовность</dt>
                    <dd>
                      <span className="status-dot" aria-hidden="true" />
                      {snapshot.node.engineReadiness}
                    </dd>
                  </div>
                  <div data-state={snapshot.node.occupancy}>
                    <dt>Занятость</dt>
                    <dd>
                      <span className="status-dot" aria-hidden="true" />
                      {snapshot.node.occupancy}
                    </dd>
                  </div>
                  <div
                    data-state={snapshot.node.queuePaused ? 'paused' : 'ready'}
                  >
                    <dt>Ручная пауза</dt>
                    <dd>{snapshot.node.queuePaused ? 'да' : 'нет'}</dd>
                  </div>
                </dl>
                <div className="harness-controls">
                  {snapshot.activeAttempt &&
                    snapshot.activeAttempt.state !== 'stopping' &&
                    controlAction(
                      {
                        protocolVersion: 1,
                        schemaId: 'harness-wire-v1',
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
                      'Остановить попытку',
                      snapshot.activeAttempt.dialogId,
                    )}
                  {snapshot.node.queuePaused &&
                    controlAction(
                      {
                        protocolVersion: 1,
                        schemaId: 'harness-wire-v1',
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
                <div className="queue-panel">
                  <div className="toolbar queue-heading">
                    <h3>Очередь</h3>
                    <span className="tag">
                      {snapshot.pendingQueue.length} в ожидании
                    </span>
                  </div>
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
                            <strong>{item.requestId}</strong>
                            <span className="muted">
                              {' '}
                              · позиция {item.queueSequence} · диалог{' '}
                              {item.dialogId}
                            </span>
                            <div className="harness-controls">
                              {controlAction(
                                {
                                  protocolVersion: 1,
                                  schemaId: 'harness-wire-v1',
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
                                    schemaId: 'harness-wire-v1',
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
                </div>
              </>
            )}
          </>
        )}
      </div>

      {outstandingControls.length > 0 && (
        <div className="card harness-outstanding-controls">
          <span className="eyebrow">RECOVERY</span>
          <h3>Неподтверждённые запросы</h3>
          <p className="muted">
            Эти запросы остаются доступны по сохранённому commandId, даже если
            состояние цели уже изменилось.
          </p>
          {outstandingControls.map((intent) => (
            <div
              className="harness-control-panel"
              key={intent.command.commandId}
            >
              <span className="muted">
                {intent.command.kind} · {intent.command.commandId}
              </span>
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
          className={`workspace-primary${dialogId ? '' : ' workspace-primary--single'}`}
        >
          <div className="card dialog-list-card" id="agent-dialogs">
            <div className="toolbar">
              <div>
                <span className="eyebrow">CONVERSATIONS</span>
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
                {dialogs.map((dialog) => (
                  <button
                    className="record"
                    key={dialog.dialogId}
                    aria-current={
                      dialog.dialogId === dialogId ? 'true' : undefined
                    }
                    onClick={() => selectDialog(nodeId, dialog.dialogId)}
                  >
                    <strong>{dialog.title || 'Без названия'}</strong>
                    <span className="record-id">{dialog.dialogId}</span>
                    <span className="muted">Версия {dialog.version}</span>
                  </button>
                ))}
              </div>
            )}
          </div>

          {dialogId && (
            <div className="card conversation-card" id="agent-conversation">
              <div className="toolbar">
                <div>
                  <span className="eyebrow">ACTIVE DIALOG</span>
                  <h3>{selectedDialog?.title || 'Диалог'}</h3>
                  <span className="record-id">{dialogId}</span>
                </div>
                <span
                  className="tag status-pill"
                  data-state={snapshot?.node.occupancy ?? 'unknown'}
                >
                  <span className="status-dot" aria-hidden="true" />
                  {snapshot?.node.occupancy ?? 'unknown'}
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
                        {item.role === 'user'
                          ? item.disposition
                          : item.finishReason}
                      </span>
                    </div>
                    <p className="content">
                      {item.role === 'user'
                        ? item.text
                        : item.content.kind === 'inline'
                          ? item.content.content
                          : item.content.kind === 'artifact'
                            ? `Артефакт ${item.content.artifactId}`
                            : `[недоступно: ${item.content.reason}]`}
                    </p>
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
                      ? `Исход неизвестен; commandId ${draft.command?.commandId ?? ''} сохранён.`
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
                  <button onClick={reconcileMessage}>Проверить отправку</button>
                )}
                {!session.writes_enabled && (
                  <p className="muted">Команды отключены для этой сессии.</p>
                )}
              </div>
            </div>
          )}
        </div>
      )}

      {dialogId && (
        <div className="card harness-operations" id="agent-operations">
          <div className="toolbar">
            <div>
              <span className="eyebrow">CONTROL</span>
              <h3>Поручения, попытки и инструменты</h3>
            </div>
            <span className="tag">{dialogRequests.length} поручений</span>
          </div>
          <p className="muted">
            Terminal попытки доступны независимо от наличия сообщения агента.
            Принятый control-запрос ждёт отдельного состояния или события.
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
                {dialogRequests.map((request) => (
                  <option key={request.requestId} value={request.requestId}>
                    {request.requestId} · {request.status}
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
                <p className="muted">Попытка ещё не создана.</p>
              ) : visibleAttempts ? (
                <>
                  <label htmlFor="harness-attempt">Попытка</label>
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
                      <option key={attempt.attemptId} value={attempt.attemptId}>
                        generation {attempt.generation} · {attempt.state} ·
                        effect {attempt.effectStatus}
                      </option>
                    ))}
                  </select>
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
                <h4>Повторить terminal попытку</h4>
                <p className="muted">
                  Новая попытка будет поставлена в хвост FIFO только после
                  принятого запроса Harness.
                </p>
                {selectedAttempt.effectStatus === 'known' && (
                  <label className="harness-check">
                    <input
                      type="checkbox"
                      checked={
                        retryAcknowledgements[selectedAttempt.attemptId] ??
                        false
                      }
                      onChange={(event) =>
                        setRetryAcknowledgements((old) => ({
                          ...old,
                          [selectedAttempt.attemptId]: event.target.checked,
                        }))
                      }
                    />
                    Я проверил известные эффекты в ленте и подтверждаю повтор.
                  </label>
                )}
                {selectedAttempt.effectStatus === 'unknown' ? (
                  <p className="notice error">
                    Эффекты неизвестны. Повтор недоступен до серверной сверки.
                  </p>
                ) : (
                  controlAction(
                    {
                      protocolVersion: 1,
                      schemaId: 'harness-wire-v1',
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
                      !retryAcknowledgements[selectedAttempt.attemptId],
                  )
                )}
              </div>
            )}

          {visibleTimeline?.hasMore && (
            <p className="notice">
              Лента загружена не полностью. Approval/input заблокированы до
              загрузки следующих событий.
            </p>
          )}
          {pendingApprovals.map((event) => (
            <div
              className="harness-control-panel"
              key={`approval:${event.payload.approvalId}:${event.payload.approvalVersion}`}
            >
              <h4>Запрос разрешения</h4>
              <p>{event.payload.safePrompt}</p>
              <p className="muted">actionHash {event.payload.actionHash}</p>
              <div className="harness-controls">
                {controlAction(
                  {
                    protocolVersion: 1,
                    schemaId: 'harness-wire-v1',
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
                    schemaId: 'harness-wire-v1',
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
            const key = `${nodeId}:${event.attemptId}:${event.payload.inputRequestId}:${event.payload.inputVersion}:${selectedAttempt?.generation ?? 0}`;
            const text = inputDrafts[key] ?? '';
            const proposal: ControlCommand = {
              protocolVersion: 1,
              schemaId: 'harness-wire-v1',
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
                  aria-label={`Ответ агенту ${event.payload.inputRequestId}`}
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
                {controlAction(
                  proposal,
                  'Ответить агенту',
                  event.dialogId,
                  visibleTimeline?.hasMore || !selectedAttempt || !text.trim(),
                )}
              </div>
            );
          })}

          <div className="toolbar">
            <h4>Лента попытки</h4>
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
              aria-label="Лента событий попытки"
            >
              {visibleTimeline?.events.map(timelineEvent)}
            </div>
          )}
        </div>
      )}
    </section>
  );
}

export { targetKey, emptyDraft };
