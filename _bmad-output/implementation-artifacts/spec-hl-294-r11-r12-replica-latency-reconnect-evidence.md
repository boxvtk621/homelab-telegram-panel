---
title: 'HL-294 R11-R12 replica latency, reconnect and hash preservation evidence'
type: 'bugfix'
created: '2026-09-18'
status: 'complete'
route: 'oneshot'
review_loop_iteration: 4
context:
  - '{project-root}/AGENTS.md'
  - '{project-root}/CONTRIBUTING.md'
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** HL-294 still lacks current-candidate proof for PostgreSQL restart/reconnect backfill and the committed-to-replica five-second objective, while review found that JSONB persistence may not preserve the exact payload bytes bound into immutable record hashes.

**Approach:** Reproduce the hash-preservation risk on disposable PostgreSQL, apply only a backward-compatible persistence fix if confirmed, and gather bounded local coordinator/reconnect evidence without representing fixture-backed or local checks as native/deployed acceptance. Preserve future assets, transfer, retirement and R13 tombstone work as explicit dependencies; do not start R15.

</frozen-after-approval>

## Implementation Notes

- Base and remote `main` were independently read back as `1e180fa497b67105cad0d0b34c97d5f79b89628e`; R11-R12 implementation commit `a99435cc405f83c0035f3119a377c1dd600f6454` is already an ancestor, so no prior commit/push/integration is repeated.
- Canonical scope is HL-294 Task Revision 1 and HL-279 Story Revision 2. Existing component tests cover atomic apply, exact replay, gap/no-advance, client reopen, source reopen and mock coordinator gap recovery; they do not by themselves prove deployed/native latency.
- Review identified two distinct constraints: Go and JavaScript do not publish one portable nested-JSON canonicalization profile, and PostgreSQL JSONB normalizes the accepted record JSON. Changing the wire hash algorithm would alter immutable hashes. The confirmed at-rest loss is corrected narrowly by migration 008, which preserves the exact already-accepted record-core bytes in `bytea`; it does not change wire hashes or claim portable canonicalization.
- `historysync.Coordinator.Run` is wired by `internal/panel/server.go` at a two-second interval but serially scans nodes/bindings/backlog. Any local healthy-stream latency result is a declared profile, not a global worst-case guarantee across slow nodes.
- Added a two-phase real-Pg regression that leaves a partial stream before an externally controlled PostgreSQL restart. A persisted run marker binds setup/verify to one Pg `system_identifier` and requires a strictly later `pg_postmaster_start_time`; verify exact-replays the prefix, repairs simulated legacy `NULL` hash inputs, backfills the tail, validates offline read and checks every stored record/hash input.
- Added an opt-in Harness component profile: a real Harness SQLite producer commits entries, `historysync.Coordinator.Run` imports through the real agent-service Unix HTTP API into isolated PostgreSQL, and a fresh coordinator backfills a commit made while it was stopped. The exporter is a direct in-process call with test-supplied trust, so it is not authentication, mTLS, deployed or live evidence.
- Final post-fix fresh-owner rerun used production `historysync.DefaultInterval=2s` and observed `2.059315459s` from before committed-command admission to durable readback and `48.482ms` from coordinator recreation to backfilled readback. This is one declared local profile observation, not a global upper bound. A clean-target guard requires exact HTTP 404/not_found before the run.
- The Pg diagnostic confirmed `stored_record_hash=add479...7822` while re-encoding `record_json` after JSONB round-trip produced `459b44...5fab`. Migration 008 preserves exact accepted bytes for new rows and repairs legacy `NULL` only on exact replay; unreplayed legacy bytes and cross-language canonicalization remain unproven.
- Final post-fix gates: agent-service model/migrations/store `-race` PASS, full Harness node `-race` PASS, historysync/client `-race` PASS, schema checker PASS, and `pg_dump`/`pg_restore` source/restore digest equality PASS.

## Review Triage — iteration 1

