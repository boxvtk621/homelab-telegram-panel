import { useCallback, useEffect, useRef, useState } from 'react';
import { AgentChat } from './panel-agent';
import {
  api,
  APIError,
  fieldText,
  message,
  type Article,
  type Comment,
  type Issue,
  type Session,
  type Snapshot,
} from './panel-api';

export function Panel() {
  const [session, setSession] = useState<Session | null>(null);
  const [booting, setBooting] = useState(true);
  const [token, setToken] = useState('');
  const [authError, setAuthError] = useState('');
  const [loggingIn, setLoggingIn] = useState(false);
  const expire = useCallback(() => setSession(null), []);
  useEffect(() => {
    let live = true;
    api<Session>('session')
      .then((s) => {
        if (live) setSession(s);
      })
      .catch((e) => {
        if (live && !(e instanceof APIError && e.status === 401))
          setAuthError(message(e));
      })
      .finally(() => {
        if (live) setBooting(false);
      });
    return () => {
      live = false;
    };
  }, []);
  async function login(event: React.SyntheticEvent<HTMLFormElement>) {
    event.preventDefault();
    setLoggingIn(true);
    setAuthError('');
    const submitted = token;
    setToken('');
    try {
      setSession(await api<Session>('login', { body: { token: submitted } }));
    } catch (e) {
      setAuthError(message(e));
    } finally {
      setLoggingIn(false);
    }
  }
  async function logout() {
    try {
      await api('logout', { body: {}, csrf: session?.csrf });
      setSession(null);
    } catch (e) {
      setAuthError(message(e));
    }
  }
  return (
    <div className="panel-shell">
      <header>
        <div>
          <span className="eyebrow">HOMELAB</span>
          <h1>Рабочее место AI-агента</h1>
        </div>
        {session && <button onClick={logout}>Выйти</button>}
      </header>
      {authError && (
        <p role="alert" className="notice error">
          {authError}
        </p>
      )}
      {booting ? (
        <output>Проверяем сессию…</output>
      ) : session ? (
        <Workspace key={session.csrf} session={session} onExpired={expire} />
      ) : (
        <main className="login card">
          <h2>Независимая веб-панель</h2>
          <p>
            Задачи, обсуждения и база знаний находятся в YouTrack. Telegram и
            работающий бот для входа не нужны.
          </p>
          <form onSubmit={login}>
            <label htmlFor="access-token">
              Персональный API-токен YouTrack
            </label>
            <input
              id="access-token"
              type="password"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              required
              maxLength={4096}
            />
            <button className="primary" disabled={loggingIn}>
              {loggingIn ? 'Проверяем…' : 'Войти'}
            </button>
          </form>
          <p className="muted">
            Токен создаётся в вашем профиле YouTrack с минимально необходимыми
            правами. Это не пароль и не токен Telegram-бота. Он не сохраняется в
            браузере или на диске панели; сервер хранит его только в памяти
            текущей сессии.
          </p>
        </main>
      )}
      <footer>
        Общий источник информации — YouTrack. Запись комментария не означает,
        что агент принял или выполнил команду.
      </footer>
    </div>
  );
}

