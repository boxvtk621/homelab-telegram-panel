import { useCallback, useEffect, useState } from 'react';
import { HarnessManagement } from './harness-management';
import { HarnessWorkspace } from './harness-workspace';
import { api, APIError, message, type Session } from './panel-api';

export function Panel() {
  const [session, setSession] = useState<Session | null>(null);
  const [booting, setBooting] = useState(true);
  const [error, setError] = useState('');
  const [bootstrapGeneration, setBootstrapGeneration] = useState(0);

  const recover = useCallback(() => {
    setSession(null);
    setBooting(true);
    setError('');
    setBootstrapGeneration((generation) => generation + 1);
  }, []);

  useEffect(() => {
    const abort = new AbortController();
    async function restore() {
      try {
        try {
          setSession(await api<Session>('session', { signal: abort.signal }));
        } catch (cause) {
          if (!(cause instanceof APIError && cause.status === 401)) throw cause;
          setSession(
            await api<Session>('bootstrap', {
              body: {},
              signal: abort.signal,
            }),
          );
        }
      } catch (cause) {
        if (!abort.signal.aborted) setError(message(cause));
      } finally {
        if (!abort.signal.aborted) setBooting(false);
      }
    }
    void restore();
    return () => abort.abort();
  }, [bootstrapGeneration]);

  return (
    <div className="panel-shell">
      <a className="skip-link" href="#panel-main">
        К основному содержимому
      </a>
      <header className="panel-header">
        <div className="brand-lockup">
          <span className="brand-mark" aria-hidden="true">
            H
          </span>
          <div>
            <span className="eyebrow">Управление агентами</span>
            <h1>Рабочее место агентов</h1>
          </div>
        </div>
        <span className="header-context">Диалоги и ход работы</span>
      </header>
      {error && (
        <main className="card login" aria-labelledby="connection-title">
          <span className="eyebrow">Подключение</span>
          <h2 id="connection-title">Рабочее место недоступно</h2>
          <p role="alert" className="notice error">
            {error}
          </p>
          <button className="primary" onClick={recover}>
            Повторить подключение
          </button>
        </main>
      )}
      {booting && !error && (
        <output className="state-panel" aria-live="polite">
          <span className="loading-indicator" aria-hidden="true" />
          Подключаем рабочее место…
        </output>
      )}
      {!booting && session && (
        <Workspace key={session.csrf} session={session} onExpired={recover} />
      )}
      <footer>
        <span>HomeLab</span>
        <span aria-hidden="true">·</span>
        <span>агентские диалоги</span>
      </footer>
    </div>
  );
}

function Workspace({
  session,
  onExpired,
}: {
  session: Session;
  onExpired: () => void;
}) {
  const [selectedNodeID, setSelectedNodeID] = useState('');
  const [workspaceOpen, setWorkspaceOpen] = useState(false);
  const [view, setView] = useState<'management' | 'interaction'>('management');

  return (
    <main className="panel-main" data-view={view} id="panel-main">
      <div className="identity" aria-label="Текущая сессия">
        <span className="identity-user">
          <span className="identity-avatar" aria-hidden="true">
            {(session.user.name || session.user.login)
              .slice(0, 1)
              .toUpperCase()}
          </span>
          <span>
            <small>Оператор</small>
            <strong>{session.user.name || session.user.login}</strong>
          </span>
        </span>
        <span className="tag">Рабочее место</span>
      </div>
      {view === 'management' && (
        <HarnessManagement
          session={session}
          selectedNodeId={selectedNodeID}
          onExpired={onExpired}
          onOpen={(nodeID) => {
            setSelectedNodeID(nodeID);
            setWorkspaceOpen(true);
            setView('interaction');
          }}
        />
      )}
      {workspaceOpen && (
        <div className="workspace-stage" hidden={view !== 'interaction'}>
          <HarnessWorkspace
            session={session}
            onExpired={onExpired}
            selectedNodeId={selectedNodeID}
            onBack={() => setView('management')}
          />
        </div>
      )}
    </main>
  );
}
