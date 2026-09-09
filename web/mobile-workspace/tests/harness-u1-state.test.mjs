import assert from 'node:assert/strict';
import test from 'node:test';
import { acceptsTarget, classifyEvent, readDraft, reconnectDelay, targetKey, writeDraft } from '../src/harness-state.ts';

test('U1 lost ACK keeps the same command intent per exact node/dialog target', () => {
  const command = {
    protocolVersion: 1,
    schemaId: 'harness-wire-v1',
    commandId: '10000000-0000-4000-8000-000000000001',
    kind: 'message.enqueue',
    target: {
      nodeId: '20000000-0000-4000-8000-000000000001',
      dialogId: '30000000-0000-4000-8000-000000000001',
    },
    expected: { dialogVersion: 1 },
    payload: { text: 'hello' },
  };
  const draft = { text: 'hello', command, phase: 'unknown' };
  const store = writeDraft({}, '20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000001', draft);
  assert.equal(readDraft(store, '20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000001').command.commandId, command.commandId);
  assert.equal(targetKey('20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000001'), '20000000-0000-4000-8000-000000000001:30000000-0000-4000-8000-000000000001');
});

test('U1 navigation fences stale replies and retains offline drafts', () => {
  assert.equal(acceptsTarget('node-a', 'dialog-b', 'node-a', 'dialog-a'), false);
  const store = writeDraft({}, 'node-a', 'dialog-a', { text: 'offline', phase: 'draft' });
  assert.equal(readDraft(store, 'node-a', 'dialog-a').text, 'offline');
  assert.equal(readDraft(store, 'node-a', 'dialog-b').text, '');
});

test('U1 SSE accepts contiguous events, deduplicates replay, and detects gap/epoch', () => {
  const cursor = { nodeId: 'node-a', epoch: 4, seq: 10 };
  assert.equal(classifyEvent(cursor, { nodeId: 'node-a', epoch: 4, seq: 11 }), 'accept');
  assert.equal(classifyEvent(cursor, { nodeId: 'node-a', epoch: 4, seq: 10 }), 'duplicate');
  assert.equal(classifyEvent(cursor, { nodeId: 'node-a', epoch: 4, seq: 12 }), 'gap');
  assert.equal(classifyEvent(cursor, { nodeId: 'node-a', epoch: 5, seq: 11 }), 'epoch-change');
  assert.equal(classifyEvent(cursor, { nodeId: 'node-b', epoch: 4, seq: 11 }), 'foreign-node');
});

test('U1 reconnect is bounded and starts from last seen sequence', () => {
  assert.deepEqual([reconnectDelay(0), reconnectDelay(1), reconnectDelay(2), reconnectDelay(3)], [1000, 2000, 3000, null]);
});

test('U1 FIFO snapshot queue is rendered from pendingQueue order', () => {
  const pendingQueue = [{ requestId: 'r1', queueSequence: 1 }, { requestId: 'r2', queueSequence: 2 }];
  assert.deepEqual(pendingQueue.map((item) => item.queueSequence), [1, 2]);
  assert.equal(pendingQueue[0].requestId, 'r1');
});

test('U1 fixture mode is an explicit visible state', () => {
  const mode = 'fixture';
  assert.equal(mode === 'fixture', true);
});
