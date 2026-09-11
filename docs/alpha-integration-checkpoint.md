# HL-240: alpha integration checkpoint, 2026-09-10

Development paused at the owner's request. All three subagents completed their
assignments; no agent, model run, server or Docker workload is left running by
this checkpoint. Resume from the committed branch below, not a temporary snapshot.

The next observable result is native Cursor chat through Harness: durable command
receipt, reply, continuation in the same dialog, and stop. This checkpoint is not
live acceptance. Scope and current decisions remain in [HL-240](https://youtrack.h1-cloud.ru/issue/HL-240).

## Components and seams

| Component | Responsibility | Dependency boundary |
| --- | --- | --- |
| Management UI | Agent availability, occupancy, queue; enter an agent | C1 reads, `onOpen(nodeId)` |
| Interaction UI | Dialogs, messages, work timeline, controls | C1 commands/SSE and selected IDs |
| Harness | Durable admission, queue, execution, history, native adapter | Independent process; owns its DB and provider secret |

The two UI components currently share web packaging. Management does not import
the interaction implementation. Browser navigation neither cancels nor repeats
accepted work. HTTP receipt and long execution are separate; state/events can be
read again after reconnect. Provider credentials do not enter Panel.

## Saved source and validation

Root worktree:
`/Users/kondor/.codex/visualizations/2026/09/08/01a08211-02ea-73e3-80cb-0fb37792f329/hl240-recovery`.
Branch `codex/hl-240-recovery-20260909`, original base
`3c3e87701669df90e48f3d5e28025d8c1d03ca12`.

- `c1cdc68`: accepted B1/C1/U1/U2, root and Harness vet/race PASS;
  frontend lint/typecheck, Node 122 and Vitest 51 PASS.
- `3e8a2b1`: recovered probe/backup tooling, saved as WIP; not rerun live.
- `571b246`: native launcher, now integrated with the Cursor adapter.
- `d744de9`: optional direct Panel TLS; focused Panel/command tests PASS.

Executor commits integrated after the owner's explicit cherry-pick approval:

1. `9d0362f39adb155e56887bf38eaf2fe53db9258d`, followed by
   `59c0aab844711469c9fb78f0ddd6874d6ad086fe`, from
   `codex/hl257-cursor-alpha-20260910`. Nine files only under
   `harness/adapters/cursor/`: adapter, bridge, stream, private mapping, worker,
   tests and package/lock. Actual SDK 1.0.31; policy reaches native `systemPrompt`
   at create/resume. Focused Go race PASS (1.923s), Node 5/5 PASS.
2. `e35c5e6d5eae0d227fa3cfc190a8ea23738f11e8`, from
   `codex/hl240-alpha-panels-20260910`, base `3e8a2b1`. Five files:
   `src/harness-management.tsx`, `src/harness-workspace.tsx`, `src/panel.tsx`,
   `src/panel.css`, `tests/harness-workspace.test.tsx`, all within
   `web/mobile-workspace/`. Typecheck, scoped lint/format and 37 focused UI tests PASS.

Root integration commits: `7471d48` (adapter), `48d87a8` (policy/pin),
`f9202a2` (UI split). Worker `npm ci --ignore-scripts --no-audit --no-fund`
completed from lock. `go build -p 2 -o ../bin/harness-node ./cmd/harness-node`
with GOMAXPROCS=2 passed. Final `npm run build` passed and regenerated the
embedded UI assets. The final Panel executable has not yet been built.

Source review checked UI boundaries and the asynchronous adapter handshake.
The missing native policy propagation found by root is fixed in `59c0aab`.
The installed SDK exposes `systemPrompt`; availability for this account still
requires a live check. Native tool permissions are deny/empty for this chat alpha.

## Remaining work

1. Review and execute `scripts/harness-alpha/setup.py` and `run-panel.py`.
   These two newly saved scripts are WIP: they have not provisioned or started
   a runtime. Setup generates private TLS/registry material into a new directory,
   refers to the provider key by path, and refuses to overwrite existing state.
2. Preserve the operator-signed registry `ownerId` as the sole Harness identity.
   Panel browser sessions must not require or derive identity from a personal
   YouTrack token.
3. Build Panel, start the isolated local runtime and perform the bounded live
   smoke: receipt → Cursor reply → resume → stop. Maximum four Cursor SDK sends,
   no blind retry. Check native `systemPrompt` availability for this account.
4. Verify the management → interaction → back path in a real browser with the
   Playwright skill. Build and fixture checks do not prove this live flow.
5. Continue Codex A2 and targeted remaining acceptance after the first working
   Cursor result. Full A1/A2/I1 and production/user acceptance remain open.

The cherry-pick approval rejection was resolved by the owner's explicit reply;
all three requested commits were integrated normally through Git. No push or
deployment was performed. No provider/model calls occurred in this checkpoint.

The Cursor key is stored privately at
`/Users/kondor/.config/harness-alpha/secrets/cursor-api-key-20260910`, outside Git
and temporary storage. Do not copy it into code, reports, task assignments or
Panel configuration. `.recovery/` contains local logs/cache, remains on disk,
and is ignored; it is not part of the committed source checkpoint.

## Role skills selected and installed

Skills are installed under `/Users/kondor/.codex/skills/`. Future delegations
receive the relevant `SKILL.md` path and exact scope; they do not load the whole
catalog. Owner instructions about resource economy and approved scope prevail.

| Role | Skills | Model/effort guidance |
| --- | --- | --- |
| Root coordination | task-coordination-strategies, team-composition-patterns | Decompose by ownership/dependencies; keep the smallest useful team |
| Targeted code exploration | code-explorer | Luna low/medium; promote only for concrete complexity |
| Focused review | code-reviewer | Luna medium for simple changes; Sol medium for lifecycle invariants |
| React component boundaries | vercel-composition-patterns | Sol medium for the current UI split |
| Browser workflow validation | playwright | Luna medium for a bounded smoke; stronger only if diagnosis needs it |
| Native asynchronous adapter | Actual pinned SDK types/docs | Sol high for dispatch/cancellation/recovery |

Sources: [agent-teams](https://github.com/wshobson/agents/tree/main/plugins/agent-teams/skills),
[OpenCode Power Pack](https://github.com/waybarrios/opencode-power-pack),
[Vercel](https://github.com/vercel-labs/agent-skills),
[OpenAI Playwright](https://github.com/openai/skills/tree/main/skills/.curated/playwright).
The full feature-dev workflow was not enabled. Power-Agent/PowerSkills was
inspected and not installed because it concerns electrical power systems.
