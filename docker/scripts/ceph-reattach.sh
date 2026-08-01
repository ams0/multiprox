#!/usr/bin/env bash
##############################################################################
# ceph-reattach.sh — bring this node's existing Ceph daemons back after the
# container is recreated. Run from multiprox-ceph-reattach.service at boot.
#
# The problem
# -----------
# Ceph DATA survives a container being recreated, because /var/lib/ceph is a
# named volume. What does not survive is everything systemd knows about it:
# `pveceph mon create` and `ceph-volume lvm create` enable units
# (ceph-mon@<node>, ceph-volume@lvm-<id>-<uuid>, ceph-osd@<id>) by writing
# symlinks under /etc/systemd/system, and that is the container filesystem.
#
# So a recreated node comes back with all of its monitor and OSD data intact and
# not one daemon running. The cluster is quorate — Corosync and pmxcfs are
# fine — while Ceph is simply absent:
#
#   Error initializing cluster client: ObjectNotFound('RADOS object not found
#   (error calling conf_read_file)')
#
# which reads like a broken config rather than "nothing was started".
#
# `docker compose up -d` recreates containers whenever the image or the service
# definition changes, so this is not an edge case; it happens on any rebuild.
#
# What this does
# --------------
# Re-derives the daemons from the data that persisted and starts them. It is
# idempotent and safe on a node that has no Ceph at all — a plain PVE node
# leaves every directory below missing and this exits having done nothing.
##############################################################################

set -uo pipefail

NODE="$(hostname -s)"

log()  { echo "[ceph-reattach] $*"; }
warn() { echo "[ceph-reattach] WARNING: $*" >&2; }

# Nothing to do on a node that was never given Ceph.
[ -d /var/lib/ceph ] || exit 0

##############################################################################
# The cluster config has to be reachable at /etc/ceph/ceph.conf first: every
# ceph command reads it, and on Proxmox it is a symlink into pmxcfs that only
# exists on nodes where pveceph has run.
#
# pmxcfs is mounted by pve-cluster, which systemd may still be starting, so wait
# rather than assume. Without the config the daemons below start and then fail
# to find the monitors.
##############################################################################
for _ in $(seq 1 60); do
    [ -f /etc/pve/ceph.conf ] && break
    sleep 2
done

if [ ! -f /etc/pve/ceph.conf ]; then
    log "no /etc/pve/ceph.conf — this node is not part of a Ceph cluster yet."
    exit 0
fi

mkdir -p /etc/ceph
[ -e /etc/ceph/ceph.conf ] || ln -sf /etc/pve/ceph.conf /etc/ceph/ceph.conf
[ -e /etc/ceph/ceph.client.admin.keyring ] \
    || ln -sf /etc/pve/priv/ceph.client.admin.keyring /etc/ceph/ceph.client.admin.keyring

##############################################################################
# Monitor, manager and metadata server.
#
# Each keeps its state in /var/lib/ceph/<type>/ceph-<name>, so the presence of
# that directory is what says "this node ran one of these". Enable and start the
# matching unit for each one found.
##############################################################################
start_daemon() {
    local kind="$1" name="$2" unit="ceph-${1}@${2}"

    if systemctl is-active --quiet "${unit}"; then
        log "${unit} already running."
        return 0
    fi

    systemctl enable "${unit}" >/dev/null 2>&1 || true
    if systemctl start "${unit}" 2>/dev/null; then
        log "started ${unit}"
    else
        warn "could not start ${unit}"
    fi
}

for kind in mon mgr mds; do
    dir="/var/lib/ceph/${kind}"
    [ -d "${dir}" ] || continue
    for path in "${dir}"/ceph-*; do
        [ -d "${path}" ] || continue
        # ceph-<name> -> <name>; the name is the node for mon/mgr/mds here.
        name="$(basename "${path}")"
        start_daemon "${kind}" "${name#ceph-}"
    done
done

##############################################################################
# OSDs.
#
# `ceph-volume lvm activate --all` is the supported way to bring up every OSD
# whose LVM volumes are present: it re-reads the LVM tags, rebuilds
# /var/lib/ceph/osd/ceph-<id>, and enables and starts the units. Doing it by
# hand from the directory names would miss the tags that map an OSD to its
# block device.
#
# The LVM view must already be fenced to this node's own devices — the
# entrypoint runs `ceph-osd-create --fence-only` before systemd for exactly
# that reason — or this scans other nodes' volume groups and fails.
##############################################################################
if [ -d /var/lib/ceph/osd ] || ls /dev/ceph-osd-* >/dev/null 2>&1; then
    # Rebuild the device nodes first. LVM may only have activated these volumes
    # after the entrypoint ran, and ceph-volume activates by the /dev/<vg>/<lv>
    # path recorded in the LVM tags — absent without this, since LVM's udev
    # rules are disabled in the container.
    dmsetup mknodes >/dev/null 2>&1 || true
    vgmknodes >/dev/null 2>&1 || true

    log "activating OSDs..."
    if ceph-volume lvm activate --all 2>&1 | grep -E '^(-->|Running command: /usr/bin/systemctl start)' | tail -5; then
        :
    fi
fi

log "done."
