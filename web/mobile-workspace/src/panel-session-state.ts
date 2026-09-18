import { isValidDeleteCommand, isValidMessageCommand } from './harness-api';
import {
  emptyDraft,
  type DeleteIntent,
  type HarnessDraft,
} from './harness-state';
import { isValidLogicalDeleteRequest } from './logical-delete-api';

export type PanelView = 'interaction' | 'management' | 'history';
export type PanelTheme = 'dark' | 'light';

export type DialogBinding = {
  nodeId: string;
  nodeDialogId: string;
  logicalDialogId: string;
  bindingVersion: number;
};

export type ManagementSelection = {
  nodeId: string;
  hostId: string | null;
};

export type PanelSessionState = {
  version: 1;
  view: PanelView;
  theme: PanelTheme;
  interactionNodeId: string;
  interaction: DialogBinding | null;
  management: ManagementSelection | null;
  drafts: Record<string, HarnessDraft>;
  deletes: Record<string, DeleteIntent>;
};

export type PanelStateRead = {
  state: PanelSessionState;
  persistent: boolean;
};

const storagePrefix = 'homelab-panel:r03:';
const memoryFallback = new Map<string, PanelSessionState>();
const memoryOnly = new Set<string>();
const uuid =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function freshState(): PanelSessionState {
  return {
    version: 1,
    view: 'management',
    theme: 'dark',
    interactionNodeId: '',
    interaction: null,
    management: null,
    drafts: {},
    deletes: {},
  };
}

