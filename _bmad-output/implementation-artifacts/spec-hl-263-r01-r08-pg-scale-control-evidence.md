---
title: 'HL-263 R01/R07/R08 PostgreSQL scale and control-capacity evidence'
type: 'chore'
created: '2026-09-18'
status: 'done'
route: 'oneshot'
review_loop_iteration: 0
context:
  - '{project-root}/AGENTS.md'
  - '{project-root}/CONTRIBUTING.md'
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** Exact live candidate `1e180fa497b67105cad0d0b34c97d5f79b89628e` retains fixture evidence for control reserve, but the mandatory PostgreSQL-backed 100-agent/10-host scale cell is still recorded `NOT_RUN`; fixture-only capacity evidence must not close native/runtime cells.

**Approach:** On one disposable loopback-only PostgreSQL 17 container, rerun the existing real-Pg 100-agent/10-host import/reopen/owner-isolation test and the exact R08 control-reserve tests, preserving command output, environment identity and cleanup evidence. Append the result to existing HL-282/290/291 and HL-278 only after readback; keep unavailable 2-host/2-engine, remote SSH and OS matrix cells `NOT_RUN`, make no product change unless the checks reproduce a scoped defect, and perform no commit/push/merge/deploy.

</frozen-after-approval>

## Implementation Notes

- Integrated source base: `1e180fa497b67105cad0d0b34c97d5f79b89628e` on `codex/hl-263-r01-r14-pg-evidence-20260918`; runtime/product logic unchanged, with one uncommitted regression-test diff described below.
- Disposable target: Docker Desktop client/server `29.8.0`, macOS arm64 host / linux arm64 engine; `postgres:17-alpine` image `sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`, PostgreSQL `17.11`, tmpfs data, dynamically assigned `127.0.0.1:60452` only.
- From `agentservice/`: `GOWORK=off GOCACHE=/private/tmp/hl263-pg-scale-go-cache AGENT_SERVICE_TEST_DATABASE_URL=postgres://postgres@127.0.0.1:60452/agent_service_test?sslmode=disable go test -race ./internal/store -run '^TestPostgresMigrationImportIdentityAndInventory$' -count=1 -v` — PASS in 1.08s (package 2.517s).
- SQL readback after the test: both isolated owners contained exactly `100` agents on `10` distinct hosts; all seven migrations (`1..7`) were applied. The test also closes/reopens the store and checks stable logical identities, exact reimport, owner isolation and atomic rollback.
- From repository root: `GOCACHE=/private/tmp/hl263-pg-scale-go-cache go test -race ./internal/harnesstunnel -run '^Test(DefaultPoolConfigReservesFourControlSlotsAcrossNodes|LongStreamCannotConsumeReservedControlCapacity|FullStreamQueueCannotBlockFreeControlReserve)$' -count=1 -v` — all three PASS (package 1.372s); repeated with `-count=100` — PASS in 1.210s.
- Initial sandbox runs failed only on the protected system Go cache and loopback access; the identical focused tests passed with a task-local cache and authorized loopback access. These sandbox failures are not product failures.
- Container `hl263-r01-r08-pg-scale-evidence-20260918` was stopped and auto-removed; readback returned no matching container. No shared/production DB, remote host, runtime logic, commit, push, merge or deploy was touched.
- Evidence boundary: the PostgreSQL synthetic 100-agent/10-host cell and deterministic control-reserve cell are PASS. Real 2-host/2-engine, remote SSH/AllowTcpForwarding, other OS/arch, physical sleep/network loss and R25 managed endpoint cells remain `NOT_RUN`.
- Append-only evidence comments were added and read back on HL-282 (`Comments-7-925`), HL-290 (`Comments-7-926`), HL-291 (`Comments-7-928`) and HL-278 (`Comments-7-927`). Task revisions and AC text were unchanged; HL-278 remains `Очередь`.
- Outcome supersedes the frozen pre-run `NOT_RUN` premise only for the two exercised cells. `origin/main` denotes the published/integrated Git tip, not a deployed or live-runtime release. HL-282's historical `awaiting_integration` handoff wording was already superseded by the append-only delivery correction; it is not a perpetual Task completion gate.
- Blind review found that the original control tests exercised only a deliberately tiny pool config. The scoped regression `TestDefaultPoolConfigReservesFourControlSlotsAcrossNodes` pins `DefaultPoolConfig()` to `16/12/4/3/64`, saturates all 12 stream slots across four nodes, proves that a thirteenth stream cannot consume the reserve, then verifies that all four reserved control slots remain available; product behavior is unchanged.
- Exact uncommitted product-test diff: `internal/harnesstunnel/pool_test.go`, 82 insertions, SHA-256 `d72c708c98c7ad389baf1a51c728b898206c136d53d9f65e501fea31be8d4ca4`. `git diff --check` and the complete `internal/harnesstunnel` race-test package PASS; the three focused capacity tests PASS once and for 100 sequential repetitions.
- Reproducible commands, raw result excerpts, SQL owner/migration readback, environment identity, sandbox-only failures and container cleanup are preserved in `evidence-hl-263-r01-r08-pg-scale-control-20260918.md`. The retained `/private/tmp/hl263-pg-scale-go-cache` is a 168M disposable build cache, not product or database state.

