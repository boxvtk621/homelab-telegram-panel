#!/bin/sh
# Run as root inside the new Alpine 3.24.1 harness-backup guest only.
set -eu

EXPECTED_HOSTNAME=harness-backup
EXPECTED_SIZE_BYTES=137438953472
EXPECTED_GATEWAY=10.202.2.4
LEGACY_DHCP_GATEWAY=10.202.2.1
MOUNTPOINT=/srv/harness-backups
BACKUP_USER=harness-backup
DATA_DEVICE=
EXPECTED_SERIAL=
NETWORK_INTERFACE=

usage() {
  cat <<'EOF'
Usage: guest-bootstrap.sh --data-device PATH --expected-serial SERIAL --network-interface IFACE

Formats exactly one new, fully zeroed 128 GiB disk after guarded identity checks.
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --data-device)
      [ "$#" -ge 2 ] || die '--data-device requires a value'
      DATA_DEVICE=$2
      shift 2
      ;;
    --expected-serial)
      [ "$#" -ge 2 ] || die '--expected-serial requires a value'
      EXPECTED_SERIAL=$2
      shift 2
      ;;
    --network-interface)
      [ "$#" -ge 2 ] || die '--network-interface requires a value'
      NETWORK_INTERFACE=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die 'must run as root'
[ "$(hostname)" = "$EXPECTED_HOSTNAME" ] || die "hostname must be $EXPECTED_HOSTNAME"
[ -n "$DATA_DEVICE" ] || die '--data-device is required'
[ -n "$EXPECTED_SERIAL" ] || die '--expected-serial is required'
[ "$EXPECTED_SERIAL" = HL248-BACKUP-DATA ] || die 'unexpected serial contract'
[ -n "$NETWORK_INTERFACE" ] || die '--network-interface is required'
[ "$NETWORK_INTERFACE" = eth0 ] || die 'unexpected network interface contract'
[ -f /etc/alpine-release ] || die 'not an Alpine guest'
[ "$(cat /etc/alpine-release)" = 3.24.1 ] || die 'expected Alpine 3.24.1 exactly'

grep -Eq "^[[:space:]]*iface[[:space:]]+$NETWORK_INTERFACE[[:space:]]+inet[[:space:]]+dhcp([[:space:]]|$)" /etc/network/interfaces || \
  die "$NETWORK_INTERFACE is not configured for DHCP"
ip link show dev "$NETWORK_INTERFACE" >/dev/null 2>&1 || die "missing interface $NETWORK_INTERFACE"
ip -4 -o addr show dev "$NETWORK_INTERFACE" scope global | grep -q . || die "$NETWORK_INTERFACE has no DHCP address"
command -v dhcpcd >/dev/null 2>&1 || die 'dhcpcd is missing from the cloud image'
[ "$(dhcpcd --version | sed -n '1p')" = 'dhcpcd 10.3.2' ] || die 'expected dhcpcd 10.3.2 exactly'
[ -f /etc/dhcpcd.conf ] || die '/etc/dhcpcd.conf is missing'
if [ -e /etc/udhcpc/udhcpc.conf ]; then
  grep -Fq '# HL-248: keep the DHCP address' /etc/udhcpc/udhcpc.conf || die 'unexpected udhcpc.conf exists'
  rm -f /etc/udhcpc/udhcpc.conf
fi
network_config=$(cat <<EOF
# BEGIN HL-248 HARNESS BACKUP
interface $NETWORK_INTERFACE
noipv6
nooption routers
nooption static_routes
nooption classless_static_routes
nooption ms_classless_static_routes
nooption domain_name_servers
static routers=$EXPECTED_GATEWAY
static domain_name_servers=$EXPECTED_GATEWAY
# END HL-248 HARNESS BACKUP
EOF
)
if grep -Fq '# BEGIN HL-248 HARNESS BACKUP' /etc/dhcpcd.conf; then
  current_config=$(sed -n '/^# BEGIN HL-248 HARNESS BACKUP$/,/^# END HL-248 HARNESS BACKUP$/p' /etc/dhcpcd.conf)
  [ "$current_config" = "$network_config" ] || die 'existing HL-248 dhcpcd block differs'
else
  printf '\n%s\n' "$network_config" >> /etc/dhcpcd.conf
