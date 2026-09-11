# Independent component delivery

Target: three containers, `panel`, `cursor`, `codex`. Fixik is a different
application and is not a build, configuration, credential or deployment dependency.
Panel packages both UI screens. Each Harness owns its native runtime, private
configuration, credentials and persistent history. Panel receives only gateway
trust material. No agent port is published to LAN; Panel listens on loopback
18081 behind the existing HTTPS edge.

## Current readiness

- Panel and Cursor: source Docker builds and independent release/deploy paths.
  VM115 is enrolled on the exact `v0.2.0-rc.5` component manifests from source
  `262cae55aa4ca2ea64e8fb3347ff574317570836`; Router admission and the outbound
  CD consumer are active. Live workflow-dispatch acceptance completed on
  2026-09-11: Panel apply/rollback, Cursor apply/rollback, and a fail-closed
  request for a missing release all produced exact readback without changing the
  unselected component. The current baseline is again Panel/Cursor
  `v0.2.0-rc.5`; rollback candidates are Panel `v0.2.0-rc.6` and Cursor
  `v0.2.0-rc.7`.
- Codex: the native Harness adapter and independent image are implemented on
  exact `codex app-server 0.153.4`. Local race/quality gates and the isolated
  zero-turn container smoke cover native `thread/start`, exact feature-policy
  readback, an empty per-thread MCP inventory, generated-schema pins, an empty durable dispatch ledger, mTLS
  identity and restart without provider credentials or a model call. The
  current slice is deny-only: an empty Harness tool manifest is paired with
  explicit native tool-feature overrides, read-only/no-network execution, and
  fail-unknown handling for every tool-shaped or future native item. This does
  not claim `explicit_once` support. Authenticated model-turn acceptance,
  `explicit_once` approvals and VM115 enrollment remain separate gates; the
  Compose profile is still inactive until its private auth/config/state mounts are
  provisioned.
- Server alpha fixes from HL-241 are integrated with the owner's approval,
  including the Cursor account's native prompt compatibility correction and
  container setup. The source session remains unchanged; provider credentials
  and runtime state were not copied.
- The component workflows are available on GitHub `main`, and the host consumer
  is enrolled. Legacy `Deploy` operates the retained RC5 fallback service; it
  does not update the current alpha.

## Release and deployment

1. Merge the reviewed source into `main` after the normal publication approval.
2. Create a unique tag `panel-vX.Y.Z`, `cursor-vX.Y.Z`, or `codex-vX.Y.Z`.
   Each tag triggers **Release component** for that
   component. All tags must point to a commit reachable from `main`. Push each
   release tag separately: GitHub does not create tag push events when more than
   three tags are pushed at once. Burn a version whose event or release failed;
   never delete and recreate or reuse its tag.
3. CI runs quality, builds a pinned linux/amd64 image, performs a real isolated
   container smoke, pushes that tested image, and publishes its digest plus exact
   source revision and state compatibility in `release.json`. Never reuse a tag.
4. In **Actions → Deploy component → Run workflow**, select `main`, component,
   `apply`, and the published version. Acknowledge the selected container restart.
   The host asks Router to stop new assignments, waits for a complete idle
   snapshot with `pendingCount=0`, seals mutations, and only then replaces the
   container. It never changes or depends on Harness `queuePaused`. Panel
   restarts invalidate its web sessions.
5. The job waits for the server's result. Only the selected Compose service is
   replaced with `--no-deps`; other container IDs are checked. Harness identity
   identity epoch must stay exact and Router generation increases exactly once,
   with exact image version/revision/digest.

The single concurrency group serializes all three components, including requests
that otherwise could race through the Panel used for mTLS checks. Cancellation
of Actions cannot revoke an operation already accepted by the server. Check
`/panel/components.json` before deciding on another request. A crashed installer
reconciles only exact target/prior container identity and exact atomic Router
state. Ambiguity remains fenced with `operator_action_required=true`; it never
authorizes a second Docker mutation.

