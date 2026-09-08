import { useCallback, useEffect, useMemo, useReducer, useState } from 'react';
import {
  ArrowLeft,
  Bot,
  CheckCircle2,
  ChevronRight,
  CircleAlert,
  Clock3,
  CloudOff,
  Gauge,
  LoaderCircle,
  LockKeyhole,
  MessageCircle,
  Plus,
  RefreshCw,
  Send,
  ShieldAlert,
  ShieldCheck,
  Square,
  StopCircle,
  Wifi,
} from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
  AlertDialogAction,
} from '@/components/ui/alert-dialog';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Textarea } from '@/components/ui/textarea';
import {
  type AuthPhase,
  type Resource,
  useMobileWorkspace,
} from '@/hooks/use-mobile-workspace';
import {
  type ApiError,
  type ControlHealth,
  type Dialog,
  type DialogLifecycle,
  type DialogMessage,
  type DialogPage,
  type HealthStatus,
  type MessagePage,
  type SubmissionReceipt,
  type Task,
  type TaskPage,
  type TaskSnapshot,
  type TaskState,
} from '@/lib/api';
import {
  bindTelegramBackButton,
  initializeTelegramTheme,
  prepareTelegramWebApp,
  readDialogDraft,
  telegramWebApp,
  writeDialogDraft,
} from '@/lib/telegram';
import {
  initialWorkspaceNavigation,
  type WorkspaceView,
  workspaceNavigationReducer,
} from '@/lib/navigation';
import { cn } from '@/lib/utils';
import {
  readSubmissionIssue,
  writeSubmissionIssue,
  validSubmissionIntent,
} from '@/lib/submission-command';

const taskStatusCopy: Record<TaskState, string> = {
  ready: 'Готова к запуску',
  running: 'В работе',
  waiting: 'Ожидает',
  completed: 'Завершена',
  failed: 'Ошибка',
  cancelled: 'Остановлена',
  needs_review: 'Нужна проверка',
};

const lifecycleCopy: Record<DialogLifecycle, string> = {
  active: 'Активен',
  archived: 'В архиве',
};

export function MobileWorkspace() {
  const [navigation, navigate] = useReducer(
    workspaceNavigationReducer,
    initialWorkspaceNavigation,
  );
  const { selectedDialogID, selectedTaskID, view } = navigation;
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const workspace = useMobileWorkspace(selectedDialogID, selectedTaskID);
  const telegram = useMemo(() => telegramWebApp(), []);
  const closeDetail = useCallback(() => navigate({ type: 'close-detail' }), []);

  useEffect(() => {
    prepareTelegramWebApp(telegram);
    return initializeTelegramTheme(telegram);
  }, [telegram]);
  useEffect(
    () =>
      bindTelegramBackButton(
        telegram,
        selectedDialogID !== null || selectedTaskID !== null,
        closeDetail,
      ),
    [closeDetail, selectedDialogID, selectedTaskID, telegram],
  );
  const selectDialog = useCallback((dialogID: string) => {
    setDrafts((current) =>
      current[dialogID] === undefined
        ? { ...current, [dialogID]: readDialogDraft(dialogID) }
        : current,
    );
    navigate({ type: 'open-dialog', dialogID });
  }, []);
  const selectTask = useCallback((taskID: string, dialogID: string) => {
    navigate({ type: 'open-task', taskID, dialogID });
  }, []);
  const changeDraft = useCallback((dialogID: string, value: string) => {
    const bounded = value.slice(0, 16_384);
    setDrafts((current) => ({ ...current, [dialogID]: bounded }));
    writeDialogDraft(dialogID, bounded);
  }, []);

  const listDialog = workspace.dialogs.data?.dialogs.find(
    (dialog) => dialog.id === selectedDialogID,
  );
  const detailDialog = workspace.dialog.data?.dialog ?? listDialog ?? null;

  return (
    <main className="workspace-canvas min-h-[100dvh] sm:grid sm:place-items-center sm:px-6 sm:py-6">
      <section className="relative mx-auto flex h-[100dvh] w-full max-w-[500px] flex-col overflow-hidden bg-background sm:h-[min(880px,calc(100dvh-48px))] sm:rounded-[2rem] sm:border sm:shadow-[0_32px_90px_rgb(4_20_12/16%)]">
        <WorkspaceHeader
          selectedDialogID={selectedDialogID}
          selectedTaskID={selectedTaskID}
          authPhase={workspace.authPhase}
          eventsReady={workspace.events.status === 'ready'}
          online={workspace.online}
          refreshing={workspace.refreshing}
          stale={workspace.stale}
          onBack={closeDetail}
          onRefresh={() => void workspace.refresh()}
        />
        <WorkspaceBanner workspace={workspace} />

        <div className="scrollbar-none flex-1 overflow-y-auto">
          {workspace.authPhase !== 'authenticated' ? (
            <AuthenticationView
              phase={workspace.authPhase}
              onRetry={() => void workspace.refresh()}
            />
          ) : selectedTaskID !== null ? (
            <TaskDetail selectedTaskID={selectedTaskID} task={workspace.task} />
          ) : selectedDialogID !== null ? (
            <DialogDetail
              key={selectedDialogID}
              dialogId={selectedDialogID}
              dialog={detailDialog}
              dialogError={workspace.dialog.error}
              dialogLoading={workspace.dialog.status === 'loading'}
              draft={drafts[selectedDialogID] ?? ''}
              messages={workspace.messages}
              task={workspace.task.data?.task ?? null}
              taskError={workspace.task.error}
              taskLoading={workspace.task.status === 'loading'}
              onDraftChange={(value) => changeDraft(selectedDialogID, value)}
              canSubmit={workspace.canSubmitTask}
              submitting={workspace.submittingTask}
              submissionError={workspace.submissionError}
              targetVersion={workspace.submissionTargetVersion}
              onSubmit={workspace.submitTask}
            />
          ) : view === 'dialogs' ? (
            <DialogsView
              dialogs={workspace.dialogs}
              tasks={workspace.tasks.data}
              onOpen={selectDialog}
              canCreate={workspace.canCreateDialog}
              creating={workspace.creatingDialog}
              createError={workspace.createDialogError}
              onCreate={() => {
                void workspace.createDialog().then((id) => {
                  if (id !== null) {
                    selectDialog(id);
                  }
                });
              }}
            />
          ) : view === 'tasks' ? (
            <TasksView tasks={workspace.tasks} onOpen={selectTask} />
          ) : (
            <ControlView health={workspace.health} events={workspace.events} />
          )}
        </div>

        {workspace.authPhase === 'authenticated' &&
        selectedDialogID === null &&
        selectedTaskID === null ? (
          <BottomNavigation
            view={view}
            onNavigate={(nextView) =>
              navigate({ type: 'navigate', view: nextView })
            }
          />
        ) : null}
      </section>
    </main>
  );
}