fi
mkdir -p /etc/sysctl.d
cat > /etc/sysctl.d/90-harness-backup-egress.conf <<EOF
net.ipv4.conf.all.accept_redirects=0
net.ipv4.conf.default.accept_redirects=0
net.ipv4.conf.$NETWORK_INTERFACE.accept_redirects=0
net.ipv4.conf.all.secure_redirects=0
net.ipv4.conf.default.secure_redirects=0
net.ipv4.conf.$NETWORK_INTERFACE.secure_redirects=0
net.ipv6.conf.all.disable_ipv6=1
net.ipv6.conf.default.disable_ipv6=1
net.ipv6.conf.$NETWORK_INTERFACE.disable_ipv6=1
EOF
sysctl -p /etc/sysctl.d/90-harness-backup-egress.conf >/dev/null
printf 'nameserver %s\n' "$EXPECTED_GATEWAY" > /etc/resolv.conf
dhcpcd -n "$NETWORK_INTERFACE"
route_attempt=0
# Static routers supplied by dhcpcd use its interface metric but no "proto dhcp"
# tag. The earlier temporary route had metric 0, so it cannot satisfy this guard.
until ip -4 route show default | grep -E "^default via $EXPECTED_GATEWAY dev $NETWORK_INTERFACE([[:space:]]|$)" | grep -Eq ' metric 1002([[:space:]]|$)'; do
  route_attempt=$((route_attempt + 1))
  [ "$route_attempt" -lt 30 ] || die 'dhcpcd did not apply the expected gateway'
  sleep 1
done
# dhcpcd removes the old gateway itself. Never delete "metric 0": Linux can
# treat zero as an unspecified metric and delete the newly installed route.
default_routes=$(ip -4 route show default)
[ "$(printf '%s\n' "$default_routes" | sed '/^$/d' | wc -l | tr -d ' ')" -eq 1 ] || die 'expected exactly one IPv4 default route after dhcpcd rebind'
printf '%s\n' "$default_routes" | grep -Eq "^default via $EXPECTED_GATEWAY dev $NETWORK_INTERFACE([[:space:]]|$)" || die 'active default route mismatch after dhcpcd rebind'
[ -z "$(ip -6 route show default)" ] || die 'unexpected IPv6 default route after IPv6 disable'

apk update
apk add coreutils diffutils e2fsprogs findutils openssh qemu-guest-agent rsync tar util-linux
rc-update add qemu-guest-agent default
rc-service qemu-guest-agent status >/dev/null 2>&1 || rc-service qemu-guest-agent start

DATA_DEVICE=$(readlink -f "$DATA_DEVICE")
[ -b "$DATA_DEVICE" ] || die "$DATA_DEVICE is not a block device"
[ "$(lsblk -dn -o TYPE "$DATA_DEVICE" | tr -d '[:space:]')" = disk ] || die 'data device is not a whole disk'

