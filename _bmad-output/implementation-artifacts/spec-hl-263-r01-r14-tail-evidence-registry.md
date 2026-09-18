---
title: 'HL-263 R01-R14 evidence registry and delivery correction'
type: 'chore'
created: '2026-09-18'
status: 'done'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '1e180fa497b67105cad0d0b34c97d5f79b89628e'
context:
  - '{project-root}/AGENTS.md'
  - '{project-root}/CONTRIBUTING.md'
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** Старые handoff HL-282/283/290/291/293/294 называют кандидаты локальными или непубликованными. Live readback уже показывает `origin/main=1e180fa497b67105cad0d0b34c97d5f79b89628e`; все stage commits — его предки. Delivery state смешан с реальными runtime/native gates.

**Approach:** Создать AC-ориентированный реестр R01–R14 и append-only исправить evidence в существующих Tasks, Stories и HL-263. Интегрированные незавершённые Tasks переводить не выше `Testing`; Stories/Epic оставить открытыми с конкретным blocker, владельцем и условием продолжения.

## Boundaries & Constraints

**Always:** Exact SHA/target; раздельно Git, fixture, local Pg, native/live и owner acceptance; NOT_RUN не PASS; исторические записи сохраняются; profile/MCP/assets/transfer/retirement остаются зависимостями своих будущих этапов.

**Never:** Не менять AC/архитектуру ради delivery-сводки; не закрывать сущность без её DoD; не запускать R15–R34; без отдельного разрешения не делать commit/push/merge/deploy, production/shared-infra mutations или remote fault tests.

## I/O & Edge-Case Matrix

| State | Expected behavior |
|---|---|
| Stage SHA — ancestor live remote | Append exact integration correction |
| Native/live/Pg/latency evidence missing | Record NOT_RUN, owner and continuation condition |
| Gate needs future producer | Record dependency; do not weaken capability floor |
| Task passes, Story AC remains open | Keep Story/Epic unresolved |

</frozen-after-approval>

## Code Map

- `git log 47843206..1e180fa` and live `git ls-remote` -- cumulative R01–R14 publication chain.
- `HL-282/283/290/291/293/294/296` -- bounded Task ledger targets.
- `HL-268/278/271/279/267` and `HL-263` -- cumulative Story/Epic targets.
- `HL-A-603` C-HOST/C-TUNNEL/C-REG/C-REPLICA/C-SEARCH and `HL-A-606` R01–R14 -- read-only canonical requirements.
- `agentservice/internal/store/history_replica*.go`, `internal/historysync/*`, `internal/dockeradapter/*`, `internal/harnesstunnel/*`, `internal/harnessrouter/enrollment*.go` -- next-package verification seams; no code changes here.

## Tasks & Acceptance

**Execution:**
- [x] `origin/main` -- read exact remote SHA; prove every R01–R14 stage commit is its ancestor.
- [x] Seven Tasks -- append exact integrated SHA, retained evidence, missing proof, dependency, target/owner and completion condition.
- [x] Five Stories -- append cumulative ledger without changing AC; distinguish delivered code from open gates.
- [x] `HL-263` -- append one R01–R14 delivery summary superseding stale “not started” text as history.
- [x] Task fields -- move integrated unfinished Tasks to `Testing` only; leave Stories/Epic unresolved.
- [x] YouTrack -- read back every mutation; failed updates become explicit blockers.

**Acceptance Criteria:**
- Given live remote `1e180fa`, when the ledger is read, then R01–R14 delivery maps to exact ancestor commits without repeating publication.
- Given insufficient AC evidence, when a row is recorded, then it names missing proof, dependency, target, owner and objective continuation condition rather than PASS.
- Given future producers, when a gate depends on them, then no R15+ implementation or compatibility weakening occurs.
- Given final readback, when current fields are inspected, then no resolved status exceeds the corresponding DoD.

## Implementation Notes

