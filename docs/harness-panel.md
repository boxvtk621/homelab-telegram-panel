# Harness Panel integration (HL-253 / U1)

Panel is an authenticated browser Gateway to independently running Harness
nodes. It stores no Harness queue, receipt, execution state, or provider
credential. The wire contract is [harness-v1.md](harness-v1.md); canonical scope
and acceptance remain in [HL-253](https://youtrack.h1-cloud.ru/issue/HL-253).
Provider adapters and deployment are later packages. U1 tests use synthetic
nodes and certificates; they do not establish production readiness.

## Registry and trust configuration

With no Harness paths configured, the registry is empty. Panel remains usable
for YouTrack. No environment, credentials, or running worker are discovered
implicitly. To connect nodes, configure all seven absolute paths:

| Variable | File |
|---|---|
| `PANEL_HARNESS_REGISTRY` | signed JSON manifest |
| `PANEL_HARNESS_SIGNER_PUBLIC_KEY` | Ed25519 PKIX `PUBLIC KEY` PEM |
| `PANEL_HARNESS_CA` | node trust CA PEM |
| `PANEL_HARNESS_CLIENT_CERT` | separately provisioned Panel mTLS certificate |
| `PANEL_HARNESS_CLIENT_KEY` | corresponding private key; readable only by the service |
| `PANEL_HARNESS_ROUTER_STATE` | durable Router state on a dedicated private writable mount |
| `PANEL_HARNESS_ROUTER_SOCKET` | private Unix control socket in that same directory |

The registry JSON has exactly `manifest` and `signature`. `manifest` has
`registryVersion` (positive safe integer), `ownerId` (the authenticated
YouTrack user's opaque ID), `mode` (`live` or `fixture`), and `nodes` (0–16).
Each node has exactly `nodeId` (UUID), `name`, `adapter` (`cursor` or `codex`),
`url` (HTTPS origin without a trailing slash), and `certificateSHA256`
(lowercase SHA-256 of the DER leaf certificate). Node IDs and certificate pins
are unique. A readable name is not an authorization identity.

The Ed25519 signature is standard padded Base64, over the bytes returned by
Go `json.Marshal(harnessclient.Manifest)`. The struct field order is
`registryVersion, ownerId, mode, nodes`; node field order is
`nodeId, name, adapter, url, certificateSHA256`. Ordered nodes and Go's default
JSON string escaping are part of the signature input. Sign with the same Go
types to avoid cross-language canonicalization differences. Provisioning and
signing tooling belongs to operator preparation (O1); the private signer is
never needed by Panel. Configuring a path does not provision or rotate keys.

Registry loading rejects unknown/duplicate fields, missing bindings, and
invalid signatures. Loading is static: an operator applies a new signed
version through a service restart. Each private call verifies TLS 1.3, the CA,
hostname, leaf pin, and then the node's exact ID, registry version, schema hash,
and adapter pin. Redirects and proxy environment variables are not used.
Browser headers cannot set the trusted `X-Harness-Actor-ID` or private URL.
The browser sees only version, mode, and node ID/name/adapter.

The state directory is owned by the Panel UID with mode `0700`; state, lock and
socket are owner-only. One process holds an exclusive lock for its lifetime.
Missing, corrupt or registry-mismatched state prevents Panel startup. The first
state is created only by `router-bootstrap` while the old Panel is stopped and
starts sealed, so a restart never silently reopens admission.

## Browser requests and command outcomes

Routes are below `PANEL_BASE_PATH + /api/v2/harness`. `/nodes` returns the public
registry. All node routes begin `/nodes/{nodeId}` and mirror the C1 read,
command, history, and event routes. Health is also node scoped:
`/nodes/{nodeId}/health/ready` and `/nodes/{nodeId}/health/live`.
Existing owner login, session cookie, exact Origin/Host, same-site checks,
and `X-Panel-CSRF` protect requests. `PANEL_WRITES_ENABLED` remains required
for mutations; adding a registry does not enable writes.

`POST /nodes/{nodeId}/commands` sends one exact C1 command. The verified live
handshake must also match Router's durable identity epoch and adapter version
before Gateway performs one POST; that POST carries the expected routing
identity, which Harness compares under its admission lock. A node restart in
either side of the handshake therefore returns `409 stale` and closes admission
until an exact operator activation. Only a valid receipt bound to
the sent command ID, kind, node and target references is an acknowledgement.
There is no automatic POST retry. Transport failure or a malformed/unbound
receipt after POST is `503 node_unavailable`: admission may have happened.
The browser retains the same intent and command ID, checks the scoped command
status, and requires an explicit resend of that same intent if necessary.
Definite C1 rejection preserves the draft without claiming it was queued.
Status reconciliation also binds `canonicalPayloadHash` to the complete retained
command, including expected versions and payload. Following the C1 scenarios,
this is SHA-256 of compact UTF-8 JSON with recursively sorted object keys,
unchanged array order, safe integer values, and no string normalization or HTML
escaping. A hash mismatch leaves the draft and command outcome unknown.

Router deployment drain is not Harness queue pause. `draining` rejects
`dialog.create`, `message.enqueue`, `attempt.retry` and `queue.resume` with the
existing non-retryable `409 stale`, while exact steer/cancel/stop/approval/input
controls remain available. `sealed` rejects every mutation. The transition to
sealed rechecks a complete snapshot with online/ready transport, idle occupancy,
no active attempt and `pendingCount=0`; `queuePaused` is deliberately irrelevant.
All nodes affected by a Panel replacement transition through one batch CAS and
one durable state rename, so partial multi-node reopen is impossible. A storage
error after the rename poisons the running Router: control status and commands
fail closed until restart reloads and validates the committed file.

## Streams, downloads and resource bounds

Bootstrap reads identity and an atomic snapshot, then joins
`/nodes/{nodeId}/events?after=<lastEventSeq>`. Frames use `id: <seq>` and one
`data: <C1 JSON event>` line. Heartbeats are comments. Duplicate sequences are
ignored; a gap, foreign node, new epoch or malformed frame closes the stream
and requires a fresh baseline. Reconnection never issues an execution command.

Gateway budgets are eight ordinary requests, two controls, four browser
streams and four command-body readers. Body readers release their permit
before dispatch; JSON is capped at 8 MiB and the HTTP server's read timeout
bounds slow requests. Streams do not take ordinary/control slots. Logout is
a bounded local revocation independent of every remote-work gate. Polling and
stream heartbeats observe session expiry without renewing idle TTL. Revocation
closes browser and upstream event transport within one second, without stopping
the node's execution. SSE setup/error bodies are bounded to two seconds;
successful streams have bounded per-write deadlines and five-second heartbeats.

`/nodes/{nodeId}/artifacts/{artifactId}/metadata` returns scoped C1 metadata.
The adjacent `/artifacts/{artifactId}` path returns authenticated bytes. Gateway
reads at most the declared 16 MiB limit, verifies complete size and SHA-256, and
only then writes bytes to the browser. Metadata has a two-second read budget;
the binary transfer has a separate 15-second budget. Corruption or unavailable
durable bytes returns `503 not_durable` without partial success. A single byte
range is sliced from the verified buffer (206 and matching Content-Range);
invalid/multiple ranges return 416. Downloads always use attachment disposition
and octet-stream to prevent active content from rendering in Panel's origin.
The browser additionally binds metadata to the selected history entry and
verifies size/hash before offering a downloaded file.

## Verification

`go test -race ./internal/harnessclient ./internal/panel` covers synthetic mTLS,
signed bindings, forged browser actor, exact receipt/read scope, lost ACK with
one POST, error/status pairs, replay/gap/epoch, stalled SSE error bodies,
pre-body capacity, logout under saturated budgets, idle expiry and artifact
integrity/ranges. Frontend tests cover target changes, command ambiguity and
stream recovery. The final integration gate includes the frozen C1 corpus,
frontend lint/typecheck/tests, generated assets parity and standalone Go build.