function WorkspaceHeader({
  selectedDialogID,
  selectedTaskID,
  authPhase,
  online,
  refreshing,
  stale,
  eventsReady,
  onBack,
  onRefresh,
}: {
  selectedDialogID: string | null;
  selectedTaskID: string | null;
  authPhase: AuthPhase;
  online: boolean;
  refreshing: boolean;
  stale: boolean;
  eventsReady: boolean;
  onBack: () => void;
  onRefresh: () => void;
}) {
  const indicator = freshnessIndicator({
    authPhase,
    online,
    refreshing,
    stale,
    eventsReady,
  });
  const IndicatorIcon = indicator.icon;
  return (
    <header className="safe-top z-20 border-b bg-background/92 px-4 pb-3 backdrop-blur-xl">
      <div className="flex min-h-11 items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          {selectedDialogID !== null || selectedTaskID !== null ? (
            <Button
              aria-label="Назад к списку"
              className="size-11 rounded-full"
              onClick={onBack}
              size="icon"
              variant="ghost"
            >
              <ArrowLeft />
            </Button>
          ) : (
            <span className="grid size-10 shrink-0 place-items-center rounded-2xl bg-primary text-primary-foreground shadow-sm">
              <Bot className="size-5" aria-hidden="true" />
            </span>
          )}
          <div className="min-w-0">
            <p className="truncate text-[15px] font-semibold leading-5">
              {selectedTaskID !== null
                ? 'Задача'
                : selectedDialogID !== null
                  ? 'Диалог'
                  : 'Fixik Next'}
            </p>
            <p className="truncate font-mono text-[11px] text-muted-foreground">
              {selectedTaskID !== null
                ? compactID(selectedTaskID)
                : selectedDialogID !== null
                  ? compactID(selectedDialogID)
                  : 'Mobile Workspace'}
            </p>
          </div>
        </div>
        <Button
          aria-label="Обновить authoritative readback"
          className={cn('h-11 rounded-full px-3 text-xs', indicator.className)}
          disabled={refreshing}
          onClick={onRefresh}
          variant="ghost"
        >
          <IndicatorIcon className={cn(refreshing && 'animate-spin')} />
          {indicator.label}
        </Button>
      </div>
    </header>
  );
}

function WorkspaceBanner({
  workspace,
}: {
  workspace: ReturnType<typeof useMobileWorkspace>;
}) {
  if (workspace.authPhase !== 'authenticated') return null;
  const failed = [
    workspace.dialogs.status === 'error' ? 'диалоги' : null,
    workspace.tasks.status === 'error' ? 'задачи' : null,
    workspace.health.status === 'error' ? 'контроль' : null,
    workspace.events.status === 'error' ? 'события' : null,
    workspace.health.data?.liveness === 'unavailable' ? 'liveness' : null,
    workspace.health.data?.readiness === 'unavailable' ? 'readiness' : null,
  ].filter((value): value is string => value !== null);
  if (!workspace.online) {
    return (
      <NoticeBanner icon={CloudOff}>
        Нет сети. Показан последний readback в памяти; команды недоступны,
        локальные черновики сохранены.
      </NoticeBanner>
    );
  }
  if (failed.length > 0) {
    return (
      <NoticeBanner icon={CircleAlert}>
        Частичная недоступность: {failed.join(', ')}. Данные из успешных секций
        сохранены отдельно.
      </NoticeBanner>
    );
  }
  if (workspace.stale) {
    return (
      <NoticeBanner icon={Clock3}>
        Readback устарел. Дождитесь успешной синхронизации перед принятием
        решений.
      </NoticeBanner>
    );
  }
  return null;
}

function AuthenticationView({
  phase,
  onRetry,
}: {
  phase: AuthPhase;
  onRetry: () => void;
}) {
  const content = {
    checking: {
      icon: LoaderCircle,
      title: 'Проверяю сессию',
      description:
        'Читаю authoritative state по защищённой same-origin сессии.',
      spinning: true,
      retry: false,
    },
    authenticating: {
      icon: LockKeyhole,
      title: 'Проверяю Telegram',
      description: 'Подписанный initData обменивается на HttpOnly web-сессию.',
      spinning: true,
      retry: false,
    },
    required: {
      icon: Bot,
      title: 'Откройте Mini App из Telegram',
      description:
        'В обычной вкладке нет подписанного Telegram initData для входа.',
      spinning: false,
      retry: true,
    },
    expired: {
      icon: ShieldAlert,
      title: 'Сессия завершена',
      description:
        'Закройте и снова откройте Mini App, чтобы получить свежий initData.',
      spinning: false,
      retry: true,
    },
    forbidden: {
      icon: ShieldAlert,
      title: 'Нет доступа',
      description:
        'Telegram-пользователь не имеет owner-разрешения Mobile Workspace.',
      spinning: false,
      retry: true,
    },
    unavailable: {
      icon: CloudOff,
      title: 'Gateway недоступен',
      description: 'Проверьте сеть и повторите authoritative readback.',
      spinning: false,
      retry: true,
    },
    authenticated: {
      icon: ShieldCheck,
      title: 'Сессия подтверждена',
      description: '',
      spinning: false,
      retry: false,
    },
  }[phase];
  const Icon = content.icon;
  return (
    <div className="grid min-h-full place-items-center px-6 py-12 text-center">
      <div className="max-w-sm">
        <span className="mx-auto grid size-14 place-items-center rounded-2xl bg-primary/12 text-primary">
          <Icon className={cn('size-6', content.spinning && 'animate-spin')} />
        </span>
        <h1 className="mt-5 text-xl font-semibold tracking-[-0.025em]">
          {content.title}
        </h1>
        <p className="mt-2 text-sm leading-6 text-muted-foreground">
          {content.description}
        </p>
        {content.retry ? (
          <Button
            className="mt-6 min-h-11 w-full"
            onClick={onRetry}
            variant="outline"
          >
            <RefreshCw />
            Проверить снова
          </Button>
        ) : null}
      </div>
    </div>
  );
}

