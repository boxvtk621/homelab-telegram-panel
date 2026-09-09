# Harness probe fixture tools

`fixture_server.py` is a Python-stdlib, line-delimited JSON-RPC 2.0 MCP fixture
for local SDK probes. It is test infrastructure, not a Harness API or SDK PASS.

Run it with:

```sh
python3 -u scripts/harness-probe/tools/fixture_server.py
```

Supported methods are `initialize`, `notifications/initialized`, `tools/list`,
and `tools/call`; `ping` is also supported. Every tool call requires a bounded
opaque `correlationId` in `arguments`; its response contains that value plus a deterministic `callId`, allowing a probe to
correlate its request and result.

The allowlist intentionally contains only:

- `marker_echo` with a 1..256 byte UTF-8 marker;
- `bounded_long_wait` with `delayMs` in 1..10000;
- `deterministic_error`, which returns a stable tool-level error.

For example:

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"marker_echo","arguments":{"correlationId":"probe-001","marker":"marker-a"}}}
```

`bounded_long_wait` emits `notifications/message` with
`event=fixture_long_wait_started` before sleeping and runs in its own request
thread, so a client can observe the start and issue `ping` while it waits.
Terminate the fixture process to exercise client cancellation. There is no
denied-effect tool: a request for one fails with JSON-RPC `-32601`. The fixture contains no network, shell, file,
provider, or production side effect. Invalid, over-bounded, or extra inputs fail
closed with `-32602`.
