# multiprox — Kubernetes Operator

A Kubernetes operator that manages Proxmox VE clusters as **StatefulSets**, with optional hyper-converged **Ceph** storage configured natively from inside Proxmox via `pveceph`, backed by raw-block PersistentVolumeClaims.

## Architecture

```
ProxmoxCluster CR
       │
       ▼
multiprox-operator (Deployment, multiprox-system)
       │
       ├── StatefulSet: <name>-0, <name>-1, … <name>-N
       │     Each pod = one Proxmox VE node
       │     Systemd as PID 1, privileged
       │     PVC: cluster-data (/var/lib/pve-cluster)
       │     PVC: vm-storage   (/var/lib/vz)
       │
       ├── Service: <name>-headless  (ClusterIP: None)
       │     Stable DNS for Corosync knet ring0:
       │     <name>-0.<headless>.<ns>.svc.cluster.local
       │
       ├── Service: <name>-ui  (ClusterIP :8006)
       │     PVE web UI / REST API
       │
       └── ConfigMap: <name>-config
             corosync.conf, datacenter.cfg
             cluster-init.sh, cluster-join.sh, node-bootstrap.sh
             ceph-{init,mon,mgr,osd,pool,status}.sh   (when spec.ceph is set)
```

### Ceph disk model

Ceph is **not** managed by an external storage operator. The disks are plain
Kubernetes PVCs; Proxmox owns the Ceph cluster exactly as it would on bare metal.

```
StatefulSet volumeClaimTemplates
  ceph-osd-0 … ceph-osd-(M-1)     volumeMode: Block   ← raw, no filesystem
       │
       ▼  one PVC set per pod, via volumeDevices (not volumeMounts)
pod <name>-2
  /dev/ceph-osd-0   ← whole block device inside the container
  /dev/ceph-osd-1
       │
       ▼  operator exec: pveceph osd create /dev/ceph-osd-N
  ceph-volume creates an LVM PV/VG/LV on the device and starts the OSD daemon
```

Total OSDs = `spec.nodes` × `spec.ceph.osdsPerNode`.

### Cluster formation sequence

```
All pods Running
       │
       ▼  [operator exec]
pod-0  →  cluster-init.sh  →  pvecm create <name>
       │
       ▼  [operator exec, sequential]
pod-1  →  cluster-join.sh  →  pvecm add <pod-0-dns>
pod-2  →  cluster-join.sh  →  pvecm add <pod-0-dns>
  …
       │
       ▼  [only when spec.ceph is set]
pod-0        →  ceph-init.sh  →  pveceph init --network <cidr>
pod-0..mon-1 →  ceph-mon.sh   →  pveceph mon create
pod-0..mgr-1 →  ceph-mgr.sh   →  pveceph mgr create
every pod    →  ceph-osd.sh   →  pveceph osd create /dev/ceph-osd-N  (per disk)
pod-0        →  ceph-pool.sh  →  pveceph pool create + pvesm add rbd
pod-0        →  ceph-status.sh → poll health into status.ceph
       │
       ▼
status.phase = Ready
```

Ordering is load-bearing: monitors must exist before OSDs can register, and the
OSDs must be up before a pool with `size: N` can reach `active+clean`.

Every script is idempotent — it checks for the state it would create and exits
early if already present. The operator replays the whole plan on each pass, so a
failure mid-sequence resumes from the same point on the next reconcile rather
than restarting from scratch.

## Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes ≥ 1.28 | StatefulSet ordinal labels, cgroup v2 |
| `kubectl` + `helm` | For installation |
| Privileged pods allowed | PVE needs systemd + FUSE |
| StorageClass with `volumeMode: Block` | Only if `spec.ceph` is set |
| Nodes with KVM | Only if you want VM acceleration |

### Privileged containers

