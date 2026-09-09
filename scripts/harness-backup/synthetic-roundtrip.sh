#!/bin/sh
# Exercise a real archive write to the target and restore from that archive.
set -eu

MOUNTPOINT=/srv/harness-backups
BACKUP_USER=harness-backup
RUN_ID=

usage() {
  cat <<'EOF'
Usage: synthetic-roundtrip.sh [--run-id YYYYmmddTHHMMSSZ]
EOF
}

die() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-id) [ "$#" -ge 2 ] || die 'missing run ID'; RUN_ID=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die 'must run as root'
mountpoint -q "$MOUNTPOINT" || die "$MOUNTPOINT is not mounted"
[ "$(findmnt -nro FSTYPE --target "$MOUNTPOINT")" = ext4 ] || die 'backup target is not ext4'
id "$BACKUP_USER" >/dev/null 2>&1 || die 'backup account is missing'

if [ -z "$RUN_ID" ]; then
  RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
fi
case "$RUN_ID" in
  *[!0-9TZ]*) die 'run ID contains unsafe characters' ;;
esac

evidence_dir="$MOUNTPOINT/incoming/.synthetic-test/$RUN_ID"
[ ! -e "$evidence_dir" ] || die "evidence path already exists: $evidence_dir"
install -d -m 0700 -o "$BACKUP_USER" -g "$BACKUP_USER" "$evidence_dir"

work_dir=$(mktemp -d /tmp/harness-backup-roundtrip.XXXXXX)
chown root:"$BACKUP_USER" "$work_dir"
chmod 0750 "$work_dir"
cleanup() {
  rm -rf "$work_dir"
}
trap cleanup EXIT HUP INT TERM
source_dir="$work_dir/source"
restore_dir="$work_dir/restored"
install -d -m 0750 -o root -g "$BACKUP_USER" "$source_dir/nested dir"
printf 'harness backup synthetic payload\n' > "$source_dir/root.txt"
printf '\000\001\002\003\377\n' > "$source_dir/nested dir/binary.dat"
: > "$source_dir/empty"
ln -s 'nested dir/binary.dat' "$source_dir/link-to-binary"
chmod 0640 "$source_dir/root.txt" "$source_dir/nested dir/binary.dat"
chmod 0640 "$source_dir/empty"
chown root:"$BACKUP_USER" "$source_dir/root.txt" "$source_dir/nested dir/binary.dat" "$source_dir/empty"

manifest() {
  manifest_root=$1
  (
    cd "$manifest_root"
    find . -mindepth 1 -print | LC_ALL=C sort | while IFS= read -r item; do
      item_type=$(stat -c '%F' "$item")
      metadata=$(stat -c '%a\t%u\t%g' "$item")
      case "$item_type" in
        'regular file'|'regular empty file') payload=$(sha256sum "$item" | awk '{print $1}') ;;
        'symbolic link') payload=$(readlink "$item") ;;
        *) payload=- ;;
      esac
      printf '%s\t%s\t%s\t%s\n' "$item" "$item_type" "$metadata" "$payload"
    done
  )
}

expected_manifest="$evidence_dir/expected.manifest"
archive="$evidence_dir/payload.tar.gz"
manifest "$source_dir" > "$expected_manifest"
chown "$BACKUP_USER:$BACKUP_USER" "$expected_manifest"
chmod 0600 "$expected_manifest"

su -s /bin/sh -c "tar --numeric-owner -C '$source_dir' -czf '$archive' ." "$BACKUP_USER"
[ -s "$archive" ] || die 'backup archive is empty'
[ "$(stat -c '%U:%G' "$archive")" = "$BACKUP_USER:$BACKUP_USER" ] || die 'archive owner mismatch'

rm -rf "$source_dir"
install -d -m 0750 "$restore_dir"
tar --numeric-owner -C "$restore_dir" -xzf "$archive"
actual_manifest="$work_dir/actual.manifest"
manifest "$restore_dir" > "$actual_manifest"
diff -u "$expected_manifest" "$actual_manifest"

archive_sha256=$(sha256sum "$archive" | awk '{print $1}')
cat > "$evidence_dir/RESULT.txt" <<EOF
result=PASS
run_id=$RUN_ID
archive_sha256=$archive_sha256
manifest_sha256=$(sha256sum "$expected_manifest" | awk '{print $1}')
EOF
chown "$BACKUP_USER:$BACKUP_USER" "$evidence_dir/RESULT.txt"
chmod 0600 "$evidence_dir/RESULT.txt"

printf 'ROUNDTRIP_OK archive=%s result=%s\n' "$archive" "$evidence_dir/RESULT.txt"
