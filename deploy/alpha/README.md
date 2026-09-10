# Cursor chat alpha on VM115

This increment adds agent management and durable Harness chat at the existing
`https://h1-cloud.ru/panel/` address. Cursor uses its native system prompt;
chat guidance is user-level content on every send. SDK tools, MCP, subagents and
inherited settings are disabled. Full A1 policy/tool acceptance and Codex remain
in HL-257 / HL-240. The original YouTrack UI and login remain available.

The operator builds an allowlisted Linux/amd64 bundle with
`scripts/harness-alpha/package.py DIRECTORY --version VERSION`, then builds
the `panel` and `harness` Dockerfile targets from that directory. Pass `VERSION`,
`SOURCE_BASE` and the SHA-256 of `release.json` as `BUNDLE_SHA256` build arguments.
Configure Compose with exact image IDs. This alpha does not use the legacy RC
Deploy workflow; its status endpoint still describes the legacy release.

Generate a fresh deployment configuration with `setup.py --container`, using
the verified YouTrack owner ID and a separately provisioned Cursor key. Install
only `panel-config`, `node-config`, `compose.yaml` into
`/opt/homelab-panel-alpha`. Create `state/node`, `state/cursor/home`, and
`router-state`, and `secrets/cursor-key` there. Before the first start, stop the
old Panel and run the new image once with `router-bootstrap`; it creates
`router-state/state.json` in sealed mode and never overwrites it. All mounted
configuration, state and key paths must
belong to `10001:10001`; private directories are `0700`, files `0600`.
The Panel mounts only its client trust configuration. Only Harness receives
the provider key and its own durable state. Keep CA/signing private keys in
operator storage, outside both container mounts. Certificates last 30 days;
renewal is an explicit operator follow-up before expiry.

Start the candidate on loopback port 18081. Verify exact images, version,
Panel health 200 / anonymous session 401, mTLS node identity and the authenticated
management → interaction → management flow. The previous RC5 container continues
running on 18080 throughout this alpha cutover.

After preflight, `activate.py switch --expected-config-sha256 BASELINE` changes
only the existing VM115 nginx upstream port, validates nginx, and reloads it.
It saves both configurations under `cutover/`; NPM routes and other applications
are unchanged. Validate the public URL after reload. On failure run:

```sh
doas python3 /opt/homelab-panel-alpha/activate.py rollback
```

Rollback restores the old upstream and preserves alpha history and containers;
it does not migrate or erase state. Future image updates must retain the same
state directories and pass compatibility checks. Stopping or restarting a node
with an active attempt can produce an explicit unknown result; do not retry it
blindly. This first chat smoke does not certify full crash recovery.
