#!/usr/bin/env bash
##############################################################################
# setup-loop-osds.sh — create loop-backed block devices for Ceph OSDs.
#
#   sudo ./setup-loop-osds.sh <nodes> [osds-per-node] [size]
#   sudo ./setup-loop-osds.sh 11 3 8G
#
# Creates <nodes> * <osds-per-node> loop devices and prints the compose
# environment that maps them, one stable symlink per OSD:
#
#   /dev/multiprox/osd-<node>-<index>  ->  /dev/loopN
#
# Run on the HOST, not inside a container. Loop devices are host-kernel
# objects; the containers only receive them via `devices:` mappings.
#
# Why loop devices
# ----------------
# A Ceph OSD needs a whole block device. Real disks are finite — a host with
# three spare disks caps you at three OSDs no matter how many PVE nodes you
# run. Loop devices remove that ceiling, so an 11-node cluster can have three
# OSDs each and exercise real CRUSH placement, replication and rebalancing.
#
# They are NOT a performance substitute: everything lands on the same
# underlying filesystem, so throughput numbers from this setup are meaningless.
# Use them to test topology and behaviour, not speed.
#
# Backing files are SPARSE: an 8G file initially consumes almost nothing and
# grows as Ceph writes. That means the apparent total can exceed real free
# space — fine for testing, but do not fill it.
##############################################################################

set -euo pipefail

NODES="${1:-}"
PER_NODE="${2:-3}"
SIZE="${3:-8G}"
STORE="${STORE:-/var/lib/multiprox-osd}"
LINKDIR="/dev/multiprox"

if ! [[ "$NODES" =~ ^[0-9]+$ ]] || [ "$NODES" -lt 1 ]; then
    echo "usage: $0 <nodes> [osds-per-node] [size]" >&2
    exit 1
fi
[ "$(id -u)" -eq 0 ] || { echo "must run as root (losetup needs it)" >&2; exit 1; }

TOTAL=$(( NODES * PER_NODE ))

# Guard: refuse if the sparse total wildly exceeds free space. Sparse files can
# be over-committed, but 10x is asking for a wedged host mid-test.
avail_gb=$(df -BG --output=avail "$(dirname "$STORE")" 2>/dev/null | tail -1 | tr -dc '0-9')
size_gb=$(echo "$SIZE" | tr -dc '0-9')
want_gb=$(( TOTAL * size_gb ))
echo "==> $TOTAL devices x $SIZE = ${want_gb}G apparent (sparse), ${avail_gb}G free"
if [ "$want_gb" -gt $(( avail_gb * 10 )) ]; then
    echo "refusing: ${want_gb}G apparent is more than 10x the ${avail_gb}G available." >&2
    echo "          reduce the size or the device count." >&2
    exit 1
fi

mkdir -p "$STORE" "$LINKDIR"

for n in $(seq 1 "$NODES"); do
    for i in $(seq 0 $(( PER_NODE - 1 ))); do
        img="${STORE}/osd-${n}-${i}.img"
        link="${LINKDIR}/osd-${n}-${i}"

        # Already attached and linked? leave it alone (idempotent re-runs).
        if [ -L "$link" ] && [ -b "$link" ]; then
            continue
        fi

        [ -f "$img" ] || truncate -s "$SIZE" "$img"

        # Reuse an existing association for this file if there is one.
        dev=$(losetup -j "$img" | cut -d: -f1 | head -1)
        [ -n "$dev" ] || dev=$(losetup -f --show "$img")

        ln -sf "$dev" "$link"
    done
done

echo "==> attached; symlinks under ${LINKDIR}:"
ls -l "$LINKDIR" | tail -n +2 | awk '{printf "    %s -> %s\n", $9, $11}' | head -6
[ "$TOTAL" -gt 6 ] && echo "    ... ($TOTAL total)"

echo ""
echo "==> add to docker/.env:"
for n in $(seq 1 "$NODES"); do
    for i in $(seq 0 $(( PER_NODE - 1 ))); do
        echo "PVE${n}_OSD_DEVICE_${i}=${LINKDIR}/osd-${n}-${i}"
    done
done | head -6
echo "# ... (generated in full by scripts/gen-ceph.sh)"

cat <<'NOTE'

Loop devices do NOT survive a host reboot. Re-run this script afterwards; it is
idempotent and will re-attach the same backing files to (possibly different)
loop devices, refreshing the symlinks. The compose file references the stable
symlinks, so nothing else needs changing.
NOTE
