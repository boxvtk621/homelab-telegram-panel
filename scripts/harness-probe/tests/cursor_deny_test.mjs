import test from 'node:test';
import assert from 'node:assert/strict';
import { observePolicy, safeFailureReport, cancelUnstartedRun } from '../cursor_deny_probe.mjs';

test('preparation never cancels an already started or checkpointed run', async () => {
  for (const before of [{status:'running'}, {status:'queued',startedAt:1}, {status:'queued',latestCheckpointRef:{}}]) {
    let cancelled = false;
    const options = {local:{store:{agents:{get:async()=>({activeRunId:'initial'})},runs:{get:async()=>before}}}};
    await assert.rejects(cancelUnstartedRun({Agent:{cancelRun:async()=>{cancelled=true;}}},{agentId:'new-agent'},options));
    assert.equal(cancelled,false);
  }
  let cancelled = false;
  const options = {local:{store:{agents:{get:async()=>({activeRunId:'initial'})},runs:{get:async()=>({status:cancelled?'cancelled':'queued'})}}}};
  await cancelUnstartedRun({Agent:{cancelRun:async(id)=>{assert.equal(id,'initial');cancelled=true;}}},{agentId:'new-agent'},options);
  assert.equal(cancelled,true);
});

test('failure diagnostics keep the reserved count and never include error payloads', () => {
  const state = { phase: 'resume', sdkSendCountBefore: 8, sdkSendCountAfter: 9, operation: 'stream_wait' };
  const error = new TypeError('credential and raw provider payload');
  error.cause = { authorization: 'secret' };
  const value = safeFailureReport(error, state);
  assert.equal(value.sdkSendCountAfter, 9);
  assert.deepEqual(value.diagnostic, { code: 'native_probe_failed', operation: 'stream_wait', errorClass: 'TypeError' });
  assert.equal(JSON.stringify(value).includes('secret'), false);
  assert.equal(JSON.stringify(value).includes('credential'), false);
});

test('policy observer reads only the native allowlist and rejects shell or partial toolsets', () => {
  const proto = 'mcp_tool_call,get_mcp_tools_tool_call,list_mcp_resources_tool_call,read_mcp_resource_tool_call,mcp_auth_tool_call';
  const evidence = { policyRequests: 0, exactMcpPolicyOnEveryRequest: true };
  observePolicy(new Proxy({ 'x-cursor-agent-allowed-tools': proto }, { get(target, key) {
    assert.equal(key, 'x-cursor-agent-allowed-tools');
    return target[key];
  } }), evidence);
  assert.deepEqual(evidence, { policyRequests: 1, exactMcpPolicyOnEveryRequest: true });
  observePolicy({ 'x-cursor-agent-allowed-tools': proto + ',shell_tool_call' }, evidence);
  assert.equal(evidence.exactMcpPolicyOnEveryRequest, false);
  for (const value of ['', 'mcp', ['mcp_tool_call']]) {
    const result = { policyRequests: 0, exactMcpPolicyOnEveryRequest: true };
    observePolicy({ 'x-cursor-agent-allowed-tools': value }, result);
    assert.equal(result.exactMcpPolicyOnEveryRequest, false);
  }
  const absent = { policyRequests: 0, exactMcpPolicyOnEveryRequest: true };
  observePolicy({}, absent);
  assert.equal(absent.policyRequests, 0);
});
