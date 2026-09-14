# agent-service R01

`agent-service` owns only the R01 PostgreSQL metadata and the owner-scoped
read model. It does not connect to Harness, Docker, provider APIs or secret
stores and does not execute lifecycle actions.

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
- `agent-service serve` exposes read-only health, node inventory, and separately
  paginated permanent dialog-binding endpoints over a mode `0600` Unix socket.
  Node pages contain a bounded `dialogCount`; mapping pages are owner-and-node
  scoped and contain at most 100 bindings. Panel supplies the owner from its
  authenticated server-side session.

Configuration is passed through `AGENT_SERVICE_DATABASE_URL` plus the
command-specific `AGENT_SERVICE_SOCKET`, `AGENT_SERVICE_REGISTRY`,
`AGENT_SERVICE_SIGNER_PUBLIC_KEY` and `AGENT_SERVICE_IMPORT_SNAPSHOT`. The
database URL may contain a password and must be supplied by the runtime secret
mechanism; it must not be placed in command arguments or logs.

## Local PostgreSQL gate

Use an isolated loopback-only container with disposable storage. A typical
local gate is:

```sh
docker pull postgres:17-alpine
docker run --rm --detach --name hl282-r01-postgres \
  --publish 127.0.0.1::5432 \
  --mount type=tmpfs,destination=/var/lib/postgresql/data,tmpfs-size=536870912 \
  --env POSTGRES_HOST_AUTH_METHOD=trust \
  --env POSTGRES_DB=agent_service_test postgres:17-alpine
docker port hl282-r01-postgres 5432/tcp
```

Export `AGENT_SERVICE_TEST_DATABASE_URL` using the assigned loopback port, then
run `make agentservice-integration`. Stop the named container after the gate;
`--rm` removes its disposable state.

Production migration is intentionally not automatic. Before applying it,
record the exact database target, take and verify a restorable backup, run
`migrate`, verify `/internal/v1/healthz`, and retain database restore plus the
previous service/Panel artifacts as rollback. R01 has no down migration.