Rollback uses the selected component's exact previous manifest. The installer
rejects a changed schema/native mapping compatibility hash before Docker changes.
Compatible failure recovery restores the exact pre-deploy image; failed recovery
retains `pending` and does not report success. A schema/driver/native-store format
change requires a separate migration and backup/restore procedure, including
proof that queued/new effects will not be lost. Do not replace or delete volumes.
This hash gate is conservative, not a proof of general semantic compatibility.
Panel releases now carry the SHA-256 of `harness-router-state-v1`; the former
`stateless-panel-v1` releases are intentionally not compatible rollback targets.

## One-time enrollment on VM115

The consumer runs outside application containers, as the existing privileged
operator installer does. It has outbound HTTPS only, no persistent GitHub token,
no incoming CI SSH, and no Docker socket exposed to Panel or either Harness.
`deployments:write` is a repository-level production capability; run metadata
checks are context validation, not cryptographic attribution to a particular run.
The new consumer makes 15 idle polls/hour; together with the legacy consumer's
30 polls/hour this leaves 15 requests/hour for release validation under GitHub's
unauthenticated limit. Shared-egress use can still cause explicit rate failures.
The new workflow adds that permission only to its Deploy job.

1. Publish and install the approved exact initial images. Preserve existing node
   identity, registry, trust material, private configuration and state. The server
   alpha cannot be enrolled as a fictitious published revision: its source and
   images first need a real immutable release. Do not regenerate identities or
   silently copy auth from a desktop Codex HOME. For the first Router cutover,
   create a `0700` directory owned by UID 10001. Before stopping the old Panel,
   run the target image's read-only `harness-preflight`; it requires every node
   to report `readiness=ready`, no blocked reasons, a complete idle snapshot and
   `pendingCount=0`. The sole first-cutover exception is the exact legacy
   pristine sentinel: state/event/queue versions zero, no attempts or pending
   work, and only `policy_unavailable`. Any used or otherwise blocked state is a
   hard stop.
   Then stop the old Panel, run the new image once with `router-bootstrap`, and
   start it with the same state mount. Bootstrap starts every node sealed and
   never overwrites existing state. Replace the Harness while Router remains
   sealed; the target Harness must validate its policy files on reopen and report
   `ready` before component enrollment can activate Router admission.
2. Place reviewed `cd.py`, `deploy.py`, `component_release.py`,
   `component_deploy.py`, `component_cd.py` and `bootstrap-component-cd.py` in
   `/opt/homelab-agents-cd/executor/`, root-owned, directory 0700, files 0600.
   Bundles never update this privileged executor automatically.
3. Create `/opt/homelab-agents-cd/config.json` (root 0600). It points to the
   existing operator-owned Compose and literal image env files (root 0600).
   A new layout uses the example `compose.yaml`; it is not a migration command.
   Panel's env file retains only its Panel/registry settings. Browser auth is
   enforced at NPM and projected through the overwritten trusted user header;
   no YouTrack or provider credential belongs in Panel. Codex uses a dedicated
   `CODEX_HOME`: provision only its provider authentication and reviewed minimal
   config; never copy desktop MCP, plugin or skill configuration into it. Separate mounts must
   already exist; `create_host_path: false` avoids fake empty state.

Example for the existing alpha service names (Codex is added only after HL-258):

```json
{
  "project": "homelab-panel-alpha",
  "compose": "/opt/homelab-panel-alpha/compose.yaml",
  "env_file": "/opt/homelab-panel-alpha/images.env",
  "router_socket": "/opt/homelab-panel-alpha/router-state/control.sock",
  "components": {
    "panel": {"service": "panel"},
    "cursor": {
      "service": "harness",
      "url": "https://harness:18443",
      "node_id": "EXACT_EXISTING_NODE_UUID",
      "actor_id": "EXACT_EXISTING_OWNER_ID"
    }
  }
}
```