| # | Finding | Disposition |
|---|---|---|
| 1 | Restart phases were not mechanically bound to a postmaster restart | Fixed: same cluster ID plus strictly later persisted postmaster start is required |
| 2 | Stale owner/partial rows could pair unrelated phases | Fixed: setup requires clean owner and verify requires matching unique run marker |
| 3 | Coordinator test overclaimed reconnect | Fixed: names/evidence now say recreation/replay; source reconnect remains `NOT_RUN` |
| 4 | Direct `PeerVerified:true` overclaimed authentication/mTLS | Fixed: evidence explicitly identifies test-supplied trust and direct seam |
| 5 | Earlier 100 ms interval invalidated latency result | Rejected as stale: final code and rerun use `historysync.DefaultInterval=2s` |
| 6 | Restart test did not bind stored JSON to original hash input | Fixed: exact hash-input bytes, all immutable columns and semantic JSON are checked |
| 7 | Confirmed JSONB loss needed a backward-compatible persistence correction | Fixed: additive migration 008, atomic new writes and exact-replay legacy repair |
| 8 | Opt-in orchestration was not reproducible | Fixed in evidence: explicit secret-free command sequence and target variables |
| 9 | Import-manifest backup/restore was unproven | Accepted blocker: R12 has no separate relation; evidence says `NOT_PROVEN` |
| 10 | Disposable resources had not been removed | Deferred only through final independent review; exact container/volume/restore artifacts are then removed and absence read back |

## Review Triage — iteration 2

| Finding | Disposition |
|---|---|
| P1 exact input was not semantically bound to stored JSON and repaired complete replay could skip verification | Fixed: `HistoryRecordHashInputMatches` binds digest, strict core fields and semantic payload; duplicate replay validates stored envelope/input before repair; a repaired complete replay forces completeness verification |
| P2 legacy `NULL` rows looked fully hash-verifiable | Accepted blocker without API expansion: evidence now reports exact coverage (`77/10090` in the final clean regression corpus, final candidate `12/12`) and explicitly forbids whole-corpus hash-verification claims |
| P2 reproduction commands used wrong working directories and omitted lifecycle/dump/cleanup | Fixed: separate secret-free runbook covers disposable target, `agentservice`/`harness` cwd, service PID, restart, full gates, digest SQL and exact cleanup/readback |
| P2 import-manifest DoD is absent from R12 schema | Accepted blocker; HL-294 remains open and evidence stays `NOT_PROVEN` |
| P3 one latency sample was called an upper bound | Fixed wording: one observed healthy local component run |
| P3 no automated v7→v8 upgrade fixture | Not promoted to PASS: the additive migration and current-schema persistence are covered, but a checked-in v7 upgrade fixture remains future regression hardening |
| P3 repair rollback atomicity untested | Fixed: real-Pg negative test proves repaired inputs, inserted tail and checkpoint all roll back on final Ready failure |

## Review Triage — iteration 3

| Finding | Disposition |
|---|---|
| P1 semantic comparison decoded numbers through `float64`, allowing distinct integers above 2^53 to compare equal | Fixed: the strict decoder uses `UseNumber`; numeric equality uses exact `big.Rat`; regressions cover equivalent exponent/decimal forms and adjacent large integers |
| P2 duplicate repair trusted redundant relational columns without comparing all of them to the stored envelope and incoming record | Fixed: lookup and completeness verification now bind `record_id`, `record_type`, `entity_id`, `revision`, record/previous/chain hashes, stored JSON and preserved hash input; a real-Pg tamper regression rejects repair |
| P2 runbook omitted deterministic frontend dependency installation before the schema checker | Fixed: lockfile-based `npm ci --ignore-scripts` is explicit and remains isolated in ignored task-local `node_modules` |
| P3 readiness, socket wait and cleanup wording could hide a dead process or broaden cleanup | Fixed: bounded readiness checks assert container/process liveness; cleanup names only exact task-local paths; migration comment now says exact replay rather than a schema-version promise |

## Review Triage — iteration 4

Final independent read-only review of the fresh snapshot returned `PASS — P0/P1/P2 not found`. Reviewer checks: `git diff --check` PASS, `gofmt -d` empty and targeted numeric unit regression PASS. Long integration checks were not duplicated by the reviewer; their exact executor evidence is retained separately.

The following P3 hardening is deliberately not promoted into current DoD evidence: a checked-in v7→v8 upgrade fixture; URI-safe password construction in the reproduction runbook; structurally framed row digests; and projection-content readback from the restored database beyond counts. HL-294/HL-279 remain open for their explicit canonical blockers even though this bounded candidate/evidence package is complete.
