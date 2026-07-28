##############################################################################
# multiprox — cluster lifecycle
#
# Targets
#   build          Build (or rebuild) the PVE node image
#   up             Start all nodes in the background
#   down           Stop and remove containers (volumes preserved)
#   clean          Stop + remove containers AND named volumes
#
#   wait           Block until every node's healthcheck passes
#   cluster-init   Create the cluster on pve1 (run once after 'make up')
#   cluster-join   Join nodes 2..NODES to the cluster
#   cluster-full   cluster-init + cluster-join in one shot
#
#   status         pvecm status via pve1
#   nodes          pvecm nodes via pve1
#
#   shell N=pve1   Open a bash shell on the given node
#   logs  N=pve1   Tail logs for a specific node (or all if N unset)
#   restart N=pve1 Restart a specific node
#
#   scale NODES=11 Generate the compose override for NODES nodes total
#
#   Ceph (after the PVE cluster is formed):
#     ceph-devices  Host loop devices + the Ceph overlay (needs sudo)
#     ceph-init     pveceph init + monitors
#     ceph-osd      OSDs on every node (3 per node by default)
#     ceph-pool     RBD pool for VM/CT disks
#     ceph-fs       Shared CephFS for ISOs, templates, snippets
#     ceph-full     All of the above, in order
#
# NODES applies to every multi-node target and defaults to 3. Set it once:
#
#     export NODES=11
#
# ── A full 11-node, 33-OSD cluster from scratch ─────────────────────────────
#
#     make clean NODES=11              # only if something is already running
#     make ceph-devices-clean          # only if OSD backing files exist
#     make build
#     make scale NODES=11              # generate the node overlay
#     make ceph-devices NODES=11       # 33 loop devices + the Ceph overlay
#     make up NODES=11
#     make wait NODES=11
#     make cluster-init
#     make cluster-join NODES=11
#     make ceph-full NODES=11
#
# Order matters in one place: ceph-devices must run before up, or the nodes
# start without their OSD devices and have to be recreated.
#
# ── Creating an LXC guest ────────────────────────────────────────────────────
#
#     docker exec multiprox-pve1 pveam update
#     docker exec multiprox-pve1 pveam download cephfs \
#         opensuse-16.0-default_20260116_amd64.tar.xz
#     docker exec multiprox-pve1 pct create 102 \
#         cephfs:vztmpl/opensuse-16.0-default_20260116_amd64.tar.xz \
#         --hostname suse1 --rootfs ceph-vm:8 --cores 1 --memory 512 --swap 0 \
#         --net0 name=eth0,bridge=vmbr0,ip=10.20.0.100/24,gw=10.20.0.1 \
#         --nameserver 1.1.1.1 --password proxmox --unprivileged 1
#
#     # Required for every guest, and there is no pct option for it: LXC cannot
#     # load an AppArmor profile from inside a container, so the start fails with
#     # "Failed to run apparmor_parser ... Failed to initialize LSM".
#     docker exec multiprox-pve1 sh -c \
#         "echo 'lxc.apparmor.profile: unconfined' >> /etc/pve/lxc/102.conf"
#
#     docker exec multiprox-pve1 pct start 102
##############################################################################

PROJECT  := multiprox

# ── One compose invocation for every target ──────────────────────────────────
# The overlays are generated (see `scale` and `ceph-devices`) and are included
# whenever they exist. This has to be the single definition: `docker compose`
# without them knows only pve1-3, so on a scaled cluster `make down` and
# `make clean` quietly left nodes 4..N running with their volumes intact, and
# the next `make up` came back to a half-old cluster.
#
# Assigned with `=`, not `:=`: the files are created by other targets, so the
# wildcard must be evaluated when a recipe runs, not when the Makefile is read.
SCALE_FILE := docker-compose.scale.yml
CEPH_FILE  := docker-compose.ceph.yml
OVERLAYS    = $(wildcard $(SCALE_FILE)) $(wildcard $(CEPH_FILE))
COMPOSE     = docker compose -f docker-compose.yml $(foreach f,$(OVERLAYS),-f $(f))

# Number of PVE nodes the multi-node targets act on.
NODES ?= 3

# Default node for shell/logs/restart targets (a NAME, unlike NODES above).
N ?= pve1

.PHONY: build up down clean \
        cluster-init cluster-join cluster-full \
        status nodes \
        shell logs restart \
        scale wait \
        help

# ── Image & containers ────────────────────────────────────────────────────────

build:
	$(COMPOSE) build

