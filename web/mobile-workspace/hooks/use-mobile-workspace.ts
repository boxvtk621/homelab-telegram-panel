import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import {
  ApiError,
  type ControlHealth,
  type DialogPage,
  type DialogSnapshot,
  type EventPage,
  type MessagePage,
  MobileApi,
  type Session,
  type SubmissionReceipt,
  type TaskPage,
  type TaskSnapshot,
} from '@/lib/api';
import {
  prepareTelegramWebApp,
  stableClientInstanceID,
  telegramWebApp,
} from '@/lib/telegram';
import { eventPollDelay } from '@/lib/polling';
import { pendingDialogCommand, clearDialogCommand } from '@/lib/dialog-command';
import {
  pendingSubmission,
  clearSubmission,
  pendingSubmissionVersion,
} from '@/lib/submission-command';

const STALE_AFTER_MS = 30_000;
const CLOCK_TICK_MS = 5_000;

export type AuthPhase =
  | 'checking'
  | 'authenticating'
  | 'authenticated'
  | 'required'
  | 'expired'
  | 'forbidden'
  | 'unavailable';

export type Resource<T> = {
  status: 'idle' | 'loading' | 'ready' | 'error';
  data: T | null;
  error: ApiError | null;
};

export type WorkspaceState = {
  authPhase: AuthPhase;
  session: Session | null;
  dialogs: Resource<DialogPage>;
  tasks: Resource<TaskPage>;
  health: Resource<ControlHealth>;
  dialog: Resource<DialogSnapshot>;
  messages: Resource<MessagePage>;
  task: Resource<TaskSnapshot>;
  events: Resource<EventPage>;
  online: boolean;
  stale: boolean;
  refreshing: boolean;
  refresh: () => Promise<void>;
  signOut: () => Promise<void>;
  canCreateDialog: boolean;
  creatingDialog: boolean;
  createDialogError: string | null;
  createDialog: () => Promise<string | null>;
  canSubmitTask: boolean;
  submittingTask: boolean;
  submissionError: string | null;
  submissionTargetVersion: number | null;
  submitTask: (
    issueId: string,
    text: string,
    confirmedVersion: number,
  ) => Promise<SubmissionReceipt | null>;
};

const idle = <T>(): Resource<T> => ({
  status: 'idle',
  data: null,
  error: null,
});

