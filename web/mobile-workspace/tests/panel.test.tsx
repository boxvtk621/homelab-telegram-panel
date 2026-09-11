import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Panel } from '../src/panel';
import { api } from '../src/panel-api';

const session = {
  user: { id: 'owner-1', login: 'owner', name: 'owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
};

function json(value: unknown, status = 200) {
  return Promise.resolve(
    new Response(JSON.stringify(value), {
      status,
      headers: { 'Content-Type': 'application/json' },
    }),
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

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
  }
});

describe('Harness Panel shell', () => {
  it('bootstraps an edge-authenticated owner session without asking for a YouTrack token', async () => {
    const fetcher = vi.fn((url: string, options?: RequestInit) => {
      if (url === '/api/v2/session')
        return json({ error: 'authentication_required' }, 401);
      if (url === '/api/v2/bootstrap') {
        expect(options?.method).toBe('POST');
        expect(options?.body).toBe('{}');
        return json(session);
      }
      if (url === '/api/v2/harness/nodes')
        return json({ registryVersion: 1, mode: 'live', nodes: [] });
      throw new Error(`unexpected endpoint ${url}`);
    });
    vi.stubGlobal('fetch', fetcher);

    render(<Panel />);

    expect(
      await screen.findByRole('heading', {
        name: 'Панель управления агентами',
      }),
    ).toBeDefined();
    expect(screen.queryByText(/YouTrack/i)).toBeNull();
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Задачи' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'База знаний' })).toBeNull();
    expect(
      fetcher.mock.calls.filter(([url]) => url === '/api/v2/bootstrap'),
    ).toHaveLength(1);
  });

  it('reuses an existing session and never calls legacy product APIs', async () => {
    const fetcher = vi.fn((url: string) => {
      if (url === '/api/v2/session') return json(session);
      if (url === '/api/v2/harness/nodes')
        return json({ registryVersion: 1, mode: 'live', nodes: [] });
      throw new Error(`unexpected endpoint ${url}`);
    });
    vi.stubGlobal('fetch', fetcher);

    render(<Panel />);

    await screen.findByText('Зарегистрированных агентов нет.');
    await waitFor(() =>
      expect(
        fetcher.mock.calls.every(
          ([url]) =>
            url === '/api/v2/session' || url === '/api/v2/harness/nodes',
        ),
      ).toBe(true),
    );
  });
});
