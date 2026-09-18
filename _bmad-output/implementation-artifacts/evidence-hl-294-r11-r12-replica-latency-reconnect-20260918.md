# HL-294 R11-R12 bounded evidence — 2026-09-18

## Candidate identity and scope

- Canon: HL-294 Task Revision 1, HL-279 Story Revision 2, HL-A-603 C-ID-1/C-REPLICA-1/C-READ-1.
- Worktree: `/Users/kondor/.codex/worktrees/hl263-r11-r12-evidence/homelab-telegram-panel`.
- Branch: `codex/hl-263-r11-r12-evidence-20260918`.
- Base/HEAD and fresh remote `main`: `1e180fa497b67105cad0d0b34c97d5f79b89628e`.
- Existing R11-R12 implementation `a99435cc405f83c0035f3119a377c1dd600f6454` is already an ancestor of that base; no commit, push, merge, deploy or repeat integration was performed.
- The uncommitted candidate is bounded to migration 008, exact record-hash input persistence/legacy repair, and opt-in R11-R12 evidence tests. It does not alter the wire hash algorithm or coordinator scheduling.
- Ordered SHA-256 manifest over the seven code/migration/test candidate files: `64b752b0ce67d0fd400c7490425d79746a0092a34dc7f4829f18801f427846ed`.

## Target profile

- Docker Desktop `4.91.0`; Engine/client `29.8.0`, API `1.56`, Linux arm64.
- Task-local container `codex-hl294-r11r12-pg-20260918`, named persistent task-local volume, loopback-only dynamic port; `postgres:17-alpine`, PostgreSQL 17.11 arm64.
- Harness component profile: real `node.Node` with SQLite 3.53.3 and provider-free fixture adapter; direct in-process `Node.ExportHistory`; production `historysync.Coordinator.Run`; real agent-service Unix HTTP API; real PostgreSQL store.
- The test supplies `TrustContext{PeerVerified:true}` at the direct exporter seam. It does not authenticate a peer and is not mTLS, SSH, deployed or live-network evidence.

## Executed checks

| Check | Exact result | Evidence boundary |
|---|---|---|
| Migration/persistence candidate | migration 008 adds nullable bounded `record_hash_input bytea`; new rows store exact accepted record-core bytes; replay repair requires digest plus semantic core/payload equality with stored JSON | Backward-compatible forward migration; no invented reconstruction from JSONB |
| Negative integrity/atomicity regressions | real-Pg PASS: tampering either a duplicated relational identity column or stored JSON rejects exact replay and keeps input `NULL`; repair followed by invalid Ready tail rolls back repaired bytes, new rows and checkpoint; subsequent valid replay repairs/completes | Covers the new repair transaction and redundant-column consistency, not database-superuser tamper prevention |
| Existing real-Pg replica suite under `-race` | PASS after migration 008: atomic/dedupe/reopen/offline read; stale tool-target repair; incomplete-ready atomic reject; concurrent lost ACK; transferred origin | Real Pg and store; application-pool reopen alone is not postmaster restart |
| Two-phase postmaster restart/backfill | setup PASS with partial `2/4`, two simulated legacy `NULL` hash inputs and persisted run/cluster marker; external Docker restart; verify PASS with prefix exact replay/repair, tail backfill, offline read and all four rows checked column-by-column | Source page is deterministic test data; test now fails unless the same Pg cluster has a strictly later postmaster start |
| Restart identity | system identifier stayed `7686861617547792424`; postmaster `2026-09-18T13:08:58.652814Z` → `2026-09-18T13:10:46.218445Z`; container start `2026-09-18T13:08:57.940234094Z` → `2026-09-18T13:10:46.152726922Z`; dynamic port `49414→49480` | Mechanically proves a real server restart, not only client or pool recreation |
| Harness committed→replica | fresh owner single observed run with production `DefaultInterval=2s` PASS; `2.059315459s` from before command admission to durable Pg readback | One healthy local component-path observation near one full interval; not a statistical or global upper bound |
| Coordinator recreation/backfill | fresh `Coordinator` instance PASS; `48.482ms` from recreation to durable readback of an entry committed while the coordinator was stopped | Replays from a fresh in-memory cursor; this is not source transport reconnect or mTLS recovery |
| Candidate checkpoints | Harness owner `8/8 complete=true`, restart owner `4/4 complete=true`; imported/source chain hashes equal; both have zero `NULL` hash inputs | Read-only SQL after all checks |
| `pg_dump -Fc`→new DB→`pg_restore` | source and restore exact tuple both `8|17|242059f02fadad45e4c10387134b37d4|10090|77|cbb22588cedc1fb9596c410fe516373a|10031|18|18|6|4|14` | Proves preservation/equality of the captured corpus, including duplicated relational columns and explicit raw-input coverage count; it does not call legacy `NULL` rows hash-verifiable |
| Final regressions | agent-service model `1.130s`, migrations `1.229s`, real-Pg store `-race` `35.776s`, full Harness node `-race` `48.353s`, historysync `1.397s` and agentserviceclient `1.398s`: all PASS | Opt-in orchestration tests skip without explicit environment |
| Replica schema/checker | `{"schemaId":"harness-history-export-v1","records":1,"verdict":"PASS"}` | Lockfile-pinned AJV dependencies installed only in isolated ignored `node_modules` |
| Independent read-only review | Final fresh snapshot: `PASS — P0/P1/P2 not found`; reviewer also ran `git diff --check`, empty `gofmt -d` and the targeted numeric unit regression | P3 hardening remains explicit below; reviewer did not rerun destructive/long integration checks |