4. Run `python3 /opt/homelab-agents-cd/executor/bootstrap-component-cd.py --tag
   panel-vX.Y.Z --tag cursor-vX.Y.Z --expected-ingress-sha256 HASH` as root. It
   checks existing running digests/identity, activates the exact sealed Router
   bootstrap state, enrolls baselines, publishes the
   read-only status route, then starts the 240-second outbound consumer. It does
   not replace application containers or replay historical requests. Inspect a
   failed bootstrap's saved state before any retry; do not remove state blindly.
5. Execute one real Deploy and one compatible Rollback, verify GitHub run results,
   selected digest/health and unchanged other containers. Test a failing request.
   This acceptance was completed on 2026-09-11: Panel
   [apply](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34575495295)
   and [rollback](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34575943680),
   Cursor [apply](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34576592232)
   and [rollback](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34583399547),
   plus a [missing-release failure](https://github.com/boxvtk621/homelab-telegram-panel/actions/runs/34584140078).
   Final readback showed both exact RC5 digests, both containers running as
   UID 10001 with read-only root filesystems and zero restarts, Router
   `eligible` at generation 5 / state version 14 / identity epoch 1, and public
   health `up`. The failing request created no host deployment request and left
   those values unchanged.

### First Router cutover on the existing VM115 alpha

Use the release manifest's digest, never a mutable tag. The documented
2026-09-10 fallback is the still-running RC5 upstream on port 18080; the prior
alpha image IDs were Panel
`sha256:36f5d6b585c9fcd7fc42466514fe4cca01ab89353359c4a7543f2710a3acf83b`
and Harness
`sha256:a549b83c460c7391dfbb706b7f60b47538c9bac82d8f08fdc01ba041d0f77817`.
Verify these live before using this procedure. Record the current ingress and
Compose hashes in the private cutover directory, then perform the bounded
sequence:

```sh
(
set -eu
cd /opt/homelab-panel-alpha
doas install -d -m 0700 -o 10001 -g 10001 router-state
doas sh -c 'sha256sum /etc/homelab-panel/nginx.conf /opt/homelab-panel-alpha/compose.yaml > /opt/homelab-panel-alpha/cutover/router-preflight.sha256'
export ALPHA_PANEL_IMAGE='ghcr.io/boxvtk621/homelab-telegram-panel@sha256:EXACT_RELEASE_DIGEST'
export ALPHA_HARNESS_IMAGE='ghcr.io/boxvtk621/homelab-harness-cursor@sha256:EXACT_RELEASE_DIGEST'
docker compose run --rm --no-deps panel harness-preflight
docker compose stop panel
docker compose run --rm --no-deps panel router-bootstrap
docker compose up -d --no-deps panel
doas curl --fail --silent --show-error --max-time 10 --unix-socket router-state/control.sock http://localhost/v1/state
docker compose up -d --no-deps harness
curl --fail --silent --show-error --max-time 10 -H 'Host: h1-cloud.ru' -H 'X-Forwarded-Proto: https' \
  http://127.0.0.1:18081/panel/api/v2/healthz
)
```

The state readback must show every enrolled node `sealed` with
`operationId=bootstrap`; health alone is not permission to open admission. Only
then run `bootstrap-component-cd.py`. Its strict activation check requires the
target Harness to report `ready` with no blocked reasons, verifies exact release
manifests/runtime, and atomically opens all bootstrap nodes.

If any step after stopping Panel fails, do not delete Router state and do not
restart the incompatible old alpha Panel on port 18081. Keep the candidate
stopped and restore the retained RC5 upstream:

```sh
docker compose stop panel
doas python3 /opt/homelab-panel-alpha/activate.py rollback
```

Each successful operation persists a `<component>.override.json` in the CD root.
The operator Compose/env is pinned by hash and intentionally not rewritten. Use
the consumer for updates. A manual `compose up` with only the original image env
would revert desired images; operator reconciliation must use current manifests.
Config changes require separate operator validation and re-enrollment; no secret
config is included in releases or public status. Certificates and native login
renewal remain separate operator lifecycles.
