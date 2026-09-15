import { useCallback, useEffect, useState } from 'react';
import {
  harnessAPI,
  HarnessAPIError,
  type HarnessNode,
  type HarnessSnapshot,
} from './harness-api';
import {
  inventoryAPI,
  inventoryMessage,
  type InventoryItem,
} from './agent-inventory-api';
import { APIError, type Session } from './panel-api';

type AgentState = {
  node: HarnessNode;
  inventory?: InventoryItem;
  snapshot?: HarnessSnapshot;
  error?: string;
};

type Props = {
  session: Session;
  selectedNodeId: string;
  onOpen: (nodeId: string) => void;
  onSelect?: (nodeId: string, hostId: string | null) => void;
  onExpired: () => void;
};

function safeError(cause: unknown): string {
  return cause instanceof Error
    ? cause.message
    : 'Состояние агента недоступно.';
}

const stateLabels: Record<string, string> = {
  active: 'работает',
  idle: 'свободен',
  unknown: 'неизвестно',
  online: 'на связи',
  stale: 'данные устарели',
  offline: 'не на связи',
  ready: 'готов',
  blocked: 'нужна проверка',
  busy: 'занят',
  unready: 'не готов',
  stopped: 'остановлен',
  readonly: 'только чтение',
};

const blockedReasonLabels: Record<string, string> = {
  engine_unavailable: 'движок агента недоступен',
  auth_unavailable: 'не настроен доступ агента',
  quota_exhausted: 'исчерпан лимит агента',
  policy_unavailable: 'правила запуска недоступны',
  capability_missing: 'не хватает возможности агента',
  storage_unavailable: 'хранилище недоступно',
  execution_unknown: 'состояние выполнения не подтверждено',
  adapter_protocol: 'ошибка связи с агентом',
  operator_pause: 'оператор поставил очередь на паузу',
};

const legacySnapshotConcurrency = 4;
const inventoryPollMilliseconds = 5_000;
const inventoryStaleMilliseconds = 15_000;

function ageInventory(item: InventoryItem, now: number): InventoryItem {
  if (
    item.registrationMode === 'legacy_readonly' ||
    item.observedAt === null ||
    now - Date.parse(item.observedAt) <= inventoryStaleMilliseconds ||
    item.status === 'stale'
  ) {
    return item;
  }
  return {
    ...item,
    status: 'stale',
    actions: {
      ...item.actions,
      sendMessage: {
        allowed: false,
        reason: 'observation_stale',
        nextAction: 'Обновите состояние перед отправкой сообщения.',
      },
    },
  };
}

function ageAgentStates(states: AgentState[], now: number): AgentState[] {
  let changed = false;
  const next = states.map((state) => {
    if (!state.inventory) return state;
    const inventory = ageInventory(state.inventory, now);
    if (inventory === state.inventory) return state;
    changed = true;
    return { ...state, inventory };
  });
  return changed ? next : states;
}

async function loadLegacyAgents(
  nodes: HarnessNode[],
  session: Session,
  signal: AbortSignal,
): Promise<AgentState[]> {
  const agents: AgentState[] = nodes.map((node) => ({
    node,
    error: 'Состояние агента недоступно.',
  }));
  let cursor = 0;
  let authorizationFailure: HarnessAPIError | undefined;

  async function worker() {
    while (!signal.aborted && authorizationFailure === undefined) {
      const index = cursor;
      cursor += 1;
      if (index >= nodes.length) return;
      const node = nodes[index];
      try {
        agents[index] = {
          node,
          snapshot: await harnessAPI.snapshot(session, node.nodeId, signal),
        };
      } catch (cause) {
        if (cause instanceof HarnessAPIError && cause.status === 401) {
          authorizationFailure = cause;
          return;
        }
        agents[index] = { node, error: safeError(cause) };
      }
    }
  }

  const workerCount = Math.min(legacySnapshotConcurrency, nodes.length);
  await Promise.all(Array.from({ length: workerCount }, worker));
  if (authorizationFailure) throw authorizationFailure;
  return agents;
}

