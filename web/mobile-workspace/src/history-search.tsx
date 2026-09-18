import { Archive, Clock3, Database, Search } from 'lucide-react';
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type SyntheticEvent,
} from 'react';
import {
  readHistoryEntry,
  searchHistory,
  validHistoryLink,
  type HistoryEntryLookup,
  type HistorySearchItem,
  type HistorySearchPage,
  type HistorySearchQuery,
} from './history-search-api';
import { APIError } from './panel-api';
import { SafeMarkdown } from './safe-markdown';

type ArchiveFilter = 'all' | 'active' | 'archived';

function dateLabel(value: string): string {
  return new Intl.DateTimeFormat('ru-RU', {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value));
}

function lagLabel(milliseconds: number): string {
  if (milliseconds < 1_000) return 'менее секунды';
  if (milliseconds < 60_000)
    return `${Math.max(1, Math.round(milliseconds / 1_000))} сек`;
  return `${Math.max(1, Math.round(milliseconds / 60_000))} мин`;
}

function searchError(cause: unknown): string {
  if (!(cause instanceof APIError)) return 'Поиск истории недоступен.';
  switch (cause.status) {
    case 401:
      return 'Сессия Panel завершена.';
    case 410:
      return 'Снимок поиска истёк. Запустите поиск заново.';
    case 503:
      return 'Реплика истории временно недоступна.';
    default:
      return cause.code === 'invalid_history_link'
        ? 'Ссылка на сохранённую запись некорректна.'
        : 'Не удалось прочитать сохранённую историю.';
  }
}

function readLink(): { logicalDialogId: string; entryId: string } | null {
  const query = new URL(window.location.href).searchParams;
  const logicalDialogId = query.get('historyDialog') ?? '';
  const entryId = query.get('historyEntry') ?? '';
  return validHistoryLink(logicalDialogId, entryId)
    ? { logicalDialogId, entryId }
    : null;
}

export function hasHistoryDeepLink(): boolean {
  return readLink() !== null;
}

function writeLink(logicalDialogId: string, entryId: string) {
  const target = new URL(window.location.href);
  target.searchParams.set('historyDialog', logicalDialogId);
  target.searchParams.set('historyEntry', entryId);
  window.history.pushState(
    null,
    '',
    target.pathname + target.search + target.hash,
  );
}

function entryContent(entry: HistoryEntryLookup) {
  const content = entry.entry.content;
  if (content.kind === 'inline') {
    return entry.entry.role === 'assistant' ? (
      <SafeMarkdown markdown={content.content} />
    ) : (
      <p className="history-entry-text">{content.content}</p>
    );
  }
  if (content.kind === 'artifact') {
    return (
      <p className="notice">
        В записи сохранена ссылка на вложение. Байты файла не входят в поисковую
        реплику.
      </p>
    );
  }
  return (
    <p className="notice">
      Содержимое этой записи недоступно в безопасной проекции.
    </p>
  );
}

