import { useEffect, useRef, useState } from 'react';
import {
  api,
  APIError,
  message,
  type AgentRun,
  type AgentRuns,
  type Issue,
  type Session,
  type Snapshot,
} from './panel-api';

type Submission = {
  command_id: string;
  issue_id: string;
  prompt: string;
  parent_id: string;
  expected_updated: number;
};
const stages: Record<string, string> = {
  loading_context: 'Читает задачу и инструкции',
  running: 'Агент работает',
  analyzing: 'Анализирует',
  reading_youtrack: 'Читает YouTrack',
  writing_answer: 'Готовит ответ',
};
const statuses: Record<string, string> = {
  running: 'Выполняется',
  cancel_requested: 'Останавливается',
  finished: 'Ответ готов',
  failed: 'Ошибка',
  cancelled: 'Остановлен',
};

// Kept mounted in Workspace when navigating; pending command identity and
// drafts cannot disappear just because the issue detail was closed.
export function AgentChat({
  issueID,
  session,
  onDraft,
  onExpired,
}: {
  issueID: string;
  session: Session;
  onDraft: (id: string, text: string) => void;
  onExpired: () => void;
}) {
  const [runs, setRuns] = useState<AgentRun[]>([]);
  const [model, setModel] = useState('');
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [parent, setParent] = useState<Record<string, string>>({});
  const [fresh, setFresh] = useState(false);
  const [error, setError] = useState('');
  const [pending, setPending] = useState<Submission | null>(null);
  const [sending, setSending] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const inflight = useRef(false);
  const pendingRef = useRef<Submission | null>(null);
  function setSubmission(value: Submission | null) {
    pendingRef.current = value;
    setPending(value);
  }
  function acknowledge(input: Submission) {
    setDrafts((old) => ({
      ...old,
      [input.issue_id]:
        old[input.issue_id] === input.prompt ? '' : (old[input.issue_id] ?? ''),
    }));
    setSubmission(null);
  }
  useEffect(() => {
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const reply = await api<AgentRuns>('agent/runs', {
          signal: abort.signal,
        });
        if (abort.signal.aborted) return;
        setRuns(reply.runs);
        setModel(reply.model);
        setFresh(true);
        setError('');
        const sent = pendingRef.current;
        if (
          sent &&
          reply.runs.some(
            (r) =>
              r.id === sent.command_id &&
              r.issue_id === sent.issue_id &&
              r.prompt === sent.prompt &&
              r.parent_id === sent.parent_id,
          )
        )
          acknowledge(sent);
      } catch (e) {
        if (!abort.signal.aborted) {
          setFresh(false);
          setError(message(e));
          if (e instanceof APIError && e.status === 401) onExpired();
        }
      }
      if (!abort.signal.aborted) timer = setTimeout(poll, 2500);
    };
    void poll();
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
  }, [refresh, onExpired]);
  const active = runs.some(
    (r) => r.status === 'running' || r.status === 'cancel_requested',
  );
  const shown = runs.filter((r) => r.issue_id === issueID);
  const draft = drafts[issueID] ?? '';
  async function dispatch(input: Submission) {
    setSubmission(input);
    try {
      await api<AgentRun>('agent/runs', { body: input, csrf: session.csrf });
      acknowledge(input);
    } catch (e) {
      setError(message(e));
      if (e instanceof APIError && [400, 403, 429].includes(e.status))
        setSubmission(null);
      if (e instanceof APIError && e.code === 'context_unavailable') {
        setSubmission(null);
        setParent((old) => ({ ...old, [input.issue_id]: '' }));
      }
      if (e instanceof APIError && e.status === 401) onExpired();
    } finally {
      setSending(false);
      inflight.current = false;
      setRefresh((n) => n + 1);
    }
  }
  async function send(event: React.SyntheticEvent<HTMLFormElement>) {
    event.preventDefault();
    if (inflight.current || pending || active || !fresh) return;
    inflight.current = true;
    setSending(true);
    setError('');
    try {
      const snapshot = await api<Snapshot<Issue>>(`issues/${issueID}`);
      await dispatch({
        command_id: crypto.randomUUID(),
        issue_id: issueID,
        prompt: draft,
        parent_id: parent[issueID] ?? '',
        expected_updated: snapshot.data.updated,
      });
    } catch (e) {
      setError(message(e));
      setSending(false);
      inflight.current = false;
    }
  }
  async function cancel(run: AgentRun) {
    try {
      await api(`agent/runs/${run.id}/cancel`, {
        body: {},
        csrf: session.csrf,
      });
    } catch (e) {
      setError(message(e));
    } finally {
      setRefresh((n) => n + 1);
    }
  }
  return (
    <section
      className="card agent-chat"
      hidden={!issueID}
      aria-label="Диалог с Cursor SDK"
    >
      <div className="toolbar">
        <h2>Cursor · {issueID}</h2>
        <span className="tag">{model || 'SDK'}</span>
      </div>
      <p className="muted">
        Агент читает задачу, обсуждение и базу знаний. В этом инкременте —
        анализ и ответы; изменения инфраструктуры не выполняются. Выход не
        останавливает агента. История хранится в памяти сервера до восьми часов
        после завершения; перезапуск сервиса её сбросит. В продолжение
        передаются последние три завершённых ответа.
      </p>
      {!fresh && (
        <p className="notice">
          Связь не подтверждена — новые запросы отключены.
        </p>
      )}
      {error && (
        <p role="alert" className="notice error">
          {error}
        </p>
      )}
      <div aria-live="polite">
        {shown.map((run) => (
          <article className="comment" key={run.id}>
            <p className="muted">
              {statuses[run.status]} ·{' '}
              {new Date(run.observed_at).toLocaleString('ru-RU')} · {run.id}
            </p>
            <pre className="content">{run.prompt}</pre>
            {run.status === 'running' && <output>{stages[run.stage]}</output>}
            {run.result && (
              <pre className="content agent-answer">{run.result}</pre>
            )}
            {run.error && (
              <p role="alert">{message(new APIError(503, run.error))}</p>
            )}
            {run.status === 'running' && (
              <button disabled={!fresh} onClick={() => cancel(run)}>
                Остановить {run.id.slice(0, 8)}
              </button>
            )}
            {run.status === 'finished' && (
              <div className="toolbar">
                <button
                  disabled={
                    active ||
                    !!pending ||
                    sending ||
                    new TextEncoder().encode(run.result).length > 8192
                  }
                  onClick={() =>
                    setParent((old) => ({ ...old, [issueID]: run.id }))
                  }
                >
                  Продолжить этот ответ
                </button>
                {session.writes_enabled && (
                  <button
                    onClick={() =>
                      onDraft(
                        issueID,
                        `Ответ Cursor SDK (${run.id}):\n\n${run.result}`,
                      )
                    }
                  >
                    В черновик комментария
                  </button>
                )}
              </div>
            )}
          </article>
        ))}
      </div>
      {pending && (
        <div className="notice">
          <p>
            Подтверждение запуска {pending.command_id} в {pending.issue_id} ещё
            не получено. Новый запрос заблокирован. Можно безопасно повторить
            тот же ID, пока запуск хранится на сервере.
          </p>
          <button
            disabled={sending || !fresh}
            onClick={() => {
              if (inflight.current) return;
              inflight.current = true;
              setSending(true);
              void dispatch(pending);
            }}
          >
            Повторить тот же запрос
          </button>
        </div>
      )}
      <form onSubmit={send}>
        <label htmlFor="agent-prompt">Вопрос агенту по {issueID}</label>
        <textarea
          id="agent-prompt"
          value={draft}
          onChange={(e) =>
            setDrafts((old) => ({ ...old, [issueID]: e.target.value }))
          }
          maxLength={8192}
          rows={4}
        />
        <p className="muted">
          {parent[issueID]
            ? `Продолжение ${parent[issueID]}`
            : 'Новый диалог по этой задаче'}
        </p>
        {parent[issueID] && (
          <button
            type="button"
            disabled={sending || !!pending || active}
            onClick={() => setParent((old) => ({ ...old, [issueID]: '' }))}
          >
            Новый диалог
          </button>
        )}
        <button
          className="primary"
          disabled={
            !draft.trim() ||
            new TextEncoder().encode(draft).length > 8192 ||
            !fresh ||
            active ||
            sending ||
            !!pending
          }
        >
          {sending ? 'Отправляем…' : 'Спросить Cursor'}
        </button>
      </form>
    </section>
  );
}
