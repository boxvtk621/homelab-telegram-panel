import { api, type Session } from './panel-api';

export type LogicalDeleteRequest = {
  schemaId: 'logical-dialog-delete-v1';
  operationId: string;
  commandId: string;
  logicalDialogId: string;
  expectedBindingVersion: number;
  expectedDialogVersion: number;
};

export type LogicalDeleteStatus = LogicalDeleteRequest & {
  requestHash: string;
  nodeId: string;
  nodeDialogId: string;
  registryVersion: number;
  identityEpoch: number;
  holdScopeRevision: number;
  holdVersion?: number;
  phase: 'accepted' | 'holding' | 'reconciling' | 'succeeded' | 'failed';
  effectState: 'not_sent' | 'sent' | 'unknown' | 'reconciled' | 'failed';
  operationVersion: number;
  archived: boolean;
  resultCode?: string;
  nodeReceipt?: LogicalDeleteNodeReceipt;
  updatedAt: string;
};

export type LogicalDeleteNodeReceipt = {
  schemaId: 'logical-dialog-node-delete-v1';
  operationId: string;
  nodeRequestHash: string;
  coordinatorRequestHash: string;
  receiptId: string;
  commandId: string;
  logicalDialogId: string;
  nodeId: string;
  nodeDialogId: string;
  epoch: number;
  registryVersion: number;
  bindingVersion: number;
  deletedDialogVersion: number;
  holdVersion: number;
  holdScopeRevision: number;
  tombstoneEventSeq: number;
  commandReceipt: unknown;
  deletedAt: string;
};

const uuid =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha256 = /^[0-9a-f]{64}$/;
const resultCode = /^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$/;

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function exactKeys(
  value: Record<string, unknown>,
  required: readonly string[],
  optional: readonly string[] = [],
): boolean {
  const keys = Object.keys(value);
  return (
    required.every((key) => key in value) &&
    keys.every((key) => required.includes(key) || optional.includes(key))
  );
}

function positive(value: unknown): value is number {
  return Number.isSafeInteger(value) && Number(value) >= 1;
}

function nonnegative(value: unknown): value is number {
  return Number.isSafeInteger(value) && Number(value) >= 0;
}

export function isValidLogicalDeleteRequest(
  value: unknown,
): value is LogicalDeleteRequest {
  return (
    record(value) &&
    exactKeys(value, [
      'schemaId',
      'operationId',
      'commandId',
      'logicalDialogId',
      'expectedBindingVersion',
      'expectedDialogVersion',
    ]) &&
    value.schemaId === 'logical-dialog-delete-v1' &&
    typeof value.operationId === 'string' &&
    uuid.test(value.operationId) &&
    typeof value.commandId === 'string' &&
    uuid.test(value.commandId) &&
    typeof value.logicalDialogId === 'string' &&
    uuid.test(value.logicalDialogId) &&
    positive(value.expectedBindingVersion) &&
    positive(value.expectedDialogVersion) &&
    value.expectedDialogVersion < Number.MAX_SAFE_INTEGER
  );
}

function dateTime(value: unknown): value is string {
  return typeof value === 'string' && !Number.isNaN(Date.parse(value));
}

function validNodeReceipt(
  value: unknown,
  status: LogicalDeleteStatus,
): value is LogicalDeleteNodeReceipt {
  if (!record(value)) return false;
  const required = [
    'schemaId',
    'operationId',
    'nodeRequestHash',
    'coordinatorRequestHash',
    'receiptId',
    'commandId',
    'logicalDialogId',
    'nodeId',
    'nodeDialogId',
    'epoch',
    'registryVersion',
    'bindingVersion',
    'deletedDialogVersion',
    'holdVersion',
    'holdScopeRevision',
    'tombstoneEventSeq',
    'commandReceipt',
    'deletedAt',
  ] as const;
  if (!exactKeys(value, required) || !record(value.commandReceipt))
    return false;
  const command = value.commandReceipt;
  const commandKeys = [
    'protocolVersion',
    'schemaId',
    'commandId',
    'commandKind',
    'receiptId',
    'acceptedAt',
    'nodeId',
    'eventSeq',
    'result',
    'references',
  ] as const;
  if (!exactKeys(command, commandKeys) || !record(command.references))
    return false;
  return (
    value.schemaId === 'logical-dialog-node-delete-v1' &&
    value.operationId === status.operationId &&
    typeof value.nodeRequestHash === 'string' &&
    sha256.test(value.nodeRequestHash) &&
    value.coordinatorRequestHash === status.requestHash &&
    typeof value.receiptId === 'string' &&
    uuid.test(value.receiptId) &&
    value.commandId === status.commandId &&
    value.logicalDialogId === status.logicalDialogId &&
    value.nodeId === status.nodeId &&
    value.nodeDialogId === status.nodeDialogId &&
    value.epoch === status.identityEpoch &&
    value.registryVersion === status.registryVersion &&
    value.bindingVersion === status.expectedBindingVersion &&
    value.deletedDialogVersion === status.expectedDialogVersion + 1 &&
    value.holdVersion === status.holdVersion &&
    value.holdScopeRevision === status.holdScopeRevision &&
    positive(value.tombstoneEventSeq) &&
    dateTime(value.deletedAt) &&
    command.protocolVersion === 1 &&
    command.schemaId === 'harness-wire-v2' &&
    command.commandId === status.commandId &&
    command.commandKind === 'dialog.delete' &&
    typeof command.receiptId === 'string' &&
    uuid.test(command.receiptId) &&
    command.acceptedAt === value.deletedAt &&
    command.nodeId === status.nodeId &&
    command.eventSeq === value.tombstoneEventSeq &&
    command.result === 'deleted' &&
    exactKeys(command.references, ['dialogId']) &&
    command.references.dialogId === status.nodeDialogId
  );
}

