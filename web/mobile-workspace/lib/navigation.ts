export type WorkspaceView = 'dialogs' | 'tasks' | 'control';

export type WorkspaceNavigation = {
  view: WorkspaceView;
  selectedDialogID: string | null;
  selectedTaskID: string | null;
};

export type WorkspaceNavigationAction =
  | { type: 'navigate'; view: WorkspaceView }
  | { type: 'open-dialog'; dialogID: string }
  | { type: 'open-task'; dialogID: string; taskID: string }
  | { type: 'close-detail' };

export const initialWorkspaceNavigation: WorkspaceNavigation = {
  view: 'dialogs',
  selectedDialogID: null,
  selectedTaskID: null,
};

export function workspaceNavigationReducer(
  state: WorkspaceNavigation,
  action: WorkspaceNavigationAction,
): WorkspaceNavigation {
  switch (action.type) {
    case 'navigate':
      return {
        view: action.view,
        selectedDialogID: null,
        selectedTaskID: null,
      };
    case 'open-dialog':
      return {
        view: 'dialogs',
        selectedDialogID: action.dialogID,
        selectedTaskID: null,
      };
    case 'open-task':
      return {
        view: 'tasks',
        selectedDialogID: action.dialogID,
        selectedTaskID: action.taskID,
      };
    case 'close-detail':
      return {
        ...state,
        selectedDialogID: null,
        selectedTaskID: null,
      };
  }
}
