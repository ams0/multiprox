#!/usr/bin/env bash
##############################################################################
# setup-bridge.sh — build the vmbr0 guest bridge inside a PVE node container.
#
# Run automatically from the entrypoint, before systemd starts. Safe to re-run.
#
# The problem
# -----------
# Proxmox attaches every VM and LXC container to a Linux bridge, conventionally
# vmbr0. A containerised node has no bridge at all — only the veth Docker gave
# it — so `pct create ... -net0 bridge=vmbr0` fails and the UI's Network panel
# is empty. A bridge has to be built by hand.
#
# Why not bridge eth0
# -------------------
# The obvious move — `bridge-ports eth0`, as a bare-metal node would do — breaks
# the node. eth0 is the veth carrying Corosync, the Ceph public network, pmxcfs
# and the web UI. Enslaving it drops its address, and by the time you would fix
# that up the cluster has already lost quorum. Worse, it happens at boot, so the
# node comes back unreachable.
#
# So the node gets a SECOND Docker network, and vmbr0 is built on that. eth0 is
# never touched: cluster traffic and guest traffic are cleanly separated, which
# is what a real deployment does anyway (a management NIC and a guest NIC).
#
# Why a shared Docker network rather than an isolated per-node bridge
# ------------------------------------------------------------------
# Each node could own a private bridge and NAT out of it. That is simpler, but
# every node ends up its own broadcast domain: guests on different nodes cannot
# reach each other on L2, and migrating a guest changes its network. Putting all
# the nodes' guest NICs on ONE Docker network instead gives a single L2 spanning
# the whole cluster, which is the topology a Proxmox cluster actually has. A
# guest keeps its address wherever it runs.
#
# Docker's bridge learns MACs like any Linux bridge and does no port security,
# so guest MACs behind vmbr0 pass fine, and Docker's own MASQUERADE rule for the
# subnet gives them outbound connectivity without extra NAT here.
##############################################################################

set -uo pipefail

BRIDGE="${GUEST_BRIDGE:-vmbr0}"
# Subnet of the Docker network carrying guest traffic. The NIC holding an
# address in this range is the one to enslave.
GUEST_SUBNET="${GUEST_SUBNET:-10.20.0.}"

log()  { echo "[bridge] $*"; }
warn() { echo "[bridge] WARNING: $*" >&2; }

# Already built (container restart with the same netns)? Nothing to do.
if ip link show "${BRIDGE}" >/dev/null 2>&1; then
    log "${BRIDGE} already exists — leaving it alone."
    exit 0
fi

# Find the guest NIC by address, not by name: interface naming order is not
# guaranteed when a container is on two Docker networks, so eth1 is a guess
# while "the NIC in the guest subnet" is a fact.
#
# Read the name from `ip -o -4 addr`, whose second field is the bare interface
# name. `ip -o link` looks equivalent but reports veths as "eth1@if234", and
# that suffix is not a usable device name — every lookup with it fails, so the
# guest NIC is never found and vmbr0 is silently skipped on every node.
#
# index($4,pfx)==1 rather than a regex match: the prefix contains dots, which a
# regex would treat as wildcards.
read -r GUEST_IF GUEST_CIDR < <(
    ip -o -4 addr show 2>/dev/null \
        | awk -v pfx="${GUEST_SUBNET}" 'index($4, pfx) == 1 { print $2, $4; exit }'
)
GUEST_IF="${GUEST_IF:-}"
GUEST_CIDR="${GUEST_CIDR:-}"

if [ -z "${GUEST_IF}" ]; then
    warn "no interface found in ${GUEST_SUBNET}0/24."
    warn "The node has no guest network attached, so ${BRIDGE} cannot be built."
    warn "Add the guest-net network to this service in docker-compose.yml."
    # Not fatal: the node is still a perfectly good cluster member, it just
    # cannot host guests. Failing here would take down the whole cluster over
    # a feature that only matters once you create a VM or container.
    exit 0
fi

log "guest NIC ${GUEST_IF} (${GUEST_CIDR}) -> ${BRIDGE}"

# Build the bridge, then move the address onto it. Order matters: enslave first
# and only then re-add the address, so the NIC is never left addressable while
# detached from the bridge.
ip link add name "${BRIDGE}" type bridge 2>/dev/null || {
    warn "could not create ${BRIDGE}."; exit 0; }

# STP off and forward delay 0: with a single port there is no loop to detect,
# and the default 15s listening/learning delay would stall every guest's DHCP.
ip link set "${BRIDGE}" type bridge stp_state 0 forward_delay 0 2>/dev/null || true

ip addr flush dev "${GUEST_IF}" 2>/dev/null || true
ip link set "${GUEST_IF}" master "${BRIDGE}" 2>/dev/null || {
    warn "could not enslave ${GUEST_IF} to ${BRIDGE}."
    ip addr add "${GUEST_CIDR}" dev "${GUEST_IF}" 2>/dev/null || true
    exit 0; }

ip link set "${GUEST_IF}" up
ip link set "${BRIDGE}" up
ip addr add "${GUEST_CIDR}" dev "${BRIDGE}" 2>/dev/null || true

log "${BRIDGE} up with ${GUEST_CIDR}"

##############################################################################
# Describe the result in /etc/network/interfaces.
#
# This is not bookkeeping: the PVE web UI's Node -> Network panel and the
# /nodes/<node>/network API both read this file, and `pct`/`qm` validate a
# requested bridge against it. A bridge that exists only in the kernel shows up
# nowhere in Proxmox and cannot be selected when creating a guest.
#
# eth0 is declared manual on purpose. ifupdown must never try to reconfigure the
# interface Docker set up — a DHCP attempt there (there is no DHCP server on a
# Docker bridge network) hangs boot and can flush the address the cluster is
# using.
##############################################################################
cat > /etc/network/interfaces <<EOF
# Generated by setup-bridge.sh — do not edit by hand.
#
# eth0  Docker-managed cluster network (Corosync, Ceph, pmxcfs, web UI).
#       Declared manual: ifupdown must not touch what Docker configured.
# ${BRIDGE} guest bridge over ${GUEST_IF}, shared L2 across all nodes.

auto lo
iface lo inet loopback

iface ${GUEST_IF} inet manual

auto ${BRIDGE}
iface ${BRIDGE} inet static
    address ${GUEST_CIDR}
    bridge-ports ${GUEST_IF}
    bridge-stp off
    bridge-fd 0
EOF

log "wrote /etc/network/interfaces (${BRIDGE} visible to the PVE UI)"
