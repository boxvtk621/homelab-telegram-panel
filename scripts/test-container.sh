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

# Non-secret synthetic inputs. validate does not require a live Controller.
fixture_environment=(
  -e FIXIK_NEXT_MOBILE_LISTEN_ADDRESS=127.0.0.1:18080
  -e FIXIK_NEXT_MOBILE_PUBLIC_ORIGIN=https://panel.example.invalid
  -e FIXIK_NEXT_MOBILE_TELEGRAM_BOT_ID=100001
  -e FIXIK_NEXT_MOBILE_TELEGRAM_OWNER_ID=200002
  -e FIXIK_NEXT_MOBILE_TELEGRAM_ENVIRONMENT=test
  -e FIXIK_NEXT_MOBILE_CONTROLLER_BUSINESS_SOCKET=/run/fixik-mobile/application.sock
  -e FIXIK_NEXT_MOBILE_CONTROLLER_HEALTH_SOCKET=/run/fixik-mobile/application.sock.health
  -e FIXIK_NEXT_MOBILE_CONTROLLER_CONTROL_SOCKET=/run/fixik-mobile/application.sock.control
  -e FIXIK_NEXT_MOBILE_CONTROLLER_RECOVERY_SOCKET=/run/fixik-mobile/application.sock.recovery
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
printf 'Container packaging and fail-closed startup checks passed (not live Controller E2E).\n'