actual_serial=$(lsblk -dn -o SERIAL "$DATA_DEVICE" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
if [ -z "$actual_serial" ]; then
  vpd_page80="/sys/class/block/$(basename "$DATA_DEVICE")/device/vpd_pg80"
  [ -r "$vpd_page80" ] || die 'serial is absent from lsblk and SCSI VPD page 0x80 is unavailable'
  vpd_page80_hex=$(od -An -tx1 -v "$vpd_page80" | tr -d '[:space:]')
  [ "$vpd_page80_hex" = 00800011484c3234382d4241434b55502d44415441 ] || \
    die 'SCSI VPD page 0x80 does not exactly contain the expected 17-byte serial'
  actual_serial=HL248-BACKUP-DATA
fi
[ "$actual_serial" = "$EXPECTED_SERIAL" ] || die "serial mismatch: got '$actual_serial'"
actual_size=$(blockdev --getsize64 "$DATA_DEVICE")
[ "$actual_size" = "$EXPECTED_SIZE_BYTES" ] || die "size mismatch: got $actual_size bytes"

root_source=$(findmnt -nro SOURCE /)
root_parent=$(lsblk -nrpo PKNAME "$root_source" 2>/dev/null | head -n 1 || true)
[ "$root_source" != "$DATA_DEVICE" ] || die 'refusing to format the root device'
[ "$root_parent" != "$DATA_DEVICE" ] || die 'refusing to format the root parent device'

device_tree=$(lsblk -nrpo NAME "$DATA_DEVICE")
[ "$(printf '%s\n' "$device_tree" | sed '/^$/d' | wc -l | tr -d ' ')" -eq 1 ] || die 'data device has child partitions or mappings'
[ "$device_tree" = "$DATA_DEVICE" ] || die 'lsblk identity mismatch'
[ -z "$(lsblk -dn -o FSTYPE "$DATA_DEVICE" | tr -d '[:space:]')" ] || die 'data device already has a filesystem'
[ -z "$(findmnt -rn -S "$DATA_DEVICE")" ] || die 'data device is mounted'
[ -z "$(lsblk -dn -o MOUNTPOINTS "$DATA_DEVICE" | tr -d '[:space:]')" ] || die 'data device has a mountpoint'
[ ! -d "/sys/class/block/$(basename "$DATA_DEVICE")/holders" ] || \
  [ -z "$(ls -A "/sys/class/block/$(basename "$DATA_DEVICE")/holders")" ] || die 'data device has holders'

if blkid -p "$DATA_DEVICE" >/dev/null 2>&1; then
  die 'blkid found an existing signature'
fi
[ -z "$(wipefs -n "$DATA_DEVICE")" ] || die 'wipefs found an existing signature'

printf 'Scanning all %s bytes for non-zero data before format...\n' "$actual_size"
cmp -n "$actual_size" "$DATA_DEVICE" /dev/zero || die 'data device contains non-zero data'

# This is the only formatting operation. Every identity and emptiness guard above
# must pass in this same process before mkfs can be reached.
mkfs.ext4 -F -L harness-backups -m 0 "$DATA_DEVICE"
filesystem_uuid=$(blkid -s UUID -o value "$DATA_DEVICE")
[ -n "$filesystem_uuid" ] || die 'new filesystem UUID is empty'

[ ! -e "$MOUNTPOINT" ] || [ -d "$MOUNTPOINT" ] || die "$MOUNTPOINT is not a directory"
install -d -m 0755 -o root -g root "$MOUNTPOINT"
! grep -Eq "[[:space:]]$MOUNTPOINT[[:space:]]" /etc/fstab || die 'mountpoint already exists in fstab'
! grep -Fq "UUID=$filesystem_uuid" /etc/fstab || die 'filesystem UUID already exists in fstab'
printf 'UUID=%s %s ext4 rw,nodev,nosuid,noexec 0 2\n' "$filesystem_uuid" "$MOUNTPOINT" >> /etc/fstab
mount "$MOUNTPOINT"

if id "$BACKUP_USER" >/dev/null 2>&1; then
  die "$BACKUP_USER already exists; refusing an ambiguous bootstrap"
fi
addgroup -S "$BACKUP_USER"
adduser -S -D -H -h /incoming -s /sbin/nologin -G "$BACKUP_USER" "$BACKUP_USER"
# OpenSSH without PAM rejects shadow values beginning with ! or * before it
# evaluates public-key authentication. NP is an invalid password hash, but it is
# not an account lock marker; PasswordAuthentication remains disabled in sshd.
sed -i "s|^$BACKUP_USER:[^:]*:|$BACKUP_USER:NP:|" /etc/shadow
grep -Eq "^$BACKUP_USER:NP:" /etc/shadow || die 'failed to set disabled-password hash'
install -d -m 0700 -o "$BACKUP_USER" -g "$BACKUP_USER" "$MOUNTPOINT/incoming"

install -d -m 0755 -o root -g root /etc/ssh/authorized_keys /etc/ssh/sshd_config.d
[ ! -e "/etc/ssh/authorized_keys/$BACKUP_USER" ] || die 'backup account key already exists; refusing ambiguous bootstrap'
grep -Eq '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' /etc/ssh/sshd_config || \
  die 'sshd_config does not include /etc/ssh/sshd_config.d/*.conf'
cat > /etc/ssh/sshd_config.d/90-harness-backup.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin no

Match User harness-backup
    AuthorizedKeysFile /etc/ssh/authorized_keys/%u
    AuthenticationMethods publickey
    ChrootDirectory /srv/harness-backups
    ForceCommand internal-sftp
    AllowAgentForwarding no
    AllowTcpForwarding no
    PermitTunnel no
    PermitTTY no
    X11Forwarding no
EOF
ssh-keygen -A
sshd -t

rc-update add sshd default
rc-service sshd restart
rc-service qemu-guest-agent status >/dev/null

install -d -m 0700 /var/lib/harness-backup
boot_id=$(cat /proc/sys/kernel/random/boot_id)
cat > /var/lib/harness-backup/bootstrap.env <<EOF
BOOTSTRAP_BOOT_ID=$boot_id
DATA_DEVICE=$DATA_DEVICE
DATA_SERIAL=$EXPECTED_SERIAL
DATA_UUID=$filesystem_uuid
NETWORK_INTERFACE=$NETWORK_INTERFACE
GATEWAY=$EXPECTED_GATEWAY
EOF
chmod 0600 /var/lib/harness-backup/bootstrap.env

printf 'Bootstrap complete. Reboot, then run validate-guest.sh --require-reboot.\n'
