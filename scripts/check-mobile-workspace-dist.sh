#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'Mobile Workspace dist check failed: %s\n' "$*" >&2
  exit 1
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
web_root="$source_root/web/mobile-workspace"
tracked_dist="$source_root/internal/mobilegatewayassets/dist"
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/fixik-mobile-workspace.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT
generated_dist="$temporary_root/dist"

[[ -d "$tracked_dist" ]] || die "tracked embedded dist is missing"
[[ -x "$web_root/node_modules/.bin/vite" ]] || die "frontend dependencies are missing; run npm ci in $web_root"

(
  cd "$web_root"
  npm run build -- --outDir "$generated_dist"
)

if find "$generated_dist" -type f -name '*.map' -print -quit | grep -q .; then
  die "generated dist contains source maps"
fi

if ! diff -ru "$tracked_dist" "$generated_dist"; then
  die "tracked embedded dist differs from a clean npm build"
fi

printf 'Mobile Workspace embedded dist is reproducible.\n'