up:
	$(COMPOSE) up -d
	@echo ""
	@echo "  Nodes starting. Monitor with: make logs"
	@echo "  When healthy, run: make cluster-full"
	@echo ""
	@echo "  Web UI:"
	@echo "    pve1 → https://localhost:8006"
	@echo "    pve2 → https://localhost:8007"
	@echo "    pve3 → https://localhost:8008"

down:
	$(COMPOSE) down

clean:
	$(COMPOSE) down -v --remove-orphans
	@echo "Volumes removed. Run 'make build && make up' to start fresh."

# ── Cluster lifecycle ─────────────────────────────────────────────────────────

cluster-init:
	@echo "==> Initialising cluster on pve1..."
	$(COMPOSE) exec pve1 cluster-init

# Joins every node from 2 to NODES. This used to name pve2 and pve3 literally,
# with a separate scale-join target for 4..N — two targets to keep in step, and
# a silent no-op for the scaled nodes if you forgot the second one.
cluster-join:
	@for n in $$(seq 2 $(NODES)); do \
	  echo "==> joining pve$$n"; \
	  $(COMPOSE) exec -T pve$$n cluster-join || exit 1; \
	done

cluster-full: cluster-init cluster-join
	@echo "==> Cluster fully formed. Status:"
	$(COMPOSE) exec pve1 pvecm status

# ── Ceph ──────────────────────────────────────────────────────────────────────
# Requires the ceph overlay and PVE*_OSD_DEVICE set in .env:
#   docker compose -f docker-compose.yml -f docker-compose.ceph.yml up -d
# See docker-compose.ceph.yml for the disk-selection warning.

# Monitors must be an ODD number, and 3 is right for anything up to ~20 nodes:
# more monitors mean more Paxos round trips, not more resilience.
MON_NODES ?= 3

.PHONY: ceph-devices ceph-devices-clean ceph-up ceph-init ceph-osd ceph-pool ceph-fs ceph-status ceph-full

# Host-side loop devices: NODES x OSD_PER_NODE, plus the compose overlay that
# maps them in. Needs root for losetup. Re-run after a host reboot — loop
# devices do not survive one.
ceph-devices:
	@echo "==> loading the rbd kernel module on the host"
	# krbd is a HOST kernel facility and the nodes cannot supply it: a container
	# has no modules of its own, so `rbd map` inside one just fails with
	#   rbd: sysfs write failed / rbd: map failed: (2) No such file or directory
	# because /sys/bus/rbd does not exist. LXC guests need it — their rootfs must
	# be a real block device — and it is easy to miss, because libceph gets
	# loaded anyway by the CephFS client, so lsmod looks reassuring.
	sudo modprobe rbd
	# Survive a host reboot; loop devices do not, but this costs nothing.
	echo rbd | sudo tee /etc/modules-load.d/multiprox-rbd.conf >/dev/null
	sudo ./scripts/setup-loop-osds.sh $(NODES) $(OSD_PER_NODE) $(OSD_SIZE)
	./scripts/gen-ceph.sh $(NODES) $(OSD_PER_NODE) > $(CEPH_FILE)
	@echo "Generated $(CEPH_FILE): $(NODES) x $(OSD_PER_NODE) = $$(( $(NODES) * $(OSD_PER_NODE) )) OSDs."

# Detach the loop devices and DELETE their backing files.
#
# Needed for a genuine from-scratch rebuild: `make clean` removes the containers
# and volumes, but the OSD backing files live on the host and survive. They
# still carry the old Ceph LVM signatures, so ceph-osd-create's idempotency
# check sees "this device already hosts an OSD" and skips every one of them —
# a rebuilt cluster with zero OSDs and no error to explain why.
ceph-devices-clean:
	@echo "==> detaching loop devices and deleting backing files"
	-sudo sh -c 'for l in $$(losetup -j /dev/null 2>/dev/null; losetup -a | grep multiprox-osd | cut -d: -f1); do losetup -d $$l 2>/dev/null; done'
	-sudo rm -rf /dev/multiprox /var/lib/multiprox-osd
	-rm -f $(CEPH_FILE)
	@echo "    done"

ceph-up:
	$(COMPOSE) up -d

# pveceph init + mon/mgr on the first MON_NODES nodes.
ceph-init:
	$(COMPOSE) exec -T pve1 pveceph init --network $(CEPH_NETWORK)
	@for n in $$(seq 1 $(MON_NODES)); do \
	  echo "==> mon/mgr on pve$$n"; \
	  $(COMPOSE) exec -T pve$$n pveceph mon create || true; \
	done