Proxmox VE runs systemd as PID 1 and mounts `/etc/pve` via FUSE (pmxcfs). This requires `privileged: true` on the PVE pods. Ensure your cluster's PSA (Pod Security Admission) or PodSecurityPolicy allows privileged pods in the namespace where you deploy `ProxmoxCluster` resources.

```bash
# Allow privileged pods in a namespace (PSA)
kubectl label namespace <your-ns> pod-security.kubernetes.io/enforce=privileged
```

## Quickstart

### 1. Build and push images

```bash
# PVE node image (from docker/Dockerfile)
make node-build NODE_IMG=ghcr.io/you/multiprox-node:latest
make node-push  NODE_IMG=ghcr.io/you/multiprox-node:latest

# Operator image
make operator-build IMG=ghcr.io/you/multiprox-operator:latest
make operator-push  IMG=ghcr.io/you/multiprox-operator:latest
```

### 2a. Install via Helm (recommended)

```bash
make helm-install \
  IMG=ghcr.io/you/multiprox-operator:latest
```

### 2b. Install via raw manifests

```bash
make install   # CRD + RBAC
make deploy    # operator Deployment
```

### 3. Create a cluster

```bash
# Minimal 3-node cluster:
kubectl apply -f examples/proxmoxcluster-basic.yaml

# Watch it come up:
kubectl get proxmoxclusters -w

# Once Ready, open the web UI via port-forward:
kubectl port-forward svc/lab-cluster-ui 8006:8006
# Then: https://localhost:8006  (login: root / proxmox)
```

## The two CRDs

| CRD | Authored by | Purpose |
|---|---|---|
| `ProxmoxCluster` (`pxc`) | **you** | Desired state. The only thing you edit. |
| `ProxmoxNode` (`pxn`) | **the operator** | Observed per-node state. Read-only projection. |

### ProxmoxNode: an observed-state projection

`ProxmoxNode` gives you a per-node table that a nested `status.nodes[]` array on
the parent could not:

```bash
kubectl get pxn
# NAME             CLUSTER        ORDINAL   PHASE   IN-CLUSTER   MON    MGR     OSDS   OBSERVED
# prod-cluster-0   prod-cluster   0         Ready   true         true   true    4/4    2m
# prod-cluster-1   prod-cluster   1         Ready   true         true   true    4/4    2m
# prod-cluster-2   prod-cluster   2         Ready   true         true   false   4/4    2m
# prod-cluster-3   prod-cluster   3         Ready   true         false  false   4/4    2m
# prod-cluster-4   prod-cluster   4         Ready   true         false  false   3/4    31s

kubectl get pxn -o wide       # adds HOST — which Kubernetes node each PVE node landed on
```

That last row shows the value of the pattern: node 4 has one OSD down, visible at
a glance without digging through YAML.

**The contract, and why this is safe:**

- The operator **writes** these objects and **never reads them back** to make
  decisions. Desired state lives in `ProxmoxCluster.spec`; observed state is
  re-derived every reconcile from the Kubernetes API (pods) and from Proxmox
  itself (`pvecm`, `ceph`). Without that discipline this becomes a second source
  of truth that can drift or be hand-edited into causing real damage.
- **Editing a ProxmoxNode does nothing.** The next reconcile overwrites it.
  `spec` holds only immutable identity (`clusterRef`, `nodeName`, `ordinal`),
  enforced by CEL validation rules.
- **It can be stale.** If the operator is down, these objects confidently
  describe a world that may no longer exist. `status.observedAt` exists so you
  can tell — but note it advances only when something else also changed, so read
  it as *"state last confirmed to differ at"*, not *"last polled at"*. A healthy
  unchanging node keeps an old timestamp by design. To monitor operator liveness,
  watch the `ProxmoxCluster` or the operator's own metrics.

**Cost control** (the two ways this pattern usually goes wrong):

- Per-node Proxmox facts come from **one** exec on node-0 per reconcile
  (`cluster-inventory.sh`), not one exec per node. Pod-level facts come from the
  cached informer and cost nothing.
