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

The outer NPM route exposes only exact unauthenticated `GET`/`HEAD` readback for
`/panel/components.json`, `/panel/deployment-status.json` and
`/panel/api/v2/healthz`; nginx rejects other methods there. Every other
`/panel/` path stays behind owner Basic Auth, and the public readback locations
clear `X-Panel-Authenticated-User` before proxying. If any readback path returns
`401`, do not dispatch or retry a component deployment: an accepted Cursor/Codex
request may already have changed the host while Actions cannot observe its result.

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

### Additive Codex registry transition

Adding Codex to an already used Cursor alpha is an offline registry operation,
not another first bootstrap. Keep the existing owner ID, Cursor node entry,
Cursor certificate pin, and Cursor Router node state exact. In particular, keep
`registryVersion` unchanged: it is part of the durable Harness node identity, so
incrementing it would make the existing Cursor volume fail identity validation.
The new signed manifest hash is the fence that binds the replacement Router
state to the replacement registry.

Do not use `scripts/harness-alpha/setup.py` for this transition: it creates new
trust roots and its original single-node certificate profile is not a Codex
service identity. First create a private staging parent owned by root with mode
`0700`, locate the protected existing CA key by an operator-approved exact path,
and run the one-shot provisioner while the live files remain read-only:

```sh
python3 scripts/provision_codex_node.py \
  --registry /opt/homelab-panel-alpha/panel-config/registry.json \
  --signer-public-key /opt/homelab-panel-alpha/panel-config/registry-signing.pem \
  --ca-certificate /opt/homelab-panel-alpha/panel-config/ca.pem \
  --ca-private-key /root/EXACT_HARNESS_CA_KEY \
  --cursor-node-config /opt/homelab-panel-alpha/node-config/node.json \
  --gateway-certificate /opt/homelab-panel-alpha/panel-config/gateway.pem \
  --model EXACT_CODEX_MODEL \
  --effort low \
  --valid-days 14 \
  --output /opt/homelab-panel-alpha/cutover/codex-node
```

The provisioner verifies the signed one-Cursor registry, the existing Cursor
identity and gateway pin, the CA/private-key match, OpenSSL 3, and that the CA
outlives the requested leaf plus a one-hour safety margin. It issues a new
server-only certificate with `DNS:codex`, publishes without replacing an
existing output, and leaves the live registry, Router state, Compose, and
containers untouched. The staged `codex-config`, `codex-state`, `codex-auth`,
and `codex-workspace` trees belong to UID/GID 10001 with private permissions.
The workspace must be mounted read-only by the later Compose migration.
`codex-auth/codex` is deliberately empty. Authenticate only that directory with
the exact released digest; override the Harness entrypoint and mount no host
data except its dedicated auth and state trees:

```sh
docker run --rm -it \
  --user 10001:10001 \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --pids-limit 128 \
  --memory 512m \
  --network bridge \
  --env HOME=/state/codex/home \
  --env CODEX_HOME=/auth/codex \
  --mount type=bind,src=/opt/homelab-panel-alpha/cutover/codex-node/codex-state,dst=/state \
  --mount type=bind,src=/opt/homelab-panel-alpha/cutover/codex-node/codex-auth,dst=/auth \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=67108864 \
  --entrypoint /opt/codex/node_modules/.bin/codex \
  ghcr.io/boxvtk621/homelab-harness-codex@sha256:3aeed3be5b21123edcd82b8c221027925d322d241e224ad06a56375705919984 \
  login --device-auth
```

Before starting Harness, perform an offline native `account/read` against that
exact `CODEX_HOME`. This command emits only a safe success marker and sends no
`thread/start` or `turn/start`, so the readback remains zero-turn:

