# HL-263 R01/R07/R08 PostgreSQL scale and control-capacity evidence

Collected: 2026-09-18 (Europe/Samara). This is local, disposable test evidence. It is not deployment, production PostgreSQL, native remote SSH, multi-engine, or owner acceptance evidence.

## Candidate and canonical revisions

- Worktree: `/Users/kondor/.codex/worktrees/hl263-r01-r14-pg-evidence/homelab-telegram-panel`
- Branch: `codex/hl-263-r01-r14-pg-evidence-20260918`
- Base and pre-test `HEAD`: `1e180fa497b67105cad0d0b34c97d5f79b89628e`
- Fresh remote readback: `git ls-remote origin refs/heads/main` returned `1e180fa497b67105cad0d0b34c97d5f79b89628e refs/heads/main`.
- Canonical issues read immediately before the run: HL-282 Task Revision 1, updated 2026-09-18 10:20:38; HL-290 Task Revision 1, updated 2026-09-18 10:20:42; HL-291 Task Revision 1, updated 2026-09-18 10:20:48; HL-278 Task Revision 2, updated 2026-09-18 10:20:44.
- Pre-test tracked tree was clean. `_bmad/` and `_bmad-output/` were untracked session artifacts. After review remediation, the only tracked product diff is `internal/harnesstunnel/pool_test.go`; no commit or push was made. `_bmad/` is generated session runtime and is excluded from any future staging/handoff.

## Environment identity

```text
$ go version
go version go1.26.5 darwin/arm64

$ sw_vers
ProductName:             macOS
ProductVersion:          27.0
BuildVersion:            26A428

$ uname -m
arm64

$ docker context show
desktop-linux

$ docker version --format 'client={{.Client.Version}}/{{.Client.Os}}/{{.Client.Arch}} server={{.Server.Version}}/{{.Server.Os}}/{{.Server.Arch}}'
client=29.8.0/darwin/arm64 server=29.8.0/linux/arm64

$ docker info --format 'id={{.ID}} name={{.Name}} os={{.OperatingSystem}} kernel={{.KernelVersion}} architecture={{.Architecture}} cpus={{.NCPU}} memory={{.MemTotal}}'
id=22c5cbea-3b89-44a1-a585-ee5d743f41d2 name=docker-desktop os=Docker Desktop kernel=7.0.12-linuxkit architecture=aarch64 cpus=4 memory=8320299008
```

The task-local Go build cache was retained at `/private/tmp/hl263-pg-scale-go-cache` (`168M` at final readback). It contains build cache only, no database volume or product data.

## Disposable PostgreSQL target

Working directory for Docker commands: repository root.

```text
$ docker run --rm --detach --name hl263-r01-r08-pg-scale-evidence-20260918 --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=agent_service_test --tmpfs /var/lib/postgresql/data:rw,noexec,nosuid,size=512m --publish 127.0.0.1::5432 postgres:17-alpine
9f21b210ace1013dd8e3e22672acf4e81db9fff1c64c485ff105e1c544d5a8f6

$ docker exec hl263-r01-r08-pg-scale-evidence-20260918 pg_isready -U postgres -d agent_service_test
/var/run/postgresql:5432 - accepting connections

$ docker port hl263-r01-r08-pg-scale-evidence-20260918 5432/tcp
127.0.0.1:60452

$ docker inspect --format 'image={{.Image}} running={{.State.Running}} started={{.State.StartedAt}} autoRemove={{.HostConfig.AutoRemove}} tmpfs={{json .HostConfig.Tmpfs}} ports={{json .NetworkSettings.Ports}}' hl263-r01-r08-pg-scale-evidence-20260918
image=sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73 running=true started=2026-09-18T10:31:57.399725304Z autoRemove=true tmpfs={"/var/lib/postgresql/data":"rw,noexec,nosuid,size=512m"} ports={"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"60452"}]}

$ docker exec hl263-r01-r08-pg-scale-evidence-20260918 psql -U postgres -d agent_service_test -Atc "SHOW server_version; SHOW server_version_num; SHOW max_connections;"
17.11
170011
100
```

The trust setting was confined to the disposable container and a randomly assigned loopback host port. Data lived only in tmpfs.

## PostgreSQL-backed 100-agent / 10-host result

Working directory: `agentservice/`.

```text
$ GOWORK=off GOCACHE=/private/tmp/hl263-pg-scale-go-cache AGENT_SERVICE_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:60452/agent_service_test?sslmode=disable' go test -race ./internal/store -run '^TestPostgresMigrationImportIdentityAndInventory$' -count=1 -v
=== RUN   TestPostgresMigrationImportIdentityAndInventory
--- PASS: TestPostgresMigrationImportIdentityAndInventory (1.08s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store  2.517s
```

