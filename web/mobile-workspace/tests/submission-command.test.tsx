import { webcrypto } from 'node:crypto';
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { MobileWorkspace } from '@/components/mobile-workspace';
import { ApiError, MobileApi, type Dialog } from '@/lib/api';
import { pendingSubmission, clearSubmission } from '@/lib/submission-command';

const dialogID = '018f0c9e-8f4b-4a6b-8c9d-000000000099';
it('rejects invalid public intent before persisting command identity', async () => {
  for (const [issue, text] of [
    ['hl-210', 'text'],
    ['', '\u0085'],
    ['', '\u0000'],
    ['', '\ud800'],
    ['', '\u0001'.repeat(2048)],
  ]) {
    await expect(
      pendingSubmission(
        localStorage,
        'client-validation',
        dialogID,
        1,
        issue,
        text,
      ),
    ).rejects.toThrow();
    expect(
      localStorage.getItem(`fixik:task-submit:client-validation:${dialogID}`),
    ).toBeNull();
  }
});
beforeEach(() => {
  vi.stubGlobal('crypto', webcrypto);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});

it('retains receipt identity but never stores plaintext and rejects a changed draft', async () => {
  const first = await pendingSubmission(
    localStorage,
    'client-test',
    dialogID,
    1,
    'HL-210',
    'private draft',
  );
  expect(
    await pendingSubmission(
      localStorage,
      'client-test',
      dialogID,
      99,
      'HL-210',
      'private draft',
    ),
  ).toEqual(first);
  expect(
    localStorage.getItem(`fixik:task-submit:client-test:${dialogID}`),
  ).not.toContain('private draft');
  await expect(
    pendingSubmission(
      localStorage,
      'client-test',
      dialogID,
      99,
      'HL-210',
      'changed draft',
    ),
  ).rejects.toThrow();
  clearSubmission(localStorage, 'client-test', {
    ...first,
    commandId: crypto.randomUUID(),
  });
  expect(
    await pendingSubmission(
      localStorage,
      'client-test',
      dialogID,
      99,
      'HL-210',
      'private draft',
    ),
  ).toEqual(first);
  clearSubmission(localStorage, 'client-test', first);
  expect(
    (
      await pendingSubmission(
        localStorage,
        'client-test',
        dialogID,
        99,
        'HL-210',
        'private draft',
      )
    ).commandId,
  ).not.toBe(first.commandId);
});

it('requires target confirmation and retries the original command after lost ACK and reopen', async () => {
  vi.stubGlobal(
    'navigator',
    new Proxy(navigator, {
      get(target, key) {
        return key === 'locks'
          ? {
              request: async (
                _name: string,
                callback: () => Promise<unknown>,
              ) => callback(),
            }
          : Reflect.get(target, key, target);
      },
    }),
  );
  const freshnessAt = new Date().toISOString();
  const dialog: Dialog = {
    id: dialogID,
    lifecycle: 'active',
    objectVersion: 1,
    activeTaskId: null,
    createdAt: freshnessAt,
    updatedAt: freshnessAt,
  };
  vi.spyOn(MobileApi.prototype, 'resumeSession').mockResolvedValue({
    role: 'owner',
    capabilityVersion: 1,
    expiresAt: new Date(Date.now() + 60_000).toISOString(),
    csrfToken: 'synthetic-csrf-value',
    dialogCreationEnabled: true,
    taskSubmissionEnabled: true,
  });
  vi.spyOn(MobileApi.prototype, 'listDialogs').mockImplementation(async () => ({
    dialogs: [{ ...dialog }],
    snapshotVersion: dialog.objectVersion,
    freshnessAt,
    nextCursor: null,
    eventCheckpoint: null,
  }));
  vi.spyOn(MobileApi.prototype, 'dialog').mockImplementation(async () => ({
    dialog: { ...dialog },
    snapshotVersion: dialog.objectVersion,
    freshnessAt,
  }));
  vi.spyOn(MobileApi.prototype, 'messages').mockResolvedValue({
    messages: [],
    hasMore: false,
    nextAfterSequence: null,
    freshnessAt,
  });
  vi.spyOn(MobileApi.prototype, 'listTasks').mockResolvedValue({
    tasks: [],
    freshnessAt,
  });
  vi.spyOn(MobileApi.prototype, 'controlHealth').mockResolvedValue({
    components: [{ name: 'controller', state: 'ready' }],
    capacity: [],
    freshnessAt,
    liveness: 'ok',
    readiness: 'ok',
  });
  vi.spyOn(MobileApi.prototype, 'events').mockResolvedValue({
    events: [],
    snapshotVersion: 1,
    freshnessAt,
    nextCursor: null,
    hasMore: false,
  });
  const submit = vi
    .spyOn(MobileApi.prototype, 'submitTask')
    .mockRejectedValue(new ApiError('unavailable', 'lost ACK'));
  const first = render(<MobileWorkspace />);
  fireEvent.click(
    await screen.findByRole('button', { name: `Открыть диалог ${dialogID}` }),
  );
  const input = await screen.findByRole('textbox', {
    name: 'Локальный черновик сообщения',
  });
  await waitFor(() =>
    expect((input as HTMLTextAreaElement).disabled).toBe(false),
  );
  fireEvent.change(input, { target: { value: 'durable synthetic task' } });
  fireEvent.change(screen.getByRole('textbox', { name: 'Задача YouTrack' }), {
    target: { value: 'hl-210' },
  });
  expect(
    (
      screen.getByRole('button', {
        name: 'Поставить задачу',
      }) as HTMLButtonElement
    ).disabled,
  ).toBe(true);
  expect(submit).not.toHaveBeenCalled();
  fireEvent.change(screen.getByRole('textbox', { name: 'Задача YouTrack' }), {
    target: { value: 'HL-210' },
  });
  fireEvent.click(screen.getByRole('button', { name: 'Поставить задачу' }));
  expect(submit).not.toHaveBeenCalled();
  expect(await screen.findByRole('alertdialog')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Подтвердить отправку' }));
  await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
  await waitFor(() =>
    expect(screen.getByText(/Приём не подтверждён/)).toBeTruthy(),
  );
  const original = submit.mock.calls[0];
  first.unmount();
  dialog.objectVersion = 99;
  render(<MobileWorkspace />);
  const open = await screen.findByRole('button', {
    name: `Открыть диалог ${dialogID}`,
  });
  fireEvent.click(open);
  await waitFor(() =>
    expect(
      (
        screen.getByRole('button', {
          name: 'Поставить задачу',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false),
  );
  fireEvent.click(screen.getByRole('button', { name: 'Поставить задачу' }));
  expect((await screen.findByRole('alertdialog')).textContent).toContain(
    'версия 1',
  );
  expect(screen.getByRole('alertdialog').textContent).toContain('HL-210');
  fireEvent.click(screen.getByRole('button', { name: 'Подтвердить отправку' }));
  await waitFor(() => expect(submit).toHaveBeenCalledTimes(2));
  expect(submit.mock.calls[1]).toEqual(original);
});
