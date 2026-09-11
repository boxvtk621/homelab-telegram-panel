#!/usr/bin/env bash
set -euo pipefail
panel_image=${1:?Usage: test-container.sh IMAGE}

[[ $(docker image inspect --format '{{.Config.User}}' "$panel_image") == 10001:10001 ]]
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true "$panel_image" version

# Missing config must fail closed, not open a listener or enter a restart loop.
set +e
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true "$panel_image" serve
status=$?
set -e
[[ $status == 2 ]] || { printf 'missing-config exit=%s, expected 2\n' "$status" >&2; exit 1; }

# Non-secret synthetic inputs. Neither validate nor serve needs a bot/YouTrack.
fixture_environment=(
  -e PANEL_LISTEN=0.0.0.0:18080
  -e PANEL_PUBLIC_ORIGIN=https://panel.example.invalid
  -e PANEL_OWNER_ID=owner.example
  -e PANEL_HARNESS_COMMANDS_ENABLED=false
)
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true "${fixture_environment[@]}" "$panel_image" validate

# Root serve must be rejected even with syntactically valid config.
set +e
docker run --rm --network none --read-only --cap-drop ALL --user 0:0 \
  --security-opt no-new-privileges:true "${fixture_environment[@]}" "$panel_image" serve
status=$?
set -e
[[ $status == 1 ]] || { printf 'root serve exit=%s, expected 1\n' "$status" >&2; exit 1; }
# Start only our disposable fixture. No mounts, bot config, or real credentials.
container_id=$(docker run -d --rm --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true -p 127.0.0.1::18080 \
  "${fixture_environment[@]}" "$panel_image" serve)
trap 'docker stop "$container_id" >/dev/null 2>&1 || true' EXIT
address=$(docker port "$container_id" 18080/tcp)
healthy=false
for attempt in {1..30}; do
  if curl --fail --silent --max-time 1 -H 'Host: panel.example.invalid' "http://$address/api/v2/healthz"; then healthy=true; break; fi
  sleep 0.1
done
[[ $healthy == true ]]
[[ $(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 2 \
  -H 'Host: panel.example.invalid' "http://$address/api/v2/harness/nodes") == 401 ]]
[[ $(docker inspect --format '{{len .Mounts}}' "$container_id") == 0 ]]
printf 'Independent Harness Panel starts without bot/YouTrack, health=200, unauthenticated nodes=401, mounts=0. Not production acceptance.\n'
