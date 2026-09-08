# Panel deployment

Production UI: <https://h1-cloud.ru/panel/>. API: `/panel/api/v2/`.
The HTTPS edge is NPM host 2; Panel runs separately on VM115.

## Everyday update

1. Open repository **Actions → Deploy → Run workflow**, branch `main`.
2. Choose `apply` and enter a **published** release tag, such as `v0.1.0-rc.5`.
3. Acknowledge that container replacement loses active AI runs and web sessions,
   then press **Run workflow**. Normal logout does not stop accepted agent work;
   container replacement is a different operation.

No SSH key, registry password or application credential is entered in Actions.
The job waits for the server and reports success only after exact image/revision
and public HTTPS health/auth checks. Allow up to two minutes for request pickup,
plus image download and startup. Cancellation of an Actions run does not revoke
an operation already accepted by the server; inspect its recorded result.

For rollback, run the same workflow with `rollback`; the version input is ignored.
The server validates that the requested rollback target is still the actual
previous release (or the last committed release for pending recovery).
Automatic failure recovery restores the exact pre-deploy version, not an
arbitrarily older image. A failed rollback remains an explicit failure/pending
journal, never a green deployment.

## One-time host bootstrap

These are operator actions on the approved VM115, not recurring release steps.

- Initial release: verified five-asset bundle, root-owned `/etc/homelab-panel.env`
  mode `0600`, state `/opt/homelab-panel/state` mode `0700`.
- Configuration: origin `https://h1-cloud.ru`, base path `/panel`, project
  `HL` / database ID `0-1`, allowlisted owner `kondor`. No fake Cursor key.
- Private TLS adapter: `scripts/bootstrap-panel-ingress.py`, service
  `homelab-panel-ingress`. It binds `10.202.2.52:18443`, accepts NPM
  `10.202.2.110` only, and forwards to Panel loopback `127.0.0.1:18080`.
  Its private key stays root-only on VM115. Only the public certificate goes
  to `/data/nginx/custom/panel-backend.crt` in NPM. Renew this certificate before
  expiration and update the NPM trust certificate in the same controlled change.
- NPM change: `scripts/configure-panel-npm.mjs` supports preview, then `--apply`.
  Run inside NPM using `node --input-type=module - --apply < script`.
  It verifies the original model/file agree, tests a candidate before cutover,
  preserves other routes, uses concurrency guards and keeps an exact backup.
  A concurrent-change error requires operator inspection, not a blind retry.
- Copy reviewed `scripts/cd.py` and `scripts/deploy.py` into
  `/opt/homelab-panel/deployer/` (directory `0700`, files `0600`, root-owned).
  Run `scripts/bootstrap-panel-cd.py` as root on VM115. It creates a separate
  OpenRC `homelab-panel-cd` service and the exact read-only status route.
  Release bundles do **not** update this privileged host executor automatically.

## Trust and isolation

The host polls the public repository's native GitHub Deployments API over
outbound HTTPS every 120 seconds. No incoming SSH, self-hosted Actions runner,
permanent GitHub token, Docker API exposure or bot credential is required.
This interval leaves room within the unauthenticated GitHub API rate limit;
shared-IP exhaustion is reported as a failed/late deployment, not bypassed.

`deployments:write` on this repository is a production capability. It is granted
only to the Deploy job here. Any other Actions writer deliberately granted that
permission is also a trusted deployer. Run ID/main/workflow checks validate
context; they are **not** cryptographic proof that the named run created the
request. Repository administrators and trusted release publishers are within the
trust boundary. Fork PR/read-only tokens cannot submit deployment requests.

Requests cannot supply commands, URLs, file paths, configuration or credentials.
Only published Panel releases, exact GitHub asset digests, successful release CI,
manifest revision and image metadata are accepted. Local installer locking and
the deployment journal protect against parallel/repeated operations.

Public `/panel/deployment-status.json` contains only deployment ID, bounded status
codes and release version/revision/image. It never contains operator config,
container environment, user content or secrets. The private journal is
`/opt/homelab-panel/cd/request.json`; a crash is not replayed as a new deployment.

Operator configuration is separate from releases. If it changes, CD stops with
`CONFIG_CHANGED_USE_OPERATOR_APPLY`: validate/apply that configuration separately
using the operator installer, then continue normal release updates. No secret
configuration is backed up into release/CD artifacts or rolled back from them.

The chosen path mount shares a browser origin with the other `h1-cloud.ru`
applications. Cookie Path is not a JavaScript security boundary. Panel still has
its own container, sessions and SDK; its only exchange with Fixik is YouTrack.
Without its independently provisioned Cursor API key, the UI works but model
execution is explicitly unavailable. That is not AI acceptance.
