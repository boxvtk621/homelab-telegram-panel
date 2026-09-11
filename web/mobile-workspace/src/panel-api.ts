export type Session = {
  user: { id: string; login: string; name: string };
  csrf: string;
  writes_enabled: boolean;
};

export class APIError extends Error {
  constructor(
    public status: number,
    public code: string,
  ) {
    super(code);
  }
}

export async function api<T>(
  path: string,
  options?: { body?: unknown; csrf?: string; signal?: AbortSignal },
): Promise<T> {
  let response: Response;
  try {
    const endpoint = new URL(`api/v2/${path}`, window.location.href);
    response = await fetch(endpoint.pathname + endpoint.search, {
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
  } catch (cause) {
    if (options?.signal?.aborted) throw cause;
    throw new APIError(0, 'network_unavailable');
  }
  let value: unknown;
  try {
    const text = await response.text();
    if (new TextEncoder().encode(text).length > 64 * 1024)
      throw new Error('response_too_large');
    value = JSON.parse(text);
  } catch {
    throw new APIError(response.status, 'invalid_response');
  }
  if (!response.ok) {
    const code =
      object(value) && typeof value.error === 'string'
        ? value.error
        : 'invalid_response';
    throw new APIError(response.status, code);
  }
  if (!validResponse(path, value, response.status))
    throw new APIError(response.status, 'invalid_response');
  return value as T;
}

const object = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);
const bounded = (value: unknown, maximum = 256): value is string =>
  typeof value === 'string' &&
  value.length > 0 &&
  new TextEncoder().encode(value).length <= maximum;
const actorID = (value: unknown) =>
  bounded(value, 128) && /^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$/.test(value);
const nonce = (value: unknown) =>
  typeof value === 'string' &&
  value.length === 43 &&
  /^[A-Za-z0-9_-]{43}$/.test(value);

function validSession(value: unknown): boolean {
  if (!object(value) || Object.keys(value).length !== 3) return false;
  if (!object(value.user) || Object.keys(value.user).length !== 3) return false;
  return (
    actorID(value.user.id) &&
    bounded(value.user.login) &&
    bounded(value.user.name) &&
    nonce(value.csrf) &&
    typeof value.writes_enabled === 'boolean'
  );
}

function validResponse(path: string, value: unknown, status: number): boolean {
  if (status !== 200) return false;
  const route = path.split('?')[0];
  if (route === 'session' || route === 'bootstrap') return validSession(value);
  if (route === 'logout')
    return object(value) &&
      Object.keys(value).length === 1 &&
      value.logged_out === true;
  return true;
}

export function message(cause: unknown): string {
  if (!(cause instanceof APIError)) return 'Не удалось подключить Panel.';
  switch (cause.code) {
    case 'edge_authentication_required':
      return 'Доступ к Panel не подтверждён внешним контуром.';
    case 'capacity_exhausted':
    case 'bootstrap_rate_limited':
      return 'Panel временно занята. Повторите подключение позже.';
    case 'network_unavailable':
      return 'Panel недоступна по сети.';
    default:
      return 'Не удалось открыть рабочее место.';
  }
}