- Each object is patched **only when its content actually changed** — timestamps
  are excluded from the comparison, otherwise every node would be rewritten on
  every loop and write load would scale with node count for no benefit.

Objects are owned by the `ProxmoxCluster` via `ownerReferences`, so they are
garbage-collected with it, and explicitly pruned on scale-down.

> **When *not* to use this pattern:** don't turn it into a telemetry sink.
> Low-cardinality, slow-changing facts (is this node joined, does it host a mon)
> are legitimate API state. Per-OSD IOPS and latency histograms belong in
> Prometheus, not etcd.

## ProxmoxCluster API reference

```yaml
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: my-cluster
spec:
  # Required
  nodes: 3                              # Total PVE node count (min 1; 3+ for quorum)
  image: ghcr.io/you/multiprox-node:latest

  # Optional
  clusterName: my-cluster              # pvecm cluster name (default: multiprox)
  imagePullPolicy: IfNotPresent

  resources:
    requests: { cpu: "500m", memory: "2Gi" }
    limits:   { cpu: "4",    memory: "16Gi" }

  storage:
    clusterData: { size: "2Gi",   storageClass: fast-ssd }
    vmStorage:   { size: "100Gi", storageClass: standard }

  corosync:
    transport: knet      # knet (default) | udpu
    crypto:    aes256    # aes256 | aes128 | none
    hash:      sha256    # sha256 | sha384 | sha512 | none

  ceph:                            # omit to skip Ceph entirely
    osdsPerNode:     4              # raw-block PVCs per node → OSD disks
    osdSize:         200Gi
    osdStorageClass: topolvm-provisioner   # MUST support volumeMode: Block
    osdDevicePrefix: /dev/ceph-osd-
    network:         10.244.0.0/16  # omit to derive a /16 from the pod IP
    clusterNetwork:  ""             # optional, needs a 2nd interface (Multus)
    monCount:        3              # odd, ≤ nodes
    mgrCount:        2
    pools:
      - name:    ceph-vm
        size:    3                  # replicas; must be ≤ total OSD count
        minSize: 2
        pgNum:   0                  # 0/unset → PG autoscaler decides
        content: [images]           # images = VM disks, rootdir = LXC
        addAsStorage: true          # false = create pool, don't expose to PVE

  nodeSelector: {}
  tolerations:  []
  affinity:     {}
```

### Status fields

```bash
kubectl get pxc my-cluster -o yaml
```

```yaml
status:
  phase: Ready        # Pending | Initializing | Joining | ConfiguringCeph | Ready | Degraded | Failed
  joinedNodes: 5
  clusterID: "abc123"
  ceph:
    initialized: true
    monitors: [prod-cluster-0, prod-cluster-1, prod-cluster-2]
    managers: [prod-cluster-0]
    osdsReady: 20
    osdsExpected: 20
    osdSummary: "20/20"
    pools: [ceph-vm, ceph-ct, ceph-scratch]
    health: HEALTH_OK
  conditions:
    - type: StatefulSetReady      # all pods Ready
    - type: ClusterInitialized    # pvecm create done
    - type: AllNodesJoined        # pvecm add done on every node
    - type: QuorumHealthy         # Corosync quorum
    - type: CephInitialized       # pveceph init done      ┐
    - type: CephMonsReady         # mons + mgrs created    │ only when
    - type: CephOSDsReady         # all OSDs up and in     │ spec.ceph
    - type: CephPoolsReady        # pools + pvesm add rbd  │ is set
    - type: CephHealthy           # health != HEALTH_ERR   ┘
```

```bash
kubectl get pxc
# NAME           NODES   JOINED   PHASE   CEPH        OSDS    AGE
# prod-cluster   5       5        Ready   HEALTH_OK   20/20   12m
```

## Ceph

