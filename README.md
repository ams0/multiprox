# multiprox

Run large Proxmox VE clusters on container infrastructure — for homelab
experimentation, HA testing, and cluster automation without physical hardware.

Two independent deployment paths:

| Path | What it is | Start here |
|---|---|---|
| **[`docker/`](docker/)** | Docker Compose, 3+ nodes on one host. Quickest way to a running cluster. | this README |
| **[`kubernetes/`](kubernetes/)** | A Kubernetes operator managing clusters as StatefulSets, with hyper-converged Ceph via `pveceph` on raw-block PVCs. | [kubernetes/README.md](kubernetes/README.md) |

```bash
make help              # all targets, both paths
make docker-up         # Compose path
make k8s-helm-install  # operator path
```

The Kubernetes path introduces two CRDs — `ProxmoxCluster` (you author) and
`ProxmoxNode` (an observed-state projection the operator writes) — and ships an
e2e suite that runs against an isolated kind cluster (`make -C kubernetes e2e`).

---

## Docker path

## What you get

| Component | Status |
|---|---|
| PVE web UI + REST API (`:8006`) | ✅ Works |
| Corosync quorum + cluster filesystem | ✅ Works |
| `pvecm`, `pvesh`, `pvectl` CLI tools | ✅ Works |
| HA Manager simulation | ✅ Works |
| KVM-accelerated VMs | ⚠️ Linux host with `/dev/kvm` only |
| LXC containers within Proxmox | ⚠️ Linux host, privileged Docker |
| Nested virtualisation on Mac | ❌ Docker Desktop has no `/dev/kvm` |

## Architecture

```
Host (Mac or Linux)
└── Docker bridge: proxmox-net  10.10.0.0/24
    ├── pve1  10.10.0.11   ← cluster bootstrap leader
    ├── pve2  10.10.0.12
    └── pve3  10.10.0.13   (minimum for Corosync quorum)
```

Each node is a privileged Debian Bookworm container running:
- **systemd** as PID 1
- **pve-cluster / pmxcfs** — cluster filesystem mounted at `/etc/pve` via FUSE + Corosync
- **corosync** — knet transport, UDP 5405-5412, encrypts with AES-256
- **pveproxy / pvedaemon** — web UI and REST API
- **sshd** — required for `pvecm add` (one-time credential exchange)

## Prerequisites

| Requirement | Mac (Docker Desktop) | Linux |
|---|---|---|
| Docker Engine ≥ 26 | ✅ | ✅ |
| `docker compose` v2 | ✅ | ✅ |
| `make` | ✅ | ✅ |
| `/dev/kvm` for full VMs | ❌ | ✅ |

## Quickstart

```bash
# 1. Configure
cp .env.example .env
# Edit .env — at minimum change ROOT_PASSWORD

# 2. Build the node image (~2 GB, first build takes a few minutes)
make build

# 3. Start the 3-node cluster
make up

# 4. Wait for nodes to be healthy (watch the health check turn green)
docker compose ps

# 5. Form the cluster
make cluster-full
# Equivalent to: make cluster-init && make cluster-join

# 6. Open the web UI
open https://localhost:8006
# Login: root / <ROOT_PASSWORD from .env>
# Accept the self-signed TLS certificate warning.
```

## Common operations

```bash
make status          # pvecm status
make nodes           # pvecm nodes
make shell N=pve2    # bash shell on pve2
make logs N=pve1     # tail pve1 logs
make restart N=pve3  # restart pve3
make down            # stop containers, keep volumes
make clean           # stop + delete volumes (destructive)
```

## Scaling beyond 3 nodes

```bash
make scale N=6       # generate compose override for 6 nodes total + start
# Then join the new nodes:
docker exec -it multiprox-pve4 cluster-join
docker exec -it multiprox-pve5 cluster-join
docker exec -it multiprox-pve6 cluster-join
```

`make scale` generates `docker-compose.scale.yml` and applies it alongside the base compose file. Nodes pve4–pveN get IPs `10.10.0.14`–`10.10.0.1N` and web UI ports `8009`+.

> **Subnet limit**: the default bridge is `/24` (254 addresses). For > ~230 nodes, change `subnet` in `docker-compose.yml` to a `/22` or larger.

## Enabling VMs and LXC (Linux host only)

1. Confirm KVM is available: `ls /dev/kvm`
2. In `Dockerfile`, uncomment the `ENABLE_KVM` block.
3. In `docker-compose.yml` uncomment `args: ENABLE_KVM: "1"` under `build:`.
4. Add `/dev/kvm` and `/dev/net/tun` to the `devices:` section of each node.
5. Rebuild: `make build && make clean && make up && make cluster-full`

## File structure

```
multiprox/
├── Dockerfile                # Debian Bookworm + PVE packages (no kernel)
├── docker-compose.yml        # 3-node cluster definition
├── .env.example              # Config template (copy to .env)
├── Makefile                  # Cluster lifecycle commands
├── config/
│   ├── corosync.conf.tmpl    # Reference template for Corosync config
│   ├── datacenter.cfg        # PVE datacenter defaults
│   └── interfaces            # /etc/network/interfaces (vmbr0 bridge)
└── scripts/
    ├── entrypoint.sh         # Container init → exec systemd
    ├── cluster-init.sh       # pvecm create (run once on pve1)
    └── cluster-join.sh       # pvecm add (run on each joining node)
```

## Limitations & caveats

- **No hardware fencing**: containers cannot trigger IPMI/iLO power actions. The HA manager works but node fencing is simulated.
- **Data volatility**: if you remove volumes (`make clean`), all cluster state and VM configs are gone. Use external NFS or named volumes for anything you want to persist.
- **Corosync latency**: Corosync is sensitive to network jitter. Running many nodes on a single Docker Desktop VM may cause false quorum failures under load.
- **pmxcfs needs `/dev/fuse`**: Docker Desktop exposes `/dev/fuse` inside containers in privileged mode. On some Linux hosts you may need `--device /dev/fuse` explicitly.
- **Not production**: this setup lacks hardware HA, IPMI/BMC fencing, and redundant physical networks. Use for learning and development only.

## References

- [Proxmox Cluster Manager docs](https://pve.proxmox.com/wiki/Cluster_Manager)
- [pmxcfs internals](https://pve.proxmox.com/wiki/Proxmox_Cluster_File_System_(pmxcfs))
- [dockur/proxmox](https://github.com/dockur/proxmox) — alternative image (KVM-in-Docker, Linux only)
- [Corosync knet transport](https://github.com/kronosnet/kronosnet)
