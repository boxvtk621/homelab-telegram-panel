# Cursor alpha release — 2026-09-10

Live: https://h1-cloud.ru/panel/ — `alpha-20260910.2`, VM115 `10.202.2.52`.
Canonical result and remaining acceptance: [HL-241@5](https://youtrack.h1-cloud.ru/issue/HL-241).
This release adds the Agents management screen and independent durable Harness
chat. Cursor has no tools; guidance accompanies each message at user priority,
with the owner's approval after the server rejected the gated `systemPrompt`.

## Source handoff

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

- Panel image: `sha256:36f5d6b585c9fcd7fc42466514fe4cca01ab89353359c4a7543f2710a3acf83b`.
- Harness image: `sha256:a549b83c460c7391dfbb706b7f60b47538c9bac82d8f08fdc01ba041d0f77817`.
- Node: `2fd6caa6-0b0e-440d-9503-701fedcc2568`; verified owner `kondor`, ID `2-1`.
- Deployment root: `/opt/homelab-panel-alpha`; internal candidate port 18081.
- Existing VM115 nginx upstream changed from 18080 to 18081. NPM routes,
  Portainer and the old RC5 container were retained.
- Return command: `doas python3 /opt/homelab-panel-alpha/activate.py rollback`.
  Exact before/after configurations are in `cutover/`. Reverse cutover was
  prepared and reviewed but was not executed during this delivery.

Owner login and actual server chat remain user acceptance. The supplied
YouTrack credential belongs to `Cursor_Agent`; it was used only to resolve the
owner ID and cannot log into the owner-only Panel. Use a personal `kondor` token.
No token values are stored in this report or in Git.

Initial node status may display `policy_unavailable` until the first dispatch;
message admission still works. Full A1 policy/tool acceptance and Codex remain
in HL-257 / HL-240. The legacy Deploy workflow/status still describes RC5 and
does not update this alpha. Node certificates expire after 30 days; plan their
renewal before expiry. See [deployment procedure](../deploy/alpha/README.md).

## Subsequent CI/CD integration

On 2026-09-10 the owner authorized integrating this source checkpoint and
commit/push under HL-242@5. The eleven allowlisted source, packaging and report
files were copied and SHA-256 verified in a separate CI/CD worktree. The original
source session and runtime credentials/state remain unchanged. See
[component delivery](../deploy/components/README.md) for the maintained pipeline.
