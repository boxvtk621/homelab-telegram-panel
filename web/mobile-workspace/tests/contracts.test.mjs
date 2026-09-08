import assert from 'node:assert/strict';
import test from 'node:test';

import { ApiError, MobileApi } from '../lib/api.ts';
import { eventPollDelay } from '../lib/polling.ts';
import { readDialogDraft, writeDialogDraft } from '../lib/telegram.ts';

test('Telegram exchange decodes numeric capability_version', async () => {
  const originalFetch = globalThis.fetch;
  const originalWindow = globalThis.window;
  globalThis.window = globalThis;
  globalThis.fetch = async () =>
    new Response(
      JSON.stringify({
        schema_version: 1,
        request_id: 'contract-fixture',
        server_time: '2026-08-29T12:00:00.000000Z',
        data: {
          role: 'owner',
          capability_version: 7,
          expires_at: '2026-08-29T13:00:00Z',
          csrf_token: '0123456789abcdef',
        },
      }),
      { headers: { 'Content-Type': 'application/json' } },
    );
  try {
    const session = await new MobileApi(
      'web:contract-fixture',
    ).exchangeTelegram('query_id=fixture');
    assert.equal(session.capabilityVersion, 7);
  } finally {
    globalThis.fetch = originalFetch;
    globalThis.window = originalWindow;
  }
});

test('Telegram exchange rejects a string capability_version', async () => {
  const originalFetch = globalThis.fetch;
  const originalWindow = globalThis.window;
  globalThis.window = globalThis;
  globalThis.fetch = async () =>
    new Response(
      JSON.stringify({
        schema_version: 1,
        request_id: 'contract-fixture',
        server_time: '2026-08-29T12:00:00.000000Z',
        data: {
          role: 'owner',
          capability_version: '7',
          expires_at: '2026-08-29T13:00:00Z',
          csrf_token: '0123456789abcdef',
        },
      }),
      { headers: { 'Content-Type': 'application/json' } },
    );
  try {
    await assert.rejects(
      new MobileApi('web:contract-fixture').exchangeTelegram(
        'query_id=fixture',
      ),
      (error) => error instanceof ApiError && error.kind === 'invalid_response',
    );
  } finally {
    globalThis.fetch = originalFetch;
    globalThis.window = originalWindow;
  }
});

test('Task detail uses the exact owner-scoped task route', async () => {
  const originalFetch = globalThis.fetch;
  const originalWindow = globalThis.window;
  globalThis.window = globalThis;
  const taskID = 'task-terminal-01';
  globalThis.fetch = async (input, init) => {
    assert.equal(input, `/api/v1/tasks/${taskID}`);
    assert.equal(init?.method ?? 'GET', 'GET');
    assert.equal(init?.credentials, 'same-origin');
    return new Response(
      JSON.stringify({
        schema_version: 1,
        request_id: 'task-detail-fixture',
        server_time: '2026-08-29T12:00:01.000000Z',
        data: {
          task: {
            id: taskID,
            dialog_id: '018f0c9e-8f4b-4a6b-8c9d-000000000099',
            issue_id: 'HL-210',
            expected_revision: 17,
            state: 'completed',
            cancel_state: 'none',
            created_at: '2026-08-29T11:00:00.000000Z',
            updated_at: '2026-08-29T12:00:00.000000Z',
          },
          freshness_at: '2026-08-29T12:00:00.000000Z',
        },
      }),
      { headers: { 'Content-Type': 'application/json' } },
    );
  };
  try {
    const snapshot = await new MobileApi('web:contract-fixture').task(taskID);
    assert.equal(snapshot.task.id, taskID);
    assert.equal(snapshot.task.state, 'completed');
  } finally {
    globalThis.fetch = originalFetch;
    globalThis.window = originalWindow;
  }
});

test('event polling jitter stays within the canonical visibility profiles', () => {
  assert.equal(
    eventPollDelay('visible', () => 0),
    1_600,
  );
  assert.equal(
    eventPollDelay('visible', () => 1),
    2_400,
  );
  assert.equal(
    eventPollDelay('hidden', () => 0),
    8_000,
  );
  assert.equal(
    eventPollDelay('hidden', () => 1),
    12_000,
  );
});

test('Dialog drafts use tab-scoped sessionStorage, never localStorage', () => {
  const originalWindow = globalThis.window;
  const local = memoryStorage();
  const session = memoryStorage();
  globalThis.window = { localStorage: local, sessionStorage: session };
  const dialogID = '018f0c9e-8f4b-4a6b-8c9d-000000000099';
  try {
    writeDialogDraft(dialogID, 'protected draft');
    assert.equal(readDialogDraft(dialogID), 'protected draft');
    assert.equal(session.size(), 1);
    assert.equal(local.size(), 0);
  } finally {
    globalThis.window = originalWindow;
  }
});

function memoryStorage() {
  const values = new Map();
  return {
    getItem(key) {
      return values.get(key) ?? null;
    },
    removeItem(key) {
      values.delete(key);
    },
    setItem(key, value) {
      values.set(key, String(value));
    },
    size() {
      return values.size;
    },
  };
}