## Hash/canonicalization constraint and correction

The bounded diagnostic confirmed that JSONB is not an archive of the original record-hash input:

```text
stored_record_hash=add4797827d066ade8ff808da17f796963a4618fad726842f4c76760979f7822
jsonb_roundtrip_hash=459b44c534c4bbd67f3b3489ad857aca31b960f86707fa35b71a2999aeea5fab
```

PostgreSQL reordered the nested object. The candidate therefore persists the exact bytes already accepted by the existing Go wire-hash validator in a separate `bytea` column. New imports populate it atomically with `record_json`; exact duplicate replay repairs a legacy `NULL` only after every duplicated immutable relational column, the stored JSON envelope, hash/chain and semantic payload match. Semantic number comparison uses `json.Decoder.UseNumber` plus exact rational values: a regression proves `1e2` equals `100.00`, while `9007199254740992` does not equal `9007199254740993`. The restart test deliberately nulls two inputs and proves repair after a real postmaster restart. Dump/restore preserves the raw bytes and digest.

The freshly created task-local database contained 10,090 records after the full regression suite; scale and legacy-compatibility fixtures intentionally insert rows without recoverable exact inputs, so 77 had exact inputs. Migration 008 does not fabricate lost bytes for those rows. The two final candidate streams contain 12/12 exact inputs after replay (`8/8` Harness, `4/4` restart). The internal API/checkpoint currently has no integrity-coverage field, so the explicit SQL coverage count is evidence and a remaining operational/API gap rather than a hidden PASS. Therefore:

- accepted Go record-hash input preservation for new/exact-replayed rows: proven on this candidate;
- immutable record/hash/chain/checkpoint preservation through restart and dump/restore: proven;
- reconstruction of original bytes for unreplayed legacy rows: impossible and not claimed;
- arbitrary compatible non-Go producer portability: not proven because the wire contract still does not publish a cross-language nested-JSON canonicalization profile;
- RFC 8785/JCS conversion: not attempted because it would change immutable hashes and requires a versioned contract decision.

## Exact DoD/AC disposition

| HL-294 / HL-279 concern | Disposition |
|---|---|
| Stable identity/origin and A→B→A separate streams | Existing Harness tests passed in full `-race`; no new runtime claim |
| Terminal facts without assistant and future queue excluded | Existing Harness tests passed in full `-race` |
| Atomic batch/checkpoint, duplicate, gap/hash no-advance | Existing real-Pg suite PASS |
| PostgreSQL restart plus prefix replay/tail backfill, no partial-ready | New same-cluster/later-postmaster two-phase test PASS |
| Offline owner-scoped read with synced-through/lag/incomplete | Existing real-Pg suite PASS |
| Backup/restore transcript/facts/receipts/checkpoints/hashes | Current-candidate dump/restore equality PASS; separate import-manifest relation is `NOT_PROVEN` because R12 schema has none |
| committed→replica ≤5 seconds | PASS only for the declared one-stream healthy local component profile; not closed for deployed mTLS/SSH topology or slow-neighbour worst case |
| reconnect/backfill | Pg restart/client reopen and coordinator recreation/replay PASS; source transport reconnect remains `NOT_RUN` |

## Reproduction boundary

The complete secret-free prerequisites, per-module working directories, service lifecycle, restart phases, full regressions, dump/restore digest SQL and cleanup/readback are recorded in `runbook-hl-294-r11-r12-evidence.md`. The opt-in tests intentionally require an explicit disposable target; no credential or full database URL is persisted.

## Remaining blockers and dependencies

- `Coordinator.SyncOnce` serially visits nodes, bindings and backlog. A preceding slow/unavailable node can consume exporter/client timeouts, so the two-second interval is not a global five-second upper bound for another healthy node. Changing this scheduling/resource model is an architecture decision, not a test-only correction.
- Production `harnessclient.ExportHistory` over mTLS/private tunnel, SSH reconnect and a deployed source/replica pair remain unverified here.
- Portable nested-JSON canonicalization remains a future versioned contract decision; migration 008 only preserves bytes accepted by the present validator.
- Legacy unreplayed rows have no recoverable exact input and the current API exposes no integrity-coverage status; do not label the whole accumulated corpus hash-verified.
- R13 owns tombstone/no-resurrection evidence. R20-R22 own attachment bytes/completeness, R26-R28 transfer, and R31-R32 retirement/final ownership. These are not promoted into R11-R12 evidence.
- Reviewer P3 hardening, not represented as current DoD proof: add a checked-in v7→v8 upgrade fixture, URI-safe secret construction in the reproduction runbook, structurally framed per-row backup digests and restored projection readback beyond counts.
- R15 was not started.

## Cleanup

- Temporary agent-service process stopped.
- Exact task-local PostgreSQL container `codex-hl294-r11r12-pg-20260918`, volume `codex-hl294-r11r12-pgdata-20260918`, Unix-socket directory, ignored frontend `node_modules` and generated `_bmad` runtime cache were removed after review.
- Readback result: `TASK_LOCAL_CLEANUP_PASS`. Only the uncommitted scoped candidate and `_bmad-output` evidence remain; test data is intentionally not recoverable after volume deletion.
