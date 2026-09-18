import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  parseHistoryEntryLookup,
  parseHistorySearchPage,
} from '../src/history-search-api';
import { HistorySearch } from '../src/history-search';

const dialogId = '10000000-0000-4000-8000-000000000001';
const entryId = '10000000-0000-4000-8000-000000000002';
const nodeId = '10000000-0000-4000-8000-000000000003';
const nodeDialogId = '10000000-0000-4000-8000-000000000004';
const observedAt = '2026-09-18T08:00:00Z';

function searchPage(overrides: Record<string, unknown> = {}) {
  return {
    schemaId: 'agent-search-v1',
    query: { q: 'кириллица exact-id' },
    items: [
      {
        logicalDialogId: dialogId,
        entryId,
        nodeId,
        nodeDialogId,
        title: 'Архивный диалог',
        role: 'assistant',
        kind: 'message',
        createdAt: '2026-09-18T07:30:00Z',
        rank: 0.75,
        snippet: 'кириллица exact-id безопасный результат',
      },
    ],
    nextCursor: null,
    totalCount: 1,
    observedAt,
    lagMillis: 2_500,
    incomplete: true,
    ...overrides,
  };
}

function entryLookup(overrides: Record<string, unknown> = {}) {
  return {
    schemaId: 'agent-history-entry-v1',
    entry: {
      entryId,
      origin: {
        nodeId,
        nodeDialogId,
        authorEventId: 'event:42',
      },
      logicalDialogId: dialogId,
      messageId: entryId,
      attemptId: '10000000-0000-4000-8000-000000000005',
      kind: 'message',
      role: 'assistant',
      content: {
        kind: 'inline',
        content: '**Сохранённый ответ**',
        redaction: 'none',
        truncated: false,
      },
      createdAt: '2026-09-18T07:30:00Z',
      profileHash: 'a'.repeat(64),
      sourceGeneration: 1,
      executionOrdinal: 2,
      executionStatus: 'recorded',
    },
    observedAt,
    lagMillis: 2_500,
    incomplete: true,
    ...overrides,
  };
}

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
  window.history.replaceState(null, '', '/');
});

describe('agent-search-v1 browser consumer', () => {
  it('strictly accepts the bounded search and exact-entry contracts', () => {
    expect(parseHistorySearchPage(searchPage()).items[0]?.snippet).toContain(
      'безопасный',
    );
    expect(parseHistoryEntryLookup(entryLookup()).entry.content.kind).toBe(
      'inline',
    );
    expect(() =>
      parseHistorySearchPage(searchPage({ rawPayload: { secret: true } })),
    ).toThrow('invalid_history_search_response');
    expect(() =>
      parseHistoryEntryLookup(
        entryLookup({
          entry: {
            ...(entryLookup().entry as Record<string, unknown>),
            content: { kind: 'native_path', content: '/private/db.sqlite' },
          },
        }),
      ),
    ).toThrow('invalid_history_entry_response');
  });

  it('searches without a live agent, reports incomplete coverage and opens by stable IDs', async () => {
    const fetcher = vi.fn((url: string) => {
      if (url.startsWith('/api/v2/history/search?')) return json(searchPage());
      if (url === `/api/v2/history/dialogs/${dialogId}/entries/${entryId}`)
        return json(entryLookup());
      throw new Error(`unexpected endpoint ${url}`);
    });
    vi.stubGlobal('fetch', fetcher);

    render(<HistorySearch currentNodeId="" onExpired={vi.fn()} />);
    expect(screen.getByText('Текущий агент не выбран')).toBeDefined();
    const input = screen.getByRole('textbox', { name: 'Запрос' });
    fireEvent.change(input, { target: { value: 'кириллица exact-id' } });
    fireEvent.click(screen.getByRole('button', { name: 'Найти' }));

    const results = await screen.findByLabelText('Результаты поиска');
    expect(
      within(results).getByText(/Найдено в доступной реплике/),
    ).toBeDefined();
    expect(within(results).getByText(/Реплика неполная/)).toBeDefined();
    expect(within(results).getByText(/безопасный результат/)).toBeDefined();

    const result = within(results).getByRole('button', {
      name: 'Открыть сохранённую запись: Архивный диалог',
    });
    fireEvent.click(result);
    expect(await screen.findByText('Сохранённый ответ')).toBeDefined();
    expect(result.getAttribute('aria-current')).toBe('true');
    expect(window.location.search).toContain(`historyDialog=${dialogId}`);
    expect(window.location.search).toContain(`historyEntry=${entryId}`);
    expect(
      fetcher.mock.calls.every(
        ([url]) =>
          String(url).includes('/api/v2/history/') &&
          !String(url).includes('/commands'),
      ),
    ).toBe(true);
  });

  it('loads a deep link directly and rechecks a tombstone on every exact read', async () => {
    window.history.replaceState(
      null,
      '',
      `/?historyDialog=${dialogId}&historyEntry=${entryId}`,
    );
    const fetcher = vi.fn((_url: string) =>
      json({ error: 'history_not_found' }, 404),
    );
    vi.stubGlobal('fetch', fetcher);

    render(<HistorySearch currentNodeId="" onExpired={vi.fn()} />);
    expect(
      await screen.findByText(
        'Запись удалена или больше не доступна этому владельцу.',
      ),
    ).toBeDefined();
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(fetcher.mock.calls[0]?.[0]).toBe(
      `/api/v2/history/dialogs/${dialogId}/entries/${entryId}`,
    );
  });

  it('does not silently restart an expired snapshot', async () => {
    const first = searchPage({ nextCursor: 'snapshot.next', totalCount: 80 });
    const fetcher = vi
      .fn()
      .mockImplementationOnce(() => json(first))
      .mockImplementationOnce(() =>
        json({ error: 'search_snapshot_expired' }, 410),
      );
    vi.stubGlobal('fetch', fetcher);

    render(<HistorySearch currentNodeId={nodeId} onExpired={vi.fn()} />);
    fireEvent.change(screen.getByRole('textbox', { name: 'Запрос' }), {
      target: { value: 'кириллица exact-id' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Найти' }));
    fireEvent.click(
      await screen.findByRole('button', { name: 'Показать ещё' }),
    );

    expect(
      await screen.findByText('Снимок поиска истёк. Запустите поиск заново.'),
    ).toBeDefined();
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  });
});
