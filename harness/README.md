# Harness node

This module contains the B1 supervisor, SQLite store, private HTTP/SSE server,
and deterministic adapter fixtures for [HL-252](https://youtrack.h1-cloud.ru/issue/HL-252).
The parent module contains the shared C1 contract and the Panel client. The Go
workspace connects these modules; Panel does not import the node or its SQLite
driver. Native Cursor/Codex adapters and the production entry point are separate
implementation packages tracked by HL-257, HL-258, and HL-259.

## Execution and recovery

`node.Open` starts the supervisor. A committed enqueue wakes FIFO dispatch;
there is one active attempt per node. `ManualDispatchForTesting` disables only
automatic dispatch and must remain false in production configuration. Tests
that control ordering explicitly opt in. Native calls execute outside SQLite
transactions and the node lock. Stop has a reserved control worker, persists
manual pause before cancellation, and keeps the slot until terminal evidence.
After a terminal transaction commits, the supervisor closes only that attempt's
event stream so the next queued request can execute. Unknown state keeps the
active slot and its generation fence.

Receipts, domain changes, and events commit together. A lost response is resolved
by command status or an explicit replay of the same command ID and payload.
The client must not invent a receipt or create a replacement intent.

Opening existing state preserves manual pause and the event epoch. A recovered
active attempt becomes unknown and retains the slot; it is never automatically
resent. Readiness checks cannot release this fence. `ReconcileUnknown` is an
in-process host operation with an exact attempt/generation reference, not an
HTTP repair route. Only confirmed terminal evidence with resolved effects can
release the slot. Pending or unknown steer, approval, input, and tool effects
still prevent release. Earlier assistant messages are preserved when a
reconciled final response must be appended after a crash.

## Storage and output

The driver and accepted SQLite runtime are pinned. Startup verifies schema,
identity, foreign keys, DELETE journal mode, EXTRA synchronization, file
ownership, and exclusive volume locking. Control writes can release a verified
physical 8 MiB reserve before their first write. An uncertain database commit
is not retried as a new transaction.

Adapters submit redacted artifact bytes through the constructor-injected
`ArtifactSink`. A file and its containing directory are synchronized before its
reference is committed. Missing or corrupt bytes make both metadata and download
fail. An uncertain commit may leave an orphan file; deleting potentially
referenced bytes would be unsafe.

The attempt output budget is 64 MiB, with at most 16 MiB per artifact. Each
distinct persisted provider text contributes its UTF-8 bytes, including stream
deltas, complete messages, tool inputs/results, and interaction prompts, including
suppressed and late observations. Artifact bytes contribute once when stored;
later references do not charge them again. This is a conservative
persistence budget, not a provider token or billing counter. Duplicate event
replay does not consume it. Exceeding the budget records an explicit truncated
`output_limit` marker. Cancellation is requested only for the exact active
attempt when its state permits it; late output cannot stop a newer attempt.
Cancellation ACK alone still does not prove terminal state.

## Local verification

From this module in its linked worktree, with a writable external Go cache:

```sh
GOMAXPROCS=2 go vet -p=2 ./...
GOMAXPROCS=2 go test -p=2 -race -timeout=60s ./...
```

The integration suite uses the real node, SQLite, mTLS server, and Panel client;
only the provider adapter is synthetic. Crash tests terminate their own child
processes. The Linux physical-full-disk test is opt-in and must run only on its
disposable 24–64 MiB tmpfs with an external watchdog, never a host data volume.
Fixture success does not establish native-provider or deployment acceptance.
