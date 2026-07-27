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
#
# Why loop devices cannot go through pveceph
# ------------------------------------------
# Resolving the name is necessary but not sufficient. PVE::Diskmanage::get_disks
# enumerates /sys/block through a hard WHITELIST (Diskmanage.pm):
#
#   return if $dev !~ m/^(h|s|x?v)d[a-z]+$/
#          && $dev !~ m/^nvme\d+n\d+$/
#          && $dev !~ m/^cciss\!c\d+d\d+$/;
#
# `loopN` matches none of them, so get_disks(["loop9"]) comes back EMPTY and
# `pveceph osd create` dies in its own pre-flight check:
#
#   PVE/API2/Ceph/OSD.pm:382
#     die "unable to get device info for '$dev'\n" if !$disklist->{$devname};
#
# There is no flag to relax this — Proxmox only ever wants to build OSDs on real
# disks, which is entirely reasonable on bare metal and useless to us here.
#
# So for loop devices we call ceph-volume directly. That is not a hack around
# Ceph, it is precisely what pveceph shells out to once its checks pass; we are
# skipping the Proxmox-side disk validation, not the Ceph-side work. The result
# is an ordinary OSD: it registers with the mons, appears in `ceph osd tree`,
# and shows up in the PVE web UI under Ceph → OSD (that panel reads the OSD map
# from Ceph, not from Diskmanage). Only the UI's *create OSD* wizard stays blind
# to loop devices, which is why this script exists.
#
# ceph-volume has its own loop-device guard, off by default and released by
# CEPH_VOLUME_ALLOW_LOOP_DEVICES (ceph_volume/__init__.py). It is set only for
# the loop path, so a mis-resolved real disk cannot silently take it.
##############################################################################

set -uo pipefail

log()  { echo "[ceph-osd] $*"; }
warn() { echo "[ceph-osd] WARNING: $*" >&2; }
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

##############################################################################
# Pass 1 — resolve every device, then reconcile this node's view of LVM.
#
# Containers get a private /dev, but NOT a private block layer: the loop devices
# belong to the host kernel and every node can see all of them. LVM therefore
# scans the whole set, and each node discovers the OSD volume groups created by
# all the others. The device-mapper NODES for those, though, were created in the
# originating container's /dev, so they are missing here — and ceph-volume dies
# during its scan, before it ever looks at the device it was asked about:
#
#   RuntimeError: /dev/mapper/ceph--<other node's vg>-osd--block--<uuid> not found.
#
# One node creating its OSDs is enough to break every node after it, which is
# why this presents as "the first node works and the other ten all fail".
#
# Two distinct things are needed, and it is worth being precise about which does
# what, because the obvious one is NOT the one that fixes it:
#
#   1. `dmsetup mknodes` — creates /dev/mapper entries for every device-mapper
#      device the KERNEL currently has, including those set up by other nodes.
#      This is what makes ceph-volume's scan succeed. Device-mapper devices are
#      global to the host; only the /dev nodes were container-local.
#
#   2. An LVM filter restricted to this node's own devices. This does NOT stop
#      the ceph-volume failure above — ceph-volume drives LVM through its own
#      command wrappers and does its scan regardless. What the filter does buy
#      is a node-local answer from plain `pvs`/`vgs`, which the idempotency
#      check below depends on: without it, every node sees ceph VGs belonging to
#      other nodes and can draw the wrong conclusion about its own devices.
##############################################################################

REAL_DEVS=()
for mapped in "${DEVICES[@]}"; do
    if [ ! -b "${mapped}" ]; then
        warn "${mapped} is not a block device — skipping."
        continue
    fi

    real=$(resolve_device "${mapped}") \
        || fail "cannot resolve ${mapped} to a kernel device name via /sys/dev/block."

    log "${mapped} -> ${real}"

    # Refuse to touch anything mounted or carrying a filesystem. OSD creation
    # destroys the device without prompting, so this guard is the last defence
    # against a mis-set device mapping. Fatal, not skipped: a mounted device
    # means the mapping is wrong, and the next one is likely wrong too.
    if findmnt -S "${real}" >/dev/null 2>&1; then
        fail "${real} is MOUNTED. Refusing to use it as an OSD."
    fi
    if lsblk -no MOUNTPOINT "${real}" 2>/dev/null | grep -q .; then
        fail "${real} has mounted partitions. Refusing to use it as an OSD."
    fi

    REAL_DEVS+=("${real}")
done