```sh
docker run --rm \
  --user 10001:10001 \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --pids-limit 64 \
  --memory 256m \
  --network none \
  --env HOME=/state/codex/home \
  --env CODEX_HOME=/auth/codex \
  --mount type=bind,src=/opt/homelab-panel-alpha/cutover/codex-node/codex-state,dst=/state \
  --mount type=bind,src=/opt/homelab-panel-alpha/cutover/codex-node/codex-auth,dst=/auth \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=67108864 \
  --entrypoint /usr/local/bin/node \
  ghcr.io/boxvtk621/homelab-harness-codex@sha256:3aeed3be5b21123edcd82b8c221027925d322d241e224ad06a56375705919984 \
  -e '
    const {spawn}=require("node:child_process");
    const child=spawn("/opt/codex/node_modules/.bin/codex",
      ["-c","forced_login_method=\"chatgpt\"","app-server"],
      {stdio:["pipe","pipe","ignore"]});
    let pending="",total=0,initialized=false,finished=false;
    const fail=()=>{
      if(finished)return;
      finished=true;
      child.kill("SIGKILL");
      process.exit(1);
    };
    const timer=setTimeout(fail,15000);
    const send=value=>child.stdin.write(JSON.stringify(value)+"\n");
    child.stdout.setEncoding("utf8");
    child.stdout.on("data",chunk=>{
      total+=Buffer.byteLength(chunk);
      if(total>1048576)return fail();
      pending+=chunk;
      for(;;){
        const newline=pending.indexOf("\n");
        if(newline<0)break;
        const line=pending.slice(0,newline);
        pending=pending.slice(newline+1);
        let value;
        try{value=JSON.parse(line);}catch{return fail();}
        if(value.id===1&&!initialized){
          if(value.error||value.result?.codexHome!=="/auth/codex")return fail();
          initialized=true;
          send({method:"initialized",params:{}});
          send({id:2,method:"account/read",params:{refreshToken:false}});
        }else if(value.id===2){
          if(!initialized||value.error||value.result?.account?.type!=="chatgpt")return fail();
          finished=true;
          clearTimeout(timer);
          process.stdout.write("{\"status\":\"CODEX_ACCOUNT_READY\",\"modelCalls\":0}\n");
          child.kill("SIGTERM");
          setTimeout(()=>child.kill("SIGKILL"),1000).unref();
        }
      }
    });
    child.on("error",fail);
    child.stdin.on("error",fail);
    child.on("exit",()=>{if(!finished)fail();});
    send({id:1,method:"initialize",params:{clientInfo:{name:"hl260_enrollment",version:"1"}}});
  '
```

Never copy a desktop `CODEX_HOME`, MCP configuration, plugins, skills, or
browser session.
If publication reports `PUBLICATION_DURABILITY_UNKNOWN`, inspect and preserve
the published directory; do not rerun with another node ID.

First stop the component consumer and Panel. Do not stop, replace, or modify the
Cursor container or its state volume. Prepare a Codex node config with the same
owner and registry version, a new node UUID, and a new server certificate issued
by the existing Harness CA. Then run the repository copy of the preparation tool
with exact absolute paths:

```sh
python3 scripts/registry_transition.py \
  --registry /opt/homelab-panel-alpha/panel-config/registry.json \
  --router-state /opt/homelab-panel-alpha/router-state/state.json \
  --signer-public-key /opt/homelab-panel-alpha/panel-config/registry-signing.pem \
  --signer-private-key /root/EXACT_REGISTRY_SIGNING_KEY \
  --ca /opt/homelab-panel-alpha/panel-config/ca.pem \
  --codex-node-config /opt/homelab-panel-alpha/cutover/codex-node/codex-config/node.json \
  --codex-certificate /opt/homelab-panel-alpha/cutover/codex-node/codex-config/node.pem \
  --codex-name 'Codex alpha' \
  --codex-url https://codex:18443 \
  --output /opt/homelab-panel-alpha/cutover/add-codex-registry
```

Use OpenSSL 3; pass its absolute path with `--openssl` where the system
`openssl` is LibreSSL. The tool acquires the Router lock and refuses a running
Panel. It verifies the current registry signature and matching Router-state
hash, the existing single Cursor binding, the signer key, Codex config identity,
certificate CA/hostname/pin, and duplicate node ID, certificate, or endpoint;
endpoint identity uses a lower-case hostname and effective HTTPS port. Its
output parent must be an absolute, real directory owned by the invoking euid
with exact mode `0700`. The tool snapshots the already file-validated signer
keys, CA, and node certificate into a private temporary directory before
passing them to OpenSSL. It does not read provider auth, contact either node, or
modify the source files.