type WriteState = {
  state: 'idle' | 'pending' | 'unknown' | 'recorded';
  error?: string;
};
function Workspace({
  session,
  onExpired,
}: {
  session: Session;
  onExpired: () => void;
}) {
  const [tab, setTab] = useState<'issues' | 'articles'>('issues');
  const [items, setItems] = useState<(Issue | Article)[]>([]);
  const [skip, setSkip] = useState(0);
  const [selected, setSelected] = useState('');
  const [refresh, setRefresh] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [observed, setObserved] = useState('');
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [writes, setWrites] = useState<Record<string, WriteState>>({});
  const inflight = useRef(new Set<string>());
  const fail = useCallback(
    (e: unknown) => {
      if (e instanceof APIError && e.status === 401) onExpired();
      else setError(message(e));
    },
    [onExpired],
  );
  useEffect(() => {
    const abort = new AbortController();
    api<Snapshot<(Issue | Article)[]>>(`${tab}?skip=${skip}`, {
      signal: abort.signal,
    })
      .then((r) => {
        if (!abort.signal.aborted) {
          setItems(r.data);
          setObserved(r.observed_at);
        }
      })
      .catch((e) => {
        if (!abort.signal.aborted) fail(e);
      })
      .finally(() => {
        if (!abort.signal.aborted) setLoading(false);
      });
    return () => abort.abort();
  }, [tab, skip, refresh, fail]);
  function reload() {
    setLoading(true);
    setError('');
    setItems([]);
    setObserved('');
    setRefresh((n) => n + 1);
  }
  function navigate(next: 'issues' | 'articles') {
    setSelected('');
    setSkip(0);
    setTab(next);
    reload();
  }
  async function sendComment(id: string, text: string): Promise<boolean> {
    if (inflight.current.has(id) || writes[id]?.state === 'unknown')
      return false;
    inflight.current.add(id);
    setWrites((old) => ({ ...old, [id]: { state: 'pending' } }));
    let dispatched = false;
    try {
      const { permit } = await api<{ permit: string }>('permits', {
        body: { target: `${id}/comments` },
        csrf: session.csrf,
      });
      dispatched = true;
      await api(`issues/${id}/comments`, {
        body: { permit, text },
        csrf: session.csrf,
      });
      setDrafts((old) => (old[id] === text ? { ...old, [id]: '' } : old));
      setWrites((old) => ({ ...old, [id]: { state: 'recorded' } }));
      return true;
    } catch (e) {
      setWrites((old) => ({
        ...old,
        [id]: { state: dispatched ? 'unknown' : 'idle', error: message(e) },
      }));
      if (e instanceof APIError && e.status === 401 && !dispatched) onExpired();
      return false;
    } finally {
      inflight.current.delete(id);
    }
  }
  return (
    <main>
      <div className="identity">
        <span>
          {session.user.name || session.user.login} · {session.project}
        </span>
        <span className="tag">
          {session.writes_enabled ? 'Комментарии доступны' : 'Только чтение'}
        </span>
      </div>
      <nav aria-label="Разделы">
        <button
          aria-current={tab === 'issues' ? 'page' : undefined}
          onClick={() => navigate('issues')}
        >
          Задачи
        </button>
        <button
          aria-current={tab === 'articles' ? 'page' : undefined}
          onClick={() => navigate('articles')}
        >
          База знаний
        </button>
      </nav>
      {Object.entries(writes)
        .filter(
          ([, value]) => value.state === 'pending' || value.state === 'unknown',
        )
        .map(([id, value]) => (
          <p className="notice" key={id}>
            {id}:{' '}
            {value.state === 'pending'
              ? 'сохранение комментария…'
              : 'результат отправки требует проверки в YouTrack'}
          </p>
        ))}
      {selected ? (
        <Detail
          key={`${tab}:${selected}`}
          kind={tab}
          id={selected}
          session={session}
          draft={drafts[selected] ?? ''}
          setDraft={(value) =>
            setDrafts((old) => ({ ...old, [selected]: value }))
          }
          write={writes[selected] ?? { state: 'idle' }}
          sendComment={sendComment}
          resolveUnknown={() => {
            setDrafts((old) => ({ ...old, [selected]: '' }));
            setWrites((old) => ({ ...old, [selected]: { state: 'idle' } }));
          }}
          back={() => setSelected('')}
        />
      ) : (
        <section className="card">
          <div className="toolbar">
            <h2>{tab === 'issues' ? 'Задачи YouTrack' : 'База знаний'}</h2>
            <button disabled={loading} onClick={reload}>
              Обновить
            </button>
          </div>
          <p className="muted">
            {tab === 'issues'
              ? 'Поля и статусы из YouTrack, не состояние процесса бота.'
              : 'Канонические статьи проекта. Редактирование — в YouTrack.'}
          </p>
          {error && (
            <p className="notice error" role="alert">
              {error}
            </p>
          )}
          {loading && <output>Загружаем…</output>}
          {!loading && !error && items.length === 0 && (
            <p>На этой странице записей нет.</p>
          )}
          <div className="record-list">
            {items.map((item) => (
              <button
                className="record"
                aria-label={`${item.idReadable} ${item.summary}`}
                key={item.id}
                onClick={() => setSelected(item.idReadable)}
              >
                <span className="record-id">{item.idReadable}</span>
                <strong>{item.summary}</strong>
                <span className="muted">
                  {new Date(item.updated).toLocaleString('ru-RU')}
                </span>
              </button>
            ))}
          </div>
          {observed && (
            <p className="muted">
              Прочитано: {new Date(observed).toLocaleString('ru-RU')}
            </p>
          )}
          <div className="pager">
            <button
              disabled={skip === 0 || loading}
              onClick={() => {
                setSkip((n) => Math.max(0, n - 30));
                reload();
              }}
            >
              Назад
            </button>
            <span>Страница {skip / 30 + 1}</span>
            <button
              disabled={items.length < 30 || loading}
              onClick={() => {
                setSkip((n) => n + 30);
                reload();
              }}
            >
              Далее
            </button>
          </div>
          <a
            href={`${session.youtrack_url}/${tab === 'issues' ? 'issues' : 'articles'}/${session.project}`}
            target="_blank"
            rel="noreferrer"
          >
            {tab === 'issues'
              ? 'Создать или изменить задачу в YouTrack ↗'
              : 'Открыть базу знаний в YouTrack ↗'}
          </a>
        </section>
      )}
      <AgentChat
        issueID={tab === 'issues' ? selected : ''}
        session={session}
        onExpired={onExpired}
        onDraft={(id, text) =>
          setDrafts((old) => ({
            ...old,
            [id]: old[id] ? `${old[id]}\n\n${text}` : text,
          }))
        }
      />
    </main>
  );
}