[ ${#REAL_DEVS[@]} -gt 0 ] || fail "none of the mapped devices resolved to a block device."

# Rewrite lvmlocal.conf with this node's filter. The activation settings are
# repeated verbatim from the Dockerfile: this file replaces that one, and
# dropping them would reinstate the udev wait that breaks LV creation.
{
    echo '# multiprox: generated by ceph-osd-create — node-local LVM view.'
    echo 'activation {'
    echo '    udev_sync = 0'
    echo '    udev_rules = 0'
    echo '    verify_udev_operations = 0'
    echo '}'
    echo 'devices {'
    echo '    obtain_device_list_from_udev = 0'
    printf '    global_filter = [ '
    for d in "${REAL_DEVS[@]}"; do printf '"a|^%s$|", ' "${d}"; done
    echo '"r|.*|" ]'
    printf '    filter = [ '
    for d in "${REAL_DEVS[@]}"; do printf '"a|^%s$|", ' "${d}"; done
    echo '"r|.*|" ]'
    echo '}'
} > /etc/lvm/lvmlocal.conf

log "LVM fenced to this node's devices: ${REAL_DEVS[*]}"

# Materialise /dev/mapper nodes for every dm device the kernel has, so
# ceph-volume's scan does not trip over another node's OSD volumes.
dmsetup mknodes >/dev/null 2>&1 || true

# Drop any cached scan of the devices we just excluded, or LVM keeps answering
# from the stale view for the rest of this run.
rm -rf /run/lvm/cache /etc/lvm/cache 2>/dev/null || true
pvscan --cache >/dev/null 2>&1 || true

# ceph-volume authenticates as client.bootstrap-osd. pveceph would mint this on
# demand; calling ceph-volume directly, we have to stage it ourselves. The
# cluster-wide copy lives in pmxcfs (replicated to every node by pve-cluster),
# so this normally just copies the file that is already there.
BOOTSTRAP_KEYRING=/var/lib/ceph/bootstrap-osd/ceph.keyring
if [ ! -s "${BOOTSTRAP_KEYRING}" ]; then
    mkdir -p "$(dirname "${BOOTSTRAP_KEYRING}")"
    if [ -s /etc/pve/priv/ceph.client.bootstrap-osd.keyring ]; then
        cp /etc/pve/priv/ceph.client.bootstrap-osd.keyring "${BOOTSTRAP_KEYRING}"
    else
        # First node to get here: mint it and publish to pmxcfs for the rest.
        ceph auth get client.bootstrap-osd -o "${BOOTSTRAP_KEYRING}" >/dev/null 2>&1 \
            || fail "cannot obtain the client.bootstrap-osd keyring from the monitors."
        cp "${BOOTSTRAP_KEYRING}" /etc/pve/priv/ceph.client.bootstrap-osd.keyring 2>/dev/null || true
    fi
    chown -R ceph:ceph "$(dirname "${BOOTSTRAP_KEYRING}")" 2>/dev/null || true
fi

created=0
skipped=0
failed=0

##############################################################################
# Pass 2 — create the OSDs.
##############################################################################

for real in "${REAL_DEVS[@]}"; do
    # Idempotency: ceph-volume leaves an LVM signature on a device it owns.
    if pvs --noheadings -o vg_name "${real}" 2>/dev/null | grep -q 'ceph'; then
        log "${real} already hosts an OSD — skipping."
        skipped=$((skipped + 1))
        continue
    fi

    log "Creating OSD on ${real}..."

    # Loop devices are invisible to PVE::Diskmanage (see header), so they go
    # straight to ceph-volume; real disks keep the pveceph path.
    if [[ "${real}" =~ ^/dev/loop[0-9]+$ ]]; then
        rc=0
        CEPH_VOLUME_ALLOW_LOOP_DEVICES=1 ceph-volume lvm create --data "${real}" || rc=$?
    else
        rc=0
        pveceph osd create "${real}" || rc=$?
    fi

    if [ "${rc}" -eq 0 ]; then
        created=$((created + 1))
        log "OSD created on ${real}."
    else
        # Non-fatal: one bad device must not cost us the other 32. The exit
        # status still reflects it, so callers and CI can act on it.
        warn "OSD creation failed on ${real} (exit ${rc}) — continuing."
        failed=$((failed + 1))
    fi
done

log "done: created=${created} skipped=${skipped} failed=${failed}"
ceph osd tree 2>/dev/null || true

[ "${failed}" -eq 0 ] || exit 1
