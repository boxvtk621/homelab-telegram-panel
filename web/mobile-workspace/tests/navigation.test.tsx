import { describe, expect, it } from 'vitest';

import {
  initialWorkspaceNavigation,
  workspaceNavigationReducer,
} from '@/lib/navigation';

describe('workspaceNavigationReducer', () => {
  it('keeps exact Task and Dialog identities until task detail closes', () => {
    const selected = workspaceNavigationReducer(initialWorkspaceNavigation, {
      type: 'open-task',
      taskID: 'task-terminal-01',
      dialogID: '018f0c9e-8f4b-4a6b-8c9d-000000000099',
    });

    expect(selected).toEqual({
      view: 'tasks',
      selectedDialogID: '018f0c9e-8f4b-4a6b-8c9d-000000000099',
      selectedTaskID: 'task-terminal-01',
    });
    expect(
      workspaceNavigationReducer(selected, { type: 'close-detail' }),
    ).toEqual({
      view: 'tasks',
      selectedDialogID: null,
      selectedTaskID: null,
    });
  });

  it('clears a prior Task identity when a Dialog is selected', () => {
    const selectedTask = {
      view: 'tasks' as const,
      selectedDialogID: 'dialog-old',
      selectedTaskID: 'task-old',
    };

    expect(
      workspaceNavigationReducer(selectedTask, {
        type: 'open-dialog',
        dialogID: 'dialog-new',
      }),
    ).toEqual({
      view: 'dialogs',
      selectedDialogID: 'dialog-new',
      selectedTaskID: null,
    });
  });
});
