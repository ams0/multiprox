#!/usr/bin/env bash
##############################################################################
# rbd-unmap-retry.sh — installed over /usr/bin/rbd (via dpkg-divert) to make
# `rbd unmap` retry instead of failing on a device that is still settling.
#
# Everything except unmap is passed straight through to the real binary.
#
# The problem
# -----------
# Creating a container on RBD ends with: unmount the rootfs, then unmap the
# device. On this setup the kernel keeps the rbd device open for a while after
# the unmount returns — measured at ~20 seconds — while ext4 writeback drains to
# the OSDs. The unmap fails during that window:
#
#   rbd: sysfs write failed
#   can't unmap rbd device /dev/rbd-pve/<fsid>/<pool>/vm-102-disk-0
#   unable to create CT 102 - volume deactivation failed
#
# The container is fully built by then — rootfs formatted, template extracted —
# and the whole thing is rolled back over a device that becomes free moments
# later. PVE::Storage::RBDPlugin::unmap_volume issues exactly one unmap and has
# no retry, so there is nothing to tune.
#
# Why it shows up here and not on real hardware
# ---------------------------------------------
# The OSDs are loop devices backed by files on a single disk, and every write is
# replicated three times across them. Flushing a freshly extracted root
# filesystem therefore takes far longer than on the dedicated disks Proxmox
# expects, which stretches the settling window from milliseconds into tens of
# seconds. The race exists on bare metal too; it is simply never observed.
#
# Retrying is safe: a busy device means the kernel has not finished with it, and
# the only correct response is to wait. Genuine failures (a device that does not
# exist, bad arguments) do not become busy-and-then-free, so they still fail —
# just after the timeout rather than immediately.
##############################################################################

set -uo pipefail

RBD_REAL="${RBD_REAL:-/usr/bin/rbd.distrib}"
# ~60s total. Comfortably past the ~20s observed, short enough that a real
# failure still surfaces well inside any sensible task timeout.
RETRIES="${RBD_UNMAP_RETRIES:-30}"
DELAY="${RBD_UNMAP_DELAY:-2}"

[ -x "${RBD_REAL}" ] || {
    echo "rbd shim: ${RBD_REAL} is missing or not executable" >&2
    exit 127
}

# Is this an unmap? It may be spelled `rbd unmap ...` or `rbd device unmap ...`.
#
# Scan every argument for the exact word. Stopping at the first non-option word
# looks tidier but is wrong: PVE puts its options first and several of them take
# a separate value —
#
#   /usr/bin/rbd -p ceph-vm -c /etc/pve/ceph.conf ... unmap /dev/rbd-pve/...
#
# so the first bare word encountered is "ceph-vm", the value of -p, and the scan
# gives up before ever reaching the subcommand. The shim then passed every
# unmap straight through and changed nothing.
is_unmap=0
for arg in "$@"; do
    [ "${arg}" = "unmap" ] && { is_unmap=1; break; }
done

if [ "${is_unmap}" -eq 0 ]; then
    exec "${RBD_REAL}" "$@"
fi

attempt=1
while : ; do
    err=$("${RBD_REAL}" "$@" 2>&1)
    rc=$?
    [ "${rc}" -eq 0 ] && exit 0

    # Only "sysfs write failed" means busy-and-may-clear. Anything else is a
    # real error and is reported straight away rather than retried for a minute.
    case "${err}" in
        *"sysfs write failed"*) ;;
        *) printf '%s\n' "${err}" >&2; exit "${rc}" ;;
    esac

    if [ "${attempt}" -ge "${RETRIES}" ]; then
        printf '%s\n' "${err}" >&2
        echo "rbd shim: device still busy after $(( RETRIES * DELAY ))s, giving up" >&2
        exit "${rc}"
    fi

    [ "${attempt}" -eq 1 ] && echo "rbd shim: device busy, retrying unmap..." >&2
    attempt=$(( attempt + 1 ))
    sleep "${DELAY}"
done