The output directory is published with an atomic no-replace directory rename
only after all files are synced. `next/registry.json` and `next/state.json` are
one inseparable pair; `rollback/` contains the exact source bytes, and
`transition.json` records source/target hashes. The next state preserves the
complete Cursor node state and adds exactly one Codex node as `sealed`,
generation/identity epoch zero, `operationId=bootstrap`. If the tool reports
`PUBLICATION_DURABILITY_UNKNOWN`, the rename completed but the parent-directory
fsync did not. Treat this as an ambiguous terminal result: inspect the existing
output bundle and its hashes, preserve it, and do not rerun the command with the
same or another output path until that inspection decides the recovery action.
Install both next files with private ownership and mode `0600` while Panel
remains stopped. A crash after only one file is replaced is fail-closed because
Panel rejects the registry/state hash mismatch; never repair that state by hand.
Restore both exact rollback files before restarting Panel.

#### Install or recover the prepared pair

The repository installer performs only this offline file transition; it does
not provision a node, invoke Compose, update the CD ledger, or run a shell
command. Run it as root after stopping Panel and every other Router consumer.
The prepared bundle and journal directory must be separate, root-owned `0700`
trees, with files `0600`; the journal path and its temporary-file parent are
rejected anywhere inside the immutable bundle. Create the empty journal
directory before `apply`, but not the journal file itself. The existing
registry, Router state, public signer key and Router
lock must be `0600` and owned by the same runtime UID; the registry and state
also keep their existing common GID, and their respective `0700` parent
directories keep that UID. The component CD lock and the authoritative
consumer-status file are root-owned `0600`.

The stopped-state seam is deliberately external to this tool. The same root
service supervisor that controls Panel must maintain the `--consumer-status`
file under the component CD lock, with exact content
`{"schema":1,"service":"panel","state":"stopped","pid":null}`, and its
configured `--consumer-pid-file` must not exist. Do not hand-write a stale
status assertion. The installer takes the Router and component CD locks
non-blockingly and rechecks both stopped signals before and between replacements.

```sh
sudo python3 scripts/registry_pair_install.py apply \
  --bundle /opt/homelab-panel-alpha/cutover/add-codex-registry \
  --registry /opt/homelab-panel-alpha/panel-config/registry.json \
  --router-state /opt/homelab-panel-alpha/router-state/state.json \
  --signer-public-key /opt/homelab-panel-alpha/panel-config/registry-signing.pem \
  --router-lock /opt/homelab-panel-alpha/router-state/router.lock \
  --deploy-lock /opt/homelab-agents-cd/deploy.lock \
  --consumer-status /run/homelab-panel/panel-status.json \
  --consumer-pid-file /run/homelab-panel/panel.pid \
  --journal /opt/homelab-panel-alpha/registry-install/install.json \
  --openssl /absolute/path/to/openssl3
```

`apply` accepts only the exact rollback registry/state pair and creates and
fsyncs its no-replace journal before replacing either runtime file. Each target
is written to a same-directory `0600` temporary file, fsynced, and installed
with `os.replace`, registry first and Router state second; the parent directory
is fsynced after each replace. Completion requires the exact requested bytes,
valid registry signature and matching Router-state manifest hash. The installer
also proves that the target preserves the complete Cursor entry/state and adds
only the expected initially sealed Codex node.

After any interruption, OSError, partial pair, or existing journal, keep Panel
stopped and do not run `apply` again. Inspect the root-owned journal and current
file hashes, then converge from the known source/target hash matrix explicitly:

```sh
sudo python3 scripts/registry_pair_install.py recover --direction target \
  --bundle /opt/homelab-panel-alpha/cutover/add-codex-registry \
  --registry /opt/homelab-panel-alpha/panel-config/registry.json \
  --router-state /opt/homelab-panel-alpha/router-state/state.json \
  --signer-public-key /opt/homelab-panel-alpha/panel-config/registry-signing.pem \
  --router-lock /opt/homelab-panel-alpha/router-state/router.lock \
  --deploy-lock /opt/homelab-agents-cd/deploy.lock \
  --consumer-status /run/homelab-panel/panel-status.json \
  --consumer-pid-file /run/homelab-panel/panel.pid \
  --journal /opt/homelab-panel-alpha/registry-install/install.json \
  --openssl /absolute/path/to/openssl3
```

Use the identical command with `--direction rollback` for an explicit rollback.

