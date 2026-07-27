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
log ""
pvecm status
log ""
log "Next steps:"
log "  1. Run 'cluster-join' on each remaining node (pve2, pve3, ...)."
log "  2. Or: 'make cluster-join' from the host to join all nodes at once."
