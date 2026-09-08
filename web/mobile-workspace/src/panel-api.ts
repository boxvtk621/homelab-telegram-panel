export type Session = {
  user: { login: string; name: string };
  csrf: string;
  writes_enabled: boolean;
  youtrack_url: string;
  project: string;
};
export type Issue = {
  id: string;
  idReadable: string;
  summary: string;
  description: string;
  updated: number;
  customFields: { name: string; value: unknown }[];
};
export type Article = {
  id: string;
  idReadable: string;
  summary: string;
  content: string;
  updated: number;
};
export type Comment = {
  id: string;
  text: string;
  created: number;
  deleted: boolean;
  author: { name: string } | null;
};
export type Snapshot<T> = { data: T; observed_at: string };
export type AgentRun = {
  id: string;
  issue_id: string;
  prompt: string;
  parent_id: string;
  status: 'running' | 'cancel_requested' | 'finished' | 'failed' | 'cancelled';
  stage: string;
  result: string;
  error: string;
  observed_at: string;
};
export type AgentRuns = { runs: AgentRun[]; durable: false; model: string };
const uuid = (v: unknown) =>
  typeof v === 'string' &&
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
    v,
  );
function agentRun(v: unknown): boolean {
  return (
    object(v) &&
    uuid(v.id) &&
    string(v.issue_id, 64) &&
    /^[A-Z][A-Z0-9_]*-[1-9][0-9]*$/.test(v.issue_id) &&
    string(v.prompt, 8192) &&
    (v.parent_id === '' || uuid(v.parent_id)) &&
    ['running', 'cancel_requested', 'finished', 'failed', 'cancelled'].includes(
      String(v.status),
    ) &&
    [
      'loading_context',
      'running',
      'analyzing',
      'reading_youtrack',
      'writing_answer',
    ].includes(String(v.stage)) &&
    string(v.result, 65536) &&
    (v.status === 'finished' ? v.result.length > 0 : v.result === '') &&
    string(v.error, 128) &&
    string(v.observed_at, 64) &&
    Number.isFinite(Date.parse(v.observed_at))
  );
}

export class APIError extends Error {
  constructor(
    public status: number,
    public code: string,
  ) {
    super(code);
  }
}

// Mutations deliberately have no retry policy. A lost response is NOT proof
// that YouTrack did not record the comment.
export async function api<T>(
  path: string,
  options?: { body?: unknown; csrf?: string; signal?: AbortSignal },
): Promise<T> {
  const mutation = options?.body !== undefined && path.endsWith('/comments');
  let response: Response;
  try {
    response = await fetch(`/api/v2/${path}`, {
      method: options?.body === undefined ? 'GET' : 'POST',
      credentials: 'same-origin',
      cache: 'no-store',
      redirect: 'error',
      headers:
        options?.body === undefined
          ? {}
          : {
              'Content-Type': 'application/json',
              ...(options?.csrf ? { 'X-Panel-CSRF': options.csrf } : {}),
            },
      body:
        options?.body === undefined ? undefined : JSON.stringify(options.body),
      signal: options?.signal,
    });
  } catch (error) {
    if (options?.signal?.aborted) throw error;
    throw new APIError(
      0,
      mutation ? 'write_outcome_unknown' : 'network_unavailable',
    );
  }
  let value: unknown;
  try {
    const text = await response.text();
    if (new TextEncoder().encode(text).length > 2 * 1024 * 1024)
      throw new Error('response_too_large');
    value = JSON.parse(text);
  } catch {
    throw new APIError(
      response.status,
      mutation ? 'write_outcome_unknown' : 'invalid_response',
    );
  }
  if (!response.ok) {
    const code =
      typeof value === 'object' &&
      value !== null &&
      'error' in value &&
      typeof value.error === 'string'
        ? value.error
        : 'invalid_response';
    throw new APIError(response.status, code);
  }
  if (!validResponse(path, value, response.status, options?.body))
    throw new APIError(
      response.status,
      mutation ? 'write_outcome_unknown' : 'invalid_response',
    );
  return value as T;
}

const object = (v: unknown): v is Record<string, unknown> =>
  typeof v === 'object' && v !== null && !Array.isArray(v);
const string = (v: unknown, max = 1024 * 1024): v is string =>
  typeof v === 'string' && new TextEncoder().encode(v).length <= max;
const integer = (v: unknown): v is number =>
  typeof v === 'number' && Number.isSafeInteger(v) && v >= 0;
const databaseID = (v: unknown) => string(v, 64) && /^\d+-\d+$/.test(v);
const nonce = (v: unknown) => string(v, 43) && /^[A-Za-z0-9_-]{43}$/.test(v);
const project = (v: unknown) =>
  object(v) &&
  databaseID(v.id) &&
  string(v.shortName, 32) &&
  /^[A-Z][A-Z0-9_]*$/.test(v.shortName);
