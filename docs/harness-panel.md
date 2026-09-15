# Harness Panel integration (HL-253 / U1)

Panel is an authenticated browser Gateway to independently running Harness
nodes. It stores no Harness queue, receipt, execution state, or provider
credential. The wire contract is [harness-v1.md](harness-v1.md); canonical scope
and acceptance remain in [HL-253](https://youtrack.h1-cloud.ru/issue/HL-253).
Provider adapters and deployment are later packages. U1 tests use synthetic
nodes and certificates; they do not establish production readiness.

## Registry and trust configuration

Harness registry is the owner and node source of truth. A production Panel
requires it; no environment, credential, or running worker is discovered
implicitly. For fixture-only development `PANEL_OWNER_ID` may supply the owner
when the registry is empty. To connect nodes, configure all seven absolute paths:

| Variable | File |
|---|---|
| `PANEL_HARNESS_REGISTRY` | signed JSON manifest |
| `PANEL_HARNESS_SIGNER_PUBLIC_KEY` | Ed25519 PKIX `PUBLIC KEY` PEM |
| `PANEL_HARNESS_CA` | node trust CA PEM |
| `PANEL_HARNESS_CLIENT_CERT` | separately provisioned Panel mTLS certificate |
| `PANEL_HARNESS_CLIENT_KEY` | corresponding private key; readable only by the service |
| `PANEL_HARNESS_ROUTER_STATE` | durable Router state on a dedicated private writable mount |
| `PANEL_HARNESS_ROUTER_SOCKET` | private Unix control socket in that same directory |

The legacy registry JSON has exactly `manifest` and `signature`. `manifest` has
`registryVersion` (positive safe integer), `ownerId` (operator-signed opaque
identity), `mode` (`live` or `fixture`), and `nodes`. The signed registry file is
bounded to 256 KiB, but there is no fixed agent-count ceiling; inventory is read
in pages of at most 100 nodes.
Each node has exactly `nodeId` (UUID), `name`, `adapter` (`cursor` or `codex`),
`url` (HTTPS origin without a trailing slash), and `certificateSHA256`
(lowercase SHA-256 of the DER leaf certificate). Node IDs and certificate pins
are unique. A readable name is not an authorization identity.

The Ed25519 signature is standard padded Base64, over the bytes returned by
Go `json.Marshal(harnessclient.Manifest)`. A dynamic manifest uses field order
`schemaId, registryVersion, ownerId, mode, wireSchemaSHA256, nodes`; its node
order is `nodeId, name, adapter, url, certificateSHA256,
registrationRevision, registrationEpoch, compatibility`. Ordered nodes and
Go's default JSON string escaping are part of the signature input. The legacy
field order remains accepted for read compatibility.

`scripts/router_registry_projection.py form-install` is the general local
operator path. It verifies the current signed registry and Router readback,
requires an explicit compatibility choice for legacy nodes, derives the
per-node revision/epoch, signs one additive or sealed-node change, durably
writes the exact CAS request, installs it through the private UDS and compares
the complete node-state readback. The retained mode `0600` request is accepted
by the `install` command for an idempotent retry after a process crash. A
terminal retry returns the immutable Agent Service receipt even when Router has
already advanced through later CAS operations; inspection of current Router
state is a separate diagnostic and cannot rewrite an earlier result. The old
`scripts/registry_transition.py` remains only as the offline legacy rollback
preparation path. The private signer stays in operator preparation and is never
needed by Panel or Gateway.

Registry loading rejects unknown/duplicate fields, missing bindings, and
invalid signatures. R02 also accepts `harness-router-registry-v1` projections
through `POST /v1/registry/install` on the private mode `0600` Router control
socket. The signature additionally covers the exact wire-schema hash and each
node's `registrationRevision`, `registrationEpoch`, and explicit
`compatible`/`legacy_readonly` gate. The install compares the expected global
version and manifest hash, permits at most one added or sealed changed node,
fsyncs one state replacement, verifies readback, and only then swaps routing.
The same operation and projection returns the installed readback; stale or
conflicting versions fail explicitly.

Global `registryVersion` is inventory generation only. The Harness identity
fence for a projected node uses its own `registrationRevision`, so adding one
node does not invalidate commands, dialogs, attempts, or admission state on
neighbors. Old manifests and state schema 1 remain readable; projected state
schema 2 embeds the signed public projection in the private state file for
restart recovery. Operator responses strip endpoints, certificate pins and the
envelope. Every swap drains synchronous calls and closes the prior client's
idle connection pools. Established event streams keep their own connection and
context, while accepted Harness work remains owned by Harness and is not
cancelled by a Router registry swap.

Each private call verifies TLS 1.3, the CA,
hostname, leaf pin, and then the node's exact ID, registry version, schema hash,
and adapter pin. Redirects and proxy environment variables are not used.
Browser headers cannot set the trusted `X-Harness-Actor-ID` or private URL.
The browser sees only version, mode, and node ID/name/adapter.

When `PANEL_AGENT_SERVICE_SOCKET` names the protected mode `0600` R01 Unix
socket, `/api/v2/agents` reads the authenticated owner's node inventory and
`/api/v2/agents/{nodeId}/dialogs` reads permanent mappings. Both use bounded
keyset pages of at most 100 entries. Panel forwards neither a browser-supplied
owner nor database/signing credentials; without this socket it retains the
bounded compatibility read from the signed Harness registry.

The state directory is owned by the Panel UID with mode `0700`; state, lock and
socket are owner-only. One process holds an exclusive lock for its lifetime.
Missing, corrupt or registry-mismatched state prevents Panel startup. The first
state is created only by `router-bootstrap` while the old Panel is stopped and
starts sealed, so a restart never silently reopens admission.
On the first R02 start, a valid schema 1 state may upgrade its existing empty R01
lock in place and bind it to a private hard-link guard. Projected schema 2 state
never recreates or adopts a missing guard.

## Browser requests and command outcomes

Routes are below `PANEL_BASE_PATH + /api/v2/harness`. `/nodes` returns the public
registry. All node routes begin `/nodes/{nodeId}` and mirror the C1 read,
command, history, and event routes. Health is also node scoped:
`/nodes/{nodeId}/health/ready` and `/nodes/{nodeId}/health/live`.
NPM Access List authenticates the owner and overwrites
`X-Panel-Authenticated-User`. Panel automatically exchanges that trusted edge
identity for its session cookie; exact Origin/Host, same-site checks and
`X-Panel-CSRF` protect requests. `ownerId` always comes from the signed registry,
never from the browser or YouTrack. `PANEL_HARNESS_COMMANDS_ENABLED` remains
required for mutations; adding a registry does not enable commands.

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

R02 adds `go test -race ./internal/harnessrouter ./internal/dockeradapter`.
The adapter test backend performs no Docker action: it proves a real private
file lock, sent-before-call fsync journal, lost-ACK restart reconciliation,
stable receipts, stale-generation rejection and fail-closed journal loss. The
journal identity is created only by the explicit one-time `journal-init`
command; normal `journal-check`/`fixture-once` execution never recreates a
missing tuple. Every unresolved `sent` file is anchored by a second hard link,
and path loss or inode replacement blocks the effect boundary until explicit
reconciliation restores the exact guarded inode. Worker proof is bound to the
complete stored operation intent. If local resolution became durable while its
completion response was ambiguous, a restart can publish only the exact
acknowledged replay as `reconciled`, without invoking the backend again.
`cmd/homelab-docker-adapter` is a separate binary; Docker build/create and real
lifecycle effects remain later-stage work.
The fixture executor rechecks the current DB-backed worker authority immediately
before its local effect boundary. A future real backend must also bind the lease
generation to the external effect atomically, or use ownership that cannot
expire between that check and the engine commit; a preflight check alone is not
a sufficient production fence.
