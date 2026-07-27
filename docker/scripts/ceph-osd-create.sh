#!/usr/bin/env bash
##############################################################################
# ceph-osd-create.sh — create a Ceph OSD from a device mapped into this
# container, working around the fact that Proxmox resolves disks through sysfs.
#
#   ceph-osd-create [/dev/ceph-osd-0 ...]
#
# Why this exists
# ---------------
# `docker --device /dev/disk/by-id/scsi-XXX:/dev/ceph-osd-0` creates a device
# NODE at the container path, but sysfs is the host's, so it still only knows
# the disk by its kernel name:
#
#   $ ls -l /dev/ceph-osd-0        -> brw-rw---- 8, 0
#   $ ls /sys/block/               -> sda sdb sdc sdd     (no ceph-osd-0)
#
# PVE::Diskmanage looks up /sys/block/<name>, so it rejects the mapped path:
#
#   unable to get device info for '/dev/ceph-osd-0'
#
# The device's major:minor is authoritative, and /sys/dev/block/<maj>:<min>
# maps it back to the kernel name. We recreate the node under that name and
# hand THAT to pveceph.
#
# This keeps the host side selecting disks by /dev/disk/by-id/... — which is
# what you must do, because kernel names shift across reboots — while giving
# Proxmox the sysfs-resolvable name it insists on.
##############################################################################

set -uo pipefail

log()  { echo "[ceph-osd] $*"; }
fail() { echo "[ceph-osd] ERROR: $*" >&2; exit 1; }

# Translate a mapped device node to its kernel name, creating the node if the
# name is not already present in /dev. Prints the resolved path.
resolve_device() {
    local mapped="$1" maj min devname

    [ -b "${mapped}" ] || return 1

    # stat prints major/minor in hex.
    maj=$(( 0x$(stat -c '%t' "${mapped}") ))
    min=$(( 0x$(stat -c '%T' "${mapped}") ))

    devname=$(sed -n 's/^DEVNAME=//p' "/sys/dev/block/${maj}:${min}/uevent" 2>/dev/null)
    [ -n "${devname}" ] || return 1

    if [ ! -b "/dev/${devname}" ]; then
        mknod "/dev/${devname}" b "${maj}" "${min}" 2>/dev/null || return 1
        chgrp disk "/dev/${devname}" 2>/dev/null || true
    fi

    echo "/dev/${devname}"
}

# Default to the devices the compose overlay maps in.
DEVICES=("$@")
if [ ${#DEVICES[@]} -eq 0 ]; then
    shopt -s nullglob
    DEVICES=(/dev/ceph-osd-*)
    shopt -u nullglob
fi

[ ${#DEVICES[@]} -gt 0 ] || fail "no OSD devices given and no /dev/ceph-osd-* present.
       Start the cluster with docker-compose.ceph.yml and set PVE*_OSD_DEVICE."

[ -f /etc/pve/ceph.conf ] || fail "/etc/pve/ceph.conf missing — run 'pveceph init' first."

created=0
skipped=0

for mapped in "${DEVICES[@]}"; do
    if [ ! -b "${mapped}" ]; then
        log "WARNING: ${mapped} is not a block device — skipping."
        continue
    fi

    real=$(resolve_device "${mapped}") \
        || fail "cannot resolve ${mapped} to a kernel device name via /sys/dev/block."

    log "${mapped} -> ${real}"

    # Refuse to touch anything mounted or carrying a filesystem. pveceph will
    # destroy the device without prompting, so this guard is the last defence
    # against a mis-set PVE*_OSD_DEVICE.
    if findmnt -S "${real}" >/dev/null 2>&1; then
        fail "${real} is MOUNTED. Refusing to use it as an OSD."
    fi
    if lsblk -no MOUNTPOINT "${real}" 2>/dev/null | grep -q .; then
        fail "${real} has mounted partitions. Refusing to use it as an OSD."
    fi

    # Idempotency: ceph-volume leaves an LVM signature on a device it owns.
    if pvs --noheadings -o vg_name "${real}" 2>/dev/null | grep -q 'ceph'; then
        log "${real} already hosts an OSD — skipping."
        skipped=$((skipped + 1))
        continue
    fi

    log "Creating OSD on ${real}..."
    if pveceph osd create "${real}"; then
        created=$((created + 1))
        log "OSD created on ${real}."
    else
        fail "pveceph osd create ${real} failed."
    fi
done

log "done: created=${created} skipped=${skipped}"
ceph osd tree 2>/dev/null || true