function stateLabel(value: string | undefined): string {
  if (!value) return 'состояние неизвестно';
  return stateLabels[value] ?? 'состояние неизвестно';
}

export function HarnessManagement({
  session,
  selectedNodeId,
  onOpen,
  onSelect,
  onExpired,
}: Props) {
  const [agents, setAgents] = useState<AgentState[]>([]);
  const [mode, setMode] = useState<'live' | 'fixture'>('live');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [refresh, setRefresh] = useState(0);

  const reload = useCallback(() => {
    setLoading(true);
    setError('');
    setRefresh((current) => current + 1);
  }, []);

  useEffect(() => {
    const abort = new AbortController();
    if (session.inventory_enabled) {
      let reading = false;
      const readInventory = async () => {
        if (reading || abort.signal.aborted) return;
        reading = true;
        try {
          const items = await inventoryAPI.all(abort.signal);
          if (abort.signal.aborted) return;
          setMode(
            items.some((item) => item.sourceMode === 'fixture')
              ? 'fixture'
              : 'live',
          );
          setAgents(
            items.map((item) => ({
              node: {
                nodeId: item.nodeId,
                name: item.name,
                adapter: item.engine,
              },
              inventory: item,
            })),
          );
          setError('');
        } catch (cause) {
          if (abort.signal.aborted) return;
          if (cause instanceof APIError && cause.status === 401) {
            abort.abort();
            onExpired();
            return;
          }
          setAgents((current) => ageAgentStates(current, Date.now()));
          setError(inventoryMessage(cause));
        } finally {
          reading = false;
          if (!abort.signal.aborted) setLoading(false);
        }
      };
      void readInventory();
      const timer = window.setInterval(() => {
        setAgents((current) => ageAgentStates(current, Date.now()));
        void readInventory();
      }, inventoryPollMilliseconds);
      return () => {
        window.clearInterval(timer);
        abort.abort();
      };
    }
    harnessAPI
      .nodes(session, abort.signal)
      .then(async (registry) => {
        if (abort.signal.aborted) return;
        setMode(registry.mode);
        const next = await loadLegacyAgents(
          registry.nodes,
          session,
          abort.signal,
        );
        if (!abort.signal.aborted) setAgents(next);
      })
      .catch((cause) => {
        if (abort.signal.aborted) return;
        if (cause instanceof HarnessAPIError && cause.status === 401) {
          onExpired();
          return;
        }
        setError(safeError(cause));
      })
      .finally(() => {
        if (!abort.signal.aborted) setLoading(false);
      });
    return () => abort.abort();
  }, [onExpired, refresh, session]);

  return (
    <section
      className="card harness-management"
      aria-labelledby="agents-title"
      aria-busy={loading}
    >
      <div className="toolbar section-heading">
        <div className="heading-copy">
          <span className="eyebrow">Агенты</span>
          <h2 id="agents-title">Панель управления агентами</h2>
        </div>
        <button className="secondary" onClick={reload} disabled={loading}>
          {loading ? 'Обновляем…' : 'Обновить'}
        </button>
      </div>
      <p className="muted section-intro">
        Доступность, занятость и очередь агентов. Переход открывает отдельное
        рабочее место выбранного агента.
      </p>
      {mode === 'fixture' && (
        <output className="notice" aria-live="polite">
          Учебный режим: данные синтетические и не управляют реальным агентом.
        </output>
      )}
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {loading && agents.length === 0 && (
        <output className="state-panel" aria-live="polite">
          <span className="loading-indicator" aria-hidden="true" />
          Загружаем агентов…
        </output>
      )}
      {!loading && !error && agents.length === 0 && (
        <div className="empty-state">
          <span className="empty-state-mark" aria-hidden="true">
            0
          </span>
          <div>
            <strong>Нет доступных агентов</strong>
            <p>Зарегистрированных агентов нет.</p>
          </div>
        </div>
      )}
      <div className="agent-grid">
        {agents.map(({ node, inventory, snapshot, error: agentError }) => (
          <article
            className="agent-card"
            aria-current={node.nodeId === selectedNodeId ? 'true' : undefined}
            aria-label={`${node.name}, ${stateLabel(inventory?.status ?? snapshot?.node.occupancy)}`}
            data-availability={
              inventory?.status ?? snapshot?.node.transportAvailability
            }
            key={node.nodeId}
          >
            <div className="toolbar agent-card-heading">
              <div>
                <span className="eyebrow">{node.adapter}</span>
                <h3>{node.name}</h3>
              </div>
              <span
                className="tag status-pill"
                data-state={
                  inventory?.status ?? snapshot?.node.occupancy ?? 'unknown'
                }
              >
                <span className="status-dot" aria-hidden="true" />
                {stateLabel(inventory?.status ?? snapshot?.node.occupancy)}
              </span>
            </div>
            {inventory ? (
              <>
                <dl className="agent-state">
                  <div data-state={inventory.status}>
                    <dt>Состояние</dt>
                    <dd>
                      <span className="status-dot" aria-hidden="true" />
                      {stateLabel(inventory.status)}
                    </dd>
                  </div>
                  <div>
                    <dt>Хост</dt>
                    <dd>{inventory.host.name}</dd>
                  </div>
                  <div data-state={inventory.state.connection}>
                    <dt>Связь</dt>
                    <dd>{stateLabel(inventory.state.connection)}</dd>
                  </div>
                  <div data-state={inventory.state.readiness}>
                    <dt>Готовность</dt>
                    <dd>{stateLabel(inventory.state.readiness)}</dd>
                  </div>
                  <div>
                    <dt>Очередь</dt>
                    <dd>
                      {inventory.pendingCount === null
                        ? 'неизвестно'
                        : inventory.pendingCount.value}
                    </dd>
                  </div>
                  <div>
                    <dt>Диалоги</dt>
                    <dd>{inventory.dialogCount}</dd>
                  </div>
                </dl>
                {!inventory.actions.sendMessage.allowed && (
                  <output className="notice">
                    {inventory.actions.sendMessage.nextAction}
                  </output>
                )}
                <p className="muted captured-at">
                  {inventory.observedAt ? (
                    <>
                      Состояние получено:{' '}
                      <time dateTime={inventory.observedAt}>
                        {new Date(inventory.observedAt).toLocaleString('ru-RU')}
                      </time>
                      {inventory.source ? ` · ${inventory.source}` : ''}
                    </>
                  ) : (
                    'Подтверждённое наблюдение отсутствует.'
                  )}
                </p>
              </>
            ) : snapshot ? (
              <>
                <dl className="agent-state">
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
                  <div
                    data-state={snapshot.node.queuePaused ? 'paused' : 'ready'}
                  >
                    <dt>Очередь</dt>
                    <dd>
                      {snapshot.node.pendingCount}
                      {snapshot.node.queuePaused ? ' · пауза' : ''}
                    </dd>
                  </div>
                </dl>
                {snapshot.node.blockedReasons.length > 0 && (
                  <p className="notice error" role="alert">
                    {snapshot.node.blockedReasons
                      .map(
                        (reason) =>
                          blockedReasonLabels[reason] ??
                          'причина блокировки не распознана',
                      )
                      .join(', ')}
                  </p>
                )}
                <p className="muted captured-at">
                  Состояние получено:{' '}
                  <time dateTime={snapshot.capturedAt}>
                    {new Date(snapshot.capturedAt).toLocaleString('ru-RU')}
                  </time>
                </p>
              </>
            ) : (
              <p className="notice error" role="alert">
                {agentError ?? 'Состояние агента недоступно.'}
              </p>
            )}
            <div className="agent-card-actions">
              {onSelect && (
                <button
                  className="secondary"
                  onClick={() =>
                    onSelect(node.nodeId, inventory?.host.hostId ?? null)
                  }
                >
                  Выбрать для управления {node.name}
                </button>
              )}
              <button
                className="primary"
                disabled={inventory?.actions.openWorkspace.allowed === false}
                onClick={() => onOpen(node.nodeId)}
              >
                Перейти к агенту {node.name}
              </button>
            </div>
          </article>
        ))}
      </div>
    </section>
  );
}