function DialogsView({
  dialogs,
  tasks,
  onOpen,
  canCreate,
  creating,
  createError,
  onCreate,
}: {
  dialogs: Resource<DialogPage>;
  tasks: TaskPage | null;
  onOpen: (id: string) => void;
  canCreate: boolean;
  creating: boolean;
  createError: string | null;
  onCreate: () => void;
}) {
  return (
    <div className="px-4 pb-7 pt-5">
      <div className="mb-5 flex items-end justify-between gap-3">
        <div>
          <p className="text-xs font-medium uppercase tracking-[0.12em] text-muted-foreground">
            {dialogs.data === null
              ? 'Authoritative state'
              : `${dialogs.data.dialogs.length} ${countNoun(dialogs.data.dialogs.length, 'контекст', 'контекста', 'контекстов')}`}
          </p>
          <h1 className="mt-1 text-2xl font-semibold tracking-[-0.035em]">
            Диалоги
          </h1>
        </div>
        <Button
          aria-label="Создать диалог"
          className="size-11 rounded-full shadow-sm"
          disabled={!canCreate}
          size="icon"
          title={
            canCreate
              ? 'Создать диалог'
              : 'Создание недоступно до авторизации и обновления данных'
          }
          onClick={onCreate}
          aria-busy={creating}
        >
          <Plus />
        </Button>
      </div>

      {createError !== null ? (
        <p role="alert" className="mb-3 text-sm text-destructive">
          {createError}
        </p>
      ) : null}
      {dialogs.data !== null && dialogs.error !== null ? (
        <div className="mb-3">
          <ResourceFailure
            error={dialogs.error}
            label="Обновление диалогов недоступно"
          />
        </div>
      ) : null}
      {dialogs.data === null && dialogs.status === 'loading' ? (
        <LoadingCards />
      ) : dialogs.data === null && dialogs.error !== null ? (
        <ResourceFailure error={dialogs.error} label="Диалоги недоступны" />
      ) : dialogs.data?.dialogs.length === 0 ? (
        <EmptyState
          icon={MessageCircle}
          title="Диалогов нет"
          description="Gateway вернул пустой owner-scoped список. Создание пока не опубликовано."
        />
      ) : (
        <div className="space-y-3">
          {dialogs.data?.dialogs.map((dialog) => {
            const activeTask = tasks?.tasks.find(
              (task) =>
                task.id === dialog.activeTaskId && task.dialogId === dialog.id,
            );
            return (
              <button
                aria-label={`Открыть диалог ${dialog.id}`}
                className="group w-full rounded-2xl border bg-card p-4 text-left shadow-[0_8px_24px_rgb(5_25_14/5%)] transition hover:-translate-y-0.5 hover:border-primary/35 focus-visible:outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
                key={dialog.id}
                onClick={() => onOpen(dialog.id)}
                type="button"
              >
                <div className="flex min-h-11 items-start gap-3">
                  <span className="grid size-11 shrink-0 place-items-center rounded-2xl bg-emerald-500/12 text-emerald-700 dark:text-emerald-300">
                    <MessageCircle className="size-5" aria-hidden="true" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center justify-between gap-3">
                      <h2 className="truncate font-mono text-sm font-semibold">
                        {compactID(dialog.id)}
                      </h2>
                      <time
                        className="shrink-0 text-[11px] text-muted-foreground"
                        dateTime={dialog.updatedAt}
                      >
                        {relativeTime(dialog.updatedAt)}
                      </time>
                    </div>
                    <p className="mt-1 truncate text-[13px] leading-5 text-muted-foreground">
                      version {dialog.objectVersion} ·{' '}
                      {lifecycleCopy[dialog.lifecycle]}
                    </p>
                  </div>
                </div>
                <div className="mt-3 flex min-h-6 items-center justify-between gap-3 border-t pt-3">
                  <div className="flex min-w-0 items-center gap-2">
                    <StatusDot state={activeTask?.state ?? dialog.lifecycle} />
                    <span className="truncate text-xs font-medium">
                      {activeTask === undefined
                        ? dialog.activeTaskId === null
                          ? 'Нет активной задачи'
                          : compactID(dialog.activeTaskId)
                        : `${compactID(activeTask.id)} · ${taskStatusCopy[activeTask.state]}`}
                    </span>
                  </div>
                  <ChevronRight className="size-4 shrink-0 text-muted-foreground transition group-hover:translate-x-0.5" />
                </div>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

export function TaskDetail({
  selectedTaskID,
  task,
}: {
  selectedTaskID: string;
  task: Resource<TaskSnapshot>;
}) {
  const selected = task.data?.task ?? null;
  return (
    <div className="px-4 pb-7 pt-5">
      <p className="text-xs font-medium uppercase tracking-[0.12em] text-muted-foreground">
        Owner-scoped authoritative readback
      </p>
      <h1 className="mt-1 text-2xl font-semibold tracking-[-0.035em]">
        Задача
      </h1>

      {selected !== null && task.error !== null ? (
        <div className="mt-5">
          <ResourceFailure
            error={task.error}
            label="Обновление Task readback недоступно"
          />
        </div>
      ) : null}
      {selected === null && task.status === 'loading' ? (
        <div className="mt-6">
          <LoadingCard />
        </div>
      ) : selected === null ? (
        <div className="mt-6">
          <ResourceFailure error={task.error} label="Задача недоступна" />
        </div>
      ) : (
        <Card className="mt-6 border-0 shadow-[0_12px_32px_rgb(5_25_14/7%)] ring-1 ring-primary/15">
          <CardHeader>
            <CardTitle className="font-mono text-base">
              {compactID(selected.id)}
            </CardTitle>
            <CardDescription>
              {selected.issueId}@{selected.expectedRevision}
            </CardDescription>
            <CardAction>
              <Badge variant="secondary">
                {taskStatusCopy[selected.state]}
              </Badge>
            </CardAction>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-2 gap-2 text-xs">
              <Metric label="Task ID" value={compactID(selected.id)} mono />
              <Metric
                label="Dialog ID"
                value={compactID(selected.dialogId)}
                mono
              />
              <Metric
                label="Состояние"
                value={taskStatusCopy[selected.state]}
              />
              <Metric label="Cancel state" value={selected.cancelState} mono />
              <Metric label="Создана" value={formatDate(selected.createdAt)} />
              <Metric
                label="Обновлена"
                value={formatDate(selected.updatedAt)}
              />
              <Metric
                label="Next Check"
                value={
                  selected.nextCheckAt === null
                    ? 'Не назначен'
                    : formatDate(selected.nextCheckAt)
                }
              />
            </div>
            <p className="rounded-xl bg-muted px-3 py-2 text-xs leading-5 text-muted-foreground">
              Выбранный ID {compactID(selectedTaskID)} подтверждён отдельным
              owner-scoped GET. Terminal Task остаётся доступна после release
              активного work slot.
            </p>
          </CardContent>
        </Card>
      )}
    </div>
  );
}

function DialogDetail({
  dialogId,
  dialog,
  dialogError,
  dialogLoading,
  draft,
  messages,
  task,
  taskError,
  taskLoading,
  onDraftChange,
  canSubmit,
  submitting,
  submissionError,
  targetVersion,
  onSubmit,
}: {
  dialogId: string;
  dialog: Dialog | null;
  dialogError: ApiError | null;
  dialogLoading: boolean;
  draft: string;
  messages: Resource<MessagePage>;
  task: Task | null;
  taskError: ApiError | null;
  taskLoading: boolean;
  onDraftChange: (value: string) => void;
  canSubmit: boolean;
  submitting: boolean;
  submissionError: string | null;
  targetVersion: number | null;
  onSubmit: (
    issueId: string,
    text: string,
    version: number,
  ) => Promise<SubmissionReceipt | null>;
}) {
  const terminal = task === null || isTerminal(task.state);
  const [issueId, setIssueId] = useState(() => readSubmissionIssue(dialogId));
  const [issueStorageValid, setIssueStorageValid] = useState(true);
  const [confirmation, setConfirmation] = useState<{
    text: string;
    issueId: string;
    version: number;
  } | null>(null);
  const [receipt, setReceipt] = useState<SubmissionReceipt | null>(null);
  const textValid =
    validSubmissionIntent(issueId, draft, targetVersion ?? 0) &&
    issueStorageValid;
  return (
    <div className="flex min-h-full flex-col">
      <div className="space-y-4 px-4 pb-5 pt-4">
        {dialog !== null && dialogError !== null ? (
          <InlineFailure label="Обновление Dialog readback недоступно" />
        ) : null}
        {dialog === null ? (
          dialogLoading ? (
            <LoadingCard />
          ) : (
            <ResourceFailure error={dialogError} label="Диалог недоступен" />
          )
        ) : (
          <Card className="border-0 bg-[linear-gradient(145deg,var(--card),color-mix(in_oklch,var(--accent),transparent_60%))] shadow-[0_12px_32px_rgb(5_25_14/7%)] ring-1 ring-primary/15">
            <CardHeader>
              <CardTitle className="font-mono text-base">
                {compactID(dialog.id)}
              </CardTitle>
              <CardDescription>
                Dialog version {dialog.objectVersion} ·{' '}
                {lifecycleCopy[dialog.lifecycle]}
              </CardDescription>
              <CardAction>
                <Badge variant="secondary">
                  {task === null
                    ? dialog.activeTaskId === null
                      ? dialog.pendingAdmissionId
                        ? 'Ожидает приёма задачи'
                        : 'Без задачи'
                      : taskError !== null
                        ? 'Task недоступна'
                        : 'Task загружается'
                    : taskStatusCopy[task.state]}
                </Badge>
              </CardAction>
            </CardHeader>
            <CardContent>
              {taskError !== null ? (
                <InlineFailure label="Task readback недоступен" />
              ) : dialog.pendingAdmissionId ? (
                <output className="block text-xs leading-5 text-muted-foreground">
                  Сообщение сохранено. Controller проверяет задачу и связь с
                  YouTrack; выполнение ещё не разрешено. Следующая проверка:{' '}
                  {dialog.admissionNextCheckAt
                    ? formatDate(dialog.admissionNextCheckAt)
                    : 'ещё не назначена'}
                  .
                </output>
              ) : dialog.activeTaskId === null ? (
                <p className="text-xs leading-5 text-muted-foreground">
                  Активная Task к диалогу не привязана.
                </p>
              ) : task === null || taskLoading ? (
                <div
                  aria-label="Загрузка Task readback"
                  className="h-16 animate-pulse rounded-xl bg-muted/70"
                />
              ) : (
                <div className="grid grid-cols-2 gap-2 text-xs">
                  <Metric label="Task" value={compactID(task.id)} mono />
                  <Metric
                    label="Состояние"
                    value={taskStatusCopy[task.state]}
                  />
                  <Metric
                    label="Issue target"
                    value={`${task.issueId}@${task.expectedRevision}`}
                    mono
                  />
                  <Metric
                    label="Next Check"
                    value={
                      task.nextCheckAt === null
                        ? 'Не назначен'
                        : formatDate(task.nextCheckAt)
                    }
                  />
                </div>
              )}
            </CardContent>
          </Card>
        )}
        <MessageHistory messages={messages} />
      </div>

      <div className="mt-auto border-t bg-background/96 px-4 py-3 backdrop-blur-xl">
        <div className="mb-2 flex items-center justify-between gap-2">
          <Badge variant="outline">
            {canSubmit && dialog !== null
              ? `Диалог ${compactID(dialog.id)} · версия ${targetVersion}`
              : task === null
                ? 'Нет mutation target'
                : `${compactID(task.id)}@${task.expectedRevision}`}
          </Badge>
          <Button
            className="min-h-11 px-3"
            disabled
            title="Task cancel route не опубликован"
            variant="ghost"
          >
            <StopCircle />
            Остановить
          </Button>
        </div>
        {canSubmit ? (
          <label className="mb-2 block text-xs">
            Задача YouTrack (если уже создана)
            <input
              aria-label="Задача YouTrack"
              className="mt-1 min-h-11 w-full rounded-xl border bg-card px-3"
              value={issueId}
              maxLength={64}
              placeholder="HL-…"
              onChange={(event) => {
                const value = event.target.value;
                setIssueId(value);
                try {
                  writeSubmissionIssue(dialogId, value);
                  setIssueStorageValid(true);
                } catch {
                  setIssueStorageValid(false);
                }
              }}
            />
            {issueId !== '' && !/^HL-[1-9][0-9]*$/.test(issueId) ? (
              <span role="alert">Укажите номер вида HL-210.</span>
            ) : null}
            {!issueStorageValid ? (
              <span role="alert">
                Не удалось сохранить адресата. Отправка недоступна.
              </span>
            ) : null}
          </label>
        ) : null}
        {submissionError ? (
          <p role="alert" className="mb-2 text-sm text-destructive">
            {submissionError}
          </p>
        ) : null}
        {receipt ? (
          <output className="mb-2 block text-sm">
            Сообщение получено. Задача {compactID(receipt.taskId)} ожидает
            проверки и связи с YouTrack; исполнение ещё не принято.
          </output>
        ) : null}
        <div className="flex items-end gap-2">
          <Textarea
            aria-label="Локальный черновик сообщения"
            className="max-h-36 min-h-12 resize-none rounded-2xl bg-card px-3 py-3"
            disabled={submitting || (terminal && !canSubmit)}
            maxLength={16_384}
            onChange={(event) => onDraftChange(event.target.value)}
            placeholder={
              canSubmit
                ? 'Опишите новую задачу для этого диалога'
                : terminal
                  ? 'Для этого диалога нет активной задачи'
                  : 'Локальный черновик — отправка пока не поддерживается'
            }
            value={draft}
          />
          <Button
            aria-label={
              canSubmit
                ? 'Поставить задачу'
                : 'Отправка недоступна: mutation route не опубликован'
            }
            className="size-12 shrink-0 rounded-2xl"
            disabled={!canSubmit || !textValid || submitting}
            size="icon-lg"
            title={
              canSubmit
                ? 'Подтвердить точный диалог и сообщение'
                : 'Message mutation route не опубликован'
            }
            onClick={() => {
              if (targetVersion !== null)
                setConfirmation({
                  text: draft,
                  issueId,
                  version: targetVersion,
                });
            }}
          >
            <Send />
          </Button>
        </div>
        <AlertDialog
          open={confirmation !== null}
          onOpenChange={(open) => {
            if (!open) setConfirmation(null);
          }}
        >
          <AlertDialogContent>
            <AlertDialogTitle>Поставить задачу?</AlertDialogTitle>
            <AlertDialogDescription>
              Диалог {dialog?.id}, версия {confirmation?.version}.{' '}
              {confirmation?.issueId
                ? `YouTrack: ${confirmation.issueId}.`
                : 'Связь с YouTrack будет проверена при admission.'}{' '}
              Приём сообщения не означает начало исполнения.
            </AlertDialogDescription>
            <p className="max-h-40 overflow-y-auto whitespace-pre-wrap break-words text-sm">
              {confirmation?.text}
            </p>
            <AlertDialogFooter>
              <AlertDialogCancel className="min-h-11">Назад</AlertDialogCancel>
              <AlertDialogAction
                className="min-h-11"
                disabled={!canSubmit || confirmation?.version !== targetVersion}
                onClick={() => {
                  const confirmed = confirmation;
                  if (confirmed === null) return;
                  setConfirmation(null);
                  void onSubmit(
                    confirmed.issueId,
                    confirmed.text,
                    confirmed.version,
                  ).then((result) => {
                    if (result !== null) {
                      setReceipt(result);
                      if (draft === confirmed.text) onDraftChange('');
                    }
                  });
                }}
              >
                Подтвердить отправку
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
        {!terminal ? (
          <p className="mt-2 text-[11px] leading-4 text-muted-foreground">
            Черновик хранится только в текущей вкладке. Отправка не имитируется.
          </p>
        ) : null}
      </div>
    </div>
  );
}

function MessageHistory({ messages }: { messages: Resource<MessagePage> }) {
  if (messages.data === null && messages.status === 'loading')
    return <LoadingMessages />;
  if (messages.data === null && messages.error !== null)
    return (
      <ResourceFailure error={messages.error} label="Сообщения недоступны" />
    );
  return (
    <div className="space-y-3">
      {messages.error !== null ? (
        <ResourceFailure
          error={messages.error}
          label="Обновление сообщений недоступно"
        />
      ) : null}
      {messages.data?.messages.length === 0 ? (
        <EmptyState
          icon={MessageCircle}
          title="Сообщений нет"
          description="Controller вернул пустую историю этого диалога."
        />
      ) : (
        <div className="space-y-3" aria-label="История диалога">
          {messages.data?.messages.map((message) => (
            <MessageBubble key={message.id} message={message} />
          ))}
        </div>
      )}
    </div>
  );
}

export function TasksView({
  tasks,
  onOpen,
}: {
  tasks: Resource<TaskPage>;
  onOpen: (taskID: string, dialogID: string) => void;
}) {
  const sections = tasks.data === null ? [] : taskSections(tasks.data.tasks);
  return (
    <div className="px-4 pb-7 pt-5">
      <p className="text-xs font-medium uppercase tracking-[0.12em] text-muted-foreground">
        Attention first
      </p>
      <h1 className="mt-1 text-2xl font-semibold tracking-[-0.035em]">
        Задачи
      </h1>
      {tasks.data !== null && tasks.error !== null ? (
        <div className="mt-6">
          <ResourceFailure
            error={tasks.error}
            label="Обновление задач недоступно"
          />
        </div>
      ) : null}
      {tasks.data === null && tasks.status === 'loading' ? (
        <div className="mt-6">
          <LoadingCards />
        </div>
      ) : tasks.data === null && tasks.error !== null ? (
        <div className="mt-6">
          <ResourceFailure error={tasks.error} label="Задачи недоступны" />
        </div>
      ) : tasks.data?.tasks.length === 0 ? (
        <div className="mt-6">
          <EmptyState
            icon={Square}
            title="Задач нет"
            description="Gateway вернул пустой owner-scoped список задач."
          />
        </div>
      ) : (
        <div className="mt-6 space-y-7">
          {sections.map((section) => (
            <section key={section.title}>
              <h2 className="mb-3 text-sm font-semibold">{section.title}</h2>
              <div className="space-y-2">
                {section.items.map((item) => (
                  <button
                    aria-label={`Открыть задачу ${item.id}`}
                    className="flex min-h-[76px] w-full items-center gap-3 rounded-2xl border bg-card p-3 text-left hover:border-primary/35 focus-visible:outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
                    key={item.id}
                    onClick={() => onOpen(item.id, item.dialogId)}
                    type="button"
                  >
                    <StatusDot state={item.state} large />
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-mono text-sm font-semibold">
                        {compactID(item.id)}
                      </span>
                      <span className="mt-1 block truncate text-xs text-muted-foreground">
                        {item.issueId}@{item.expectedRevision} ·{' '}
                        {taskStatusCopy[item.state]}
                      </span>
                      <span className="mt-0.5 block truncate font-mono text-[10px] text-muted-foreground">
                        Dialog {compactID(item.dialogId)}
                      </span>
                    </span>
                    <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
                  </button>
                ))}
              </div>
            </section>
          ))}
        </div>
      )}
    </div>
  );
}

function ControlView({
  health,
  events,
}: {
  health: Resource<ControlHealth>;
  events: Resource<unknown>;
}) {
  const status =
    health.data === null ? 'unavailable' : overallHealth(health.data);
  return (
    <div className="px-4 pb-7 pt-5">
      <p className="text-xs font-medium uppercase tracking-[0.12em] text-muted-foreground">
        Только чтение
      </p>
      <h1 className="mt-1 text-2xl font-semibold tracking-[-0.035em]">
        Контроль
      </h1>

      {health.data !== null && health.error !== null ? (
        <div className="mt-6">
          <ResourceFailure
            error={health.error}
            label="Обновление Control недоступно"
          />
        </div>
      ) : null}
      {health.data === null && health.status === 'loading' ? (
        <div className="mt-6">
          <LoadingCard />
        </div>
      ) : health.data === null && health.error !== null ? (
        <div className="mt-6">
          <ResourceFailure
            error={health.error}
            label="Control readback недоступен"
          />
        </div>
      ) : health.data !== null ? (
        <>
          <Card
            className={cn(
              'mt-6 text-primary-foreground ring-0',
              healthTone(status),
            )}
          >
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                {status === 'healthy' ? <ShieldCheck /> : <ShieldAlert />}
                {healthStatusCopy(status)}
              </CardTitle>
              <CardDescription className="text-current opacity-75">
                Read-only snapshot · {formatDate(health.data.freshnessAt)}
              </CardDescription>
            </CardHeader>
          </Card>
          <div className="mt-4 space-y-2">
            {health.data.components.map((component) => (
              <div
                className="flex min-h-[68px] items-center gap-3 rounded-2xl border bg-card px-4 py-3"
                key={component.name}
              >
                <span
                  className={cn(
                    'grid size-10 shrink-0 place-items-center rounded-xl',
                    healthIconTone(component.state),
                  )}
                >
                  {component.state === 'ready' ? (
                    <CheckCircle2 className="size-5" />
                  ) : (
                    <Clock3 className="size-5" />
                  )}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="block font-mono text-sm font-semibold">
                    {component.name}
                  </span>
                  <span className="mt-0.5 block text-xs text-muted-foreground">
                    {componentStateCopy(component.state)}
                  </span>
                </span>
              </div>
            ))}
            {health.data.capacity.map((capacity) => (
              <div
                className="flex min-h-[68px] items-center gap-3 rounded-2xl border bg-card px-4 py-3"
                key={capacity.class}
              >
                <span className="grid size-10 shrink-0 place-items-center rounded-xl bg-sky-500/12 text-sky-700 dark:text-sky-300">
                  <Gauge className="size-5" />
                </span>
                <span className="min-w-0 flex-1">
                  <span className="block font-mono text-sm font-semibold">
                    capacity.{capacity.class}
                  </span>
                  <span className="mt-0.5 block text-xs text-muted-foreground">
                    in flight {capacity.inFlight} / limit {capacity.limit}
                  </span>
                </span>
              </div>
            ))}
          </div>
          <div className="mt-4 grid grid-cols-2 gap-2 text-xs">
            <Metric label="Liveness" value={health.data.liveness} mono />
            <Metric label="Readiness" value={health.data.readiness} mono />
          </div>
        </>
      ) : null}
      <p className="mt-5 rounded-2xl bg-muted px-4 py-3 text-xs leading-5 text-muted-foreground">
        Event readback:{' '}
        {events.status === 'ready'
          ? 'получен'
          : events.status === 'error'
            ? 'недоступен'
            : 'проверяется'}
        . Управляющие действия здесь отсутствуют; внешний runbook остаётся
        out-of-band каналом.
      </p>
    </div>
  );
}

function BottomNavigation({
  view,
  onNavigate,
}: {
  view: WorkspaceView;
  onNavigate: (view: WorkspaceView) => void;
}) {
  const items = [
    { id: 'dialogs' as const, label: 'Диалоги', icon: MessageCircle },
    { id: 'tasks' as const, label: 'Задачи', icon: Square },
    { id: 'control' as const, label: 'Контроль', icon: Gauge },
  ];
  return (
    <nav
      aria-label="Основная навигация"
      className="safe-bottom z-20 grid grid-cols-3 border-t bg-background/94 px-2 pt-2 backdrop-blur-xl"
    >
      {items.map((item) => {
        const Icon = item.icon;
        const active = item.id === view;
        return (
          <button
            aria-current={active ? 'page' : undefined}
            className={cn(
              'mx-1 flex min-h-14 flex-col items-center justify-center gap-1 rounded-2xl text-[11px] font-medium transition focus-visible:outline-none focus-visible:ring-3 focus-visible:ring-ring/50',
              active
                ? 'bg-primary/10 text-primary'
                : 'text-muted-foreground hover:bg-muted hover:text-foreground',
            )}
            key={item.id}
            onClick={() => onNavigate(item.id)}
            type="button"
          >
            <Icon className="size-5" aria-hidden="true" />
            {item.label}
          </button>
        );
      })}
    </nav>
  );
}

function MessageBubble({ message }: { message: DialogMessage }) {
  const owner = message.actor === 'owner';
  return (
    <article
      className={cn(
        'max-w-[88%] rounded-2xl px-4 py-3 text-sm leading-6 shadow-sm',
        owner
          ? 'ml-auto rounded-br-md bg-primary text-primary-foreground'
          : 'rounded-bl-md border bg-card',
      )}
    >
      <p className="whitespace-pre-wrap break-words">{message.content}</p>
      <footer
        className={cn(
          'mt-1 flex items-center gap-2 text-[10px]',
          owner ? 'text-primary-foreground/70' : 'text-muted-foreground',
        )}
      >
        <span>{message.actor}</span>
        <span>#{message.sequence}</span>
        <time dateTime={message.createdAt}>
          {formatDate(message.createdAt)}
        </time>
      </footer>
    </article>
  );
}

function StatusDot({
  state,
  large = false,
}: {
  state: TaskState | DialogLifecycle;
  large?: boolean;
}) {
  const tone = stateTone(state);
  return (
    <span
      aria-label={
        state in taskStatusCopy
          ? taskStatusCopy[state as TaskState]
          : lifecycleCopy[state as DialogLifecycle]
      }
      className={cn(
        'shrink-0 rounded-full ring-4 ring-current/10',
        large ? 'size-3' : 'size-2',
        tone,
      )}
    />
  );
}

function Metric({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div className="min-w-0 rounded-xl bg-background/70 p-3 ring-1 ring-border/70">
      <p className="text-[10px] font-medium uppercase tracking-[0.08em] text-muted-foreground">
        {label}
      </p>
      <p
        className={cn(
          'mt-1 truncate font-semibold',
          mono && 'font-mono text-[11px]',
        )}
      >
        {value}
      </p>
    </div>
  );
}

function EmptyState({
  icon: Icon,
  title,
  description,
}: {
  icon: typeof MessageCircle;
  title: string;
  description: string;
}) {
  return (
    <div className="rounded-2xl border border-dashed bg-card/50 px-5 py-9 text-center">
      <Icon className="mx-auto size-6 text-muted-foreground" />
      <h2 className="mt-3 text-sm font-semibold">{title}</h2>
      <p className="mx-auto mt-1 max-w-xs text-xs leading-5 text-muted-foreground">
        {description}
      </p>
    </div>
  );
}

function ResourceFailure({
  error,
  label,
}: {
  error: ApiError | null;
  label: string;
}) {
  return (
    <div className="rounded-2xl border border-destructive/25 bg-destructive/8 p-4">
      <div className="flex items-center gap-2 text-sm font-semibold text-destructive">
        <CircleAlert className="size-4" />
        {label}
      </div>
      <p className="mt-2 text-xs leading-5 text-muted-foreground">
        {errorCopy(error)}
      </p>
      {error?.requestId !== null && error?.requestId !== undefined ? (
        <p className="mt-1 truncate font-mono text-[10px] text-muted-foreground">
          request {error.requestId}
        </p>
      ) : null}
    </div>
  );
}

function InlineFailure({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-2 rounded-xl bg-amber-500/10 px-3 py-2 text-xs text-amber-800 dark:text-amber-200">
      <CircleAlert className="size-4 shrink-0" />
      {label}
    </div>
  );
}

function NoticeBanner({
  icon: Icon,
  children,
}: {
  icon: typeof CloudOff;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-start gap-2 border-b border-amber-500/25 bg-amber-500/10 px-4 py-3 text-xs leading-5 text-amber-800 dark:text-amber-200">
      <Icon className="mt-0.5 size-4 shrink-0" />
      <p>{children}</p>
    </div>
  );
}

function LoadingCards() {
  return (
    <div aria-label="Загрузка" className="space-y-3">
      <LoadingCard />
      <LoadingCard />
      <LoadingCard />
    </div>
  );
}

function LoadingCard() {
  return <div className="h-28 animate-pulse rounded-2xl border bg-muted/70" />;
}

function LoadingMessages() {
  return (
    <div aria-label="Загрузка сообщений" className="space-y-3">
      <div className="ml-auto h-20 w-4/5 animate-pulse rounded-2xl bg-primary/15" />
      <div className="h-24 w-5/6 animate-pulse rounded-2xl bg-muted" />
    </div>
  );
}

function taskSections(tasks: Task[]) {
  const definitions: Array<{
    title: string;
    matches: (task: Task) => boolean;
  }> = [
    {
      title: 'Нужно внимание',
      matches: (task) =>
        task.state === 'waiting' ||
        task.state === 'needs_review' ||
        task.cancelState === 'stop_uncertain',
    },
    {
      title: 'Активные',
      matches: (task) => task.state === 'ready' || task.state === 'running',
    },
    { title: 'Завершённые', matches: (task) => isTerminal(task.state) },
  ];
  return definitions
    .map((definition) => ({
      title: definition.title,
      items: tasks.filter(definition.matches),
    }))
    .filter((section) => section.items.length > 0);
}

function isTerminal(state: TaskState): boolean {
  return state === 'completed' || state === 'failed' || state === 'cancelled';
}

function stateTone(state: TaskState | DialogLifecycle): string {
  if (state === 'running' || state === 'ready' || state === 'active')
    return 'bg-emerald-500 text-emerald-500';
  if (state === 'waiting' || state === 'needs_review')
    return 'bg-amber-500 text-amber-500';
  if (state === 'failed' || state === 'cancelled')
    return 'bg-rose-500 text-rose-500';
  return 'bg-sky-500 text-sky-500';
}

function freshnessIndicator({
  authPhase,
  online,
  refreshing,
  stale,
  eventsReady,
}: {
  authPhase: AuthPhase;
  online: boolean;
  refreshing: boolean;
  stale: boolean;
  eventsReady: boolean;
}) {
  if (refreshing || authPhase === 'checking' || authPhase === 'authenticating')
    return {
      label: 'Readback',
      icon: LoaderCircle,
      className: 'text-muted-foreground',
    };
  if (!online)
    return {
      label: 'Offline',
      icon: CloudOff,
      className: 'text-amber-700 dark:text-amber-300',
    };
  if (authPhase !== 'authenticated')
    return {
      label: 'Auth',
      icon: LockKeyhole,
      className: 'text-amber-700 dark:text-amber-300',
    };
  if (stale || !eventsReady)
    return {
      label: 'Stale',
      icon: Clock3,
      className: 'text-amber-700 dark:text-amber-300',
    };
  return {
    label: 'Live',
    icon: Wifi,
    className: 'text-emerald-700 dark:text-emerald-300',
  };
}

function overallHealth(health: ControlHealth): HealthStatus {
  if (
    health.liveness !== 'ok' ||
    health.components.some((component) => component.state === 'unavailable')
  )
    return 'unavailable';
  if (
    health.readiness !== 'ok' ||
    health.components.some((component) => component.state === 'degraded')
  )
    return 'degraded';
  return 'healthy';
}

function healthStatusCopy(status: HealthStatus): string {
  if (status === 'healthy') return 'Контур доступен';
  if (status === 'degraded') return 'Контур деградирован';
  return 'Контур недоступен';
}

function healthTone(status: HealthStatus): string {
  if (status === 'healthy') return 'bg-primary';
  if (status === 'degraded') return 'bg-amber-700';
  return 'bg-destructive';
}

function healthIconTone(
  status: ControlHealth['components'][number]['state'],
): string {
  if (status === 'ready')
    return 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300';
  if (status === 'degraded')
    return 'bg-amber-500/14 text-amber-700 dark:text-amber-300';
  return 'bg-rose-500/12 text-rose-700 dark:text-rose-300';
}

function componentStateCopy(
  status: ControlHealth['components'][number]['state'],
): string {
  if (status === 'ready') return 'Готов';
  if (status === 'degraded') return 'Деградирован';
  return 'Недоступен';
}

function compactID(id: string): string {
  return id.length <= 24 ? id : `${id.slice(0, 10)}…${id.slice(-8)}`;
}

function relativeTime(value: string): string {
  const seconds = Math.round((Date.parse(value) - Date.now()) / 1000);
  const formatter = new Intl.RelativeTimeFormat('ru', { numeric: 'auto' });
  if (Math.abs(seconds) < 60) return formatter.format(seconds, 'second');
  const minutes = Math.round(seconds / 60);
  if (Math.abs(minutes) < 60) return formatter.format(minutes, 'minute');
  const hours = Math.round(minutes / 60);
  if (Math.abs(hours) < 24) return formatter.format(hours, 'hour');
  return formatter.format(Math.round(hours / 24), 'day');
}

function formatDate(value: string): string {
  return new Intl.DateTimeFormat('ru-RU', {
    day: '2-digit',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(value));
}

function countNoun(
  value: number,
  one: string,
  few: string,
  many: string,
): string {
  const modulo100 = value % 100;
  const modulo10 = value % 10;
  if (modulo100 >= 11 && modulo100 <= 19) return many;
  if (modulo10 === 1) return one;
  if (modulo10 >= 2 && modulo10 <= 4) return few;
  return many;
}

function errorCopy(error: ApiError | null): string {
  if (error === null) return 'Authoritative readback не получен.';
  if (error.kind === 'offline') return 'Устройство сейчас без сети.';
  if (error.kind === 'overloaded')
    return error.retryAfterSeconds === null
      ? 'Gateway исчерпал capacity. Повторите позже.'
      : `Gateway исчерпал capacity. Повторите через ${error.retryAfterSeconds} сек.`;
  if (error.kind === 'invalid_response')
    return 'Ответ Gateway не соответствует schema v1 и был отклонён.';
  if (error.kind === 'forbidden')
    return 'Текущей роли не разрешён этот readback.';
  if (error.kind === 'unauthorized') return 'Web-сессия завершена.';
  return 'Gateway или Controller не ответил. Данные не считаются актуальными.';
}