export function HistorySearch({
  currentNodeId,
  onExpired,
}: {
  currentNodeId: string;
  onExpired: () => void;
}) {
  const [draft, setDraft] = useState('');
  const [archiveFilter, setArchiveFilter] = useState<ArchiveFilter>('all');
  const [scopedNodeId, setScopedNodeId] = useState('');
  const [submitted, setSubmitted] = useState<HistorySearchQuery | null>(null);
  const [page, setPage] = useState<HistorySearchPage | null>(null);
  const [items, setItems] = useState<HistorySearchItem[]>([]);
  const [searching, setSearching] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState('');
  const [selected, setSelected] = useState<HistoryEntryLookup | null>(null);
  const [selectedTitle, setSelectedTitle] = useState('');
  const [entryLoading, setEntryLoading] = useState(false);
  const [entryError, setEntryError] = useState('');
  const searchAbort = useRef<AbortController | null>(null);
  const entryAbort = useRef<AbortController | null>(null);
  const scopeCurrent = currentNodeId !== '' && scopedNodeId === currentNodeId;

  const loadEntry = useCallback(
    async (logicalDialogId: string, entryId: string, title = '') => {
      entryAbort.current?.abort();
      const abort = new AbortController();
      entryAbort.current = abort;
      setEntryLoading(true);
      setEntryError('');
      setSelected(null);
      setSelectedTitle(title);
      try {
        const result = await readHistoryEntry(
          logicalDialogId,
          entryId,
          abort.signal,
        );
        if (!abort.signal.aborted) setSelected(result);
      } catch (cause) {
        if (abort.signal.aborted) return;
        if (cause instanceof APIError && cause.status === 401) onExpired();
        setEntryError(
          cause instanceof APIError && cause.status === 404
            ? 'Запись удалена или больше не доступна этому владельцу.'
            : searchError(cause),
        );
      } finally {
        if (!abort.signal.aborted) setEntryLoading(false);
      }
    },
    [onExpired],
  );

  useEffect(() => {
    const openLink = () => {
      const link = readLink();
      if (link) void loadEntry(link.logicalDialogId, link.entryId);
    };
    openLink();
    window.addEventListener('popstate', openLink);
    return () => {
      window.removeEventListener('popstate', openLink);
      searchAbort.current?.abort();
      entryAbort.current?.abort();
    };
  }, [loadEntry]);

  const runSearch = async (query: HistorySearchQuery) => {
    searchAbort.current?.abort();
    const abort = new AbortController();
    searchAbort.current = abort;
    setSearching(true);
    setError('');
    setPage(null);
    setItems([]);
    try {
      const result = await searchHistory(query, null, abort.signal);
      if (abort.signal.aborted) return;
      setSubmitted(query);
      setPage(result);
      setItems(result.items);
    } catch (cause) {
      if (abort.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onExpired();
      setError(searchError(cause));
    } finally {
      if (!abort.signal.aborted) setSearching(false);
    }
  };

  const submit = (event: SyntheticEvent<HTMLFormElement>) => {
    event.preventDefault();
    const q = draft.trim();
    if (q === '') return;
    const query: HistorySearchQuery = {
      q,
      ...(scopeCurrent && currentNodeId ? { nodeId: currentNodeId } : {}),
      ...(archiveFilter === 'all'
        ? {}
        : { archived: archiveFilter === 'archived' }),
    };
    void runSearch(query);
  };

  const loadMore = async () => {
    if (!submitted || !page?.nextCursor || loadingMore) return;
    const abort = new AbortController();
    searchAbort.current?.abort();
    searchAbort.current = abort;
    setLoadingMore(true);
    setError('');
    try {
      const next = await searchHistory(
        submitted,
        page.nextCursor,
        abort.signal,
      );
      if (abort.signal.aborted) return;
      setItems((current) => [...current, ...next.items]);
      setPage(next);
    } catch (cause) {
      if (abort.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onExpired();
      setError(searchError(cause));
    } finally {
      if (!abort.signal.aborted) setLoadingMore(false);
    }
  };

  return (
    <section className="history-search" aria-labelledby="history-search-title">
      <header className="history-search-heading">
        <span className="history-search-mark" aria-hidden="true">
          <Database size={18} />
        </span>
        <div>
          <span className="eyebrow">PostgreSQL replica</span>
          <h1 id="history-search-title">Поиск в сохранённой истории</h1>
          <p>
            Заголовки, сообщения и безопасный текст инструментов. Агент может
            быть выключен или перенесён.
          </p>
        </div>
      </header>

      <search>
        <form className="history-search-form" onSubmit={submit}>
          <label className="history-query">
            <span>Запрос</span>
            <span className="history-query-control">
              <Search aria-hidden="true" size={17} />
              <input
                value={draft}
                maxLength={1000}
                onChange={(event) => setDraft(event.currentTarget.value)}
                placeholder={'слова AND или "точная фраза"'}
                autoComplete="off"
              />
            </span>
          </label>
          <label>
            <span>Диалоги</span>
            <select
              value={archiveFilter}
              onChange={(event) =>
                setArchiveFilter(event.currentTarget.value as ArchiveFilter)
              }
            >
              <option value="all">Активные и архивные</option>
              <option value="active">Только активные</option>
              <option value="archived">Только архивные</option>
            </select>
          </label>
          <label className="history-scope-current">
            <input
              type="checkbox"
              checked={scopeCurrent}
              disabled={currentNodeId === ''}
              onChange={(event) =>
                setScopedNodeId(
                  event.currentTarget.checked ? currentNodeId : '',
                )
              }
            />
            <span>
              {currentNodeId === ''
                ? 'Текущий агент не выбран'
                : 'Только текущий агент'}
            </span>
          </label>
          <button
            className="primary"
            disabled={searching || draft.trim() === ''}
          >
            {searching ? 'Ищем…' : 'Найти'}
          </button>
        </form>
      </search>

      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}

      <div className="history-search-body">
        <section className="history-results" aria-label="Результаты поиска">
          {searching && (
            <output className="state-panel" aria-live="polite">
              <span className="loading-indicator" aria-hidden="true" />
              Ищем в сохранённой истории…
            </output>
          )}
          {!searching && page && (
            <output className="history-coverage">
              <span>
                Найдено в доступной реплике: <strong>{page.totalCount}</strong>
              </span>
              <span>
                <Clock3 aria-hidden="true" size={14} />
                Срез {dateLabel(page.observedAt)} · лаг{' '}
                {lagLabel(page.lagMillis)}
              </span>
              {page.incomplete && (
                <span className="notice warning">
                  Реплика неполная: новые или недоступные записи могут не войти
                  в результат.
                </span>
              )}
            </output>
          )}
          {!searching && page && items.length === 0 && (
            <div className="empty-state compact">
              <span className="empty-state-mark" aria-hidden="true">
                ···
              </span>
              <p>В доступном срезе совпадений нет.</p>
            </div>
          )}
          <ol className="history-result-list">
            {items.map((item) => (
              <li key={`${item.logicalDialogId}:${item.entryId}`}>
                <button
                  className="history-result"
                  onClick={() => {
                    writeLink(item.logicalDialogId, item.entryId);
                    void loadEntry(
                      item.logicalDialogId,
                      item.entryId,
                      item.title,
                    );
                  }}
                  aria-label={`Открыть сохранённую запись: ${item.title || 'Диалог без названия'}`}
                  aria-current={
                    selected?.entry.entryId === item.entryId &&
                    selected.entry.logicalDialogId === item.logicalDialogId
                      ? 'true'
                      : undefined
                  }
                >
                  <span className="history-result-meta">
                    <strong>{item.title || 'Диалог без названия'}</strong>
                    <span>{item.role === 'user' ? 'Вы' : 'Агент'}</span>
                    <time dateTime={item.createdAt}>
                      {dateLabel(item.createdAt)}
                    </time>
                  </span>
                  <span className="history-result-snippet">
                    {item.snippet || 'Совпадение в заголовке диалога'}
                  </span>
                </button>
              </li>
            ))}
          </ol>
          {page?.nextCursor && (
            <button
              className="history-load-more"
              onClick={() => void loadMore()}
              disabled={loadingMore}
            >
              {loadingMore ? 'Загружаем…' : 'Показать ещё'}
            </button>
          )}
        </section>

        <section className="history-entry" aria-label="Сохранённая запись">
          {entryLoading && (
            <output className="state-panel" aria-live="polite">
              <span className="loading-indicator" aria-hidden="true" />
              Открываем сохранённую запись…
            </output>
          )}
          {entryError && (
            <div className="history-entry-state">
              <Archive aria-hidden="true" size={22} />
              <p className="notice error" role="alert">
                {entryError}
              </p>
            </div>
          )}
          {!entryLoading && !entryError && !selected && (
            <div className="history-entry-state">
              <Archive aria-hidden="true" size={22} />
              <h2>Сохранённая запись</h2>
              <p>
                Выберите результат. Открытие читает реплику по stable ID и не
                запускает агент и не создаёт команду.
              </p>
            </div>
          )}
          {selected && (
            <article
              className="history-entry-card"
              data-role={selected.entry.role}
            >
              <header>
                <div>
                  <span className="eyebrow">Сохранённый диалог</span>
                  <h2>{selectedTitle || 'Запись из истории'}</h2>
                </div>
                <span className="history-entry-author">
                  {selected.entry.role === 'user' ? 'Вы' : 'Агент'}
                </span>
              </header>
              <time dateTime={selected.entry.createdAt}>
                {dateLabel(selected.entry.createdAt)}
              </time>
              <div className="history-entry-content">
                {entryContent(selected)}
              </div>
              <footer className="history-entry-coverage">
                <span>Срез {dateLabel(selected.observedAt)}</span>
                <span>Лаг {lagLabel(selected.lagMillis)}</span>
                {selected.incomplete && (
                  <span className="notice warning">Покрытие неполное</span>
                )}
              </footer>
            </article>
          )}
        </section>
      </div>
    </section>
  );
}
