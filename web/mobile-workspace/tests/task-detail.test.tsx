import {
  cleanup,
  fireEvent,
  render,
  renderHook,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { TaskDetail, TasksView } from '@/components/mobile-workspace';
import { useMobileWorkspace } from '@/hooks/use-mobile-workspace';
import {
  MobileApi,
  type Task,
  type TaskPage,
  type TaskSnapshot,
} from '@/lib/api';

const dialogID = '018f0c9e-8f4b-4a6b-8c9d-000000000099';
const taskID = 'task-terminal-01';
const freshnessAt = '2026-08-29T12:00:00.000000Z';

const terminalTask: Task = {
  id: taskID,
  dialogId: dialogID,
  issueId: 'HL-210',
  expectedRevision: 17,
  state: 'completed',
  cancelState: 'none',
  nextCheckAt: null,
  createdAt: '2026-08-29T11:00:00.000000Z',
  updatedAt: freshnessAt,
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('selected Task readback', () => {
  it('loads the exact terminal Task without consulting Dialog.activeTaskId', async () => {
    mockDashboard();
    const task = vi
      .spyOn(MobileApi.prototype, 'task')
      .mockResolvedValue({ task: terminalTask, freshnessAt });
    const dialog = vi.spyOn(MobileApi.prototype, 'dialog');
    const messages = vi.spyOn(MobileApi.prototype, 'messages');

    const { result } = renderHook(() => useMobileWorkspace(dialogID, taskID));

    await waitFor(() => {
      expect(result.current.task.status).toBe('ready');
    });
    expect(result.current.task.data?.task).toEqual(terminalTask);
    expect(task).toHaveBeenCalledWith(taskID, expect.any(AbortSignal));
    expect(dialog).not.toHaveBeenCalled();
    expect(messages).not.toHaveBeenCalled();
  });

  it('rejects a Task whose server readback does not match the navigation Dialog hint', async () => {
    mockDashboard();
    vi.spyOn(MobileApi.prototype, 'task').mockResolvedValue({
      task: { ...terminalTask, dialogId: 'dialog-foreign' },
      freshnessAt,
    });

    const { result } = renderHook(() => useMobileWorkspace(dialogID, taskID));

    await waitFor(() => {
      expect(result.current.task.status).toBe('error');
    });
    expect(result.current.task.data).toBeNull();
    expect(result.current.task.error?.kind).toBe('invalid_response');
  });
});

describe('Task components', () => {
  it('passes the exact Task ID and Dialog ID from a task-list click', () => {
    const onOpen = vi.fn();
    const page: TaskPage = { tasks: [terminalTask], freshnessAt };
    render(
      <TasksView
        tasks={{ status: 'ready', data: page, error: null }}
        onOpen={onOpen}
      />,
    );

    fireEvent.click(
      screen.getByRole('button', { name: `Открыть задачу ${taskID}` }),
    );
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(onOpen).toHaveBeenCalledWith(taskID, dialogID);
  });

  it('renders a selected terminal Task as its own detail', () => {
    const snapshot: TaskSnapshot = { task: terminalTask, freshnessAt };
    render(
      <TaskDetail
        selectedTaskID={taskID}
        task={{ status: 'ready', data: snapshot, error: null }}
      />,
    );

    expect(screen.getAllByText('Завершена').length).toBeGreaterThan(0);
    expect(screen.getByText(/Terminal Task остаётся доступна/)).toBeTruthy();
    expect(screen.getAllByText(taskID).length).toBeGreaterThan(0);
  });
});

function mockDashboard() {
  vi.spyOn(MobileApi.prototype, 'resumeSession').mockResolvedValue({
    role: 'owner',
    capabilityVersion: 1,
    csrfToken: 'fixture-csrf-token',
    expiresAt: '2026-08-29T13:00:00Z',
    dialogCreationEnabled: false,
  });
  vi.spyOn(MobileApi.prototype, 'listDialogs').mockResolvedValue({
    dialogs: [],
    snapshotVersion: 0,
    freshnessAt,
    nextCursor: null,
    eventCheckpoint: 'cursor-r0',
  });
  vi.spyOn(MobileApi.prototype, 'listTasks').mockResolvedValue({
    tasks: [terminalTask],
    freshnessAt,
  });
  vi.spyOn(MobileApi.prototype, 'controlHealth').mockResolvedValue({
    components: [{ name: 'controller', state: 'ready' }],
    capacity: [
      { class: 'general', inFlight: 0, limit: 24 },
      { class: 'reserve', inFlight: 0, limit: 8 },
    ],
    freshnessAt,
    liveness: 'ok',
    readiness: 'ok',
  });
  vi.spyOn(MobileApi.prototype, 'events').mockResolvedValue({
    events: [],
    snapshotVersion: 0,
    freshnessAt,
    nextCursor: 'cursor-r0',
    hasMore: false,
  });
}
