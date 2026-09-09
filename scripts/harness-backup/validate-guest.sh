#!/bin/sh
# Read-only validation, except for a bounded create/delete probe in incoming/.
set -eu

EXPECTED_HOSTNAME=harness-backup
EXPECTED_SIZE_BYTES=137438953472
EXPECTED_GATEWAY=10.202.2.4
MOUNTPOINT=/srv/harness-backups
BACKUP_USER=harness-backup
DATA_DEVICE=
EXPECTED_SERIAL=
NETWORK_INTERFACE=
REQUIRE_REBOOT=0
EXPECT_NO_BACKUP_KEY=0

usage() {
  cat <<'EOF'
Usage: validate-guest.sh --data-device PATH --expected-serial SERIAL --network-interface IFACE [--require-reboot] [--expect-no-backup-key]
EOF
}

die() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

pass() {
  printf 'PASS: %s\n' "$*"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --data-device) [ "$#" -ge 2 ] || die 'missing device'; DATA_DEVICE=$2; shift 2 ;;
    --expected-serial) [ "$#" -ge 2 ] || die 'missing serial'; EXPECTED_SERIAL=$2; shift 2 ;;
    --network-interface) [ "$#" -ge 2 ] || die 'missing interface'; NETWORK_INTERFACE=$2; shift 2 ;;
    --require-reboot) REQUIRE_REBOOT=1; shift ;;
    --expect-no-backup-key) EXPECT_NO_BACKUP_KEY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die 'must run as root'
[ "$(hostname)" = "$EXPECTED_HOSTNAME" ] || die 'unexpected hostname'
[ -n "$DATA_DEVICE" ] || die '--data-device is required'
[ "$EXPECTED_SERIAL" = HL248-BACKUP-DATA ] || die 'unexpected serial contract'
[ "$NETWORK_INTERFACE" = eth0 ] || die 'unexpected interface contract'
[ -s /var/lib/harness-backup/bootstrap.env ] || die 'bootstrap marker is missing'

