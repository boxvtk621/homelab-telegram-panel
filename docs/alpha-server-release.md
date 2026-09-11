# Cursor alpha release — 2026-09-10

Live: https://h1-cloud.ru/panel/ — component baseline Panel/Cursor
`v0.2.0-rc.5`, source `262cae55aa4ca2ea64e8fb3347ff574317570836`, VM115
`10.202.2.52`. The original bundled alpha was `alpha-20260910.2`.
Canonical result and remaining acceptance: [HL-241@5](https://youtrack.h1-cloud.ru/issue/HL-241).
This release adds the Agents management screen and independent durable Harness
chat. Cursor has no tools; guidance accompanies each message at user priority,
with the owner's approval after the server rejected the gated `systemPrompt`.

## Source handoff

This section preserves the original bundled-alpha handoff. Its allowlisted
source was subsequently integrated through a separate linked worktree; current
component source is committed and published on `main` at `262cae5`.

- Worktree: `/Users/kondor/.codex/visualizations/2026/09/10/01a089ec-9126-7000-9a0b-9398a973fd1a/panel-alpha`.
- Branch: `codex/hl-241-cursor-alpha-release-20260910`.
- Base: `3bf12489649b029b866f9d168553b6b53911d15f`.
- Changes remain uncommitted; no commit, push or main integration was requested.
  Integration requires a scoped commit and separately authorized merge/push.
- Changed paths: `harness/adapters/cursor/worker/{worker.mjs,worker_test.mjs}`,
  `scripts/harness-alpha/{setup.py,smoke.py,package.py,activate.py}`,
  `deploy/alpha/{Dockerfile,compose.yaml,README.md}`,
  `docs/preview/harness-smoke.cjs`, and this report.

## Evidence

- `make quality` passed in this worktree. After the worker correction its
  focused Node suite passed 5/5.
- Native local mTLS smoke: two completed attempts returned `КЕДР-482`,
  including continuation in the same native dialog. The third attempt stopped
  as `interrupted`, `effectStatus=known`, with durable queue pause.
- Four SDK sends total, including the first unsupported-option failure.
  No retry of an unknown command. No further model sends on the server.
- Two assistant replies and manual pause survived a local node restart.
- Existing browser fixture tests passed desktop/mobile navigation, queued
  acknowledgement and controls; zero page errors. These are synthetic UI tests.
- Independent bounded review passed after tightening the smoke assertions.
- VM115 image/version/SDK checks passed. Both services run as UID10001,
  read-only rootfs, with separate private configuration and Harness state.
- Real Panel → Harness mTLS identity/snapshot passed. Public health is 200;
  anonymous session is 401. Public HTML, JS and CSS match the checked bundle
  byte-for-byte.

Local evidence is in `.recovery/quality.log`, `native-smoke-2.log`,
`native-stop-readback.json`, `browser-smoke.log`, `browser-controls.log`.
The credential-free bundle is `.recovery/alpha-20260910.2`; its checksum manifest
and files also remain at `/home/alpine/alpha-20260910.2` on VM115.

## Runtime and rollback

- Panel image: `ghcr.io/boxvtk621/homelab-telegram-panel@sha256:0f9d9217e091a4d7b62a894983ef093876cb1d6d05ebefd839a135e6afab2bb5`
  (`panel-v0.2.0-rc.5`).
- Harness image: `ghcr.io/boxvtk621/homelab-harness-cursor@sha256:f67cafe1082174c78cba6130074c113ed30464c4a59e0f442e67874f1d01670c`
  (`cursor-v0.2.0-rc.5`).
- Node: `2fd6caa6-0b0e-440d-9503-701fedcc2568`; verified owner `kondor`, ID `2-1`.
- Deployment root: `/opt/homelab-panel-alpha`; internal candidate port 18081.
- Existing VM115 nginx upstream changed from 18080 to 18081. NPM routes,
  Portainer and the old RC5 container were retained.
- Router is `eligible`, generation 5, state version 14, identity epoch 1; the
  Cursor snapshot is ready, complete and idle with no pending or active attempt. The root-only
  outbound component consumer is enrolled and its public projection is
  `/panel/components.json`.
- Live component workflow acceptance completed on 2026-09-11. Panel moved to
  `v0.2.0-rc.6` and back to RC5; Cursor moved to `v0.2.0-rc.7` and back to RC5.
  The final Panel/Cursor containers match the exact RC5 digests above, are
  running as UID 10001 with read-only root filesystems and zero restarts, and
  public health reports `up`. Rollback candidates are Panel RC6 and Cursor RC7.
  Evidence: Panel [apply](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34575495295)
  / [rollback](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34575943680),
  Cursor [apply](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34576592232)
  / [rollback](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34583399547).
- A request for missing `panel-v0.2.0-rc.999` failed closed in
  [run 6](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34584140078)
  before a host deployment request: public request ID, both container
  IDs/digests, Router generation/state version and health remained unchanged.
- Return command: `doas python3 /opt/homelab-panel-alpha/activate.py rollback`.
  Exact before/after configurations are in `cutover/`. Reverse cutover was
  prepared and reviewed but was not executed during this delivery.

Owner login and actual server chat remain user acceptance. The supplied
YouTrack credential belongs to `Cursor_Agent`; it was used only to resolve the
owner ID and cannot log into the owner-only Panel. Use a personal `kondor` token.
No token values are stored in this report or in Git.

The historical alpha image may leave a never-used node at
`policy_unavailable` until first dispatch. For the first Router cutover, only
that exact zero-version, empty sentinel is accepted by preflight; the target
Harness validates configured policy on reopen and becomes ready without a
provider call. Any used or otherwise blocked state remains a hard stop. Full A1
policy/tool acceptance and Codex remain in HL-257 / HL-240. The legacy Deploy
workflow remains separate from the enrolled component consumer. Real component
apply/rollback/failure workflow acceptance is complete as recorded above. Node
certificates expire after 30 days; plan their renewal before expiry. See
[deployment procedure](../deploy/alpha/README.md).

## Subsequent CI/CD integration

On 2026-09-10 the owner authorized integrating this source checkpoint and
commit/push under HL-242@5. The eleven allowlisted source, packaging and report
files were copied and SHA-256 verified in a separate CI/CD worktree. The original
source session and runtime credentials/state remain unchanged. See
[component delivery](../deploy/components/README.md) for the maintained pipeline.