function validStatus(
  value: unknown,
  operationId: string,
): value is LogicalDeleteStatus {
  if (!record(value)) return false;
  const commonKeys = [
    'schemaId',
    'operationId',
    'requestHash',
    'commandId',
    'logicalDialogId',
    'expectedBindingVersion',
    'expectedDialogVersion',
    'nodeId',
    'nodeDialogId',
    'registryVersion',
    'identityEpoch',
    'holdScopeRevision',
    'phase',
    'effectState',
    'operationVersion',
    'archived',
    'updatedAt',
  ] as const;
  if (
    !exactKeys(value, commonKeys, ['holdVersion', 'resultCode', 'nodeReceipt'])
  )
    return false;
  const status = value as Partial<LogicalDeleteStatus>;
  if (
    !(
      status.schemaId === 'logical-dialog-delete-v1' &&
      status.operationId === operationId &&
      typeof status.requestHash === 'string' &&
      sha256.test(status.requestHash) &&
      typeof status.commandId === 'string' &&
      uuid.test(status.commandId) &&
      typeof status.logicalDialogId === 'string' &&
      uuid.test(status.logicalDialogId) &&
      typeof status.nodeId === 'string' &&
      uuid.test(status.nodeId) &&
      typeof status.nodeDialogId === 'string' &&
      uuid.test(status.nodeDialogId) &&
      positive(status.expectedBindingVersion) &&
      positive(status.expectedDialogVersion) &&
      Number(status.expectedDialogVersion) < Number.MAX_SAFE_INTEGER &&
      typeof status.nodeId === 'string' &&
      uuid.test(status.nodeId) &&
      typeof status.nodeDialogId === 'string' &&
      uuid.test(status.nodeDialogId) &&
      positive(status.registryVersion) &&
      positive(status.identityEpoch) &&
      nonnegative(status.holdScopeRevision) &&
      positive(status.operationVersion) &&
      ['accepted', 'holding', 'reconciling', 'succeeded', 'failed'].includes(
        String(status.phase),
      ) &&
      ['not_sent', 'sent', 'unknown', 'reconciled', 'failed'].includes(
        String(status.effectState),
      ) &&
      typeof status.archived === 'boolean' &&
      dateTime(status.updatedAt)
    )
  )
    return false;
  const complete = status as LogicalDeleteStatus;
  switch (complete.phase) {
    case 'accepted':
      return (
        complete.effectState === 'not_sent' &&
        !complete.archived &&
        complete.holdVersion === undefined &&
        complete.resultCode === undefined &&
        complete.nodeReceipt === undefined
      );
    case 'holding':
      return (
        complete.effectState === 'sent' &&
        !complete.archived &&
        positive(complete.holdVersion) &&
        complete.resultCode === undefined &&
        complete.nodeReceipt === undefined
      );
    case 'reconciling':
      return (
        complete.effectState === 'unknown' &&
        !complete.archived &&
        positive(complete.holdVersion) &&
        complete.resultCode === undefined &&
        complete.nodeReceipt === undefined
      );
    case 'succeeded':
      if (complete.effectState !== 'reconciled') return false;
      if (complete.archived) {
        return (
          complete.resultCode === 'archive_tombstoned' &&
          complete.holdVersion === undefined &&
          complete.nodeReceipt === undefined
        );
      }
      return (
        complete.resultCode === 'node_tombstoned' &&
        positive(complete.holdVersion) &&
        validNodeReceipt(complete.nodeReceipt, complete)
      );
    case 'failed':
      return (
        complete.effectState === 'failed' &&
        !complete.archived &&
        (complete.holdVersion === undefined ||
          positive(complete.holdVersion)) &&
        typeof complete.resultCode === 'string' &&
        resultCode.test(complete.resultCode) &&
        complete.nodeReceipt === undefined
      );
  }
}

async function checked(
  value: Promise<unknown>,
  operationId: string,
): Promise<LogicalDeleteStatus> {
  const result = await value;
  if (!validStatus(result, operationId))
    throw new Error('invalid_logical_delete_response');
  return result;
}

export const logicalDeleteAPI = {
  begin: (
    session: Session,
    request: LogicalDeleteRequest,
    signal?: AbortSignal,
  ) =>
    checked(
      api('logical-dialog-deletes', {
        body: request,
        csrf: session.csrf,
        signal,
      }),
      request.operationId,
    ),
  status: (session: Session, operationId: string, signal?: AbortSignal) =>
    checked(
      api(`logical-dialog-deletes/${encodeURIComponent(operationId)}`, {
        signal,
      }),
      operationId,
    ),
};
