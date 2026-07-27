#!/usr/bin/env bash
##############################################################################
# multiprox — cluster-join
#
# Joins this node to the cluster already running on node1 (pve1).
# Run this on every non-bootstrap node (pve2, pve3, ...) AFTER cluster-init
# has completed on pve1.
#
# Usage (from the host, for each joining node):
#   docker exec -it multiprox-pve2 cluster-join
#   docker exec -it multiprox-pve3 cluster-join
#
# Or via make:
#   make cluster-join         # joins all nodes in sequence
#
# Environment variables:
#   NODE1_IP      IP of the bootstrap node (default: 10.10.0.11)
#   NODE_IP       this node's Corosync link0 address
#   ROOT_PASSWORD root password of node1 (used for one-time SSH auth)
##############################################################################

set -euo pipefail

NODE1_IP="${NODE1_IP:-10.10.0.11}"
NODE_IP="${NODE_IP:-}"          # leave empty to let pvecm detect it
ROOT_PASSWORD="${ROOT_PASSWORD:-proxmox}"
NODE_NAME="${NODE_NAME:-}"

log()  { echo "[cluster-join/${NODE_NAME:-$(hostname)}] $*"; }
fail() { echo "[cluster-join/${NODE_NAME:-$(hostname)}] ERROR: $*" >&2; exit 1; }

# ── Guard: don't run on pve1 ─────────────────────────────────────────────────
[[ "${NODE_NAME}" != "pve1" ]] \
    || fail "cluster-join must NOT run on pve1 (the bootstrap node)."

# ── Guard: already in a cluster? ─────────────────────────────────────────────
if pvecm status &>/dev/null; then
    log "Already part of a cluster:"
    pvecm status
    exit 0
fi

# ── Wait for local pve-cluster ────────────────────────────────────────────────
log "Waiting for local pve-cluster service..."
for i in $(seq 1 30); do
    systemctl is-active pve-cluster &>/dev/null && break
    sleep 2
    [ "$i" -eq 30 ] && fail "pve-cluster not active after 60s."
done

# ── Wait for SSH on node1 ─────────────────────────────────────────────────────
log "Waiting for SSH on node1 (${NODE1_IP})..."
for i in $(seq 1 30); do
    nc -z "${NODE1_IP}" 22 2>/dev/null && break
    sleep 2
    [ "$i" -eq 30 ] && fail "SSH on ${NODE1_IP}:22 not reachable after 60s."
done

# ── Disable host-key checking for this join operation ─────────────────────────
# pvecm add uses SSH internally. In a dev/lab cluster we accept the key.
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
export SSH_OPTS

# ── Build pvecm add arguments ────────────────────────────────────────────────
LINK0_ARG=""
[[ -n "${NODE_IP}" ]] && LINK0_ARG="--link0 address=${NODE_IP}"

log "Joining cluster on ${NODE1_IP}..."
# pvecm reads ROOT_PASSWORD via stdin when --password flag is used.
pvecm add "${NODE1_IP}" \
    --password "${ROOT_PASSWORD}" \
    ${LINK0_ARG}

log "Join complete. Cluster status:"
pvecm status
