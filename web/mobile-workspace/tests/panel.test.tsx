import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Panel } from '../src/panel';
import { api } from '../src/panel-api';

it('keeps API calls under the current path mount', async () => {
  const fetcher = vi.fn((_url: string) => json(session));
  vi.stubGlobal('fetch', fetcher);
  window.history.replaceState(null, '', '/panel/');
  try {
    await api('session');
    expect(fetcher.mock.calls[0]?.[0]).toBe('/panel/api/v2/session');
    window.history.replaceState(null, '', '/panel/index.html');
    await api('session');
    expect(fetcher.mock.calls[1]?.[0]).toBe('/panel/api/v2/session');
  } finally {
    window.history.replaceState(null, '', '/');
    vi.unstubAllGlobals();
  }
});

const session = {
  user: { id: '1-1', login: 'owner', name: 'Owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
  youtrack_url: 'https://youtrack.example.test',
  project: 'HL',
};
const issue = {
  id: '2-1',
  idReadable: 'HL-210',
  project: { id: '0-1', shortName: 'HL' },
  summary: 'Точная задача',
  description: '<script>alert(1)</script>',
  updated: 1000,
  customFields: [{ name: 'State', value: { name: 'В обработке' } }],
};
const permit = 'p'.repeat(43);
function snapshot(data: unknown) {
  return { data, observed_at: '2026-09-08T00:00:00Z' };
}
function json(value: unknown, status = 200) {
  return Promise.resolve(
    new Response(JSON.stringify(value), {
      status,
      headers: { 'Content-Type': 'application/json' },
    }),
  );
}
function fixture(
  extra?: (
    path: string,
    options?: RequestInit,
  ) => Promise<Response> | undefined,
) {
  const fetcher = vi.fn(
    (url: string, options?: RequestInit): Promise<Response> => {
      const path = url;
      const custom = extra?.(path, options);
      if (custom) return custom;
      if (path === '/api/v2/session') return json(session);
      if (path === '/api/v2/agent/runs')
        return json({ runs: [], durable: false, model: 'synthetic' });
      if (path === '/api/v2/issues?skip=0')
        return json({ data: [issue], observed_at: '2026-09-08T00:00:00Z' });
      if (path === '/api/v2/issues/HL-210') return json(snapshot(issue));
      if (path === '/api/v2/issues/HL-210/comments?skip=0')
        return json(snapshot([]));
      if (path === '/api/v2/articles?skip=0') return json(snapshot([]));
      if (path === '/api/v2/permits') return json({ permit });
      if (path === '/api/v2/issues/HL-210/comments') {
        if (typeof options?.body !== 'string')
          throw new Error('missing JSON body');
        const input = JSON.parse(options.body) as { text: string };
        return json(
          {
            data: {
              id: '7-1',
              text: input.text,
              created: 1000,
              deleted: false,
              author: { name: 'Owner' },
            },
            recorded_in: 'youtrack',
            execution_accepted: false,
          },
          201,
        );
      }
      if (path === '/api/v2/logout') return json({ logged_out: true });
      throw new Error(`unexpected endpoint ${path}`);
    },
  );
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('independent YouTrack panel', () => {
  it('runs the task-bound AI flow, recovers a lost ACK without another POST and copies the result as a draft', async () => {
    let result: unknown[] = [];
    const fetcher = fixture((path, options) => {
      if (path !== '/api/v2/agent/runs') return undefined;
      if (options?.method === 'POST') {
        if (typeof options.body !== 'string')
          throw new Error('missing JSON body');
        const input = JSON.parse(options.body);
        expect(input.issue_id).toBe('HL-210');
        expect(input.expected_updated).toBe(1000);
        result = [
          {
            id: input.command_id,
            issue_id: input.issue_id,
            prompt: input.prompt,
            parent_id: '',
            status: 'finished',
            stage: 'writing_answer',
            result: 'Synthetic SDK answer',
            error: '',
            observed_at: '2026-09-08T10:00:00Z',
          },
        ];
        return Promise.reject(new TypeError('synthetic lost ACK'));
      }
      return json({ runs: result, durable: false, model: 'synthetic' });
    });
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    const prompt = await screen.findByLabelText('Вопрос агенту по HL-210');
    fireEvent.change(prompt, { target: { value: 'Synthetic question' } });
    fireEvent.click(screen.getByRole('button', { name: 'Спросить Cursor' }));
    await screen.findByText('Synthetic SDK answer');
    await waitFor(() =>
      expect(
        (
          screen.getByLabelText(
            'Вопрос агенту по HL-210',
          ) as HTMLTextAreaElement
        ).value,
      ).toBe(''),
    );
    expect(
      fetcher.mock.calls.filter(
        ([url, opts]) =>
          url === '/api/v2/agent/runs' && opts?.method === 'POST',
      ),
    ).toHaveLength(1);
    fireEvent.change(screen.getByLabelText('Комментарий в HL-210'), {
      target: { value: 'Existing draft' },
    });
    fireEvent.click(
      screen.getByRole('button', { name: 'В черновик комментария' }),
    );
    expect(
      (screen.getByLabelText('Комментарий в HL-210') as HTMLTextAreaElement)
        .value,
    ).toContain('Existing draft');
    expect(
      (screen.getByLabelText('Комментарий в HL-210') as HTMLTextAreaElement)
        .value,
    ).toContain('Synthetic SDK answer');
    expect(
      fetcher.mock.calls.some(
        ([url, opts]) =>
          url === '/api/v2/issues/HL-210/comments' && opts?.method === 'POST',
      ),
    ).toBe(false);
  });
  it('releases definite AI rejection without discarding the question', async () => {
    fixture((path, options) =>
      path === '/api/v2/agent/runs' && options?.method === 'POST'
        ? json({ error: 'context_unavailable' }, 409)
        : undefined,
    );
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    const prompt = await screen.findByLabelText('Вопрос агенту по HL-210');
    fireEvent.change(prompt, { target: { value: 'Keep this question' } });
    fireEvent.click(screen.getByRole('button', { name: 'Спросить Cursor' }));
    await waitFor(() =>
      expect(
        (
          screen.getByRole('button', {
            name: 'Спросить Cursor',
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    expect((prompt as HTMLTextAreaElement).value).toBe('Keep this question');
    expect(
      screen.queryByRole('button', { name: 'Повторить тот же запрос' }),
    ).toBeNull();
  });
  it('opens without Telegram and never invokes legacy APIs', async () => {
    const fetcher = fixture();
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    await screen.findByText('Обсуждение HL-210');
    expect(screen.getByText('<script>alert(1)</script>')).toBeDefined();
    expect(document.querySelector('script')).toBeNull();
    expect(
      fetcher.mock.calls.every(([url]) => url.startsWith('/api/v2/')),
    ).toBe(true);
  });
  it('uses personal YouTrack login, clears input, and never stores the token', async () => {
    const fetcher = fixture((path) =>
      path === '/api/v2/session'
        ? json({ error: 'authentication_required' }, 401)
        : path === '/api/v2/login'
          ? json(session)
          : undefined,
    );
    const stored = vi.spyOn(Storage.prototype, 'setItem');
    render(<Panel />);
    const field = await screen.findByLabelText(
      'Персональный API-токен YouTrack',
    );
    fireEvent.change(field, {
      target: { value: 'perm:synthetic-no-real-credential' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Войти' }));
    await screen.findByText('Задачи YouTrack');
    expect(stored).not.toHaveBeenCalled();
    expect(
      fetcher.mock.calls.filter(([url]) => url === '/api/v2/login'),
    ).toHaveLength(1);
  });
  it('posts only to the displayed issue after a single-use permit', async () => {
    const fetcher = fixture();
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    fireEvent.change(await screen.findByLabelText('Комментарий в HL-210'), {
      target: { value: 'Точный ответ' },
    });
    await waitFor(() =>
      expect(
        (
          screen.getByRole('button', {
            name: 'Отправить в HL-210',
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Отправить в HL-210' }));
    await screen.findByText(/Комментарий сохранён в HL-210/);
    const posts = fetcher.mock.calls.filter(
      ([, options]) => options?.method === 'POST',
    );
    expect(posts.map(([url]) => url)).toEqual([
      '/api/v2/permits',
      '/api/v2/issues/HL-210/comments',
    ]);
    const body = posts[1]?.[1]?.body;
    if (typeof body !== 'string') throw new Error('expected JSON body');
    expect(JSON.parse(body)).toEqual({ permit, text: 'Точный ответ' });
  });
  it('does not retry or claim execution after a lost write response', async () => {
    const fetcher = fixture((path) =>
      path === '/api/v2/issues/HL-210/comments'
        ? Promise.reject(new TypeError('synthetic network loss'))
        : undefined,
    );
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    fireEvent.change(await screen.findByLabelText('Комментарий в HL-210'), {
      target: { value: 'Не дублировать' },
    });
    await waitFor(() =>
      expect(
        (
          screen.getByRole('button', {
            name: 'Отправить в HL-210',
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Отправить в HL-210' }));
    await screen.findByText(/Повтор заблокирован/);
    expect(
      (
        screen.getByRole('button', {
          name: 'Отправить в HL-210',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(
      fetcher.mock.calls.filter(
        ([url]) => url === '/api/v2/issues/HL-210/comments',
      ),
    ).toHaveLength(1);
  });
  it('reads knowledge articles without interpreting their content as HTML', async () => {
    const article = {
      id: '226-1',
      idReadable: 'HL-A-23',
      project: issue.project,
      summary: 'Каноническая инструкция',
      content: '<img src=x onerror=alert(1)>',
      updated: 1000,
    };
    fixture((path) =>
      path === '/api/v2/articles?skip=0'
        ? json(snapshot([article]))
        : path === '/api/v2/articles/HL-A-23'
          ? json(snapshot(article))
          : undefined,
    );
    render(<Panel />);
    await screen.findByText('Задачи YouTrack');
    fireEvent.click(screen.getByRole('button', { name: 'База знаний' }));
    fireEvent.click(
      await screen.findByRole('button', {
        name: /HL-A-23 Каноническая инструкция/,
      }),
    );
    await screen.findByText('<img src=x onerror=alert(1)>');
    expect(document.querySelector('img')).toBeNull();
  });
  it('rejects a malformed successful ACK without clearing the draft', async () => {
    fixture((path) =>
      path === '/api/v2/issues/HL-210/comments' ? json({}, 201) : undefined,
    );
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    const field = await screen.findByLabelText('Комментарий в HL-210');
    fireEvent.change(field, { target: { value: 'Сохранить черновик' } });
    await waitFor(() =>
      expect(
        (
          screen.getByRole('button', {
            name: 'Отправить в HL-210',
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Отправить в HL-210' }));
    await screen.findByText(/Повтор заблокирован/);
    expect((field as HTMLTextAreaElement).value).toBe('Сохранить черновик');
    expect(screen.queryByText(/Комментарий сохранён/)).toBeNull();
  });
  it('retains pending and unknown state across navigation during POST', async () => {
    let rejectWrite: (e: Error) => void = () => {
      throw new Error('POST not started');
    };
    const pending = new Promise<Response>((_resolve, reject) => {
      rejectWrite = reject;
    });
    const fetcher = fixture((path) =>
      path === '/api/v2/issues/HL-210/comments' ? pending : undefined,
    );
    render(<Panel />);
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    fireEvent.change(await screen.findByLabelText('Комментарий в HL-210'), {
      target: { value: 'Не повторять после перехода' },
    });
    await waitFor(() =>
      expect(
        (
          screen.getByRole('button', {
            name: 'Отправить в HL-210',
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Отправить в HL-210' }));
    await waitFor(() =>
      expect(
        fetcher.mock.calls.some(
          ([p]) => p === '/api/v2/issues/HL-210/comments',
        ),
      ).toBe(true),
    );
    fireEvent.click(screen.getByRole('button', { name: 'База знаний' }));
    rejectWrite(new Error('lost ACK'));
    await screen.findByText(/HL-210: результат отправки требует проверки/);
    fireEvent.click(screen.getByRole('button', { name: 'Задачи' }));
    fireEvent.click(
      await screen.findByRole('button', { name: /HL-210 Точная задача/ }),
    );
    await screen.findByText(/Повтор заблокирован/);
    expect(
      (
        screen.getByRole('button', {
          name: 'Отправить в HL-210',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(
      fetcher.mock.calls.filter(
        ([p]) => p === '/api/v2/issues/HL-210/comments',
      ),
    ).toHaveLength(1);
  });
});
