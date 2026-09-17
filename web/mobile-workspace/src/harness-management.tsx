import { Plus, RefreshCw, Search } from 'lucide-react';
import { useCallback, useEffect, useMemo, useState } from 'react';
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
import { ConfigurationEditor } from './configuration-editor';
import { ExternalEnrollment } from './external-enrollment';

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
  running: 'запущен',
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

function engineLabel(value: string): string {
  if (value === 'cursor') return 'Cursor';
  if (value === 'codex') return 'Codex';
  return value;
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
  const [query, setQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState('all');
  const [configurationNodeId, setConfigurationNodeId] = useState<string>();
  const [enrollmentOpen, setEnrollmentOpen] = useState(false);

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

  const selectedAgent = agents.find(
    ({ node }) => node.nodeId === selectedNodeId,
  );
  const activeConfigurationNodeId =
    configurationNodeId === selectedNodeId ? configurationNodeId : undefined;
  const visibleAgents = useMemo(() => {
    const normalizedQuery = query.trim().toLocaleLowerCase('ru-RU');
    return agents.filter(({ node, inventory, snapshot }) => {
      const status = inventory?.status ?? snapshot?.node.occupancy ?? 'unknown';
      if (statusFilter !== 'all' && status !== statusFilter) return false;
      if (!normalizedQuery) return true;
      return [node.name, node.adapter, inventory?.host.name]
        .filter(Boolean)
        .some((value) =>
          String(value).toLocaleLowerCase('ru-RU').includes(normalizedQuery),
        );
    });
  }, [agents, query, statusFilter]);

  return (
    <section
      className="harness-management"
      aria-labelledby="harness-management-title"
      aria-busy={loading}
    >
      <aside className="management-rail-context" aria-label="Harness-инстансы">
        <div className="rail-context-heading">
          <strong>Harness-инстансы</strong>
          <button
            type="button"
            aria-label="Добавить существующий Harness"
            title={session.enrollment_enabled ? 'Добавить существующий Harness' : 'Подключение Harness не настроено'}
            disabled={!session.enrollment_enabled}
            onClick={() => setEnrollmentOpen(true)}
          >
            <Plus aria-hidden="true" size={15} />
          </button>
        </div>
        <div className="rail-harness-list">
          {agents.slice(0, 4).map(({ node, inventory, snapshot }) => {
            const status =
              inventory?.status ?? snapshot?.node.occupancy ?? 'unknown';
            return (
              <button
                className="rail-harness-item"
                aria-current={
                  node.nodeId === selectedNodeId ? 'true' : undefined
                }
                aria-label={`Выбрать Harness ${node.name} в реестре`}
                key={node.nodeId}
                onClick={() => {
                  setConfigurationNodeId(undefined);
                  onSelect?.(node.nodeId, inventory?.host.hostId ?? null);
                }}
              >
                <span
                  className="status-dot"
                  data-state={status}
                  aria-hidden="true"
                />
                <span>{node.name}</span>
              </button>
            );
          })}
        </div>
        <button
          className="rail-add-harness"
          type="button"
          disabled={!session.enrollment_enabled}
          title={session.enrollment_enabled ? 'Добавить существующий Harness' : 'Подключение Harness не настроено'}
          onClick={() => setEnrollmentOpen(true)}
        >
          <Plus aria-hidden="true" size={15} />
          Добавить Harness
        </button>
      </aside>
      <header className="management-context-row">
        <div className="context-title">
          <h1 id="harness-management-title" aria-label="Harness / Инстансы">
            Harness <span aria-hidden="true">/</span> Инстансы
          </h1>
        </div>
        {mode === 'fixture' && (
          <output
            className="context-mode-note"
            aria-label="Учебные данные: реальный Harness не изменяется"
            aria-live="polite"
          >
            Макет · демонстрационные данные
          </output>
        )}
      </header>
      <ExternalEnrollment
        open={enrollmentOpen}
        session={session}
        onClose={() => setEnrollmentOpen(false)}
        onExpired={onExpired}
        onSuccess={() => {
          setEnrollmentOpen(false);
          reload();
        }}
      />
      {selectedAgent &&
      activeConfigurationNodeId === selectedAgent.node.nodeId &&
      selectedAgent.inventory ? (
        <ConfigurationEditor
          key={activeConfigurationNodeId}
          session={session}
          nodeId={selectedAgent.node.nodeId}
          nodeName={selectedAgent.node.name}
          hostId={selectedAgent.inventory.host.hostId}
          onBack={() => setConfigurationNodeId(undefined)}
          onExpired={onExpired}
        />
      ) : null}
      {!activeConfigurationNodeId && error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {!activeConfigurationNodeId && loading && agents.length === 0 && (
        <output className="state-panel management-state" aria-live="polite">
          <span className="loading-indicator" aria-hidden="true" />
          Загружаем реестр Harness…
        </output>
      )}
      {!activeConfigurationNodeId && !loading && !error && agents.length === 0 && (
        <div className="empty-state management-state">
          <span className="empty-state-mark" aria-hidden="true">
            0
          </span>
          <div>
            <strong>Нет Harness-инстансов</strong>
            <p>Зарегистрированных Harness пока нет.</p>
          </div>
        </div>
      )}
      {!activeConfigurationNodeId && agents.length > 0 && (
        <div className="management-layout">
          <section className="registry-pane" aria-labelledby="registry-title">
            <div className="management-toolbar">
              <h2 className="visually-hidden" id="registry-title">
                Реестр Harness
              </h2>
              <label className="management-search" htmlFor="harness-search">
                <Search aria-hidden="true" size={16} />
                <input
                  id="harness-search"
                  type="search"
                  value={query}
                  placeholder="Найти инстанс"
                  onChange={(event) => setQuery(event.target.value)}
                />
              </label>
              <select
                aria-label="Фильтр состояния Harness"
                value={statusFilter}
                onChange={(event) => setStatusFilter(event.target.value)}
              >
                <option value="all">Все состояния</option>
                <option value="online">На связи</option>
                <option value="busy">Занят</option>
                <option value="unready">Не готов</option>
                <option value="stale">Данные устарели</option>
                <option value="stopped">Остановлен</option>
                <option value="unknown">Неизвестно</option>
                <option value="readonly">Только чтение</option>
              </select>
              <button
                className="secondary compact-action icon-action"
                aria-label="Обновить реестр Harness"
                onClick={reload}
                disabled={loading}
              >
                <RefreshCw aria-hidden="true" size={16} />
                <span className="visually-hidden">
                  {loading ? 'Обновляем…' : 'Обновить'}
                </span>
              </button>
              <button
                className="secondary compact-action management-add-action"
                type="button"
                disabled={!session.enrollment_enabled}
                title={session.enrollment_enabled ? 'Добавить существующий Harness' : 'Подключение Harness не настроено'}
                onClick={() => setEnrollmentOpen(true)}
              >
                <Plus aria-hidden="true" size={15} />
                Добавить
              </button>
            </div>
            <div className="registry-columns" aria-hidden="true">
              <span>Harness</span>
              <span>Движок</span>
              <span>Хост</span>
              <span>Состояние</span>
              <span>Очередь</span>
              <span>Действие</span>
            </div>
            <ul className="agent-registry" aria-label="Реестр Harness">
              {visibleAgents.map(
                ({ node, inventory, snapshot, error: agentError }) => {
                  const status =
                    inventory?.status ?? snapshot?.node.occupancy ?? 'unknown';
                  const host = inventory?.host.name ?? '—';
                  const queue = inventory
                    ? (inventory.pendingCount?.value ?? '—')
                    : (snapshot?.node.pendingCount ?? '—');
                  return (
                    <li
                      className="agent-row"
                      aria-current={
                        node.nodeId === selectedNodeId ? 'true' : undefined
                      }
                      data-availability={status}
                      key={node.nodeId}
                    >
                      <button
                        className="agent-row-select"
                        aria-label={`Выбрать Harness ${node.name} для управления, ${stateLabel(status)}`}
                        onClick={() =>
                          onSelect?.(
                            node.nodeId,
                            inventory?.host.hostId ?? null,
                          )
                        }
                      >
                        <span
                          className="agent-cell agent-name"
                          data-label="Harness"
                        >
                          <strong>{node.name}</strong>
                          {agentError && !inventory && !snapshot && (
                            <small>{agentError}</small>
                          )}
                        </span>
                        <span
                          className="agent-cell agent-engine"
                          data-label="Движок"
                        >
                          {engineLabel(node.adapter)}
                        </span>
                        <span
                          className="agent-cell agent-host"
                          data-label="Хост"
                        >
                          {host}
                        </span>
                        <span
                          className="agent-cell row-status"
                          data-label="Состояние"
                          data-state={status}
                        >
                          <span className="status-dot" aria-hidden="true" />
                          {stateLabel(status)}
                        </span>
                        <span
                          className="agent-cell agent-queue"
                          data-label="Очередь"
                        >
                          {queue}
                        </span>
                      </button>
                      <button
                        className="agent-row-open"
                        aria-label={`Открыть диалог Harness ${node.name}`}
                        disabled={
                          inventory?.actions.openWorkspace.allowed === false
                        }
                        onClick={() => onOpen(node.nodeId)}
                      >
                        Диалог
                      </button>
                    </li>
                  );
                },
              )}
              {visibleAgents.length === 0 && (
                <li className="registry-filter-empty">
                  По текущему фильтру инстансы не найдены.
                </li>
              )}
            </ul>
            <footer className="registry-summary">
              <span>{visibleAgents.length} инстансов</span>
              <span>
                Для URI действия зависят от возможностей удалённого Harness.
              </span>
            </footer>
            {mode === 'fixture' && (
              <section
                className="management-events"
                aria-labelledby="management-events-title"
              >
                <div className="management-event-tabs">
                  <h3 id="management-events-title">События управления</h3>
                </div>
                <ol>
                  <li>
                    <time dateTime="2026-09-15T09:14:00Z">09:14</time>
                    <span className="status-dot" data-state="online" />
                    <span>Media remote — подключение добавлено</span>
                  </li>
                  <li>
                    <time dateTime="2026-09-15T09:10:00Z">09:10</time>
                    <span className="status-dot" data-state="stopped" />
                    <span>Codex alpha — остановлен</span>
                  </li>
                  <li>
                    <time dateTime="2026-09-15T09:06:00Z">09:06</time>
                    <span className="status-dot" data-state="online" />
                    <span>Cursor alpha — запущен</span>
                  </li>
                </ol>
                <p>Демонстрационные события</p>
              </section>
            )}
          </section>
          <aside
            className="management-inspector"
            aria-label="Инспектор выбранного Harness"
          >
            {selectedAgent ? (
              <>
                <div className="inspector-heading">
                  <div>
                    <h2>{selectedAgent.node.name}</h2>
                    <p className="muted">
                      {engineLabel(selectedAgent.node.adapter)}
                      {selectedAgent.inventory
                        ? ` · ${selectedAgent.inventory.host.name}`
                        : ''}
                    </p>
                  </div>
                  <span
                    className="tag status-pill"
                    data-state={
                      selectedAgent.inventory?.status ??
                      selectedAgent.snapshot?.node.occupancy ??
                      'unknown'
                    }
                  >
                    <span className="status-dot" aria-hidden="true" />
                    {stateLabel(
                      selectedAgent.inventory?.status ??
                        selectedAgent.snapshot?.node.occupancy,
                    )}
                  </span>
                </div>
                {selectedAgent.inventory ? (
                  <>
                    <dl className="inspector-state">
                      <div data-state={selectedAgent.inventory.state.process}>
                        <dt>Процесс</dt>
                        <dd>
                          {stateLabel(selectedAgent.inventory.state.process)}
                        </dd>
                      </div>
                      <div
                        data-state={selectedAgent.inventory.state.connection}
                      >
                        <dt>Связь</dt>
                        <dd>
                          {stateLabel(selectedAgent.inventory.state.connection)}
                        </dd>
                      </div>
                      <div data-state={selectedAgent.inventory.state.readiness}>
                        <dt>Готовность</dt>
                        <dd>
                          {stateLabel(selectedAgent.inventory.state.readiness)}
                        </dd>
                      </div>
                      <div data-state={selectedAgent.inventory.state.occupancy}>
                        <dt>Занятость</dt>
                        <dd>
                          {stateLabel(selectedAgent.inventory.state.occupancy)}
                        </dd>
                      </div>
                      <div>
                        <dt>Очередь</dt>
                        <dd>
                          {selectedAgent.inventory.pendingCount?.value ??
                            'неизвестно'}
                        </dd>
                      </div>
                      <div>
                        <dt>Диалоги</dt>
                        <dd>{selectedAgent.inventory.dialogCount}</dd>
                      </div>
                    </dl>
                    {!selectedAgent.inventory.actions.sendMessage.allowed && (
                      <output className="notice">
                        {selectedAgent.inventory.actions.sendMessage.nextAction}
                      </output>
                    )}
                    <p className="muted captured-at">
                      {selectedAgent.inventory.observedAt ? (
                        <>
                          Получено:{' '}
                          <time dateTime={selectedAgent.inventory.observedAt}>
                            {new Date(
                              selectedAgent.inventory.observedAt,
                            ).toLocaleString('ru-RU')}
                          </time>
                          {selectedAgent.inventory.source
                            ? ` · ${selectedAgent.inventory.source}`
                            : ''}
                        </>
                      ) : (
                        'Подтверждённое наблюдение отсутствует.'
                      )}
                    </p>
                    <p className="muted inspector-boundary">
                      {selectedAgent.inventory.actions.lifecycle.allowed
                        ? 'Доступные lifecycle-действия определяет Harness.'
                        : selectedAgent.inventory.actions.lifecycle.nextAction}
                    </p>
                    <details className="technical-details">
                      <summary>Технические сведения</summary>
                      <code>nodeId: {selectedAgent.node.nodeId}</code>
                      <code>hostId: {selectedAgent.inventory.host.hostId}</code>
                      <span>
                        registration: {selectedAgent.inventory.registrationMode}
                      </span>
                    </details>
                    <button
                      type="button"
                      className="secondary inspector-open"
                      onClick={() =>
                        setConfigurationNodeId(selectedAgent.node.nodeId)
                      }
                    >
                      Открыть конфигурацию
                    </button>
                  </>
                ) : selectedAgent.snapshot ? (
                  <>
                    <dl className="inspector-state">
                      <div
                        data-state={
                          selectedAgent.snapshot.node.transportAvailability
                        }
                      >
                        <dt>Доступность</dt>
                        <dd>
                          {stateLabel(
                            selectedAgent.snapshot.node.transportAvailability,
                          )}
                        </dd>
                      </div>
                      <div
                        data-state={selectedAgent.snapshot.node.engineReadiness}
                      >
                        <dt>Готовность</dt>
                        <dd>
                          {stateLabel(
                            selectedAgent.snapshot.node.engineReadiness,
                          )}
                        </dd>
                      </div>
                      <div data-state={selectedAgent.snapshot.node.occupancy}>
                        <dt>Занятость</dt>
                        <dd>
                          {stateLabel(selectedAgent.snapshot.node.occupancy)}
                        </dd>
                      </div>
                      <div>
                        <dt>Очередь</dt>
                        <dd>{selectedAgent.snapshot.node.pendingCount}</dd>
                      </div>
                    </dl>
                    {selectedAgent.snapshot.node.blockedReasons.length > 0 && (
                      <p className="notice error" role="alert">
                        {selectedAgent.snapshot.node.blockedReasons
                          .map(
                            (reason) =>
                              blockedReasonLabels[reason] ??
                              'причина блокировки не распознана',
                          )
                          .join(', ')}
                      </p>
                    )}
                    <p className="muted captured-at">
                      Получено:{' '}
                      <time dateTime={selectedAgent.snapshot.capturedAt}>
                        {new Date(
                          selectedAgent.snapshot.capturedAt,
                        ).toLocaleString('ru-RU')}
                      </time>
                    </p>
                  </>
                ) : (
                  <p className="notice error" role="alert">
                    {selectedAgent.error ?? 'Состояние Harness недоступно.'}
                  </p>
                )}
                <button
                  className="primary inspector-open"
                  aria-label={`Открыть диалог Harness ${selectedAgent.node.name}`}
                  disabled={
                    selectedAgent.inventory?.actions.openWorkspace.allowed ===
                    false
                  }
                  onClick={() => onOpen(selectedAgent.node.nodeId)}
                >
                  Открыть диалог
                </button>
              </>
            ) : (
              <div className="inspector-empty">
                <span className="empty-state-mark" aria-hidden="true">
                  H
                </span>
                <h2>Выберите Harness</h2>
                <p className="muted">
                  Инспектор покажет подтверждённое состояние и доступный переход
                  к диалогу.
                </p>
              </div>
            )}
          </aside>
        </div>
      )}
    </section>
  );
}