function Detail({
  kind,
  id,
  session,
  draft,
  setDraft,
  write,
  sendComment,
  resolveUnknown,
  back,
}: {
  kind: 'issues' | 'articles';
  id: string;
  session: Session;
  draft: string;
  setDraft: (value: string) => void;
  write: WriteState;
  sendComment: (id: string, text: string) => Promise<boolean>;
  resolveUnknown: () => void;
  back: () => void;
}) {
  const [record, setRecord] = useState<Issue | Article | null>(null);
  const [comments, setComments] = useState<Comment[]>([]);
  const [skip, setSkip] = useState(0);
  const [refresh, setRefresh] = useState(0);
  const [error, setError] = useState('');
  const busy = write.state === 'pending';
  const [loading, setLoading] = useState(true);
  const uncertain = write.state === 'unknown';
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    const abort = new AbortController();
    Promise.all([
      api<Snapshot<Issue | Article>>(`${kind}/${id}`, { signal: abort.signal }),
      kind === 'issues'
        ? api<Snapshot<Comment[]>>(`issues/${id}/comments?skip=${skip}`, {
            signal: abort.signal,
          })
        : Promise.resolve(null),
    ])
      .then(([r, c]) => {
        if (!abort.signal.aborted) {
          setRecord(r.data);
          setComments(c?.data ?? []);
        }
      })
      .catch((e) => {
        if (!abort.signal.aborted) {
          setError(message(e));
          setRecord(null);
        }
      })
      .finally(() => {
        if (!abort.signal.aborted) setLoading(false);
      });
    return () => abort.abort();
  }, [id, kind, skip, refresh]);
  function reload() {
    setLoading(true);
    setError('');
    setComments([]);
    setRefresh((n) => n + 1);
  }
  async function send(event: React.SyntheticEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy || uncertain || !draft.trim()) return;
    const ok = await sendComment(id, draft);
    if (ok && alive.current) reload();
  }
  const link = `${session.youtrack_url}/${kind === 'issues' ? 'issue' : 'articles'}/${id}`;
  return (
    <section className="card">
      <div className="toolbar">
        <button disabled={busy} onClick={back}>
          ← К списку
        </button>
        <a href={link} target="_blank" rel="noreferrer">
          {id} в YouTrack ↗
        </a>
      </div>
      {(error || write.error) && (
        <p role="alert" className="notice error">
          {error || write.error}
        </p>
      )}
      {write.state === 'recorded' && (
        <output className="notice">
          Комментарий сохранён в {id}. Это не подтверждение исполнения агентом.
        </output>
      )}
      <button disabled={loading || busy} onClick={reload}>
        Обновить данные
      </button>
      {loading && <output>Загружаем {id}…</output>}
      {record && (
        <>
          <h2>{record.summary}</h2>
          {'customFields' in record && (
            <dl>
              {record.customFields.map((f) => (
                <div key={f.name}>
                  <dt>{f.name}</dt>
                  <dd>{fieldText(f.value)}</dd>
                </div>
              ))}
            </dl>
          )}
          <pre className="content">
            {'description' in record ? record.description : record.content}
          </pre>
        </>
      )}
      {kind === 'issues' && (
        <>
          <h3>Обсуждение {id}</h3>
          <p className="muted">
            Сообщения сохраняются в комментариях этой задачи. Они не меняют её
            revision и не останавливают исполнителя.
          </p>
          {comments
            .filter((c) => !c.deleted)
            .map((c) => (
              <article className="comment" key={c.id}>
                <div className="muted">
                  {c.author?.name ?? 'YouTrack'} ·{' '}
                  {new Date(c.created).toLocaleString('ru-RU')}
                </div>
                <pre className="content">{c.text}</pre>
              </article>
            ))}
          <div className="pager">
            <button
              disabled={skip === 0 || loading || busy}
              onClick={() => {
                setSkip((n) => Math.max(0, n - 30));
                reload();
              }}
            >
              Ранее
            </button>
            <span>Страница {skip / 30 + 1}</span>
            <button
              disabled={comments.length < 30 || loading || busy}
              onClick={() => {
                setSkip((n) => n + 30);
                reload();
              }}
            >
              Позднее
            </button>
          </div>
          {session.writes_enabled && (
            <form onSubmit={send}>
              <label htmlFor="comment">Комментарий в {id}</label>
              <textarea
                id="comment"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                disabled={busy}
                maxLength={65536}
                rows={5}
              />
              <button
                className="primary"
                disabled={
                  busy || loading || !record || uncertain || !draft.trim()
                }
              >
                {busy ? 'Сохраняем…' : `Отправить в ${id}`}
              </button>
            </form>
          )}
          {uncertain && (
            <div className="notice">
              <p>
                Повтор заблокирован.{' '}
                <a href={link} target="_blank" rel="noreferrer">
                  Проверьте комментарии в YouTrack
                </a>
                . Не отправляйте дубликат.
              </p>
              <button
                onClick={() => {
                  if (
                    window.confirm(
                      'Вы проверили обсуждение в YouTrack? Это очистит прежний черновик и разрешит новое сообщение, но не повторит старое.',
                    )
                  )
                    resolveUnknown();
                }}
              >
                Проверено в YouTrack — начать новое сообщение
              </button>
            </div>
          )}
        </>
      )}
    </section>
  );
}