Recovery rejects an unknown file hash, a modified journal or a journal belonging
to another bundle. It never issues an automatic inverse operation. Repeating
the same completed recovery is an idempotent exact readback, not another
replacement. Keep the journal as transition evidence.

This rollback is valid only before the new Codex node is activated or accepts
work. After installing the next pair, read Router state and require Cursor to be
unchanged and Codex to remain sealed. Activation, component-ledger enrollment,
Codex authorization, provider smoke, release, and deployment are separate gates;
keep the consumer stopped until those gates are explicitly completed.

#### Enroll the running Codex node in component CD

Enrollment is a separate one-shot operation after the registry pair is installed,
Panel is running against that pair, and the exact Codex container has already
been started from an operator-reviewed Compose candidate. Stop
`homelab-components-cd` first and require its pid file to be absent. Do not stop
or replace Panel, Cursor, or Codex for this step. The current component ledger
must have `pending=null` and contain exactly Panel and Cursor.

Keep the candidate beside the live Compose file so relative bind paths retain
their meaning. It must add the already-running `codex` service and its private
mounts; review it independently and pin the exact released digest in that
candidate without changing the existing image env, then record both hashes. The
enrollment tool validates resolved Compose without printing its interpolation
and never runs `compose up`, `pull`, `stop`, or another container mutation.

```sh
sha256sum \
  /opt/homelab-panel-alpha/compose.yaml \
  /opt/homelab-panel-alpha/compose.codex-candidate.yaml

sudo python3 scripts/enroll_codex_component_cd.py apply \
  --root /opt/homelab-agents-cd \
  --journal /opt/homelab-panel-alpha/codex-cd-enrollment \
  --router-lock /opt/homelab-panel-alpha/router-state/router.lock \
  --consumer-pid-file /run/homelab-components-cd.pid \
  --candidate-compose /opt/homelab-panel-alpha/compose.codex-candidate.yaml \
  --expected-current-compose-sha256 EXACT_CURRENT_COMPOSE_SHA256 \
  --candidate-compose-sha256 EXACT_REVIEWED_CANDIDATE_SHA256 \
  --codex-tag codex-vX.Y.Z \
  --codex-node-id EXACT_PROVISIONED_CODEX_NODE_UUID
```

The root-only tool holds the existing component `deploy.lock` for the complete
operation and requires the live Panel to retain the Router's exclusive
`router.lock`; Router state changes still use its versioned CAS API. Before the
first write it downloads and verifies the exact Codex release manifest, image
digest, labels, non-root/read-only/capability isolation, native Harness identity,
idle snapshot, and the candidate Compose. Cursor must remain the exact eligible
route recorded at entry, while Codex must be the exact sealed `bootstrap` route.
No provider auth, token, message, or Harness database is read.

The no-replace `0700` journal contains private `0600` source/target bytes and
hashes before the live Compose, config, or ledger changes. The tool atomically
installs candidate Compose, config that adds only Codex, and ledger that adds only
the exact Codex manifest with `previous=null`; Panel/Cursor entries remain exact
and the config fingerprint is updated. Only after all three read back durably
does it persist `activation-pending` and activate exactly the Codex node. Cursor
is never an activation target.

Any interruption or `ENROLLMENT_STATE_UNKNOWN` is an operator gate: do not run
`apply` again and do not delete the journal. Inspect its `state.json` and the live
file/Router readback, then choose an explicit direction:

```sh
sudo python3 scripts/enroll_codex_component_cd.py recover \
  --direction target \
  --root /opt/homelab-agents-cd \
  --journal /opt/homelab-panel-alpha/codex-cd-enrollment \
  --router-lock /opt/homelab-panel-alpha/router-state/router.lock \
  --consumer-pid-file /run/homelab-components-cd.pid
```

Use `--direction rollback` with the same arguments only while Codex is still the
exact sealed bootstrap route. Once exact activation is observed, rollback is
rejected; forward recovery reads that result without replaying activation.
Enrollment rollback restores the exact prior Compose/config/ledger, but it does
not roll back the registry pair or stop Codex. Keep the component consumer
stopped, explicitly roll back the registry pair, and verify the old Router before
starting the consumer. After successful target completion, start the consumer
and verify public component status names all three exact manifests before any
Codex deploy/rollback acceptance.

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
