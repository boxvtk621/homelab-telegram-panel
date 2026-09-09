import type { HarnessCommand } from './harness-protocol-types.ts';

export type IntentPhase =
  | 'draft'
  | 'sending'
  | 'unknown'
  | 'checking'
  | 'retry-ready'
  | 'rejected'
  | 'queued';

export type MessageCommand = Extract<
  HarnessCommand,
  { kind: 'message.enqueue' }
>;
export type CreateCommand = Extract<HarnessCommand, { kind: 'dialog.create' }>;
export type ControlCommand = Exclude<
  HarnessCommand,
  { kind: 'dialog.create' | 'message.enqueue' }
>;

export type ControlPhase =
  | 'sending'
  | 'unknown'
  | 'checking'
  | 'retry-ready'
  | 'rejected'
  | 'accepted';

export type ControlIntent = {
  phase: ControlPhase;
  command: ControlCommand;
  error?: string;
};

export type HarnessDraft = {
  text: string;
  phase: IntentPhase;
  command?: MessageCommand;
  error?: string;
};

export type CreateIntent = {
  phase: Exclude<IntentPhase, 'draft' | 'queued'>;
  command: CreateCommand;
  error?: string;
};

export type EventCursor = { nodeId: string; epoch: number; seq: number };
export type EventDecision =
  | 'accept'
  | 'duplicate'
  | 'gap'
  | 'epoch-change'
  | 'foreign-node';

export function targetKey(nodeId: string, dialogId: string): string {
  return `${nodeId}:${dialogId}`;
}

export function controlIntentKey(command: ControlCommand): string {
  return `${command.kind}:${JSON.stringify(command.target)}:${JSON.stringify(command.expected)}`;
}

export function classifyControlFailure(
  command: ControlCommand,
  cause: {
    outcome?: 'definite' | 'unknown';
    retryable?: boolean;
    message: string;
  },
): ControlIntent {
  if (cause.outcome === 'unknown') {
    return { command, phase: 'unknown', error: cause.message };
  }
  if (cause.retryable) {
    return { command, phase: 'retry-ready', error: cause.message };
  }
  return { command, phase: 'rejected', error: cause.message };
}

export function emptyDraft(): HarnessDraft {
  return { text: '', phase: 'draft' };
}

export function readDraft(
  store: Readonly<Record<string, HarnessDraft>>,
  nodeId: string,
  dialogId: string,
): HarnessDraft {
  return store[targetKey(nodeId, dialogId)] ?? emptyDraft();
}

export function writeDraft(
  store: Readonly<Record<string, HarnessDraft>>,
  nodeId: string,
  dialogId: string,
  draft: HarnessDraft,
): Record<string, HarnessDraft> {
  return { ...store, [targetKey(nodeId, dialogId)]: draft };
}

export function acceptsTarget(
  currentNodeId: string,
  currentDialogId: string,
  resultNodeId: string,
  resultDialogId: string,
): boolean {
  return currentNodeId === resultNodeId && currentDialogId === resultDialogId;
}

export function classifyEvent(
  cursor: EventCursor | null,
  event: EventCursor,
): EventDecision {
  if (!cursor || cursor.nodeId !== event.nodeId) return 'foreign-node';
  if (event.epoch !== cursor.epoch) return 'epoch-change';
  if (event.seq <= cursor.seq) return 'duplicate';
  if (event.seq > cursor.seq + 1) return 'gap';
  return 'accept';
}

export function reconnectDelay(attempt: number): number | null {
  if (attempt < 0 || attempt >= 3) return null;
  return 1000 * (attempt + 1);
}
