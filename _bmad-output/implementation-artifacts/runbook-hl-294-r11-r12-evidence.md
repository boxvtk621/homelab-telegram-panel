# HL-294 R11-R12 disposable evidence runbook

Run from the repository worktree. Supply `TASK_DB_PASSWORD` and `TASK_WORKER_TOKEN` through the local secret mechanism; do not print or persist them. Every Docker name and filesystem path below is task-local.

```sh
set -eu
TASK_CONTAINER=codex-hl294-r11r12-pg
TASK_VOLUME=codex-hl294-r11r12-pgdata
TASK_SOCKET_DIR=/private/tmp/codex-hl294-agent-service
TASK_SOCKET="$TASK_SOCKET_DIR/agent-service.sock"
TASK_NODE_MODULES=web/mobile-workspace/node_modules
TASK_RESTORE_DB=hl294_restore
TASK_RESTART_OWNER="hl294-restart-$(date +%s)"
TASK_RESTART_RUN="hl294-restart-run-$(date +%s)"
TASK_COMPONENT_OWNER="hl294-component-$(date +%s)"
TASK_AGENT_PID=

cleanup_hl294() {
  set +e
  if test -n "$TASK_AGENT_PID" && kill -0 "$TASK_AGENT_PID" 2>/dev/null; then
    kill "$TASK_AGENT_PID"
    wait "$TASK_AGENT_PID"
  fi
  if test -n "$(docker ps -aq --filter "name=^/${TASK_CONTAINER}$")"; then docker rm -f "$TASK_CONTAINER"; fi
  if test -n "$(docker volume ls -q --filter "name=^${TASK_VOLUME}$")"; then docker volume rm "$TASK_VOLUME"; fi
  test ! -e "$TASK_SOCKET" || unlink "$TASK_SOCKET"
  test ! -e "$TASK_SOCKET.lock" || unlink "$TASK_SOCKET.lock"
  test ! -e "$TASK_SOCKET_DIR/agent-service" || unlink "$TASK_SOCKET_DIR/agent-service"
  test ! -e "$TASK_SOCKET_DIR/agent-service.log" || unlink "$TASK_SOCKET_DIR/agent-service.log"
  test ! -d "$TASK_SOCKET_DIR" || rmdir "$TASK_SOCKET_DIR"
  test ! -e "$TASK_NODE_MODULES" || rm -rf "$TASK_NODE_MODULES"
  set -e
}

wait_postgres_hl294() {
  TASK_WAIT=0
  while ! docker exec "$TASK_CONTAINER" pg_isready -U postgres -d postgres >/dev/null 2>&1; do
    test "$(docker inspect --format '{{.State.Running}}' "$TASK_CONTAINER")" = true
    TASK_WAIT=$((TASK_WAIT + 1))
    test "$TASK_WAIT" -lt 60
    sleep 1
  done
}

test -z "$(docker ps -aq --filter "name=^/${TASK_CONTAINER}$")"
test -z "$(docker volume ls -q --filter "name=^${TASK_VOLUME}$")"
test ! -e "$TASK_SOCKET_DIR"
test ! -e "$TASK_NODE_MODULES"
trap cleanup_hl294 EXIT INT TERM

docker volume create "$TASK_VOLUME"
docker run --detach --name "$TASK_CONTAINER" --publish 127.0.0.1::5432 \
  --mount "type=volume,source=$TASK_VOLUME,destination=/var/lib/postgresql/data" \
  --env POSTGRES_PASSWORD="$TASK_DB_PASSWORD" postgres:17-alpine
wait_postgres_hl294
TASK_PORT=$(docker port "$TASK_CONTAINER" 5432/tcp | sed -n 's/.*://p')
TASK_DATABASE_URL="postgres://postgres:${TASK_DB_PASSWORD}@127.0.0.1:${TASK_PORT}/postgres?sslmode=disable"

(cd agentservice && AGENT_SERVICE_TEST_DATABASE_URL="$TASK_DATABASE_URL" \
  HL294_HISTORY_RESTART_OWNER="$TASK_RESTART_OWNER" HL294_HISTORY_RESTART_RUN_ID="$TASK_RESTART_RUN" \
  HL294_HISTORY_RESTART_PHASE=setup GOWORK=off go test -race ./internal/store \
  -run '^TestPostgresHistoryReplicaServerRestartBackfill$' -count=1 -v)
docker inspect --format 'pre_container_started_at={{.State.StartedAt}}' "$TASK_CONTAINER"
docker restart "$TASK_CONTAINER"
wait_postgres_hl294
docker inspect --format 'post_container_started_at={{.State.StartedAt}}' "$TASK_CONTAINER"
TASK_PORT=$(docker port "$TASK_CONTAINER" 5432/tcp | sed -n 's/.*://p')
TASK_DATABASE_URL="postgres://postgres:${TASK_DB_PASSWORD}@127.0.0.1:${TASK_PORT}/postgres?sslmode=disable"
(cd agentservice && AGENT_SERVICE_TEST_DATABASE_URL="$TASK_DATABASE_URL" \
  HL294_HISTORY_RESTART_OWNER="$TASK_RESTART_OWNER" HL294_HISTORY_RESTART_RUN_ID="$TASK_RESTART_RUN" \
  HL294_HISTORY_RESTART_PHASE=verify-server-restart GOWORK=off go test -race ./internal/store \
  -run '^TestPostgresHistoryReplicaServerRestartBackfill$' -count=1 -v)

(cd agentservice && AGENT_SERVICE_TEST_DATABASE_URL="$TASK_DATABASE_URL" \
  HL294_HARNESS_COMPONENT_REPLICA_OWNER="$TASK_COMPONENT_OWNER" GOWORK=off go test -race ./internal/store \
  -run '^TestPostgresHistoryReplicaHarnessComponentSeed$' -count=1 -v)
mkdir -p -m 700 "$TASK_SOCKET_DIR"
(cd agentservice && GOWORK=off go build -o "$TASK_SOCKET_DIR/agent-service" ./cmd/agent-service)
AGENT_SERVICE_DATABASE_URL="$TASK_DATABASE_URL" AGENT_SERVICE_SOCKET="$TASK_SOCKET" \
  AGENT_SERVICE_WORKER_TOKEN="$TASK_WORKER_TOKEN" "$TASK_SOCKET_DIR/agent-service" serve \
  >"$TASK_SOCKET_DIR/agent-service.log" 2>&1 &
TASK_AGENT_PID=$!
TASK_WAIT=0
while test ! -S "$TASK_SOCKET"; do
  kill -0 "$TASK_AGENT_PID"
  TASK_WAIT=$((TASK_WAIT + 1))
  test "$TASK_WAIT" -lt 100
  sleep 0.1
done
(cd harness && HL294_HARNESS_COMPONENT_REPLICA_SOCKET="$TASK_SOCKET" \
  HL294_HARNESS_COMPONENT_REPLICA_OWNER="$TASK_COMPONENT_OWNER" \
  HL294_HARNESS_COMPONENT_REPLICA_WORKER_TOKEN="$TASK_WORKER_TOKEN" GOWORK=off go test -race ./node \
  -run '^TestHarnessHistoryCoordinatorCopiesCommittedEntryAndBackfillsAfterRecreation$' -count=1 -v)
kill "$TASK_AGENT_PID"
wait "$TASK_AGENT_PID" || true
TASK_AGENT_PID=

(cd agentservice && AGENT_SERVICE_TEST_DATABASE_URL="$TASK_DATABASE_URL" GOWORK=off \
  go test -race ./internal/model ./migrations ./internal/store -count=1)
(cd harness && GOWORK=off go test -race ./node -count=1)
GOWORK=off go test -race ./internal/historysync ./internal/agentserviceclient -count=1
(cd web/mobile-workspace && npm ci --ignore-scripts)
node api/check-history-replica-v1.mjs

docker exec "$TASK_CONTAINER" dropdb -U postgres --if-exists "$TASK_RESTORE_DB"
docker exec "$TASK_CONTAINER" pg_dump -U postgres -d postgres -Fc -f /tmp/hl294.dump
docker exec "$TASK_CONTAINER" createdb -U postgres "$TASK_RESTORE_DB"
docker exec "$TASK_CONTAINER" pg_restore -U postgres -d "$TASK_RESTORE_DB" --exit-on-error /tmp/hl294.dump
```

