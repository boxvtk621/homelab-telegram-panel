# HL-296 R13-R14 cross-seam evidence — 2026-09-18

## Candidate identity

- Canon: HL-295 Task Revision 1 (`Done`), HL-296 Task Revision 1 (`Testing`), HL-279 Story Revision 2 and HL-267 Story Revision 3; HL-A-603 C-DELETE-1/C-REPLICA-1/C-SEARCH-1; HL-A-606 R13/R14.
- Worktree: `/Users/kondor/.codex/worktrees/hl263-r13-r14-evidence/homelab-telegram-panel`.
- Branch: `codex/hl-263-r13-r14-evidence-20260918`.
- Base/HEAD and fresh remote `main`: `1e180fa497b67105cad0d0b34c97d5f79b89628e`.
- R13 `a0280c4b51eca294d959ca0a553238a328a76199` and R14 `1e180fa497b67105cad0d0b34c97d5f79b89628e` were already integrated. No commit, push, merge, deploy or repeat integration was performed.
- Candidate scope is two test-only additions plus this BMAD/SDD evidence. Production code and contracts are unchanged.
- Ordered SHA-256 manifest over the two test candidate files: `33a5f8db53abada0228de3af15bcbad604e5f78f5feb77c3d62f1179fd4fb842`.

## Why this package is new evidence

The retained R13 and R14 suites independently proved delete/tombstone visibility and search snapshot behavior. The R14 test hid a dialog by directly setting `logical_dialogs.deleted_at`; it did not execute the R13 active-delete state machine. The R13 test denied history/receipt/text/replay, but did not create and re-read an R14 search snapshot or exact search entry around the actual tombstone. The new check closes only that available cross-stage proof. It does not rerun the completed 10,000-row performance profile, full UI matrix or publication.

The Panel route already places history reads in the general-read channel, while Harness commands use the reserved control channel. The new focused concurrency test blocks a real search handler in the former and proves a control POST remains admitted through the latter.

## Declared test target

- Host: macOS 27.0 build 26A428, arm64.
- Docker client/engine 29.8.0; engine `linux/arm64`; exact task-local loopback-only container `codex-hl296-r13r14-pg-20260918`, named task-local volume, dynamic port `127.0.0.1:49958`.
- Image: `postgres:17-alpine`; database: PostgreSQL 17.11 aarch64, container start `2026-09-18T13:30:25.645473135Z`.
- This is one available local macOS Docker Desktop target. It is not Linux amd64, Windows, SSH, a deployed source/replica pair or production.

## New checks

| Check | Exact result | Evidence boundary |
|---|---|---|
| R13 tombstone → R14 stale snapshot/fresh search/exact entry/late replay | real-Pg `-race` PASS, package `1.545s`; owner `hl295-delete-1789738482666540000`, snapshot `f70070dd-90ed-4213-83e0-2f2a9363d2fa`, entry `71000000-0000-4000-8000-000000000001`; visible `1` before, late replica page rejected, stale snapshot `0` after, fresh search `0` after, exact entry absent, `6` replica records retained and `1` tombstone durable | Uses production store methods and real Pg, but the Harness receipt/source page are deterministic component fixtures; this is not native/live delete or R31-R32 retirement evidence |
| Search/control capacity isolation | focused Panel `-race` PASS, package `1.472s`; while search blocked: `general_in_flight=1`, `control_in_flight_before=0`; Harness control POST returned `202` | Proves Panel capacity-channel separation on the component path; it is not a deployed load or fairness benchmark |
| Existing owner/offline UI contract | Not rerun: retained R14 browser test already searches with no selected/live agent, renders incomplete/lag, opens stable IDs, and rechecks deleted exact deep links | Fixture-backed browser evidence, not live source failure or owner acceptance |
| Independent read-only review | Final snapshot `PASS`, P0/P1/P2/P3 absent; reviewer independently reran the Panel `-race` check (`1.339s`) and `git diff --check` | Reviewer did not mutate files, YouTrack or the PostgreSQL target |

The first in-sandbox Panel attempt failed before product code with `listen tcp6 [::1]:0: bind: operation not permitted`. The exact command was rerun in the permitted local context and passed; this is recorded as a sandbox constraint, not a product failure.

## Commands

```sh
GOCACHE=/private/tmp/codex-hl296-gocache GOWORK=off \
  go test -race ./internal/panel \
  -run '^TestHistory(SearchAndEntryUseSessionOwner|SearchDoesNotConsumeControlCapacity)$' -count=1 -v

cd agentservice
AGENT_SERVICE_TEST_DATABASE_URL='postgres://postgres:<test-only>@127.0.0.1:49958/postgres?sslmode=disable' \
  GOCACHE=/private/tmp/codex-hl296-gocache GOWORK=off \
  go test -race ./internal/store \
  -run '^TestPostgresLogicalDeleteActiveCASAndTombstoneVisibility$' -count=1 -v
```

## Exact AC disposition

| Concern | Disposition |
|---|---|
| R13 durable tombstone prevents old search resurrection | PASS on the declared local real-Pg component profile, including an already-created snapshot, fresh query, exact entry and late replica replay |
| R14 search does not consume execution/control slots | PASS for Panel capacity-channel separation; deployed contention/fairness is not claimed |
| Retained/offline replica opens by stable IDs with visible lag/incomplete | Existing Pg + browser component evidence retained; no additional run was needed |
| 10k Cyrillic/Latin/ID/phrase p95 <=2s | Existing exact-main evidence retained at p95 `199.431542ms`; deliberately not repeated |
| Actual retired producer and deployed/live failover | `NOT_RUN`; R31-R32 producer and a declared deployed target are required |
| SSH/OS matrix | `NOT_RUN` beyond the declared macOS arm64 Docker Desktop target; no explicit remote fault target was authorized |

## Remaining blockers and dependencies

- HL-296/HL-267 remain open for deployed/live failover, owner acceptance, broad concurrent/pathological-volume stress and actual R31-R32 retained/retired producer integration.
- Arbitrary attachment bytes remain outside C-SEARCH indexing by contract; future R20-R22/R26-R28/R31-R32 evidence must not be inferred from this test.
- No approved SSH target was named for this package; remote fault injection was therefore not attempted.
- R15 was not started.

## Review and cleanup

- Independent read-only review: `PASS`, no P0-P3 findings on the exact uncommitted snapshot.
- Exact task-local PostgreSQL container `codex-hl296-r13r14-pg-20260918`, volume `codex-hl296-r13r14-pgdata-20260918` and `/private/tmp/codex-hl296-gocache` were removed after review. Readback result: `TASK_LOCAL_CLEANUP_PASS`; disposable test data is intentionally not recoverable.