Ceph runs **inside** the Proxmox cluster, managed by `pveceph`. The operator's
only job is to attach the disks and drive the CLI; Proxmox owns everything after
that, so `pvecm`, the PVE web UI's Ceph panel, and `ceph` CLI all behave exactly
as on bare metal.

### The disks are raw-block PVCs

`spec.ceph.osdsPerNode` raw-block PVCs are added to the StatefulSet's
`volumeClaimTemplates` and surfaced into each pod through `volumeDevices`:

```yaml
# What the operator generates (abbreviated):
volumeClaimTemplates:
  - metadata: { name: ceph-osd-0 }
    spec:
      volumeMode: Block          # ← no filesystem; a whole device
      accessModes: [ReadWriteOnce]
      storageClassName: topolvm-provisioner
      resources: { requests: { storage: 200Gi } }
# …and in the container:
volumeDevices:
  - name: ceph-osd-0
    devicePath: /dev/ceph-osd-0
```

`pveceph osd create /dev/ceph-osd-0` hands the device to `ceph-volume`, which
puts LVM on it and starts the OSD daemon.

### Requirements

| Requirement | Why |
|---|---|
| StorageClass supporting `volumeMode: Block` | Ceph OSDs need whole devices, not filesystems |
| Node image built with `--build-arg ENABLE_CEPH=1` | Provides `pveceph`, `ceph-volume`, `lvm2` |
| Privileged pods | `ceph-volume` performs LVM operations |
| `monCount` odd and ≤ `spec.nodes` | Monitor quorum; rejected at admission otherwise |
| Pool `size` ≤ total OSD count | Otherwise the pool never reaches `active+clean` |

The operator validates the last two before provisioning anything and reports
`phase: Failed` with the reason rather than creating a cluster that cannot converge.

### Verified block-capable StorageClasses

The cluster default StorageClass often does **not** support block mode. Check first:

```bash
kubectl get sc
```

Known-good: **static local PVs over real disks** (no provisioner needed),
**sig-storage local-static-provisioner**, **TopoLVM**, **OpenEBS**
(device/lvm engines), **Longhorn**, and most cloud CSI drivers
(`ebs.csi.aws.com`, `pd.csi.storage.gke.io`, `disk.csi.azure.com`).

Known-bad: `local-path` (Rancher/K3s and kind default) and anything NFS-backed —
these are filesystem-only. Note this is a **provisioner limitation, not an OS
one**: `local-path` offers no block mode on Linux either, so switching hosts does
not by itself fix it. `ceph-osd.sh` reports the device as missing rather than
failing obscurely, and the OSD condition stays false.

**→ See [docs/block-storage.md](docs/block-storage.md)** for concrete, copy-pasteable
setups for each option, including the gotchas (stable `/dev/disk/by-id` paths,
PV-count arithmetic, PV node affinity vs pod anti-affinity, wiping disks).

```bash
# Build the Ceph-capable node image:
docker build --build-arg ENABLE_CEPH=1 -t <registry>/multiprox-node:ceph ./docker

# Deploy:
kubectl apply -f examples/proxmoxcluster-with-ceph.yaml
```

### Changing OSD layout after creation

`volumeClaimTemplates` are immutable on an existing StatefulSet. Changing
`osdsPerNode` or `osdSize` therefore has **no effect** on a running cluster — the
operator patches only replicas and the pod template, deliberately, because
recreating the StatefulSet would orphan OSD data. To change the disk layout,
delete and recreate the ProxmoxCluster, or add OSDs manually with `pveceph`.

Note that PVCs from `volumeClaimTemplates` survive CR deletion by design; remove
them explicitly to reclaim the space.

## Scaling

```bash
# Scale to 5 nodes
kubectl patch proxmoxcluster lab-cluster --type=merge \
  -p '{"spec":{"nodes":5}}'

# The operator will:
# 1. Update StatefulSet replicas to 5
# 2. Wait for pods 3 and 4 to be Running
# 3. Exec cluster-join.sh on each new pod
# 4. Update status.joinedNodes
```

