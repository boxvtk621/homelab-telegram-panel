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
      <header>
        <div>
          <span className="eyebrow">HOMELAB</span>
          <h1>Рабочее место AI-агентов</h1>
        </div>
      </header>
      {error && (
        <main className="card login">
          <p role="alert" className="notice error">
            {error}
          </p>
          <button className="primary" onClick={recover}>
            Повторить подключение
          </button>
        </main>
      )}
      {booting && !error && <output>Подключаем рабочее место…</output>}
      {!booting && session && (
        <Workspace
          key={session.csrf}
          session={session}
          onExpired={recover}
        />
      )}
      <footer>Panel → Router → выбранный Harness</footer>
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
    <main>
      <div className="identity">
        <span>{session.user.name || session.user.login}</span>
        <span className="tag">Harness workspace</span>
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
        <div hidden={view !== 'interaction'}>
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
