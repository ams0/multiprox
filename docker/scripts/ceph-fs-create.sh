#!/usr/bin/env bash
##############################################################################
# ceph-fs-create.sh — create a CephFS shared by every node, for ISOs, container
# templates, snippets and backups.
#
#   ceph-fs-create [fs-name]        # default: cephfs
#
# Run on ONE node once the cluster is quorate and the OSDs are up. It creates
# the metadata server here, the filesystem, and registers it as PVE storage —
# which lands in pmxcfs, so every node picks it up with no further action.
#
# Why CephFS and not the RBD pool
# -------------------------------
# The RBD pool holds VM and container disks, and that is all it can hold: RBD is
# block storage, and a block device cannot be safely mounted read-write on
# several nodes at once. Proxmox therefore refuses `iso`, `vztmpl` and `snippets`
# content types on an RBD storage.
#
# Without shared file storage each node keeps its own ISOs under /var/lib/vz,
# which means uploading the same image once per node and losing the ability to
# create a guest from an ISO on a node that does not happen to have it. CephFS
# is a real POSIX filesystem on the same OSDs, mountable everywhere at once, so
# one upload is visible cluster-wide.
#
# Extra MDS daemons
# -----------------
# One MDS is enough to serve the filesystem, but it is a single point of
# failure: if its node goes down the filesystem stalls until it comes back.
# Standbys take over in seconds. Pass MDS_NODES to place them:
#
#   MDS_NODES="pve1 pve2 pve3" ceph-fs-create
#
# The extra ones are created over SSH, which the cluster already relies on for
# pvecm and migration, so no new trust is needed.
##############################################################################

set -uo pipefail

FS_NAME="${1:-cephfs}"
STORAGE_ID="${STORAGE_ID:-${FS_NAME}}"
MDS_NODES="${MDS_NODES:-}"

log()  { echo "[ceph-fs] $*"; }
warn() { echo "[ceph-fs] WARNING: $*" >&2; }
fail() { echo "[ceph-fs] ERROR: $*" >&2; exit 1; }

[ -f /etc/pve/ceph.conf ] || fail "/etc/pve/ceph.conf missing — run 'pveceph init' first."

# A CephFS needs OSDs to place its data and metadata pools on. Creating it
# against an empty cluster leaves the pools stuck in an unhealthy state, so
# check first rather than produce something that looks created and does not work.
osd_up=$(ceph osd stat --format json 2>/dev/null | sed -n 's/.*"num_up_osds":\([0-9]*\).*/\1/p')
[ -n "${osd_up}" ] || fail "cannot reach the Ceph monitors."
[ "${osd_up}" -gt 0 ] || fail "no OSDs are up — create OSDs before the filesystem."
log "${osd_up} OSDs up."

##############################################################################
# Metadata server
##############################################################################
THIS_NODE="$(hostname -s)"

# Remote nodes are driven through the PVE API, not SSH.
#
# `ssh <node> pveceph mds create` is the obvious approach and it fails with
# "Host key verification failed": joining a cluster establishes trust from the
# joining node TO node 1, so the reverse direction has no host key, and short
# names are not in known_hosts even when the addresses are.
#
# pvesh proxies to the target node over the cluster's own authenticated channel,
# which every node already trusts because pmxcfs distributes the certificates.
# Nothing extra to set up, and it works in whichever direction it is called.
create_mds() {
    local node="$1"
    if ceph node ls mds 2>/dev/null | grep -q "\"${node}\""; then
        log "MDS already present on ${node} — skipping."
        return 0
    fi
    if [ "${node}" = "${THIS_NODE}" ]; then
        pveceph mds create >/dev/null 2>&1
    else
        pvesh create "/nodes/${node}/ceph/mds/${node}" >/dev/null 2>&1
    fi
}

if ! ceph mds stat 2>/dev/null | grep -q 'up:'; then
    log "Creating MDS on ${THIS_NODE}..."
    create_mds "${THIS_NODE}" || warn "MDS creation on ${THIS_NODE} reported an error."
else
    log "An MDS is already running."
fi

for node in ${MDS_NODES}; do
    [ "${node}" = "${THIS_NODE}" ] && continue
    log "Creating standby MDS on ${node}..."
    create_mds "${node}" || warn "MDS creation on ${node} failed — continuing."
done

# The filesystem cannot be created until an MDS has registered with the mons.
for _ in $(seq 1 30); do
    ceph mds stat 2>/dev/null | grep -qE 'up:|standby' && break
    sleep 2
done

##############################################################################
# Filesystem
##############################################################################
if ceph fs ls 2>/dev/null | grep -q "name: ${FS_NAME},"; then
    log "CephFS '${FS_NAME}' already exists — skipping creation."
else
    log "Creating CephFS '${FS_NAME}'..."
    # --add-storage 0 (singular — the plural spelling is rejected outright by
    # PVE 9). Left off deliberately: pveceph registers the storage with the
    # default content types, and we want a specific set, added below.
    pveceph fs create --name "${FS_NAME}" --pg_num 32 --add-storage 0 \
        || fail "pveceph fs create failed."
fi

# Wait for the filesystem to become usable — an MDS has to claim rank 0 before
# any node can mount it, and registering the storage before that produces a
# storage that appears active and fails on first access.
log "Waiting for an MDS to become active..."
active=0
for _ in $(seq 1 45); do
    if ceph fs status "${FS_NAME}" 2>/dev/null | grep -q 'active'; then
        active=1; break
    fi
    sleep 2
done
[ "${active}" -eq 1 ] || warn "no active MDS yet; the storage may take a moment to come online."

##############################################################################
# Register as PVE storage (cluster-wide via pmxcfs)
##############################################################################
if pvesm status 2>/dev/null | awk '{print $1}' | grep -qx "${STORAGE_ID}"; then
    log "PVE storage '${STORAGE_ID}' already defined — skipping."
else
    log "Registering '${STORAGE_ID}' as PVE storage..."
    # These four content types are the whole point: they are what RBD cannot
    # hold. Written once here, replicated to every node by pmxcfs.
    pvesm add cephfs "${STORAGE_ID}" \
        --fs-name "${FS_NAME}" \
        --content iso,vztmpl,snippets,backup \
        || fail "pvesm add cephfs failed."
fi

log "done."
ceph fs status "${FS_NAME}" 2>/dev/null | head -12 || true
pvesm status 2>/dev/null | grep -E "^Name|^${STORAGE_ID}" || true
