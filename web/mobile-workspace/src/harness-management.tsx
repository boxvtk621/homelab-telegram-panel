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
    <section className="card harness-management" aria-labelledby="agents-title">
      <div className="toolbar">
        <div>
          <span className="eyebrow">CONTROL</span>
          <h2 id="agents-title">Панель управления агентами</h2>
        </div>
        <button onClick={reload} disabled={loading}>
          {loading ? 'Обновляем…' : 'Обновить'}
        </button>
      </div>
      <p className="muted">
        Доступность, занятость и очередь агентов. Переход открывает отдельное
        рабочее место выбранного агента.
      </p>
      {mode === 'fixture' && (
        <output className="notice">
          Fixture режим: данные синтетические и не управляют реальным агентом.
        </output>
      )}
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {loading && agents.length === 0 && <output>Загружаем агентов…</output>}
      {!loading && !error && agents.length === 0 && (
        <p>Зарегистрированных агентов нет.</p>
      )}
      <div className="agent-grid">
        {agents.map(({ node, snapshot, error: agentError }) => (
          <article
            className="agent-card"
            aria-current={node.nodeId === selectedNodeId ? 'true' : undefined}
            key={node.nodeId}
          >
            <div className="toolbar">
              <div>
                <span className="eyebrow">{node.adapter}</span>
                <h3>{node.name}</h3>
              </div>
              <span className="tag">
                {snapshot?.node.occupancy ?? 'unknown'}
              </span>
            </div>
            {snapshot ? (
              <>
                <dl className="agent-state">
                  <div>
                    <dt>Доступность</dt>
                    <dd>{snapshot.node.transportAvailability}</dd>
                  </div>
                  <div>
                    <dt>Готовность</dt>
                    <dd>{snapshot.node.engineReadiness}</dd>
                  </div>
                  <div>
                    <dt>Очередь</dt>
                    <dd>
                      {snapshot.node.pendingCount}
                      {snapshot.node.queuePaused ? ' · пауза' : ''}
                    </dd>
                  </div>
                </dl>
                {snapshot.node.blockedReasons.length > 0 && (
                  <p className="notice error">
                    {snapshot.node.blockedReasons.join(', ')}
                  </p>
                )}
                <p className="muted">
                  Состояние получено:{' '}
                  {new Date(snapshot.capturedAt).toLocaleString('ru-RU')}
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
