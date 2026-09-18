---
title: 'HL-296 R13-R14 tombstone/search cross-seam evidence'
type: 'test'
created: '2026-09-18'
status: 'complete'
route: 'oneshot'
review_loop_iteration: 1
baseline_commit: '1e180fa497b67105cad0d0b34c97d5f79b89628e'
context:
  - '{project-root}/AGENTS.md'
  - '{project-root}/CONTRIBUTING.md'
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** R13 and R14 are integrated and already have separate real-PostgreSQL evidence, but the retained handoff does not identify one exact test that creates an R14 search snapshot and exact-entry deep link, applies the real R13 active tombstone workflow, and then proves that the stale snapshot, a new search, the deep link, and late replica replay all remain denied. Repeating the completed 10k profile, UI matrix or publication would add no new proof.

**Approach:** Add one narrow assertion path to the existing disposable-PostgreSQL R13 integration test. Use the production store methods on one owner/dialog/database before and after the durable tombstone. Run only the focused store test and small read-only Panel search routing test, record the exact environment and boundary, and obtain an independent read-only review. Do not change production behavior unless the new check exposes a confirmed defect.

## Boundaries

- Canon: HL-295@1, HL-296@1, HL-279@2, HL-267@3; HL-A-603 C-DELETE-1/C-REPLICA-1/C-SEARCH-1; HL-A-606 R13/R14.
- Base and current remote `main`: `1e180fa497b67105cad0d0b34c97d5f79b89628e`; R13 `a0280c4b51eca294d959ca0a553238a328a76199` is already integrated. No commit, push, merge, deploy or repeat integration.
- Allowed target: isolated local PostgreSQL container on the available macOS Docker Desktop engine. No remote fault action, shared infrastructure or production mutation.
- The test uses retained replica data. It is not a real R31-R32 retirement producer, native/owner acceptance, live Harness failover or SSH/OS matrix proof.
- R15 is not started.

</frozen-after-approval>

## Ready when

1. Before deletion, one real-Pg R14 snapshot and exact-entry lookup expose the selected retained entry.
2. The production R13 active delete state machine reaches its durable tombstone on the same owner/dialog; the old search snapshot, a fresh search and exact-entry lookup expose no deleted data.
3. A late exact replica page remains rejected, proving the search denial is not followed by resurrection.
4. Focused checks and independent read-only review pass; unavailable SSH/live/retirement gates remain explicit instead of PASS.

## Plan

1. Map the existing R13 and R14 integration seams and reuse the smallest fixture already present.
2. Add only cross-seam pre/post assertions to `TestPostgresLogicalDeleteActiveCASAndTombstoneVisibility`.
3. Run the focused real-Pg test on a clean task-local PostgreSQL 17 target and the small Panel history-search routing test.
4. Freeze evidence, independent review, exact cleanup/readback, then append YouTrack evidence without changing Story AC or closing HL-296.

## Review Triage — iteration 1

Independent read-only review of the final snapshot returned `PASS` with no P0/P1/P2/P3 findings. The reviewer confirmed that late replica replay is rejected before both stale/fresh search readback and exact-entry denial; the Panel concurrency test synchronizes on real handler entry, observes `general=1/control=0`, admits a real control POST with `202`, and cannot leak its blocked goroutine on failure paths. Evidence boundaries correctly keep native/live/SSH/R31-R32 gates open.