# OSDs on every node from the mapped devices. ceph-osd-create resolves each
# mapped path to the kernel name, fences LVM to this node's devices, and
# refuses mounted devices.
ceph-osd:
	@for n in $$(seq 1 $(NODES)); do \
	  echo "==> OSDs on pve$$n"; \
	  $(COMPOSE) exec -T pve$$n ceph-osd-create || true; \
	done

ceph-pool:
	$(COMPOSE) exec -T pve1 pveceph pool create $(CEPH_POOL) \
	  --size 3 --min_size 2 --add_storages 1
	# PVE creates pools with pg_autoscale_mode=warn and a default pg_num of 128.
	# On a small cluster that is too many, so Ceph sits at HEALTH_WARN
	# ("POOL_TOO_MANY_PGS ... should have 32") and will not fix it itself.
	# Turning the autoscaler on lets it converge to the right count.
	$(COMPOSE) exec -T pve1 ceph osd pool set $(CEPH_POOL) pg_autoscale_mode on

# Shared CephFS for ISOs, CT templates, snippets and backups. The RBD pool
# cannot hold these: it is block storage, so Proxmox rejects those content
# types on it. Without this, ISOs live on each node's local disk and have to be
# uploaded once per node.
ceph-fs:
	$(COMPOSE) exec -T \
	  -e MDS_NODES="$$(seq -f 'pve%g' 1 $(MON_NODES) | tr '\n' ' ')" \
	  pve1 ceph-fs-create $(CEPH_FS)

ceph-status:
	$(COMPOSE) exec -T pve1 ceph -s
	@echo ""
	$(COMPOSE) exec -T pve1 pvesm status

# Everything, assuming the PVE cluster is already formed.
ceph-full: ceph-init ceph-osd ceph-pool ceph-fs ceph-status

CEPH_NETWORK ?= 10.10.0.0/24
CEPH_POOL    ?= ceph-vm
CEPH_FS      ?= cephfs
OSD_PER_NODE ?= 3
OSD_SIZE     ?= 8G

# ── Cluster inspection ────────────────────────────────────────────────────────

status:
	$(COMPOSE) exec pve1 pvecm status

nodes:
	$(COMPOSE) exec pve1 pvecm nodes

# ── Node operations ───────────────────────────────────────────────────────────

shell:
	$(COMPOSE) exec $(N) bash

logs:
ifdef N
	$(COMPOSE) logs -f $(N)
else
	$(COMPOSE) logs -f
endif

restart:
	$(COMPOSE) restart $(N)

# ── Scale: generate compose override for N nodes ──────────────────────────────
# Usage: make scale N=6
# Generates docker-compose.scale.yml with nodes pve4 … pveN and
# applies it alongside the base compose file.
#
# NOTE: After scaling, join new nodes manually:
#   docker exec -it multiprox-pveX cluster-join

# Generates the overlay only; `make up` starts things. It used to also bring the
# cluster up, which meant nodes started before ceph-devices had generated the
# Ceph overlay, so they came up with no OSD devices and had to be recreated.
scale:
	./scripts/gen-scale.sh $(NODES) > $(SCALE_FILE)
	@echo "Generated $(SCALE_FILE) for $(NODES) nodes."

# Block until every node's healthcheck passes. `docker compose up -d` returns as
# soon as the containers exist, but pmxcfs and pveproxy take a while, and
# cluster-init against a node that is not up yet just fails.
wait:
	@echo "==> waiting for $(NODES) nodes to become healthy..."
	@for i in $$(seq 1 90); do \
	  ok=0; \
	  for n in $$(seq 1 $(NODES)); do \
	    st=$$(docker inspect -f '{{.State.Health.Status}}' multiprox-pve$$n 2>/dev/null); \
	    [ "$$st" = "healthy" ] && ok=$$((ok+1)); \
	  done; \
	  if [ "$$ok" -eq "$(NODES)" ]; then echo "    all $(NODES) healthy"; exit 0; fi; \
	  printf "\r    %s/%s healthy" "$$ok" "$(NODES)"; sleep 5; \
	done; \
	echo ""; echo "    timed out waiting for nodes"; exit 1

# ── Help ──────────────────────────────────────────────────────────────────────

help:
	@grep -E '^[a-zA-Z_-]+:' Makefile \
	    | grep -v '^\.PHONY' \
	    | awk -F: '{printf "  %-20s\n", $$1}'
