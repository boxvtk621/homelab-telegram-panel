---
title: 'HL-293 R10 PostgreSQL restart and mTLS enrollment evidence'
type: 'chore'
created: '2026-09-18'
status: 'done'
route: 'oneshot'
review_loop_iteration: 1
context:
  - '{project-root}/AGENTS.md'
  - '{project-root}/CONTRIBUTING.md'
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** R10 implementation `c657a652de02a48ca110c2581b913c326c396f9a` is integrated in exact `origin/main=1e180fa497b67105cad0d0b34c97d5f79b89628e`, but HL-293 remains `Testing` because the delivery ledger lacks a current PostgreSQL/restart/mTLS evidence pass and must not conflate protocol fixtures with production-compatible native Harness enrollment.

**Approach:** On the isolated linked worktree and a disposable loopback PostgreSQL 17 target, run the existing real-Pg R10-adjacent durability checks plus focused Router/Panel/private-mTLS enrollment and lost-ACK/no-lifecycle-effect gates. Preserve exact commands/output and classify compatible implemented producers versus future profile/MCP/assets/transfer/retirement capability dependencies; use no production/remote target, make only a scoped confirmed-defect fix with independent read-only review, and do not start R15 or commit/push/merge/deploy.

</frozen-after-approval>

## Implementation Notes

- Canon: HL-293 Task Revision 1 and HL-271 Story Revision 2, read 2026-09-18; latest delivery correction records R10 integrated but Pg/restart/live-mTLS/native-compatible enrollment as missing proof.
- Worktree: `/Users/kondor/.codex/worktrees/hl263-r10-r14-runtime-evidence/homelab-telegram-panel`; branch `codex/hl-263-r10-r14-runtime-evidence-20260918`; clean base/HEAD and fresh `origin/main` are both `1e180fa497b67105cad0d0b34c97d5f79b89628e` before BMAD session artifacts.
- Existing seams: real-Pg store/restart integration under `agentservice/internal/store`; R10 replay/conflict HTTP coverage under `agentservice/internal/httpapi`; Router admission/lost-ACK/no-lifecycle tests under `internal/harnessrouter`; generated-certificate end-to-end private mTLS under `internal/harnessclient`; Panel result/reconcile boundary under `internal/panel`.
- Native Harness currently does not implement the complete R10 admission floor (`ReplicaImport`, `AssetImport`, `TargetReservation` remain absent); protocol fixture compatibility may be exercised, but native/production compatibility must remain a future-producer dependency rather than PASS.
- Confirmed evidence gap: no real-Pg test persisted the R10-specific `ExternalBinding`/`NewNodeHostID` across unknown effect, reconstructed the recovery input only from durable state, and then proved the terminal state after an actual PostgreSQL server restart. Added only `TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles`; runtime code is unchanged.
- Exact test-only diff: `agentservice/internal/store/store_integration_test.go`, 247 insertions, SHA-256 `66bc9bbd9b465e780fb06a9811ab5f843f7218b0aa7c0a343fd79d36cbb36843`; `git diff --check` PASS.
- The regression uses production-path `registry.Sign`/`registry.Verify`, an R10 SSH binding including credential and expected host key, application-pool reconnect, terminal second reconnect, exact signed envelope/hash/binding assertions, and a read-only `verify-server-restart` terminal-verification phase.
- Disposable PostgreSQL 17.11 final run used named volume `hl263-r10-pg-restart-data-20260918`. The same container ID was manually restarted; its postmaster start changed to `2026-09-18 11:12:57.552516+00`; the post-restart Go recovery phase PASSed and exact SQL readback retained `succeeded/reconciled`, operation version 4, SSH binding, registry v3/two nodes, candidate hash `e9cb5e18…a8910` and registration binding `02d18bce…41e6`. The task-local container and volume were then removed.
- Four focused real-Pg race tests PASS in 1.582s after restart. Focused mTLS/admission/Panel race gate PASS (`harnessclient` 1.409s, `harnessrouter` 1.417s, `panel` 1.328s); Router restart/readback gate PASS in 1.386s; agent-service HTTP/model enrollment/URI/hash gate PASS in 1.329s/1.287s. Evidence retains every named test/subtest, so zero-match PASS is excluded.
- Raw commands, outputs, SQL, environment identity, cleanup and fixture-vs-native boundaries are preserved in `evidence-hl-293-r10-pg-restart-mtls-20260918.md`.
- The combined native external-Harness restart+mTLS scenario remains `NOT_RUN`: no implemented compatible producer/approved target exists. R15-R25 and later transfer/retirement producers remain explicit dependencies; the R10 floor was not weakened.

## Review Triage Log

Blind review iteration 1 produced 13 findings. Classification against the exact candidate:

- **Accepted and fixed (8):** the former application-pool reopen was relabelled correctly and supplemented with a real PostgreSQL postmaster restart over a persistent named volume; fixture signatures became deterministic production-path Ed25519 signatures; recovery now reconstructs candidate/status from PostgreSQL; terminal state gets a second reconnect and post-server-restart read-only verification; exact envelope/hash/registration binding and SSH credential/host-key fields are asserted; complete named test output and exact SQL commands were retained; full worktree status/tooling exclusions are explicit.
- **Already bounded / clarified (2):** Store, Router/Panel and mTLS fixture tests remain separate seam evidence and are not represented as one live Harness E2E; combined compatible Harness process restart plus live mTLS redial remains `NOT_RUN`.
- **Rejected as redundant or outside this store test (2):** one changed binding field already proves whole-intent request-hash conflict; candidate/binding/node/host semantic matching is owned and covered by the R10 HTTP admission tests, while this regression verifies durable exact persistence/replay. No runtime Store contract change was justified by the finding.
- **Accepted as delivery hygiene (1):** generated `_bmad` workflow payload stays untracked and excluded; it is not a repository candidate.

Independent final candidate re-review: PASS, P0/P1/P2 = 0. The reviewer verified exact HEAD/base, the single tracked test-only diff and SHA-256, production-path signatures, recovery from PostgreSQL-only values, terminal second reconnect, real postmaster restart/volume evidence, complete named logs, native-vs-fixture boundary and generated-tooling exclusion. The reviewer confirms HL-293@1 may close while HL-271@2 stays open on native-compatible producer/runtime gates.

Blind re-review: P0/P1 = 0. Its remaining observations are recorded boundaries, not candidate defects: unknown-effect recovery crosses an application reconnect, while the later real postmaster restart verifies terminal durability; an in-flight unknown-effect postmaster restart is not claimed. Cross-field candidate/binding semantic admission is covered specifically by the external-enrollment HTTP handler tests, not claimed as a generic Store invariant. Exact-path staging remains mandatory because generated `_bmad` tooling is untracked and excluded.

No commit/push/merge/deploy step was executed because the user explicitly reserved each for a separate command.