> **Scaling down** requires manual Corosync quorum management (`pvecm delnode`) before reducing `spec.nodes`, or Corosync may mark the cluster as degraded.

## File structure

```
kubernetes/
├── operator/
│   ├── api/v1alpha1/
│   │   ├── proxmoxcluster_types.go    ProxmoxCluster — desired state (you author)
│   │   ├── proxmoxnode_types.go       ProxmoxNode — observed projection (operator writes)
│   │   ├── groupversion_info.go       Scheme registration
│   │   └── zz_generated.deepcopy.go  Generated DeepCopy methods
│   ├── controllers/
│   │   └── proxmoxcluster_controller.go  Main reconciler
│   ├── internal/resources/
│   │   ├── statefulset.go             StatefulSet + raw-block OSD PVC builder
│   │   ├── service.go                 Headless + UI Service builders
│   │   ├── configmap.go               ConfigMap + all in-pod script templates
│   │   ├── ceph.go                    pveceph plan, validation, status parsing
│   │   └── node.go                    ProxmoxNode builder + inventory parser
│   ├── internal/exec/
│   │   └── exec.go                    Pod exec over SPDY (drives pvecm/pveceph)
│   ├── main.go                        Operator entry point
│   ├── go.mod
│   └── Dockerfile
├── config/
│   ├── crd/proxmoxclusters.yaml       Full OpenAPI v3 CRD schema
│   ├── crd/proxmoxnodes.yaml          Projection CRD (immutable spec via CEL)
│   ├── rbac/{role,rolebinding,serviceaccount}.yaml
│   └── manager/deployment.yaml
├── helm/multiprox-operator/           Helm chart
│   ├── Chart.yaml
│   ├── values.yaml
│   ├── crds/proxmoxclusters.yaml
│   └── templates/
├── examples/
│   ├── proxmoxcluster-basic.yaml      3-node default cluster
│   └── proxmoxcluster-with-ceph.yaml  5-node, 20 OSDs, pveceph
└── Makefile
```

## Development

```bash
# Run the operator locally against your current kubeconfig:
cd operator
go run . \
  --leader-elect=false \
  --metrics-bind-address=:8080 \
  --health-probe-bind-address=:8081

# Re-generate CRD + RBAC after changing types:
make manifests   # requires controller-gen in PATH
```

## E2E tests

```bash
make e2e            # create kind cluster, build + load images, install, run tests
make e2e-run        # re-run against an existing cluster
make e2e-teardown   # delete the multiprox-e2e cluster and nothing else
```

Runs against a dedicated kind cluster (`multiprox-e2e`), addressed by an explicit
context on every command, with a preflight check that aborts if that context
contains foreign nodes — safe to run on a host with other kind clusters.

**Scope:** these tests validate the *operator* — reconciliation, resource
generation, status conditions, the ProxmoxNode projection, pruning, GC, CRD
schema behaviour, and the exec plumbing. They do **not** validate Proxmox or Ceph
themselves: Proxmox packages are amd64-only, and kind's `local-path`
StorageClass cannot provide the `volumeMode: Block` volumes OSDs require. The
node image is therefore a stub that runs the operator's real generated scripts
over faked Proxmox CLIs.

See [test/e2e/README.md](test/e2e/README.md) for exactly what is real vs stubbed,
the full assertion list, and the bugs the suite caught.

## Roadmap

- [x] Pod exec over SPDY remotecommand (`internal/exec`)
- [x] Native `pveceph` Ceph with raw-block PVCs
- [ ] Ceph OSD removal / rebalance on scale-down (`pveceph osd destroy`)
- [ ] Corosync QDevice support for even-node clusters
- [ ] Live migration controller (move VMs between nodes on node drain)
- [ ] Backup CronJob integration (vzdump → S3/NFS)
- [ ] Prometheus metrics exporter for PVE cluster health
- [ ] Multi-namespace support (one operator, many clusters)