function object(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function exactKeys(
  value: Record<string, unknown>,
  required: string[],
  optional: string[] = [],
): boolean {
  const keys = Object.keys(value);
  return (
    required.every((key) => Object.hasOwn(value, key)) &&
    keys.every((key) => required.includes(key) || optional.includes(key))
  );
}

function validBinding(value: unknown): value is DialogBinding {
  return (
    object(value) &&
    exactKeys(value, [
      'nodeId',
      'nodeDialogId',
      'logicalDialogId',
      'bindingVersion',
    ]) &&
    typeof value.nodeId === 'string' &&
    uuid.test(value.nodeId) &&
    typeof value.nodeDialogId === 'string' &&
    uuid.test(value.nodeDialogId) &&
    typeof value.logicalDialogId === 'string' &&
    uuid.test(value.logicalDialogId) &&
    typeof value.bindingVersion === 'number' &&
    Number.isSafeInteger(value.bindingVersion) &&
    value.bindingVersion >= 1
  );
}

function validManagement(value: unknown): value is ManagementSelection {
  return (
    object(value) &&
    exactKeys(value, ['nodeId', 'hostId']) &&
    typeof value.nodeId === 'string' &&
    uuid.test(value.nodeId) &&
    (value.hostId === null ||
      (typeof value.hostId === 'string' && uuid.test(value.hostId)))
  );
}

function validText(value: unknown): value is string {
  return (
    typeof value === 'string' &&
    Array.from(value).length <= 65_536 &&
    new TextEncoder().encode(value).byteLength <= 65_536
  );
}

function restoreDraft(value: unknown): HarnessDraft | null {
  if (
    !object(value) ||
    !validText(value.text) ||
    typeof value.phase !== 'string'
  )
    return null;
  const phase = value.phase;
  const command = isValidMessageCommand(value.command)
    ? value.command
    : undefined;
  const error =
    typeof value.error === 'string' &&
    value.error.length <= 500 &&
    !value.error.includes('\u0000')
      ? value.error
      : undefined;
  if (
    (phase === 'sending' || phase === 'checking' || phase === 'unknown') &&
    command
  ) {
    return {
      text: command.payload.text,
      phase: 'unknown',
      command,
      error: 'Результат отправки проверяется после восстановления вкладки.',
    };
  }
  if (phase === 'retry-ready' && command) {
    return { text: command.payload.text, phase, command, error };
  }
  if (phase === 'draft' || phase === 'rejected' || phase === 'queued') {
    return { text: value.text, phase, ...(error ? { error } : {}) };
  }
  return { text: value.text, phase: 'draft' };
}

function restoreDeleteIntent(
  storageKey: string,
  value: unknown,
): DeleteIntent | null {
  if (
    !object(value) ||
    !exactKeys(value, ['phase', 'command', 'request'], ['error']) ||
    !isValidDeleteCommand(value.command) ||
    !isValidLogicalDeleteRequest(value.request) ||
    !['sending', 'unknown', 'checking', 'resume-ready', 'rejected'].includes(
      String(value.phase),
    ) ||
    storageKey !==
      `${value.command.target.nodeId}:${value.command.target.dialogId}` ||
    value.command.commandId !== value.request.commandId ||
    value.command.expected.dialogVersion !== value.request.expectedDialogVersion
  ) {
    return null;
  }
  const error =
    typeof value.error === 'string' &&
    value.error.length <= 500 &&
    !value.error.includes('\u0000')
      ? value.error
      : undefined;
  if (value.phase === 'rejected') {
    return {
      phase: 'rejected',
      command: value.command,
      request: value.request,
      ...(error ? { error } : {}),
    };
  }
  return {
    phase: 'unknown',
    command: value.command,
    request: value.request,
    error: 'Результат удаления проверяется после восстановления вкладки.',
  };
}

function restoreState(value: unknown): PanelSessionState | null {
  const baseKeys = [
    'version',
    'view',
    'theme',
    'interactionNodeId',
    'interaction',
    'management',
    'drafts',
  ];
  if (
    !object(value) ||
    (!exactKeys(value, baseKeys) &&
      !exactKeys(value, [...baseKeys, 'deletes'])) ||
    value.version !== 1 ||
    (value.view !== 'interaction' &&
      value.view !== 'management' &&
      value.view !== 'history') ||
    (value.theme !== 'dark' && value.theme !== 'light') ||
    typeof value.interactionNodeId !== 'string' ||
    (value.interactionNodeId !== '' && !uuid.test(value.interactionNodeId)) ||
    (value.interaction !== null && !validBinding(value.interaction)) ||
    (value.management !== null && !validManagement(value.management)) ||
    !object(value.drafts) ||
    Object.keys(value.drafts).length > 200 ||
    (Object.hasOwn(value, 'deletes') &&
      (!object(value.deletes) || Object.keys(value.deletes).length > 200))
  ) {
    return null;
  }
  const drafts: Record<string, HarnessDraft> = {};
  for (const [logicalDialogId, candidate] of Object.entries(value.drafts)) {
    if (!uuid.test(logicalDialogId)) return null;
    const restored = restoreDraft(candidate);
    if (!restored) return null;
    drafts[logicalDialogId] = restored;
  }
  const deletes: Record<string, DeleteIntent> = {};
  if (object(value.deletes)) {
    for (const [target, candidate] of Object.entries(value.deletes)) {
      const restored = restoreDeleteIntent(target, candidate);
      if (!restored) return null;
      deletes[target] = restored;
    }
  }
  return {
    version: 1,
    view: value.view,
    theme: value.theme,
    interactionNodeId: value.interactionNodeId,
    interaction: value.interaction,
    management: value.management,
    drafts,
    deletes,
  };
}

function key(ownerId: string): string {
  return `${storagePrefix}${encodeURIComponent(ownerId)}`;
}

export function readPanelSessionState(ownerId: string): PanelStateRead {
  const storageKey = key(ownerId);
  if (memoryOnly.has(storageKey)) {
    const state = memoryFallback.get(storageKey) ?? freshState();
    memoryFallback.set(storageKey, state);
    return { state, persistent: false };
  }
  try {
    const raw = window.sessionStorage.getItem(storageKey);
    if (raw !== null) {
      const restored = restoreState(JSON.parse(raw));
      if (restored) {
        memoryFallback.set(storageKey, restored);
        return { state: restored, persistent: true };
      }
      window.sessionStorage.removeItem(storageKey);
    }
    const state = memoryFallback.get(storageKey) ?? freshState();
    memoryFallback.set(storageKey, state);
    return { state, persistent: true };
  } catch {
    const state = memoryFallback.get(storageKey) ?? freshState();
    memoryFallback.set(storageKey, state);
    memoryOnly.add(storageKey);
    return { state, persistent: false };
  }
}

export function updatePanelSessionState(
  ownerId: string,
  update: (current: PanelSessionState) => PanelSessionState,
): PanelStateRead {
  const storageKey = key(ownerId);
  const current = readPanelSessionState(ownerId);
  const state = update(current.state);
  memoryFallback.set(storageKey, state);
  try {
    window.sessionStorage.setItem(storageKey, JSON.stringify(state));
    memoryOnly.delete(storageKey);
    return { state, persistent: true };
  } catch {
    memoryOnly.add(storageKey);
    return { state, persistent: false };
  }
}

export function clearPanelSessionState(ownerId: string): void {
  const storageKey = key(ownerId);
  memoryFallback.delete(storageKey);
  memoryOnly.delete(storageKey);
  try {
    window.sessionStorage.removeItem(storageKey);
  } catch {
    // The owner-scoped memory copy is still cleared when storage is blocked.
  }
}

export function readLogicalDraft(
  state: PanelSessionState,
  logicalDialogId: string,
): HarnessDraft {
  return state.drafts[logicalDialogId] ?? emptyDraft();
}

export function resolveLatestBinding(
  current: DialogBinding,
  candidates: readonly DialogBinding[],
): DialogBinding {
  return candidates.reduce(
    (latest, candidate) =>
      candidate.logicalDialogId === current.logicalDialogId &&
      candidate.bindingVersion > latest.bindingVersion
        ? candidate
        : latest,
    current,
  );
}
