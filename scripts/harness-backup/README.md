# HL-248 harness backup VM guest artifacts

The guest scripts configure and validate the dedicated Alpine backup VM.
`provision_vm.py` creates the exact new VM/storage from the reviewed contract;
its default mode only checks and prints the plan. It refuses an occupied VMID,
orphan volumes, an existing storage path, or a different physical mount UUID.
`build_cloud_config.py` renders the guest scripts without credentials; the
provisioner injects only the fingerprint-checked owner public key.
No backup scheduler, retention policy, or permanent incoming node credential
is installed by this package.

`provisioning-contract.json` freezes the reviewed VM/storage inputs consumed by
the separate Proxmox provisioner. The guest uses DHCP for its address. The
Alpine cloud image's actual client is `dhcpcd 10.3.2`; its per-interface block
keeps the DHCP address while overriding the router and DNS with `10.202.2.4`,
discarding inherited router/static-route options, and disabling IPv6. The
bootstrap reloads and rebinds dhcpcd without taking the interface down. The
client replaces its old default via `10.202.2.1`; no routes are deleted manually.
IPv4 redirects are disabled so neither router can
silently change the selected egress. No OpenWrt mutation is required.

The owner account uses `doas -n`; `sudo` is absent. `harness-backup.lan`
currently returns NXDOMAIN, and no static lease is claimed. Initial acceptance
therefore uses the current DHCP address `10.202.2.3` together with a pinned SSH
host key. The address itself is not a durable identity.

The accepted ED25519 host fingerprint, obtained through PVE QGA before SSH, is
`SHA256:CFi9w+jAmCv/7k4h6ywN8H0oOdviq3UtYBD4V8F5V14`.
PVE VM116 is the inventory identity used to refresh the DHCP endpoint.

## Bootstrap

Cloud-init must copy `guest-bootstrap.sh` into the new guest and run it after
networking is available. The provisioner must pass an explicit block-device
path as well as the independent QEMU serial marker. For the current layout the
expected command is:

```sh
/root/guest-bootstrap.sh \
  --data-device /dev/sdb \
  --expected-serial HL248-BACKUP-DATA \
  --network-interface eth0
```

The script installs these Alpine packages itself, so cloud-init does not need a
second package phase:

```text
coreutils diffutils e2fsprogs findutils openssh qemu-guest-agent rsync tar util-linux
```

Before formatting, the script proves that the argument is a 128 GiB whole disk,
that its serial is exactly `HL248-BACKUP-DATA`, that it is not the root disk,
and that it has no children, mounts, holders, partition table, filesystem
signature, or non-zero byte. The full-device `cmp` is deliberately slower than
a signature-only check. Any failed guard stops before `mkfs.ext4`.
Alpine mdev leaves `lsblk SERIAL` empty on this image; the fallback validates the
entire kernel SCSI VPD page 0x80, including its header and exact serial bytes.

The resulting filesystem is mounted by UUID at `/srv/harness-backups`. The
`harness-backup` account is restricted to internal SFTP in that chroot and can
write only `/incoming`. Its shadow value is the non-login hash `NP`, because an
OpenSSH build without PAM rejects `!`/`*` account locks before checking a public
key. Password authentication is disabled. The global `AuthorizedKeysFile`
default remains intact for the cloud-init owner account; only the backup user's
Match block uses `/etc/ssh/authorized_keys/harness-backup`. That file is
deliberately not created; the per-node credential is a later operational input
and must be owned by `harness-backup` with mode `0600`.

Bootstrap records its current boot ID. Reboot the guest, then validate:

```sh
/root/validate-guest.sh \
  --data-device /dev/sdb \
  --expected-serial HL248-BACKUP-DATA \
  --network-interface eth0 \
  --require-reboot \
  --expect-no-backup-key
```

This checks the persisted dhcpcd override, single active route/DNS, disabled
IPv6, filesystem identity,
OpenRC services, QEMU guest agent, effective sshd restrictions, and a bounded
write/delete probe in `/incoming`.

## Synthetic restore acceptance

After the post-reboot validation passes, run:

```sh
/root/synthetic-roundtrip.sh
```

The test creates a small source tree outside the backup filesystem, writes a
tar archive to the target as `harness-backup`, removes only that temporary
source, restores the archive into another temporary directory, and compares a
manifest containing path type, mode, owner/group, symlink target, and file
SHA-256. The archive and result manifest remain under
`/srv/harness-backups/incoming/.synthetic-test/<UTC-run-id>/` as concrete
acceptance evidence. This validates the storage target and a real restore from
it; it makes no claim about the future Harness scheduler or retention policy.

## Accepted result

`evidence/vm116-acceptance-01.json` records the real reboot, preserved mount,
gateway/DNS, SSH/doas, QGA, local archive restore and cross-host SFTP restore.
The temporary SFTP key was revoked and its denial retested; its local private
key was removed. Synthetic archive evidence remains in the backup destination.

Initial cloud-init stopped before formatting on the serial/client differences.
The corrected bootstrap was run explicitly, and its exact source hashes match
the guest. The final cloud-init snippet was synchronized for reproducibility;
the first failure report remains as provenance. The large disk is local to PVE,
so this target does not protect against loss of the entire PVE host.
