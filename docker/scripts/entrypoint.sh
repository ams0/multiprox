#!/usr/bin/env bash
##############################################################################
# multiprox — container entrypoint
#
# Runs as the very first process (PID 1 pre-systemd). Configures node
# identity then hands off to systemd, which starts all PVE services.
#
# Environment variables (set by docker-compose):
#   NODE_NAME     short hostname, e.g. pve1
#   NODE_IP       container's static IP on proxmox-net, e.g. 10.10.0.11
#   ROOT_PASSWORD root password (default: proxmox)
##############################################################################

set -euo pipefail

NODE_NAME="${NODE_NAME:-pve1}"
NODE_IP="${NODE_IP:-10.10.0.11}"
ROOT_PASSWORD="${ROOT_PASSWORD:-proxmox}"

FQDN="${NODE_NAME}.multiprox.local"

log() { echo "[multiprox/${NODE_NAME}] $*"; }

# ── Root password ─────────────────────────────────────────────────────────────
log "Setting root password..."
echo "root:${ROOT_PASSWORD}" | chpasswd

# ── Hostname ──────────────────────────────────────────────────────────────────
log "Setting hostname to ${FQDN}..."
hostname "${FQDN}"
# Write to /etc/hostname so it survives init
echo "${FQDN}" > /etc/hostname

# ── /etc/hosts ────────────────────────────────────────────────────────────────
# PVE requires the node's own FQDN to resolve to its primary IP.
# Also add sibling nodes so pvecm can reach them by name.
log "Configuring /etc/hosts..."
{
    echo "127.0.0.1 localhost"
    echo "::1       localhost ip6-localhost ip6-loopback"
    # Self — must be the static IP, NOT 127.0.0.1, for Corosync to bind correctly.
    echo "${NODE_IP} ${FQDN} ${NODE_NAME}"
    # Sibling nodes (added statically; Docker DNS also resolves container names)
    echo "10.10.0.11 pve1.multiprox.local pve1"
    echo "10.10.0.12 pve2.multiprox.local pve2"
    echo "10.10.0.13 pve3.multiprox.local pve3"
} > /etc/hosts

# ── SSH host keys ─────────────────────────────────────────────────────────────
# Regenerate if the volume was wiped (new container, same volume).
if [ ! -f /etc/ssh/ssh_host_rsa_key ]; then
    log "Generating SSH host keys..."
    ssh-keygen -A
fi

# ── PVE datacenter config ─────────────────────────────────────────────────────
# /etc/pve is a FUSE mount (pmxcfs) — only available AFTER pve-cluster starts.
# We pre-stage the default into /root so that cluster-init can copy it later.
if [ -f /etc/pve/datacenter.cfg.default ] && [ ! -f /etc/pve/datacenter.cfg ]; then
    cp /etc/pve/datacenter.cfg.default /etc/pve/datacenter.cfg 2>/dev/null || true
fi

# ── /dev/fuse (required by pmxcfs) ───────────────────────────────────────────
if [ ! -c /dev/fuse ]; then
    log "WARNING: /dev/fuse not found. pve-cluster (pmxcfs) will fail to mount /etc/pve."
    log "         Add 'devices: - /dev/fuse:/dev/fuse' to docker-compose.yml."
fi

# ── Hand off to systemd ───────────────────────────────────────────────────────
log "Starting systemd..."
exec /lib/systemd/systemd \
    --system \
    --unit=multi-user.target
