import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { MobileWorkspace } from '@/components/mobile-workspace';
import { ApiError, MobileApi } from '@/lib/api';
import { pendingDialogCommand, clearDialogCommand } from '@/lib/dialog-command';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
});

it('retains the exact command and version across reload until an explicit outcome', () => {
  const first = pendingDialogCommand(localStorage, 'client-test', 0);
  expect(pendingDialogCommand(localStorage, 'client-test', 99)).toEqual(first);
  clearDialogCommand(localStorage, 'client-test', {
    ...first,
    commandId: crypto.randomUUID(),
  });
  expect(pendingDialogCommand(localStorage, 'client-test', 99)).toEqual(first);
  clearDialogCommand(localStorage, 'client-test', first);
  expect(
    pendingDialogCommand(localStorage, 'client-test', 99).commandId,
  ).not.toBe(first.commandId);
});

it('fails closed if browser storage cannot persist the command', () => {
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('storage disabled');
  });
  expect(() => pendingDialogCommand(localStorage, 'client-test', 0)).toThrow();
});

it('retries the same dialog command after lost ACK and remount', async () => {
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
  const page = {
    dialogs: [],
    snapshotVersion: 0,
    freshnessAt,
    nextCursor: null,
    eventCheckpoint: null,
  };
  vi.spyOn(MobileApi.prototype, 'listDialogs').mockResolvedValue(page);
  vi.spyOn(MobileApi.prototype, 'resumeSession').mockResolvedValue({
    role: 'owner',
    capabilityVersion: 1,
    expiresAt: new Date(Date.now() + 60_000).toISOString(),
    csrfToken: 'synthetic-csrf-value',
    dialogCreationEnabled: true,
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
    snapshotVersion: 0,
    freshnessAt,
    nextCursor: null,
    hasMore: false,
  });
  const create = vi
    .spyOn(MobileApi.prototype, 'createDialog')
    .mockRejectedValue(new ApiError('unavailable', 'lost ACK'));
  const firstView = render(<MobileWorkspace />);
  await waitFor(() =>
    expect(
      (
        screen.getByRole('button', {
          name: 'Создать диалог',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false),
  );
  fireEvent.click(screen.getByRole('button', { name: 'Создать диалог' }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  await screen.findByRole('alert');
  const firstArgs = create.mock.calls[0];
  firstView.unmount();
  page.snapshotVersion = 99;
  render(<MobileWorkspace />);
  await waitFor(() =>
    expect(
      (
        screen.getByRole('button', {
          name: 'Создать диалог',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false),
  );
  fireEvent.click(screen.getByRole('button', { name: 'Создать диалог' }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
  expect(create.mock.calls[1].slice(0, 2)).toEqual(firstArgs.slice(0, 2));
});
