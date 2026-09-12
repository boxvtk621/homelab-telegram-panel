import { useCallback, useEffect, useState } from 'react';
import {
  harnessAPI,
  HarnessAPIError,
  type HarnessNode,
  type HarnessSnapshot,
} from './harness-api';
import type { Session } from './panel-api';

type AgentState = {
  node: HarnessNode;
  snapshot?: HarnessSnapshot;
  error?: string;
};

type Props = {
  session: Session;
  selectedNodeId: string;
  onOpen: (nodeId: string) => void;
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

function stateLabel(value: string | undefined): string {
  if (!value) return 'состояние неизвестно';
  return stateLabels[value] ?? 'состояние неизвестно';
}

export function HarnessManagement({
  session,
  selectedNodeId,
  onOpen,
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
    harnessAPI
      .nodes(session, abort.signal)
      .then(async (registry) => {
        if (abort.signal.aborted) return;
        setMode(registry.mode);
        const next = await Promise.all(
          registry.nodes.map(async (node): Promise<AgentState> => {
            try {
              return {
                node,
                snapshot: await harnessAPI.snapshot(
                  session,
                  node.nodeId,
                  abort.signal,
                ),
              };
            } catch (cause) {
              if (cause instanceof HarnessAPIError && cause.status === 401) {
                throw cause;
              }
              return { node, error: safeError(cause) };
            }
          }),
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
        {agents.map(({ node, snapshot, error: agentError }) => (
          <article
            className="agent-card"
            aria-current={node.nodeId === selectedNodeId ? 'true' : undefined}
            aria-label={`${node.name}, ${stateLabel(snapshot?.node.occupancy)}`}
            data-availability={snapshot?.node.transportAvailability}
            key={node.nodeId}
          >
            <div className="toolbar agent-card-heading">
              <div>
                <span className="eyebrow">{node.adapter}</span>
                <h3>{node.name}</h3>
              </div>
              <span
                className="tag status-pill"
                data-state={snapshot?.node.occupancy ?? 'unknown'}
              >
                <span className="status-dot" aria-hidden="true" />
                {stateLabel(snapshot?.node.occupancy)}
              </span>
            </div>
            {snapshot ? (
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
            <button className="primary" onClick={() => onOpen(node.nodeId)}>
              Перейти к агенту {node.name}
            </button>
          </article>
        ))}
      </div>
    </section>
  );
}
