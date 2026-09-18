import { afterEach, describe, expect, it, vi } from 'vitest';
import type { DeleteCommand, MessageCommand } from '../src/harness-state';
import type { LogicalDeleteRequest } from '../src/logical-delete-api';
import {
  clearPanelSessionState,
  readLogicalDraft,
  readPanelSessionState,
  resolveLatestBinding,
  updatePanelSessionState,
  type DialogBinding,
} from '../src/panel-session-state';

const ownerA = 'owner-a';
const ownerB = 'owner-b';
const nodeA = '10000000-0000-4000-8000-000000000001';
const nodeB = '10000000-0000-4000-8000-000000000002';
const dialogA = '20000000-0000-4000-8000-000000000001';
const dialogB = '20000000-0000-4000-8000-000000000002';
const logical = '30000000-0000-4000-8000-000000000001';
const operation = '50000000-0000-4000-8000-000000000001';

function binding(
  nodeId = nodeA,
  nodeDialogId = dialogA,
  bindingVersion = 1,
): DialogBinding {
  return { nodeId, nodeDialogId, logicalDialogId: logical, bindingVersion };
}

function command(): MessageCommand {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    commandId: '40000000-0000-4000-8000-000000000001',
    kind: 'message.enqueue',
    target: { nodeId: nodeA, dialogId: dialogA },
    expected: { dialogVersion: 7 },
    payload: { text: 'точный исходный текст' },
  };
}

function deleteCommand(): DeleteCommand {
  return {
    protocolVersion: 1,
    schemaId: 'harness-wire-v2',
    commandId: '40000000-0000-4000-8000-000000000002',
    kind: 'dialog.delete',
    target: { nodeId: nodeA, dialogId: dialogA },
    expected: { dialogVersion: 7 },
    payload: {},
  };
}

function deleteRequest(): LogicalDeleteRequest {
  return {
    schemaId: 'logical-dialog-delete-v1',
    operationId: operation,
    commandId: deleteCommand().commandId,
    logicalDialogId: logical,
    expectedBindingVersion: 3,
    expectedDialogVersion: 7,
  };
}

afterEach(() => {
  clearPanelSessionState(ownerA);
  clearPanelSessionState(ownerB);
  vi.restoreAllMocks();
});

describe('R03 tab-scoped state', () => {
  it('keeps selection and drafts isolated by owner and logical dialog', () => {
    updatePanelSessionState(ownerA, (current) => ({
      ...current,
      view: 'interaction',
      interactionNodeId: nodeA,
      interaction: binding(),
      drafts: {
        ...current.drafts,
        [logical]: { text: 'черновик A', phase: 'draft' },
      },
    }));

    const restored = readPanelSessionState(ownerA).state;
    expect(restored.view).toBe('interaction');
    expect(restored.interaction).toEqual(binding());
    expect(readLogicalDraft(restored, logical).text).toBe('черновик A');
    expect(
      readLogicalDraft(readPanelSessionState(ownerB).state, logical).text,
    ).toBe('');
    expect(localStorage.length).toBe(0);
  });

  it('restores an interrupted send as unknown with the exact original command', () => {
    const original = command();
    updatePanelSessionState(ownerA, (current) => ({
      ...current,
      drafts: {
        [logical]: {
          text: original.payload.text,
          phase: 'sending',
          command: original,
        },
      },
    }));

    const restored = readLogicalDraft(
      readPanelSessionState(ownerA).state,
      logical,
    );
    expect(restored.phase).toBe('unknown');
    expect(restored.command).toEqual(original);
    expect(restored.text).toBe(original.payload.text);
  });

  it('restores an interrupted delete as unknown with the exact operation and command', () => {
    const originalCommand = deleteCommand();
    const originalRequest = deleteRequest();
    const key = `${nodeA}:${dialogA}`;
    updatePanelSessionState(ownerA, (current) => ({
      ...current,
      deletes: {
        [key]: {
          phase: 'sending',
          command: originalCommand,
          request: originalRequest,
        },
      },
    }));

    const restored = readPanelSessionState(ownerA).state.deletes[key];
    expect(restored).toEqual({
      phase: 'unknown',
      command: originalCommand,
      request: originalRequest,
      error: 'Результат удаления проверяется после восстановления вкладки.',
    });
  });

  it('keeps an in-memory copy and reports when sessionStorage is unavailable', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('quota', 'QuotaExceededError');
    });

    const saved = updatePanelSessionState(ownerA, (current) => ({
      ...current,
      drafts: {
        [logical]: { text: 'только в памяти', phase: 'draft' },
      },
    }));
    expect(saved.persistent).toBe(false);
    const restored = readPanelSessionState(ownerA);
    expect(restored.persistent).toBe(false);
    expect(readLogicalDraft(restored.state, logical).text).toBe(
      'только в памяти',
    );
  });

  it('does not roll a fresh memory draft back to stale storage after quota failure', () => {
    updatePanelSessionState(ownerA, (current) => ({
      ...current,
      drafts: {
        [logical]: { text: 'старая сохранённая копия', phase: 'draft' },
      },
    }));
    const write = vi
      .spyOn(Storage.prototype, 'setItem')
      .mockImplementation(() => {
        throw new DOMException('quota', 'QuotaExceededError');
      });

    const failedWrite = updatePanelSessionState(ownerA, (current) => ({
      ...current,
      drafts: {
        [logical]: { text: 'свежий черновик в памяти', phase: 'draft' },
      },
    }));
    expect(failedWrite.persistent).toBe(false);
    const memoryRead = readPanelSessionState(ownerA);
    expect(memoryRead.persistent).toBe(false);
    expect(readLogicalDraft(memoryRead.state, logical).text).toBe(
      'свежий черновик в памяти',
    );

    write.mockRestore();
    const recovered = updatePanelSessionState(ownerA, (current) => current);
    expect(recovered.persistent).toBe(true);
    expect(
      readLogicalDraft(readPanelSessionState(ownerA).state, logical).text,
    ).toBe('свежий черновик в памяти');
  });

  it('rebinds one logical dialog only to a newer binding', () => {
    expect(
      resolveLatestBinding(binding(), [
        binding(nodeB, dialogB, 3),
        {
          nodeId: nodeB,
          nodeDialogId: dialogB,
          logicalDialogId: '30000000-0000-4000-8000-000000000002',
          bindingVersion: 9,
        },
      ]),
    ).toEqual(binding(nodeB, dialogB, 3));
  });
});
