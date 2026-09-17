# agent-service R01–R02 + R07

`agent-service` owns the R01 PostgreSQL metadata/read model and the R02 durable
administrative operation records and the R07 safe Docker host read model. It
does not connect directly to Harness, Docker, provider APIs or secret stores
and does not execute lifecycle actions. R07 secret input and read-only probes
are forwarded over a private Unix socket to the separate Docker adapter.

One hundred agents across ten hosts is the R01 verification scale, not a
product-wide inventory ceiling. Imports are resource-bounded by the signed
registry and snapshot byte limits; inventory transport remains paginated at a
maximum of 100 items per response.

## Commands

- `agent-service migrate` applies forward-only embedded migrations under a
  PostgreSQL advisory lock and verifies their checksums.
- `agent-service import` verifies the existing Ed25519-signed Harness registry,
  validates a complete `agent-registry-import-v1` snapshot and commits the
  import atomically. Reimport preserves `logicalDialogId` and
  `bindingVersion`; version rollback and same-version manifest conflicts fail.
- `agent-service serve` exposes health, node inventory, separately paginated
  permanent dialog bindings, durable operation acceptance and status over a
  mode `0600` Unix socket.
  Node pages contain a bounded `dialogCount`; mapping pages are owner-and-node
  scoped and contain at most 100 bindings. Panel supplies the owner from its
  authenticated server-side session.

R07 adds owner-scoped, versioned local/SSH host descriptors, opaque credential
references and safe observations. `POST /internal/v1/host-secrets` forwards a
bounded write-only body under a client-generated provisioning `operationId`.
Exact replay returns the same opaque receipt; read-only
`GET /internal/v1/host-secrets/{operationId}` recovers that receipt after a
lost ACK without returning secret bytes. A changed payload under the same ID
fails with `secret_operation_conflict`;
`POST /internal/v1/hosts/{hostId}/probe` performs exact-version readback,
delegates the read-only probe to the adapter, then persists either the verified
identity/capabilities or a closed failure code and next action. Agent Service
never receives a Docker socket path, SSH private-key path or decryptable secret
store file.
Host reads remain paginated and resource-bounded; ten hosts is an acceptance
scale, not a product ceiling.

`POST /internal/v1/operations` commits the exact normalized intent, assigned
generation, initial step and immutable receipt before returning `202`. Reusing
an `operationId` with the same payload returns that receipt; changing the
payload, target `registrationRevision`, or expected generation returns a
conflict. `GET /internal/v1/operations/{operationId}` is readback only. Unknown
effects stay active in `reconciling`, so another operation for that node cannot
advance until an explicit status reconciliation reaches a known result. Worker
authority carries the stored request hash and is valid only for the complete
claimed intent. An exact acknowledged-journal replay may close a retained
`unknown` state only as `reconciled`; the generic `unknown -> acknowledged`
transition remains forbidden. Signed
registry envelopes and registry-operation intents use PostgreSQL `json` storage
so their verified representation remains byte-bounded and recoverable; effect
metadata that does not cross a signature/size boundary remains `jsonb`.

Migrations `002_r02_operations.sql` and `003_r07_hosts.sql` are additive. Per-node
`registration_revision` changes only when that node's verified binding or
explicit compatibility mode changes; global registry versions, dialog
identities and other nodes' operation generations remain independent. R02
operations stay fenced until the first post-migration import initializes that
exact per-node binding hash, and fixture effects are rejected for
`legacy_readonly` nodes.

Configuration is passed through `AGENT_SERVICE_DATABASE_URL` plus the
command-specific `AGENT_SERVICE_SOCKET`, `AGENT_SERVICE_REGISTRY`,
`AGENT_SERVICE_SIGNER_PUBLIC_KEY`, `AGENT_SERVICE_IMPORT_SNAPSHOT` and
`AGENT_SERVICE_WORKER_TOKEN`. R10 external enrollment additionally requires an
`AGENT_SERVICE_SIGNER_PRIVATE_KEY` matching the configured public key; it signs
only the additive candidate registry and is never available to Panel. R07 host flows additionally require the pair
`AGENT_SERVICE_DOCKER_ADAPTER_SOCKET` and `AGENT_SERVICE_DOCKER_ADAPTER_TOKEN`;
omitting both leaves those write/probe routes fail-closed. The worker token,
adapter token and signer belong only to trusted service boundaries. Panel may
receive the worker token through its server-side secret mechanism for R10
orchestration; none of these secrets are exposed to a browser.
The database URL may contain a password and must be supplied by the runtime
secret mechanism; it must not be placed in command arguments or logs.

## Local PostgreSQL gate

Use an isolated loopback-only container with disposable storage. A typical
local gate is:

```sh
docker pull postgres:17-alpine
docker run --rm --detach --name hl283-r02-postgres \
  --publish 127.0.0.1::5432 \
  --mount type=tmpfs,destination=/var/lib/postgresql/data,tmpfs-size=536870912 \
  --env POSTGRES_HOST_AUTH_METHOD=trust \
  --env POSTGRES_DB=agent_service_test postgres:17-alpine
docker port hl283-r02-postgres 5432/tcp
```

Export `AGENT_SERVICE_TEST_DATABASE_URL` using the assigned loopback port, then
run `make agentservice-integration`. Stop the named container after the gate;
`--rm` removes its disposable state.

Production migration is intentionally not automatic. Before applying it,
record the exact database target, take and verify a restorable backup, run
`migrate`, verify `/internal/v1/healthz`, and retain database restore plus the
previous service/Panel artifacts as rollback. R01 has no down migration.
