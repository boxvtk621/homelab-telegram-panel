import type { HarnessCommand, HarnessEvent } from './harness-protocol-types';

export function commandPayloadKind(command: HarnessCommand): string {
  if (command.kind === 'dialog.create') return command.payload.title ?? '';
  if (command.kind === 'attempt.retry') return command.payload.acknowledgeKnownEffects ? 'acknowledged' : 'unacknowledged';
  if (command.kind === 'approval.respond') return command.payload.decision;
  return command.kind;
}

export function eventEntity(event: HarnessEvent): string {
  if (event.type === 'attempt.completed') return event.payload.output.kind;
  if (event.type === 'tool.completed') return event.payload.status;
  if (event.type === 'message.accepted') return event.payload.disposition;
  return event.type;
}