DATA_DEVICE=$(readlink -f "$DATA_DEVICE")
[ -b "$DATA_DEVICE" ] || die 'data device is not a block device'
actual_serial=$(lsblk -dn -o SERIAL "$DATA_DEVICE" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
if [ -z "$actual_serial" ]; then
  vpd_page80="/sys/class/block/$(basename "$DATA_DEVICE")/device/vpd_pg80"
  [ -r "$vpd_page80" ] || die 'serial is absent from lsblk and SCSI VPD page 0x80 is unavailable'
  vpd_page80_hex=$(od -An -tx1 -v "$vpd_page80" | tr -d '[:space:]')
  [ "$vpd_page80_hex" = 00800011484c3234382d4241434b55502d44415441 ] || \
    die 'SCSI VPD page 0x80 does not exactly contain the expected 17-byte serial'
  actual_serial=HL248-BACKUP-DATA
fi
[ "$actual_serial" = "$EXPECTED_SERIAL" ] || die 'data serial mismatch'
[ "$(blockdev --getsize64 "$DATA_DEVICE")" = "$EXPECTED_SIZE_BYTES" ] || die 'data size mismatch'
[ "$(lsblk -dn -o FSTYPE "$DATA_DEVICE" | tr -d '[:space:]')" = ext4 ] || die 'data filesystem is not ext4'

filesystem_uuid=$(blkid -s UUID -o value "$DATA_DEVICE")
[ -n "$filesystem_uuid" ] || die 'data filesystem has no UUID'
[ "$(findmnt -nro SOURCE --target "$MOUNTPOINT")" = "$DATA_DEVICE" ] || die 'unexpected mount source'
[ "$(findmnt -nro FSTYPE --target "$MOUNTPOINT")" = ext4 ] || die 'unexpected mount filesystem'
mount_options=$(findmnt -nro OPTIONS --target "$MOUNTPOINT")
for required_option in rw nodev nosuid noexec; do
  printf '%s\n' "$mount_options" | tr ',' '\n' | grep -Fxq "$required_option" || die "mount option $required_option is missing"
done
grep -Eq "^UUID=$filesystem_uuid[[:space:]]+$MOUNTPOINT[[:space:]]+ext4[[:space:]]+rw,nodev,nosuid,noexec[[:space:]]+0[[:space:]]+2$" /etc/fstab || die 'fstab entry mismatch'
pass 'data disk identity and persistent mount'

[ "$(stat -c '%U:%G:%a' "$MOUNTPOINT")" = root:root:755 ] || die 'chroot root ownership/mode mismatch'
[ "$(stat -c '%U:%G:%a' "$MOUNTPOINT/incoming")" = "$BACKUP_USER:$BACKUP_USER:700" ] || die 'incoming ownership/mode mismatch'
grep -Eq "^$BACKUP_USER:NP:" /etc/shadow || die 'backup account disabled-password hash mismatch'
sshd -t
admin_sshd=$(sshd -T -C 'user=alpine,host=localhost,addr=127.0.0.1')
printf '%s\n' "$admin_sshd" | grep -Fxq 'authorizedkeysfile .ssh/authorized_keys' || die 'admin AuthorizedKeysFile default was changed'
effective_sshd=$(sshd -T -C "user=$BACKUP_USER,host=localhost,addr=127.0.0.1")
for expected in \
  'passwordauthentication no' \
  'kbdinteractiveauthentication no' \
  'authorizedkeysfile /etc/ssh/authorized_keys/%u' \
  'authenticationmethods publickey' \
  'chrootdirectory /srv/harness-backups' \
  'forcecommand internal-sftp' \
  'allowagentforwarding no' \
  'allowtcpforwarding no' \
  'permittty no' \
  'x11forwarding no'; do
  printf '%s\n' "$effective_sshd" | grep -Fxq "$expected" || die "effective sshd setting missing: $expected"
done
if [ "$EXPECT_NO_BACKUP_KEY" -eq 1 ]; then
  [ ! -s "/etc/ssh/authorized_keys/$BACKUP_USER" ] || die 'backup account key unexpectedly exists'
fi
probe="$MOUNTPOINT/incoming/.write-probe-$$"
su -s /bin/sh -c "umask 077; printf probe > '$probe'; rm -f '$probe'" "$BACKUP_USER"
[ ! -e "$probe" ] || die 'write probe cleanup failed'
pass 'restricted SFTP account and incoming write boundary'

grep -Eq "^[[:space:]]*iface[[:space:]]+$NETWORK_INTERFACE[[:space:]]+inet[[:space:]]+dhcp([[:space:]]|$)" /etc/network/interfaces || die 'interface is not DHCP'
[ "$(dhcpcd --version | sed -n '1p')" = 'dhcpcd 10.3.2' ] || die 'unexpected DHCP client version'
for expected in \
  "interface $NETWORK_INTERFACE" \
  'noipv6' \
  'nooption routers' \
  'nooption static_routes' \
  'nooption classless_static_routes' \
  'nooption ms_classless_static_routes' \
  'nooption domain_name_servers' \
  "static routers=$EXPECTED_GATEWAY" \
  "static domain_name_servers=$EXPECTED_GATEWAY"; do
  grep -Fxq "$expected" /etc/dhcpcd.conf || die "persistent dhcpcd setting missing: $expected"
done
[ "$(cat /proc/sys/net/ipv6/conf/all/disable_ipv6)" = 1 ] || die 'IPv6 is not disabled globally'
[ "$(cat /proc/sys/net/ipv6/conf/default/disable_ipv6)" = 1 ] || die 'IPv6 is not disabled by default'
[ "$(cat "/proc/sys/net/ipv6/conf/$NETWORK_INTERFACE/disable_ipv6")" = 1 ] || die 'IPv6 is not disabled on the DHCP interface'
for scope in all default "$NETWORK_INTERFACE"; do
  [ "$(cat "/proc/sys/net/ipv4/conf/$scope/accept_redirects")" = 0 ] || die "IPv4 redirects are accepted for $scope"
  [ "$(cat "/proc/sys/net/ipv4/conf/$scope/secure_redirects")" = 0 ] || die "secure IPv4 redirects are accepted for $scope"
done
default_routes=$(ip -4 route show default)
[ "$(printf '%s\n' "$default_routes" | sed '/^$/d' | wc -l | tr -d ' ')" -eq 1 ] || die 'expected exactly one IPv4 default route'
printf '%s\n' "$default_routes" | grep -Eq "^default via $EXPECTED_GATEWAY dev $NETWORK_INTERFACE([[:space:]]|$)" || die 'active default route mismatch'
[ -z "$(ip -6 route show default)" ] || die 'unexpected IPv6 default route'
nameservers=$(awk '$1 == "nameserver" {print $2}' /etc/resolv.conf)
[ "$nameservers" = "$EXPECTED_GATEWAY" ] || die 'active resolver mismatch'
pass 'dhcpcd address with guest-local routes, DNS, and IPv6 disabled'

rc-service sshd status >/dev/null
rc-service qemu-guest-agent status >/dev/null
rc-update show default | grep -Eq '^[[:space:]]*sshd([[:space:]]|$)' || die 'sshd is not in default runlevel'
rc-update show default | grep -Eq '^[[:space:]]*qemu-guest-agent([[:space:]]|$)' || die 'qemu-guest-agent is not in default runlevel'
pass 'OpenRC sshd and QEMU guest agent readiness'

if [ "$REQUIRE_REBOOT" -eq 1 ]; then
  bootstrap_boot_id=$(awk -F= '$1 == "BOOTSTRAP_BOOT_ID" {print $2}' /var/lib/harness-backup/bootstrap.env)
  [ -n "$bootstrap_boot_id" ] || die 'bootstrap boot ID is missing'
  [ "$(cat /proc/sys/kernel/random/boot_id)" != "$bootstrap_boot_id" ] || die 'guest has not rebooted since bootstrap'
  pass 'post-bootstrap reboot'
fi

printf 'VALIDATION_OK\n'
