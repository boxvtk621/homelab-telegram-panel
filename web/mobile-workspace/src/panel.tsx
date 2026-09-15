import { LogOut, MessageSquare, Moon, Server, Sun } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { HarnessManagement } from './harness-management';
import { HarnessWorkspace } from './harness-workspace';
import { api, APIError, message, type Session } from './panel-api';
import {
  clearPanelSessionState,
  readPanelSessionState,
  updatePanelSessionState,
  type DialogBinding,
  type ManagementSelection,
  type PanelTheme,
  type PanelView,
} from './panel-session-state';

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
  const [initial] = useState(() => readPanelSessionState(session.user.id));
  const [interactionNodeId, setInteractionNodeId] = useState(
    initial.state.interactionNodeId,
  );
  const [interaction, setInteraction] = useState<DialogBinding | null>(
    initial.state.interaction,
  );
  const [management, setManagement] = useState<ManagementSelection | null>(
    initial.state.management,
  );
  const [view, setView] = useState<PanelView>(initial.state.view);
  const [theme, setTheme] = useState<PanelTheme>(initial.state.theme);
  const [loggingOut, setLoggingOut] = useState(false);
  const sessionActive = useRef(true);

  const save = useCallback(
    (update: Parameters<typeof updatePanelSessionState>[1]) =>
      updatePanelSessionState(session.user.id, update),
    [session.user.id],
  );

  const changeView = useCallback(
    (next: PanelView) => {
      setView(next);
      save((current) => ({ ...current, view: next }));
    },
    [save],
  );

  const expire = useCallback(() => {
    sessionActive.current = false;
    clearPanelSessionState(session.user.id);
    onExpired();
  }, [onExpired, session.user.id]);

  const isSessionActive = useCallback(() => sessionActive.current, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
  }, [theme]);

  const workspaceOpen = interactionNodeId !== '';

  return (
    <div className="panel-app" data-view={view}>
      <aside className="panel-rail" aria-label="Навигация Panel">
        <div className="rail-brand">
          <span className="brand-mark" aria-hidden="true">
            H
          </span>
          <strong>HomeLab</strong>
        </div>
        <nav className="panel-sections" aria-label="Основные разделы">
          <button
            aria-label="Открыть раздел общения"
            aria-current={view === 'interaction' ? 'page' : undefined}
            disabled={!workspaceOpen}
            onClick={() => changeView('interaction')}
          >
            <MessageSquare aria-hidden="true" size={16} />
            <span className="rail-label">Общение</span>
          </button>
          <button
            aria-label="Открыть управление Harness"
            aria-current={view === 'management' ? 'page' : undefined}
            onClick={() => changeView('management')}
          >
            <Server aria-hidden="true" size={16} />
            <span className="rail-label">Harness</span>
          </button>
        </nav>
        <div className="rail-account" aria-label="Текущая сессия">
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
          <div className="rail-actions">
            <button
              aria-label={
                theme === 'dark'
                  ? 'Включить светлую тему'
                  : 'Включить тёмную тему'
              }
              onClick={() => {
                const next = theme === 'dark' ? 'light' : 'dark';
                setTheme(next);
                save((current) => ({ ...current, theme: next }));
              }}
            >
              {theme === 'dark' ? (
                <Sun aria-hidden="true" size={16} />
              ) : (
                <Moon aria-hidden="true" size={16} />
              )}
              <span className="rail-action-label">Тема</span>
            </button>
            <button
              aria-label="Выйти из Panel"
              disabled={loggingOut}
              onClick={() => {
                sessionActive.current = false;
                setLoggingOut(true);
                clearPanelSessionState(session.user.id);
                void api('logout', { body: {}, csrf: session.csrf })
                  .catch(() => undefined)
                  .finally(expire);
              }}
            >
              <LogOut aria-hidden="true" size={16} />
              <span className="rail-action-label">
                {loggingOut ? 'Выходим…' : 'Выйти'}
              </span>
            </button>
          </div>
        </div>
      </aside>
      <main className="panel-main" data-view={view} id="panel-main">
        <div className="management-stage" hidden={view !== 'management'}>
          <HarnessManagement
            session={session}
            onExpired={expire}
            selectedNodeId={management?.nodeId ?? ''}
            onSelect={(nodeId, hostId) => {
              const next = { nodeId, hostId };
              setManagement(next);
              save((current) => ({ ...current, management: next }));
            }}
            onOpen={(nodeId) => {
              setInteractionNodeId(nodeId);
              setInteraction((current) =>
                current?.nodeId === nodeId ? current : null,
              );
              setView('interaction');
              save((current) => ({
                ...current,
                view: 'interaction',
                interactionNodeId: nodeId,
                interaction:
                  current.interaction?.nodeId === nodeId
                    ? current.interaction
                    : null,
              }));
            }}
          />
        </div>
        {workspaceOpen && (
          <div className="workspace-stage" hidden={view !== 'interaction'}>
            <HarnessWorkspace
              session={session}
              onExpired={expire}
              selectedNodeId={interactionNodeId}
              selectedDialog={interaction}
              isSessionActive={isSessionActive}
              onSelectionChange={(next) => {
                setInteractionNodeId(next.nodeId);
                setInteraction(next);
                save((current) => ({
                  ...current,
                  interactionNodeId: next.nodeId,
                  interaction: next,
                }));
              }}
              onBack={() => changeView('management')}
            />
          </div>
        )}
      </main>
    </div>
  );
}
