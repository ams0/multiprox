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
# Uses bash's /dev/tcp rather than `nc`. netcat is NOT part of the PVE node
# image, so `nc -z` failed with "command not found" on every iteration and the
# loop always timed out — reporting node-0 as unreachable while sshd was in fact
# listening and answering. /dev/tcp is built into bash and needs no package.
log "Waiting for SSH on node1 (${NODE1_IP})..."
for i in $(seq 1 30); do
    if timeout 3 bash -c "echo > /dev/tcp/${NODE1_IP}/22" 2>/dev/null; then
        log "node1 SSH is up."
        break
    fi
    sleep 2
    [ "$i" -eq 30 ] && fail "SSH on ${NODE1_IP}:22 not reachable after 60s."
done

# ── Seed SSH key trust before joining ────────────────────────────────────────
# `pvecm add` has NO --password option (checked against PVE 9: it accepts only
# --fingerprint, --force, --link[n], --nodeid, --use_ssh, --votes). It expects
# to already be able to reach the peer as root over SSH, and its first act is to
# copy this node's key across. Without existing trust that step just fails:
#
#   unable to copy ssh ID: exit code 1
#
# So push the key ourselves first, using the root password we were given.
# PVE symlinks /root/.ssh/authorized_keys to /etc/pve/priv/authorized_keys, so
# once the key lands on node1 pmxcfs replicates it to the whole cluster.
mkdir -p /root/.ssh && chmod 700 /root/.ssh
[ -f /root/.ssh/id_rsa ] || ssh-keygen -t rsa -b 2048 -f /root/.ssh/id_rsa -N '' -q

# The host key must be PERSISTED in /root/.ssh/known_hosts, not discarded.
# pvecm runs its ssh-copy-id without StrictHostKeyChecking=no, so an unknown
# host key produces an interactive prompt that cannot be answered, and the join
# dies with "unable to copy ssh ID". Seeding with UserKnownHostsFile=/dev/null
# authenticates fine but leaves pvecm facing that prompt anyway.
log "Recording node1 host key..."
ssh-keyscan -H "${NODE1_IP}" >> /root/.ssh/known_hosts 2>/dev/null
sort -u /root/.ssh/known_hosts -o /root/.ssh/known_hosts 2>/dev/null || true
chmod 600 /root/.ssh/known_hosts 2>/dev/null || true

# ── Seed the key, retrying until it sticks ───────────────────────────────────
# On node1, /root/.ssh/authorized_keys is a symlink into /etc/pve/priv/, which
# is pmxcfs. pmxcfs is READ-ONLY until corosync reaches quorum, so a key copy
# attempted too early reports success from ssh-copy-id's point of view but the
# write never lands — and the subsequent key-auth check fails with a message
# blaming the password. Retry until key auth genuinely works.
log "Seeding SSH trust on ${NODE1_IP} (retrying until node1 accepts the key)..."
KEY_OK=0
for i in $(seq 1 30); do
    sshpass -p "${ROOT_PASSWORD}" \
        ssh-copy-id -i /root/.ssh/id_rsa.pub "root@${NODE1_IP}" >/dev/null 2>&1 || true

    if ssh -o BatchMode=yes -o ConnectTimeout=10 "root@${NODE1_IP}" true 2>/dev/null; then
        KEY_OK=1
        log "Key-based SSH to node1 confirmed after $((i * 3))s."
        break
    fi
    sleep 3
done

if [ "${KEY_OK}" -ne 1 ]; then
    fail "Cannot SSH to root@${NODE1_IP} with a key after 90s.
       Check that ROOT_PASSWORD matches node1, and that node1 is quorate —
       /etc/pve/priv/authorized_keys is read-only until it is."
fi

# ── Build pvecm add arguments ────────────────────────────────────────────────
LINK0_ARG=""
[[ -n "${NODE_IP}" ]] && LINK0_ARG="--link0 address=${NODE_IP}"

log "Joining cluster on ${NODE1_IP}..."
pvecm add "${NODE1_IP}" --use_ssh ${LINK0_ARG}

log "Join complete. Cluster status:"
pvecm status
