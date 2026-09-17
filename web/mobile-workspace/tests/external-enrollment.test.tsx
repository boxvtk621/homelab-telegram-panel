import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { ExternalEnrollment } from '../src/external-enrollment';

const operationID = '30000000-0000-4000-8000-000000000001';
const nodeID = '20000000-0000-4000-8000-000000000001';
const hostID = '10000000-0000-4000-8000-000000000001';
const session = {
  user: { id: 'owner-1', login: 'owner', name: 'Owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
  inventory_enabled: true,
  enrollment_enabled: true,
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it('retries uncertain admission with one operation id and never sends credential refs', async () => {
  vi.stubGlobal('crypto', { randomUUID: () => operationID });
  const submitted: Record<string, unknown>[] = [];
  let attempt = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v2/external-enrollment-hosts?limit=100') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              schemaId: 'external-harness-enrollment-host-page-v1',
              items: [
                {
                  hostId: hostID,
                  hostVersion: 3,
                  displayName: 'Remote Mac',
                  transport: 'ssh',
                  availability: 'ready',
                },
              ],
              nextCursor: null,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        );
      }
      if (url === '/api/v2/external-enrollments') {
        const body = init?.body;
        if (typeof body !== 'string') throw new Error('missing request body');
        submitted.push(JSON.parse(body) as Record<string, unknown>);
        attempt += 1;
        if (attempt === 1) {
          return Promise.resolve(
            new Response(
              JSON.stringify({ error: 'enrollment_reconciliation_required' }),
              { status: 503, headers: { 'Content-Type': 'application/json' } },
            ),
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({
              schemaId: 'external-harness-enrollment-result-v1',
              operationId: operationID,
              nodeId: nodeID,
              status: 'ready',
              registrationRevision: 1,
              identityEpoch: 1,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        );
      }
      throw new Error(`unexpected endpoint ${url}`);
    }),
  );
  const onSuccess = vi.fn();
  render(
    <ExternalEnrollment
      open
      session={session}
      onClose={vi.fn()}
      onSuccess={onSuccess}
      onExpired={vi.fn()}
    />,
  );
  await screen.findByRole('option', { name: 'Remote Mac · ssh · ready' });
  fireEvent.change(screen.getByLabelText('Имя Harness'), {
    target: { value: 'External Codex' },
  });
  fireEvent.change(screen.getByLabelText('Node ID'), {
    target: { value: nodeID },
  });
  fireEvent.change(screen.getByLabelText('HTTPS URI'), {
    target: { value: 'https://127.0.0.1:9443' },
  });
  fireEvent.change(screen.getByLabelText('SHA-256 сертификата Harness'), {
    target: { value: 'a'.repeat(64) },
  });
  fireEvent.click(
    screen.getByRole('button', { name: 'Проверить и подключить' }),
  );
  expect(
    await screen.findByText(
      'Результат операции пока не подтверждён. Повторите с теми же данными.',
    ),
  ).toBeDefined();
  fireEvent.click(
    screen.getByRole('button', { name: 'Проверить и подключить' }),
  );
  await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
  expect(submitted).toHaveLength(2);
  expect(submitted[0].operationId).toBe(operationID);
  expect(submitted[1].operationId).toBe(operationID);
  expect(submitted[0].expectedHostVersion).toBe(3);
  for (const body of submitted) {
    expect(body).not.toHaveProperty('credentialRef');
    expect(body).not.toHaveProperty('targetRef');
    expect(body).not.toHaveProperty('dockerContextRef');
    expect(body).not.toHaveProperty('expectedHostKey');
  }
});

it('shows explicit capability incompatibility without success', async () => {
  vi.stubGlobal('crypto', { randomUUID: () => operationID });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      if (url.startsWith('/api/v2/external-enrollment-hosts')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              schemaId: 'external-harness-enrollment-host-page-v1',
              items: [
                {
                  hostId: hostID,
                  hostVersion: 1,
                  displayName: 'Local host',
                  transport: 'local',
                  availability: 'ready',
                },
              ],
              nextCursor: null,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          ),
        );
      }
      return Promise.resolve(
        new Response(
          JSON.stringify({ error: 'admission_capability_missing' }),
          {
            status: 422,
            headers: { 'Content-Type': 'application/json' },
          },
        ),
      );
    }),
  );
  const onSuccess = vi.fn();
  render(
    <ExternalEnrollment
      open
      session={session}
      onClose={vi.fn()}
      onSuccess={onSuccess}
      onExpired={vi.fn()}
    />,
  );
  await screen.findByRole('option', { name: 'Local host · local · ready' });
  fireEvent.change(screen.getByLabelText('Имя Harness'), {
    target: { value: 'External' },
  });
  fireEvent.change(screen.getByLabelText('Node ID'), {
    target: { value: nodeID },
  });
  fireEvent.change(screen.getByLabelText('HTTPS URI'), {
    target: { value: 'https://10.20.30.40:9443' },
  });
  fireEvent.change(screen.getByLabelText('SHA-256 сертификата Harness'), {
    target: { value: 'b'.repeat(64) },
  });
  fireEvent.click(
    screen.getByRole('button', { name: 'Проверить и подключить' }),
  );
  expect(
    await screen.findByText(
      'Harness не поддерживает все обязательные возможности переноса и владения.',
    ),
  ).toBeDefined();
  expect(onSuccess).not.toHaveBeenCalled();
});