Run this exact digest SQL against both `postgres` and `$TASK_RESTORE_DB`; the two one-line tuples must be byte-identical:

```sql
SELECT
  (SELECT max(version) FROM agent_service.schema_migrations)||'|'||
  (SELECT count(*) FROM agent_service.history_replica_streams)||'|'||
  (SELECT md5(COALESCE(string_agg(owner_id||stream_id::text||imported_through::text||imported_chain_hash||source_through::text||source_chain_hash||complete::text||imported_checkpoint::text||source_checkpoint::text,'' ORDER BY owner_id,stream_id),'')) FROM agent_service.history_replica_streams)||'|'||
  (SELECT count(*) FROM agent_service.history_replica_records)||'|'||
  (SELECT count(*) FROM agent_service.history_replica_records WHERE record_hash_input IS NOT NULL)||'|'||
  (SELECT md5(COALESCE(string_agg(owner_id||stream_id::text||stream_seq::text||record_id::text||record_type||entity_id::text||revision::text||record_hash||prev_hash||chain_hash||record_json::text||encode(COALESCE(record_hash_input,''::bytea),'hex'),'' ORDER BY owner_id,stream_id,stream_seq),'')) FROM agent_service.history_replica_records)||'|'||
  (SELECT count(*) FROM agent_service.history_entries)||'|'||
  (SELECT count(*) FROM agent_service.history_execution_facts)||'|'||
  (SELECT count(*) FROM agent_service.history_receipt_revisions)||'|'||
  (SELECT count(*) FROM agent_service.history_text_manifests)||'|'||
  (SELECT count(*) FROM agent_service.history_text_chunks)||'|'||
  (SELECT count(*) FROM agent_service.history_asset_manifests);
```

`record_hash_input IS NOT NULL` is integrity coverage, not a substitute for total record count. Audit the exact candidate owners separately and require zero `NULL` inputs. Legacy unreplayed rows remain an explicit incomplete-coverage class.

After capturing the review evidence, remove and read back only the task-local resources:

```sh
cleanup_hl294
trap - EXIT INT TERM
test -z "$(docker ps -aq --filter "name=^/${TASK_CONTAINER}$")"
test -z "$(docker volume ls -q --filter "name=^${TASK_VOLUME}$")"
test ! -e "$TASK_SOCKET_DIR"
test ! -e "$TASK_NODE_MODULES"
```
