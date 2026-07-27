#!/usr/bin/env bash
##############################################################################
# multiprox — cluster-init
#
# Creates the Proxmox cluster on node1 (pve1). Run this ONCE after all
# nodes are healthy.
#
# Usage (from the host):
#   make cluster-init
#
# Or directly inside pve1:
#   docker exec -it multiprox-pve1 cluster-init
#
# Environment variables:
#   CLUSTER_NAME  name of the cluster (default: multiprox)
#   NODE_IP       this node's bind IP for Corosync link0 (default: 10.10.0.11)
##############################################################################

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-multiprox}"
NODE_IP="${NODE_IP:-10.10.0.11}"

log()  { echo "[cluster-init] $*"; }
fail() { echo "[cluster-init] ERROR: $*" >&2; exit 1; }

# ── Guard: must run on node1 ─────────────────────────────────────────────────
NODE_NAME="${NODE_NAME:-pve1}"
[[ "${NODE_NAME}" == "pve1" ]] \
    || fail "cluster-init must run on pve1, not ${NODE_NAME}. Aborting."

# ── Guard: cluster must not already exist ────────────────────────────────────
if pvecm status &>/dev/null; then
    log "Cluster already exists:"
    pvecm status
    exit 0
fi

# ── Wait for pve-cluster to be ready ─────────────────────────────────────────
log "Waiting for pve-cluster service..."
for i in $(seq 1 30); do
    systemctl is-active pve-cluster &>/dev/null && break
    sleep 2
    [ "$i" -eq 30 ] && fail "pve-cluster not active after 60s. Check: journalctl -u pve-cluster"
done

# ── Create cluster ────────────────────────────────────────────────────────────
log "Creating cluster '${CLUSTER_NAME}' (link0 = ${NODE_IP})..."
pvecm create "${CLUSTER_NAME}" \
    --link0 "address=${NODE_IP}" \
    --votes 1

log "Cluster created successfully."

# ── Wait for quorum before writing anything into /etc/pve ────────────────────
# pmxcfs is READ-ONLY until corosync establishes quorum. `pvecm create` returns
# as soon as the config is written, well before corosync has formed membership,
# so anything writing to /etc/pve immediately afterwards fails with a bare:
#
#   cp: cannot create regular file '/etc/pve/datacenter.cfg': Permission denied
#
# The same race breaks cluster-join: /root/.ssh/authorized_keys is a symlink
# into /etc/pve/priv/, so ssh-copy-id from a joining node silently fails too.
# Poll for WRITABILITY, not for quorum. `pvecm status` reports Quorate: Yes as
# soon as corosync forms membership, but pmxcfs only flips to read-write once it
# has processed the quorum change through its own CPG callback — measurably
# later. Checking quorum and then writing still loses the race; the only
# reliable signal is a write that succeeds.
log "Waiting for the cluster filesystem to become writable..."
WRITABLE=0
for i in $(seq 1 45); do
    if : > /etc/pve/.multiprox-write-probe 2>/dev/null; then
        rm -f /etc/pve/.multiprox-write-probe 2>/dev/null
        WRITABLE=1
        log "/etc/pve writable after $((i * 2))s."
        break
    fi
    sleep 2
done
[ "${WRITABLE}" -eq 1 ] || log "WARNING: /etc/pve still read-only after 90s."

# Install datacenter defaults now that pmxcfs is mounted AND writable. This
# cannot happen at image-build time: anything present in /etc/pve stops FUSE
# from mounting there at all. pmxcfs replicates the file to every node.
if [ -f /usr/share/multiprox/datacenter.cfg ] && [ ! -f /etc/pve/datacenter.cfg ]; then
    if cp /usr/share/multiprox/datacenter.cfg /etc/pve/datacenter.cfg 2>/dev/null; then
        log "Installed /etc/pve/datacenter.cfg"
    else
        log "WARNING: could not write /etc/pve/datacenter.cfg"
    fi
fi

log ""
pvecm status
log ""
log "Next steps:"
log "  1. Run 'cluster-join' on each remaining node (pve2, pve3, ...)."
log "  2. Or: 'make cluster-join' from the host to join all nodes at once."