export function useMobileWorkspace(
  selectedDialogID: string | null,
  selectedTaskID: string | null = null,
): WorkspaceState {
  const api = useMemo(() => new MobileApi(stableClientInstanceID()), []);
  const [authPhase, setAuthPhase] = useState<AuthPhase>('checking');
  const [session, setSession] = useState<Session | null>(null);
  const [dialogs, setDialogs] = useState<Resource<DialogPage>>(idle);
  const [tasks, setTasks] = useState<Resource<TaskPage>>(idle);
  const [health, setHealth] = useState<Resource<ControlHealth>>(idle);
  const [dialog, setDialog] = useState<Resource<DialogSnapshot>>(idle);
  const [messages, setMessages] = useState<Resource<MessagePage>>(idle);
  const [task, setTask] = useState<Resource<TaskSnapshot>>(idle);
  const [events, setEvents] = useState<Resource<EventPage>>(idle);
  const [detailRefreshVersion, setDetailRefreshVersion] = useState(0);
  const [online, setOnline] = useState(() => navigator.onLine);
  const [clock, setClock] = useState(() => Date.now());
  const [refreshing, setRefreshing] = useState(false);
  const [creatingDialog, setCreatingDialog] = useState(false);
  const [createDialogError, setCreateDialogError] = useState<string | null>(
    null,
  );
  const createDialogLock = useRef(false);
  const submissionLock = useRef(false);
  const [submittingTask, setSubmittingTask] = useState(false);
  const [submissionError, setSubmissionError] = useState<string | null>(null);
  const refreshLock = useRef(false);
  const eventCursor = useRef<string | null>(null);
  const dashboardAbort = useRef<AbortController | null>(null);
  const detailAbort = useRef<AbortController | null>(null);
  const detailSelectionKey = useRef<string | null>(null);

  const expireSession = useCallback((phase: AuthPhase = 'expired') => {
    setSession(null);
    setAuthPhase(phase);
  }, []);

  /* oxlint-disable react/react-compiler -- stable API and expiry dependencies are explicit for exhaustive-deps. */
  const refreshDashboard = useCallback(
    async (signal?: AbortSignal) => {
      if (refreshLock.current) {
        return;
      }
      refreshLock.current = true;
      setRefreshing(true);
      setDialogs((current) => loading(current));
      setTasks((current) => loading(current));
      setHealth((current) => loading(current));
      try {
        const [dialogResult, taskResult, healthResult] =
          await Promise.allSettled([
            api.listDialogs(signal),
            api.listTasks(signal),
            api.controlHealth(signal),
          ]);
        if (signal?.aborted) {
          return;
        }
        const unauthorized = [dialogResult, taskResult, healthResult].some(
          (result) => isUnauthorizedResult(result),
        );
        if (unauthorized) {
          expireSession('expired');
          return;
        }
        setDialogs((current) => resourceResult(dialogResult, current.data));
        setTasks((current) => resourceResult(taskResult, current.data));
        setHealth((current) => resourceResult(healthResult, current.data));
        if (dialogResult.status === 'fulfilled') {
          eventCursor.current = dialogResult.value.eventCheckpoint;
        }
      } finally {
        refreshLock.current = false;
        setRefreshing(false);
      }
    },
    [api, expireSession],
  );
  /* oxlint-enable react/react-compiler */

  const authenticate = useCallback(
    async (signal: AbortSignal) => {
      const telegram = telegramWebApp();
      prepareTelegramWebApp(telegram);
      if (telegram === null || telegram.initData.length === 0) {
        setAuthPhase('required');
        return;
      }
      setAuthPhase('authenticating');
      try {
        await api.bootstrap(signal);
        const authenticated = await api.exchangeTelegram(
          telegram.initData,
          signal,
        );
        if (signal.aborted) {
          return;
        }
        setSession(authenticated);
        setAuthPhase('authenticated');
        await refreshDashboard(signal);
      } catch (error) {
        if (signal.aborted) {
          return;
        }
        const apiError = normalizeError(error);
        if (apiError.kind === 'unauthorized') {
          expireSession('expired');
        } else if (apiError.kind === 'forbidden') {
          expireSession('forbidden');
        } else {
          expireSession('unavailable');
        }
      }
    },
    [api, expireSession, refreshDashboard],
  );

  useEffect(() => {
    const abort = new AbortController();
    let mounted = true;
    const start = async () => {
      setAuthPhase('checking');
      try {
        const initialDialogs = await api.listDialogs(abort.signal);
        if (!mounted) {
          return;
        }
        setDialogs({ status: 'ready', data: initialDialogs, error: null });
        eventCursor.current = initialDialogs.eventCheckpoint;
        const resumed = await api.resumeSession(abort.signal);
        if (!mounted || abort.signal.aborted) {
          return;
        }
        setSession(resumed);
        setAuthPhase('authenticated');
        await refreshDashboard(abort.signal);
      } catch (error) {
        if (!mounted || abort.signal.aborted) {
          return;
        }
        const apiError = normalizeError(error);
        if (apiError.kind === 'unauthorized') {
          await authenticate(abort.signal);
          return;
        }
        if (apiError.kind === 'forbidden') {
          expireSession('forbidden');
          return;
        }
        expireSession('unavailable');
        setDialogs({ status: 'error', data: null, error: apiError });
      }
    };
    void start();
    return () => {
      mounted = false;
      abort.abort();
    };
  }, [api, authenticate, expireSession, refreshDashboard]);

  useEffect(() => {
    if (session === null) {
      return;
    }
    const remaining = Date.parse(session.expiresAt) - Date.now();
    const timer = window.setTimeout(
      () => expireSession('expired'),
      Math.max(0, Math.min(remaining, 2_147_483_647)),
    );
    return () => window.clearTimeout(timer);
  }, [expireSession, session]);

  useEffect(() => {
    const update = () => setOnline(navigator.onLine);
    window.addEventListener('online', update);
    window.addEventListener('offline', update);
    const timer = window.setInterval(() => setClock(Date.now()), CLOCK_TICK_MS);
    return () => {
      window.removeEventListener('online', update);
      window.removeEventListener('offline', update);
      window.clearInterval(timer);
    };
  }, []);

  useEffect(() => {
    if (!online || authPhase !== 'authenticated') {
      return;
    }
    const abort = new AbortController();
    void refreshDashboard(abort.signal);
    return () => abort.abort();
  }, [authPhase, online, refreshDashboard]);

  /* oxlint-disable react/react-compiler -- preserving prior async Resource data triggers an upstream compiler invariant. */
  useEffect(() => {
    detailAbort.current?.abort();
    const dialogID = selectedDialogID;
    const taskID = selectedTaskID;
    const selectionKey =
      taskID !== null
        ? `task:${taskID}:${dialogID ?? ''}`
        : dialogID === null
          ? null
          : `dialog:${dialogID}`;
    if (selectionKey === null || authPhase !== 'authenticated') {
      detailSelectionKey.current = null;
      if (selectionKey === null) {
        setDialog(idle());
        setMessages(idle());
        setTask(idle());
      }
      return;
    }
    const preserveReadback = detailSelectionKey.current === selectionKey;
    detailSelectionKey.current = selectionKey;
    const abort = new AbortController();
    detailAbort.current = abort;

    if (taskID !== null) {
      setDialog(idle());
      setMessages(idle());
      setTask((current) =>
        preserveReadback
          ? loading(current)
          : { status: 'loading', data: null, error: null },
      );
      const loadSelectedTask = async () => {
        await Promise.resolve();
        if (abort.signal.aborted) {
          return;
        }
        try {
          const taskSnapshot = await api.task(taskID, abort.signal);
          if (dialogID !== null && taskSnapshot.task.dialogId !== dialogID) {
            throw new ApiError(
              'invalid_response',
              'selected task does not belong to its navigation dialog',
            );
          }
          if (!abort.signal.aborted) {
            setTask({ status: 'ready', data: taskSnapshot, error: null });
          }
        } catch (error) {
          if (abort.signal.aborted) {
            return;
          }
          const apiError = normalizeError(error);
          if (apiError.kind === 'unauthorized') {
            expireSession('expired');
            return;
          }
          setTask((current) => ({
            status: 'error',
            data: preserveReadback ? current.data : null,
            error: apiError,
          }));
        }
      };
      void loadSelectedTask();
      return () => abort.abort();
    }

    if (dialogID === null) {
      return () => abort.abort();
    }
    const load = async () => {
      await Promise.resolve();
      if (abort.signal.aborted) {
        return;
      }
      setDialog((current) =>
        preserveReadback
          ? loading(current)
          : { status: 'loading', data: null, error: null },
      );
      setMessages((current) =>
        preserveReadback
          ? loading(current)
          : { status: 'loading', data: null, error: null },
      );
      setTask((current) => (preserveReadback ? loading(current) : idle()));
      const results = await Promise.allSettled([
        api.dialog(dialogID, abort.signal),
        api.messages(dialogID, abort.signal),
      ]);
      if (abort.signal.aborted) {
        return;
      }
      if (results.some((result) => isUnauthorizedResult(result))) {
        expireSession('expired');
        return;
      }
      setDialog((current) => resourceResult(results[0], current.data));
      setMessages((current) => resourceResult(results[1], current.data));
      if (results[0].status === 'rejected') {
        const dialogFailure = normalizeError(results[0].reason);
        setTask((current) =>
          preserveReadback
            ? {
                status: 'error',
                data: current.data,
                error: dialogFailure,
              }
            : idle(),
        );
        return;
      }
      const activeTaskID = results[0].value.dialog.activeTaskId;
      if (activeTaskID === null) {
        setTask(idle());
        return;
      }
      setTask((current) =>
        current.data?.task.id === activeTaskID
          ? loading(current)
          : { status: 'loading', data: null, error: null },
      );
      try {
        const taskSnapshot = await api.task(activeTaskID, abort.signal);
        if (taskSnapshot.task.dialogId !== dialogID) {
          throw new ApiError(
            'invalid_response',
            'active task does not belong to the selected dialog',
          );
        }
        if (!abort.signal.aborted) {
          setTask({ status: 'ready', data: taskSnapshot, error: null });
        }
      } catch (error) {
        if (abort.signal.aborted) {
          return;
        }
        const apiError = normalizeError(error);
        if (apiError.kind === 'unauthorized') {
          expireSession('expired');
          return;
        }
        setTask((current) => ({
          status: 'error',
          data: current.data,
          error: apiError,
        }));
      }
    };
    void load();
    return () => abort.abort();
  }, [
    api,
    authPhase,
    detailRefreshVersion,
    expireSession,
    selectedDialogID,
    selectedTaskID,
  ]);
  /* oxlint-enable react/react-compiler */

  useEffect(() => {
    if (authPhase !== 'authenticated' || !online) {
      return;
    }
    let stopped = false;
    let timer = 0;
    let abort: AbortController | null = null;
    let running = false;
    let pollImmediatelyAfterRun = false;
    const schedule = (delay?: number) => {
      window.clearTimeout(timer);
      timer = window.setTimeout(
        () => void poll(),
        delay ?? eventPollDelay(document.visibilityState),
      );
    };
    async function poll() {
      if (stopped || running) {
        return;
      }
      running = true;
      abort = new AbortController();
      try {
        let pages = 0;
        let latest: EventPage | null = null;
        let cursor = eventCursor.current;
        do {
          latest = await api.events(cursor, abort.signal);
          cursor = latest.nextCursor;
          pages += 1;
        } while (latest.hasMore && pages < 10 && !abort.signal.aborted);
        if (latest?.hasMore) {
          throw new ApiError(
            'invalid_response',
            'event delta exceeds client bound',
          );
        }
        if (stopped || abort.signal.aborted || latest === null) {
          return;
        }
        eventCursor.current = cursor;
        setEvents({ status: 'ready', data: latest, error: null });
        if (latest.events.length > 0) {
          await refreshDashboard(abort.signal);
          setDetailRefreshVersion((current) => current + 1);
        }
      } catch (error) {
        if (stopped || abort?.signal.aborted) {
          return;
        }
        const apiError = normalizeError(error);
        if (apiError.kind === 'unauthorized') {
          expireSession('expired');
          return;
        }
        if (
          apiError.code === 'resync_required' ||
          apiError.kind === 'invalid_response'
        ) {
          eventCursor.current = null;
          setEvents({ status: 'loading', data: null, error: null });
          await refreshDashboard(abort.signal);
          setDetailRefreshVersion((current) => current + 1);
        } else {
          setEvents({ status: 'error', data: null, error: apiError });
        }
      } finally {
        running = false;
        abort = null;
        if (!stopped) {
          const delay = pollImmediatelyAfterRun ? 0 : undefined;
          pollImmediatelyAfterRun = false;
          schedule(delay);
        }
      }
    }
    const visibilityChanged = () => {
      if (document.visibilityState === 'visible') {
        if (running) {
          pollImmediatelyAfterRun = true;
        } else {
          schedule(0);
        }
      } else if (!running) {
        schedule();
      }
    };
    document.addEventListener('visibilitychange', visibilityChanged);
    schedule(0);
    return () => {
      stopped = true;
      document.removeEventListener('visibilitychange', visibilityChanged);
      window.clearTimeout(timer);
      abort?.abort();
    };
  }, [api, authPhase, expireSession, online, refreshDashboard]);

  const refresh = useCallback(async () => {
    if (authPhase !== 'authenticated') {
      const abort = new AbortController();
      await authenticate(abort.signal);
      return;
    }
    dashboardAbort.current?.abort();
    const abort = new AbortController();
    dashboardAbort.current = abort;
    await refreshDashboard(abort.signal);
    if (!abort.signal.aborted) {
      setDetailRefreshVersion((current) => current + 1);
    }
  }, [authPhase, authenticate, refreshDashboard]);

  const signOut = useCallback(async () => {
    if (session !== null) {
      try {
        await api.revokeSession(session.csrfToken);
      } catch {
        // Local session state is still cleared; the server cookie is HttpOnly.
      }
    }
    expireSession('expired');
  }, [api, expireSession, session]);

  const freshestAt = useMemo(() => {
    const values = [
      dialogs.data?.freshnessAt,
      tasks.data?.freshnessAt,
      health.data?.freshnessAt,
      events.data?.freshnessAt,
      ...(selectedDialogID === null && selectedTaskID === null
        ? []
        : [
            dialog.data?.freshnessAt,
            messages.data?.freshnessAt,
            task.data?.freshnessAt,
          ]),
    ]
      .filter((value): value is string => value !== undefined)
      .map((value) => Date.parse(value));
    return values.length === 0 ? 0 : Math.min(...values);
  }, [
    dialog.data,
    dialogs.data,
    events.data,
    health.data,
    messages.data,
    selectedDialogID,
    selectedTaskID,
    task.data,
    tasks.data,
  ]);

  const stale =
    !online ||
    (freshestAt > 0 && clock - freshestAt > STALE_AFTER_MS) ||
    dialogs.status === 'error' ||
    tasks.status === 'error' ||
    health.status === 'error' ||
    events.status === 'error' ||
    (selectedTaskID !== null
      ? task.status === 'error'
      : selectedDialogID !== null &&
        (dialog.status === 'error' ||
          messages.status === 'error' ||
          task.status === 'error'));

  let pendingVersion: number | null = null;
  let submissionStorageValid = true;
  try {
    if (selectedDialogID !== null)
      pendingVersion = pendingSubmissionVersion(
        localStorage,
        api.clientInstanceId,
        selectedDialogID,
      );
  } catch {
    submissionStorageValid = false;
  }
  const submissionTargetVersion =
    pendingVersion ?? dialog.data?.dialog.objectVersion ?? null;
  const canSubmitTask =
    authPhase === 'authenticated' &&
    session?.taskSubmissionEnabled === true &&
    online &&
    !stale &&
    dialog.status === 'ready' &&
    dialog.data?.dialog.id === selectedDialogID &&
    dialog.data.dialog.lifecycle === 'active' &&
    ((dialog.data.dialog.activeTaskId === null &&
      dialog.data.dialog.pendingAdmissionId === undefined) ||
      pendingVersion !== null) &&
    submissionStorageValid &&
    !submittingTask &&
    navigator.locks !== undefined;

  const submitTask = async (
    issueId: string,
    text: string,
    confirmedVersion: number,
  ): Promise<SubmissionReceipt | null> => {
    if (
      !canSubmitTask ||
      session === null ||
      dialog.data === null ||
      submissionLock.current ||
      confirmedVersion !== submissionTargetVersion
    )
      return null;
    const target = dialog.data.dialog;
    submissionLock.current = true;
    setSubmittingTask(true);
    setSubmissionError(null);
    try {
      return await navigator.locks.request(
        `fixik:task-submit:${api.clientInstanceId}:${target.id}`,
        async () => {
          const command = await pendingSubmission(
            localStorage,
            api.clientInstanceId,
            target.id,
            confirmedVersion,
            issueId,
            text,
          );
          if (command.expectedVersion !== confirmedVersion)
            throw new Error('Pending target changed after confirmation');
          try {
            const receipt = await api.submitTask(
              target.id,
              command.commandId,
              command.expectedVersion,
              issueId,
              text,
              session.csrfToken,
            );
            clearSubmission(localStorage, api.clientInstanceId, command);
            setDetailRefreshVersion((version) => version + 1);
            await refreshDashboard();
            return receipt;
          } catch (error) {
            if (
              error instanceof ApiError &&
              error.code === 'command_conflict'
            ) {
              clearSubmission(localStorage, api.clientInstanceId, command);
              setSubmissionError(
                'Состояние диалога изменилось. Черновик сохранён; обновите диалог и подтвердите адресата ещё раз.',
              );
              setDetailRefreshVersion((version) => version + 1);
              await refreshDashboard();
            } else {
              setSubmissionError(
                'Приём не подтверждён. Повторите тот же текст и адресат: идентификатор команды сохранён.',
              );
              if (error instanceof ApiError && error.kind === 'unauthorized')
                setSession(await api.resumeSession());
            }
            return null;
          }
        },
      );
    } catch {
      setSubmissionError(
        'Есть неподтверждённая команда с другим черновиком либо хранилище недоступно. Восстановите исходный текст и адресата.',
      );
      return null;
    } finally {
      submissionLock.current = false;
      setSubmittingTask(false);
    }
  };

  const canCreateDialog =
    authPhase === 'authenticated' &&
    session?.dialogCreationEnabled === true &&
    online &&
    !stale &&
    dialogs.status === 'ready' &&
    !creatingDialog &&
    navigator.locks !== undefined;

  const createDialog = async (): Promise<string | null> => {
    if (
      !canCreateDialog ||
      session === null ||
      dialogs.data === null ||
      createDialogLock.current
    ) {
      return null;
    }
    const expectedVersion = dialogs.data.snapshotVersion;
    createDialogLock.current = true;
    setCreatingDialog(true);
    setCreateDialogError(null);
    try {
      // Serialize the persisted command namespace across same-origin tabs.
      // Hold the lock through ACK so another tab cannot replace an uncertain ID.
      return await navigator.locks.request(
        `fixik:dialog-create:${api.clientInstanceId}`,
        async () => {
          const command = pendingDialogCommand(
            localStorage,
            api.clientInstanceId,
            expectedVersion,
          );
          try {
            const created = await api.createDialog(
              command.commandId,
              command.expectedCollectionVersion,
              session.csrfToken,
            );
            clearDialogCommand(localStorage, api.clientInstanceId, command);
            await refreshDashboard();
            return created.id;
          } catch (error) {
            if (
              error instanceof ApiError &&
              error.code === 'command_conflict'
            ) {
              clearDialogCommand(localStorage, api.clientInstanceId, command);
              setCreateDialogError(
                'Список изменился. Проверьте обновлённый список и нажмите «Создать диалог» ещё раз.',
              );
              await refreshDashboard();
            } else {
              // Keep the durable identity. A retry must never create a second command.
              setCreateDialogError(
                'Создание не подтверждено. Повторите запрос — он не создаст второй диалог.',
              );
              if (error instanceof ApiError && error.kind === 'unauthorized') {
                const resumed = await api.resumeSession();
                setSession(resumed);
              }
            }
            return null;
          }
        },
      );
    } catch {
      setCreateDialogError(
        'Не удалось сохранить запрос или восстановить сессию. Запрос не подтверждён.',
      );
      return null;
    } finally {
      createDialogLock.current = false;
      setCreatingDialog(false);
    }
  };

  return {
    submissionTargetVersion,
    canSubmitTask,
    submittingTask,
    submissionError,
    submitTask,
    authPhase,
    session,
    dialogs,
    tasks,
    health,
    dialog,
    messages,
    task,
    events,
    online,
    stale,
    refreshing,
    refresh,
    signOut,
    canCreateDialog,
    creatingDialog,
    createDialogError,
    createDialog,
  };
}

function loading<T>(current: Resource<T>): Resource<T> {
  return { status: 'loading', data: current.data, error: null };
}

function resourceResult<T>(
  result: PromiseSettledResult<T>,
  previous: T | null = null,
): Resource<T> {
  return result.status === 'fulfilled'
    ? { status: 'ready', data: result.value, error: null }
    : { status: 'error', data: previous, error: normalizeError(result.reason) };
}

function isUnauthorizedResult(result: PromiseSettledResult<unknown>): boolean {
  return (
    result.status === 'rejected' &&
    normalizeError(result.reason).kind === 'unauthorized'
  );
}

function normalizeError(error: unknown): ApiError {
  return error instanceof ApiError
    ? error
    : new ApiError('unavailable', 'API operation failed', { cause: error });
}