## Review Triage Log

| Blind-review finding | Classification | Resolution |
|---|---|---|
| Summary existed without a durable raw evidence artifact | confirmed / material | Added `evidence-hl-263-r01-r08-pg-scale-control-20260918.md` with exact commands and output excerpts. |
| Test commands omitted working directories | confirmed | Evidence artifact records repository root vs `agentservice/`. |
| Docker run, readiness, image identity and loopback publication were not reproducible | confirmed | Re-ran a disposable target and recorded exact command, container/image identity, readiness and port readback. |
| SQL query/output and exact owners were absent | confirmed | Recorded the exact grouped inventory and migration queries/results. |
| Cleanup command/readback were absent | confirmed | Recorded `docker stop` and empty post-auto-remove `docker ps -a` readback. |
| Task-local Go cache remained unexplained | confirmed / low | Explicitly retained as a 168M disposable build cache; no DB/product data. |
| Go, macOS, Docker context and daemon identity were missing | confirmed | Added exact environment identity. |
| Pre/post Git candidate and remote readback were incomplete | confirmed | Added worktree, branch, base/HEAD, tracked-diff boundary and fresh `origin/main` readback. |
| Sandbox failures were not preserved | confirmed | Added commands/error classes and marked them infrastructure-only; authorized identical tests PASS. |
| Historical reserve tests used `2/1/2/1`, not the default `16/12/4/3` config | confirmed / material | Added a production-default regression; focused and package race gates PASS. |
| `-count=100` could be read as 100-agent concurrency | wording gap | Evidence now explicitly says 100 sequential repetitions; the 100-agent claim belongs only to the real-Pg store test. |
| Store test could be read as API/UI/runtime throughput proof | wording gap | Boundary now says store-level synthetic scale only; no API/UI/live throughput claim. |
| Canonical issue revisions/timestamps were missing | confirmed | Added HL-282/290/291/278 revisions and read timestamps to raw evidence. |
| Frozen phrase “Exact live candidate” was ambiguous | confirmed wording gap | Human-owned frozen text was not altered; implementation note clarifies Git tip, not deployment/live runtime. |
| Generated `_bmad/` tree could leak into handoff | operational caution | `_bmad/` remains untracked session runtime and is explicitly excluded from staging/handoff; no commit is authorized. |
| First remediation test did not assert the exact default config or rejection of stream 13 | independent review P1 / confirmed | Pinned `16/12/4/3/64`, added bounded expected-success acquisitions and a pre-cancelled deterministic rejection probe for the thirteenth stream; all race gates rerun PASS. |
| Implementation notes still described the earlier run and contradicted the test-only diff | independent review P2 / confirmed | Synchronized target port, timings, container name, command selector, diff identity and runtime-vs-test wording with the final raw evidence. |
| Full-package PASS was summarized but not preserved in the raw artifact | independent re-review P2 / confirmed | Added the exact `go test -race ./internal/harnesstunnel -count=1` command and `1.201s` PASS output to the durable evidence artifact. |

## Completion Readback

- Independent exact-diff re-review after both remediation rounds: **PASS, open P0/P1/P2 = 0**.
- Append-only closure/correction comments read back: HL-282 `Comments-7-929`, HL-290 `Comments-7-930`, HL-291 `Comments-7-931`, HL-278 `Comments-7-932`.
- HL-282, HL-290 and HL-291 resolved as `Готово` with Task Revision `1` unchanged. HL-278 remains unresolved in `Очередь`, Task Revision `2` unchanged.
- No commit, push, merge or deploy was performed. The test-only diff remains an uncommitted handoff candidate.