The integration test performs migrations, imports the 100-node/10-host signed snapshot, reopens the store, verifies stable logical identities and binding versions, repeats the import, checks owner isolation, and verifies atomic rollback on an invalid snapshot.

Raw SQL readback after the test:

```text
$ docker exec hl263-r01-r08-pg-scale-evidence-20260918 psql -U postgres -d agent_service_test -P pager=off -c "SELECT owner_id, count(*) AS agents, count(DISTINCT host_id) AS hosts FROM agent_service.instances GROUP BY owner_id ORDER BY owner_id;"
            owner_id             | agents | hosts
---------------------------------+--------+-------
 hl282-1789727554707084000       |    100 |    10
 hl282-1789727554707084000-other |    100 |    10
(2 rows)

$ docker exec hl263-r01-r08-pg-scale-evidence-20260918 psql -U postgres -d agent_service_test -P pager=off -c "SELECT count(*) AS migrations, min(version) AS min_version, max(version) AS max_version FROM agent_service.schema_migrations;"
 migrations | min_version | max_version
------------+-------------+-------------
          7 |           1 |           7
(1 row)
```

Two preliminary sandbox attempts were infrastructure-only failures. The first used the protected default Go cache and failed with `open /Users/kondor/Library/Caches/go-build/...: operation not permitted`. The second used the task-local cache but sandboxed loopback access caused the test's safe `database unavailable` result. The identical focused command above then passed with authorized loopback access; neither preliminary failure reproduced a product defect.

## Default control-capacity result

Working directory: repository root.

The review identified that the historical tests used a deliberately tiny `2/1/2/1` pool. The scoped regression `TestDefaultPoolConfigReservesFourControlSlotsAcrossNodes` pins the production default to `Total=16, Streams=12, PerNode=4, PerNodeStreams=3, Pending=64`: four nodes each hold three long streams, a thirteenth stream is deterministically rejected without consuming reserve, then each saturated node obtains one control lease. Expected-success acquisitions have a one-second bound. This is an algorithmic pool invariant, not a claim of 100 concurrent agents or live network throughput.

```text
$ GOCACHE=/private/tmp/hl263-pg-scale-go-cache go test -race ./internal/harnesstunnel -run '^Test(DefaultPoolConfigReservesFourControlSlotsAcrossNodes|LongStreamCannotConsumeReservedControlCapacity|FullStreamQueueCannotBlockFreeControlReserve)$' -count=1 -v
=== RUN   TestDefaultPoolConfigReservesFourControlSlotsAcrossNodes
--- PASS: TestDefaultPoolConfigReservesFourControlSlotsAcrossNodes (0.00s)
=== RUN   TestLongStreamCannotConsumeReservedControlCapacity
--- PASS: TestLongStreamCannotConsumeReservedControlCapacity (0.00s)
=== RUN   TestFullStreamQueueCannotBlockFreeControlReserve
--- PASS: TestFullStreamQueueCannotBlockFreeControlReserve (0.00s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel  1.372s

$ GOCACHE=/private/tmp/hl263-pg-scale-go-cache go test -race ./internal/harnesstunnel -run '^Test(DefaultPoolConfigReservesFourControlSlotsAcrossNodes|LongStreamCannotConsumeReservedControlCapacity|FullStreamQueueCannotBlockFreeControlReserve)$' -count=100
ok      github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel  1.210s

$ GOCACHE=/private/tmp/hl263-pg-scale-go-cache go test -race ./internal/harnesstunnel -count=1
ok      github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel  1.201s
```

## Cleanup

```text
$ docker stop hl263-r01-r08-pg-scale-evidence-20260918
hl263-r01-r08-pg-scale-evidence-20260918

$ docker ps -a --filter name=^/hl263-r01-r08-pg-scale-evidence-20260918$ --format '{{.ID}} {{.Names}} {{.Status}}'
<no output>
```

The named container and tmpfs database no longer exist.

## Evidence boundary

- PASS: real disposable PostgreSQL 17 store/migration/restart test with SQL readback of two owner-isolated 100-agent/10-host inventories.
- PASS: deterministic control reserve on `DefaultPoolConfig`, plus the existing queue/starvation regressions, all under the race detector and 100 sequential test repetitions.
- NOT_RUN: approved two-host/two-engine live matrix, real remote SSH and `AllowTcpForwarding`, Linux amd64/arm64 external engines, macOS Intel, Windows Desktop, physical sleep/network loss, and R25 managed endpoint allocation/inspect.
- No product runtime, shared infrastructure, production database, commit, push, merge, or deploy was touched.