function record(v: unknown, article: boolean): boolean {
  if (
    !object(v) ||
    !databaseID(v.id) ||
    !string(v.idReadable, 64) ||
    !string(v.summary, 65536) ||
    !integer(v.updated) ||
    !project(v.project)
  )
    return false;
  if (article)
    return (
      /^[A-Z][A-Z0-9_]*-A-[1-9][0-9]*$/.test(v.idReadable) && string(v.content)
    );
  return (
    /^[A-Z][A-Z0-9_]*-[1-9][0-9]*$/.test(v.idReadable) &&
    string(v.description) &&
    Array.isArray(v.customFields) &&
    v.customFields.length <= 100 &&
    v.customFields.every(
      (f) => object(f) && string(f.name, 256) && 'value' in f,
    )
  );
}
function comment(v: unknown): boolean {
  return (
    object(v) &&
    databaseID(v.id) &&
    string(v.text) &&
    integer(v.created) &&
    typeof v.deleted === 'boolean' &&
    object(v.author) &&
    string(v.author.name, 1024)
  );
}
function validResponse(
  path: string,
  v: unknown,
  status: number,
  body: unknown,
): boolean {
  const route = path.split('?')[0] ?? '';
  if (route.endsWith('/comments') && body !== undefined) {
    return (
      status === 201 &&
      object(v) &&
      v.recorded_in === 'youtrack' &&
      v.execution_accepted === false &&
      comment(v.data) &&
      object(v.data) &&
      v.data.deleted === false &&
      object(body) &&
      v.data.text === body.text
    );
  }
  if (status !== 200 || !object(v)) return false;
  if (route === 'agent/runs') {
    if (body === undefined)
      return (
        v.durable === false &&
        string(v.model, 128) &&
        Array.isArray(v.runs) &&
        v.runs.length <= 16 &&
        v.runs.every(agentRun) &&
        new Set(v.runs.map((r: AgentRun) => r.id)).size === v.runs.length
      );
    return (
      agentRun(v) &&
      object(body) &&
      v.id === body.command_id &&
      v.issue_id === body.issue_id &&
      v.prompt === body.prompt &&
      v.parent_id === body.parent_id
    );
  }
  if (route.startsWith('agent/runs/') && route.endsWith('/cancel'))
    return agentRun(v) && v.id === route.split('/')[2];
  if (route === 'session' || route === 'login') {
    if (
      !object(v.user) ||
      !databaseID(v.user.id) ||
      !string(v.user.login, 256) ||
      !string(v.user.name, 1024) ||
      !nonce(v.csrf) ||
      typeof v.writes_enabled !== 'boolean' ||
      !string(v.project, 32) ||
      !/^[A-Z][A-Z0-9_]*$/.test(v.project) ||
      !string(v.youtrack_url, 2048)
    )
      return false;
    try {
      const url = new URL(v.youtrack_url);
      return (
        url.protocol === 'https:' &&
        url.origin === v.youtrack_url &&
        !url.username &&
        !url.password
      );
    } catch {
      return false;
    }
  }
  if (route === 'logout') return v.logged_out === true;
  if (route === 'permits') return nonce(v.permit);
  if (!string(v.observed_at, 64) || !Number.isFinite(Date.parse(v.observed_at)))
    return false;
  if (route.endsWith('/comments'))
    return (
      Array.isArray(v.data) && v.data.length <= 30 && v.data.every(comment)
    );
  const article = route.startsWith('articles');
  if (route === 'articles' || route === 'issues')
    return (
      Array.isArray(v.data) &&
      v.data.length <= 30 &&
      v.data.every((item) => record(item, article))
    );
  return (
    record(v.data, article) &&
    object(v.data) &&
    v.data.idReadable === route.split('/')[1]
  );
}

export function message(error: unknown): string {
  if (!(error instanceof APIError))
    return 'Не удалось получить подтверждённый ответ. Обновите данные.';
  if (error.status === 401) return 'Сессия истекла. Войдите заново.';
  const agentErrors: Record<string, string> = {
    cursor_not_configured:
      'Cursor SDK не настроен: нужен отдельный API-ключ панели.',
    cursor_run_failed:
      'Cursor SDK завершился с ошибкой. Повторный запуск не выполнялся.',
    issue_changed: 'Задача изменилась. Обновите её перед новым запросом.',
    instructions_unavailable: 'Не удалось прочитать инструкции из базы знаний.',
    agent_capacity_exhausted: 'Агент занят или достигнут лимит истории сессии.',
    context_unavailable:
      'Этот контекст недоступен или слишком велик. Начните новый диалог.',
    agent_timeout: 'Достигнут предел времени запуска (10 минут).',
  };
  if (agentErrors[error.code]) return agentErrors[error.code];
  if (error.code === 'write_outcome_unknown')
    return 'Ответ потерян или разрешение уже использовано. Комментарий мог сохраниться. Проверьте обсуждение в YouTrack перед новой отправкой.';
  if (error.code === 'access_denied')
    return 'Доступ не разрешён. Проверьте пользователя и права токена в YouTrack.';
  if (error.status === 429)
    return 'Слишком много запросов. Попробуйте позднее.';
  if (error.code === 'invalid_request')
    return 'Запрос отклонён: проверьте введённый текст.';
  return 'YouTrack или сеть недоступны. Показанные данные могут быть устаревшими.';
}

export function fieldText(value: unknown): string {
  if (typeof value === 'string' || typeof value === 'number')
    return String(value);
  if (
    value &&
    typeof value === 'object' &&
    'name' in value &&
    typeof value.name === 'string'
  )
    return value.name;
  return '—';
}