- 2026-09-18: live `origin/main` read back as `1e180fa497b67105cad0d0b34c97d5f79b89628e`; exact R01–R14 ancestry gate PASS.
- Added append-only correction comments to HL-282/283/290/291/293/294/296, HL-268/278/271/279/267 and HL-263. No description, AC or Task Revision was changed.
- HL-283/290/291/293/294 moved to `Testing`; HL-282/296 already `Testing`. All five Stories and HL-263 remain `Очередь`.
- Independent machine readback asserted exact main SHA, `NOT_RUN`, dependency, owner and continuation condition on all 13 targets.

## Spec Change Log

## Review Triage Log

- `BH-01` — **false / rejected**: exact full R01–R14 SHA registry is the live append-only HL-263 comment, which was re-read at `2026-09-18 08:51:59`; the build spec is the execution account, not the canonical tracker ledger.
- `BH-02` — **false / rejected**: current readback of all seven Tasks, five Stories and HL-263 shows per-target retained evidence, `NOT_RUN`, dependency, owner/target and objective continuation gate; those records deliberately live on their YouTrack targets.
- `BH-03` — **false / rejected**: `<stage-sha>` is a repeatable command parameter, not an unresolved delivery value; full SHAs are durable in HL-263 and the direct remote/ancestry gate was rerun. Raw terminal transcripts and comment IDs were not an acceptance requirement.
- `BH-04` — **false / rejected**: `_bmad/**` is untracked session runtime materialized by the required BMAD skill, not an HL-263 product/integration candidate; no commit is authorized and the handoff excludes it from product changes.
- `BH-05` — **low / rejected**: rendered absolute paths are real but intentional for an ephemeral worktree-bound workflow snapshot; publishing or reusing that snapshot after worktree deletion is not part of this intent, and changing the renderer is not justified here.
- `BH-06` — **medium / defer**: generated step-05/oneshot commit instructions conflict with this repository's explicit-authorization rule. Higher-level user/project rules prevent a commit in this run; the upstream BMAD workflow still needs correction.
- `BH-07` — **medium / defer**: direct collection of `_bmad/scripts/tests/test_render_skill.py` fails because the installed runtime lacks its source-tree assets/skills, and `make quality` does not collect these tests.
- `BH-08` — **medium / defer**: `memlog.write_atomic` uses a predictable followed `.tmp` path, so a pre-created symlink can redirect truncation; this is upstream BMAD runtime code, not HL-263 product code.
- `BH-09` — **medium / defer**: concurrent memlog append is an unlocked read-modify-replace; the verification reviewer reproduced lost entries and `FileNotFoundError`.
- `BH-10` — **medium / defer**: `--type`, `--by`, and frontmatter keys are not structurally validated, so multiline input can corrupt the one-line/frontmatter shape.
- `BH-11` — **high / defer**: `output_folder()` accepts non-string, absolute and upward-traversing values before `ensure_dir(project_root / value)`, permitting failure or writes outside the project.
- `BH-12` — **high / defer**: renderer publication does not reject symlink components or symlink outputs; containment and immutable-generation claims can be bypassed in an adversarial local tree.
- `BH-13` — **medium / defer**: update source parsing validates `version` but not the expected module identity, so unrelated manifest metadata can drive status.
- `BH-14` — **low / defer**: declared module scripts are represented as bytes and repaired with `write_bytes`, so executable mode is not preserved; direct execution can break.
- `BH-15` — **medium / defer**: keyed-array detection silently falls back to append when an override omits an identity, and duplicate base identities remain ambiguous.
- `VG-01` — **medium / defer**: renderer integrity tests are outside `make quality` and the standalone installed copy cannot collect; a hash-verification regression can escape the repository gate.
- `VG-02` — **medium / defer**: setup/doctor preservation and replacement rollback have no repository test; deleting the seed-copy behavior would not fail an existing gate.
- `VG-03` — **medium / defer**: update-state/SemVer classification has no offline matrix test, so reversed or incomplete state decisions are undetected.
- `VG-04` — **medium / defer**: independently reproduced 20-process memlog append lost entries and raised during shared-temp replacement; same root cause as `BH-09`.
- `VG-05` — **low / defer**: invalid UTF-8 TOML raises an uncaught `UnicodeError` and traceback because `load_toml` catches syntax and I/O errors only.
- `EC-01` — **medium / defer**: a keyed base plus an override missing the key changes semantics to append instead of failing closed; verified in `_detect_keyed_merge_field(base + override)`.
- `EC-02` — **medium / defer**: duplicate keyed base entries overwrite only `index_by_key` while both list entries survive; the override updates only the last duplicate.
- `EC-03` — **medium / defer**: concurrent memlog writers share both the source version and fixed temp path; verified and grouped with `BH-09`/`VG-04`.
- `EC-04` — **medium / defer**: multiline `type`/`by` is interpolated without normalization and can create multiple physical lines from one append.
- `EC-05` — **medium / defer**: field/set keys accept empty, multiline and reserved `updated`; rendering can corrupt frontmatter or silently discard the requested reserved value.
- `EC-06` — **medium / defer**: TOML date/time/datetime values reach `json.dumps` in `resolve_config.py` without normalization and raise `TypeError`.
- `EC-07` — **medium / defer**: the same TOML scalar serialization failure exists in `resolve_customization.py`.
- `EC-08` — **low / defer**: a `.md` symlink loop can raise during `candidate.resolve(strict=True)` outside the renderer's wrapped read errors, leaking a traceback.
- `EC-09` — **low / defer**: `file:` update sources use `read_bytes()` before the size check, so a large local file is fully loaded; this is a bounded upstream hardening item, not an HL-263 defect.
- `EC-10` — **medium / defer**: source manifest module mismatch is not checked; grouped with `BH-13`.
- `EC-11` — **low / rejected**: only a pathological SemVer component beyond Python's integer digit limit triggers the uncaught conversion error; ordinary installed/source versions do not reach it and the proposed guard adds a new exceptional path.
- `EC-12` — **medium / defer**: `has_path` treats any existing leaf as an answered config question, including a non-string value, allowing invalid module configuration to pass setup.
- `EC-13` — **high / defer**: unsafe/non-string `output_folder` is accepted; grouped with `BH-11`.
- `EC-14` — **medium / defer**: setup seeds the existing `_bmad` tree and writes declared scripts without deleting scripts removed by a newer manifest, leaving stale executable code.
- `EC-15` — **medium / defer**: `ensure_dir` silently accepts an existing regular file or broken symlink, so setup can report success with malformed required layout.
- `EC-16` — **medium / defer**: installed-checkout collection reads absent assets at module import; grouped with `BH-07`/`VG-01`.
- `EC-17` — **medium / defer**: step-02 accepts externally changed approval-pending spec text without requesting fresh approval, allowing unreviewed intent edits to become frozen.
- `EC-18` — **false / rejected**: this run has Git and exact `baseline_commit=1e180fa497b67105cad0d0b34c97d5f79b89628e`; the `NO_VCS` branch is unreachable for this change.
- `EC-19` — **medium / defer**: step-04 patches can proceed after affected tests without rerunning independent review layers, so a review fix can introduce a new regression.
- `EC-20` — **high / defer**: loopback says “revert code changes” without a path ownership mechanism; in a mixed tree an implementation could delete the spec or foreign work.
- `EC-21` — **medium / defer**: step-05 auto-commit conflicts with explicit authorization; grouped with `BH-06`.
- `EC-22` — **medium / defer**: oneshot patches are not independently re-reviewed before finalization.
- `EC-23` — **medium / defer**: oneshot auto-commit conflicts with explicit authorization; grouped with `BH-06`.
- `EC-24` — **maybe-false / rejected**: `sync-sprint-status` has no explicit ordering for custom states, but this chore has no `story_key` or sprint-status file and never invokes the helper; a state-order test plus a real custom-state caller would be needed to show harm.

## Verification

**Commands:**
- `git ls-remote origin refs/heads/main` -- exact `1e180fa497b67105cad0d0b34c97d5f79b89628e`.
- `git merge-base --is-ancestor <stage-sha> origin/main` -- exit 0 for each stage.

**Manual checks:**
- Re-read every mutated issue: append-only evidence, exact fields, open blockers, no description/AC rewrite.
