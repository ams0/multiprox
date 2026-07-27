# Getting `volumeMode: Block` for Ceph OSDs

Ceph OSDs need **whole raw block devices**. `pveceph osd create /dev/X` hands the
device to `ceph-volume`, which builds an LVM PV/VG/LV on it. A filesystem
volume cannot be used, so `spec.ceph` requires a StorageClass that supports
`volumeMode: Block`.

## First, the correction: this is not a Linux vs macOS problem

`local-path` (Rancher's local-path-provisioner, and kind's default) **does not
implement block volumes on any operating system**. It creates a directory on the
host and bind-mounts it — there is no device to hand out. Running kind on Linux
changes nothing about that: the default StorageClass is still `local-path`, and
OSD PVCs will still sit `Pending`.

What Linux genuinely changes is your **options**. On Linux you have real
`/dev` access, so you can point Kubernetes at actual disks, carve up LVM, or
fabricate loop devices. On macOS everything runs inside Docker Desktop's VM,
where you do not readily own the block layer. So:

> Moving to an x86 Linux server does not fix the StorageClass. It makes the
> fixes below available.

## Two separate questions, often conflated

**"Can I fabricate block devices on Linux?"** Yes — loop devices, LVM logical
volumes, ZFS zvols, device-mapper targets. All are real block devices to the
kernel. Verified below.

**"Do I expose them to CSI?"** Usually **no**, and this is where the confusion
sits. There are two unrelated delivery mechanisms:

```
                  ┌─ Static local PVs ─────────────────────────────┐
your device ──────┤   NO CSI driver. NO provisioner.               │──→ PVC
/dev/sdb          │   You hand-write PersistentVolume objects.     │
/dev/loop0        │   provisioner: kubernetes.io/no-provisioner    │
                  └────────────────────────────────────────────────┘

                  ┌─ A CSI driver (TopoLVM, OpenEBS, …) ──────────┐
your device ──────┤   You give the DRIVER a pool (e.g. an LVM VG). │──→ PVC
/dev/sdb          │   It carves volumes out on demand.             │
                  │   You never write PV objects yourself.         │
                  └────────────────────────────────────────────────┘
```

Option **A** below is the first path: no CSI, no driver, no DaemonSet — just PV
YAML pointing at a device path. That is why it is the recommendation for a server
with spare disks; there is nothing extra to install or debug.

Options **C** and **D** are the second path. And the two compose: a CSI driver
does not care whether its volume group sits on a real disk or on a loop device,
so "fake devices + TopoLVM" is a perfectly valid lab setup.

## Which option to pick

| Option | Dynamic? | Extra software | Best for |
|---|---|---|---|
| **A. Real disks via static local PVs** | No | None | A server with spare disks — closest to real Proxmox |
| **B. `local-static-provisioner`** | Semi | One DaemonSet | Many disks across many nodes |
| **C. TopoLVM** | Yes | CSI driver + a VG | Dynamic provisioning from a disk pool |
| **D. Loop devices** | No | None | CI / laptops with no spare disks |

For a real x86 test server with spare disks, **A** is the honest choice: it is
the closest analogue to how Proxmox+Ceph is actually deployed, and it adds no
moving parts to debug alongside the operator.

---

## A. Real disks via static local PVs

No provisioner. You declare one PV per physical disk. `local` volumes support
`volumeMode: Block`, and node affinity pins each PV to the machine holding it.

```yaml
# A StorageClass with no provisioner — binding is manual, and deferred until a
# pod is scheduled so the scheduler can honour each PV's node affinity.
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ceph-osd-local
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Retain
```

```yaml
# One PV per disk. Repeat for every disk on every node.
apiVersion: v1
kind: PersistentVolume
metadata:
  name: osd-node1-sdb
spec:
  capacity:
    storage: 500Gi          # must be >= spec.ceph.osdSize
  volumeMode: Block          # ← the whole point
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ceph-osd-local
  local:
    path: /dev/disk/by-id/ata-Samsung_SSD_870_S5Y1NX0R123456   # stable path
  nodeAffinity:
    required:
      nodeSelectorTerms:
        - matchExpressions:
            - key: kubernetes.io/hostname
              operator: In
              values: [node1]
```

Then:

```yaml
spec:
  ceph:
    osdsPerNode: 2
    osdSize: 500Gi              # <= the PV capacity
    osdStorageClass: ceph-osd-local
```

**Use `/dev/disk/by-id/...`, not `/dev/sdb`.** Kernel device names are not
stable across reboots, and a PV pointing at the wrong disk after a reboot means
Ceph adopts — and wipes — something you did not intend.

### Things that will bite you

- **Count must match exactly.** You need `spec.nodes × osdsPerNode` PVs. A
  missing PV leaves one PVC `Pending` forever; `ceph-osd.sh` then reports the
  device as absent and `CephOSDsReady` stays false.
- **PV node affinity vs pod anti-affinity.** The operator's default anti-affinity
  spreads PVE pods across hosts, and each PV pins its PVC to one host. These must
  be satisfiable together — i.e. spread your PVs across nodes the way you want
  pods spread.
- **Disks must be empty.** `ceph-volume` refuses devices with existing
  partitions or signatures. Wipe first: `wipefs -a /dev/disk/by-id/...` and
  `sgdisk --zap-all`. This destroys data — check the device twice.
- **`Retain` is deliberate.** With `Delete`, removing a ProxmoxCluster would make
  the PV eligible for reuse while it still holds OSD data.

---

## B. sig-storage local-static-provisioner

The automated form of A: a DaemonSet watches a directory for block devices you
symlink in, and creates/deletes PVs for you. Worth it past a handful of disks.

```bash
helm repo add sig-storage-local-static-provisioner \
  https://kubernetes-sigs.github.io/sig-storage-local-static-provisioner
helm install local-static-provisioner \
  sig-storage-local-static-provisioner/local-static-provisioner \
  --namespace kube-system \
  --set classes[0].name=ceph-osd-local \
  --set classes[0].hostDir=/mnt/ceph-disks \
  --set classes[0].volumeMode=Block \
  --set classes[0].storageClass=true
```

Then symlink each disk into `hostDir` on each node:

```bash
sudo mkdir -p /mnt/ceph-disks
sudo ln -s /dev/disk/by-id/ata-Samsung_SSD_870_S5Y1NX0R123456 /mnt/ceph-disks/osd0
```

Set `volumeMode: Block` in the class config — the chart defaults to
`Filesystem`, which silently reintroduces the original problem.

---

## C. TopoLVM — dynamic provisioning

Carve OSD volumes out of an LVM volume group on demand. The only option here
that gives you real dynamic provisioning, so scaling `osdsPerNode` does not
require pre-creating PVs.

```bash
# On each node: one VG that TopoLVM will carve up.
sudo vgcreate ceph-vg /dev/disk/by-id/ata-...

helm repo add topolvm https://topolvm.github.io/topolvm
helm install topolvm topolvm/topolvm \
  --namespace topolvm-system --create-namespace \
  --set lvmd.deviceClasses[0].name=ceph \
  --set lvmd.deviceClasses[0].volume-group=ceph-vg \
  --set lvmd.deviceClasses[0].default=true \
  --set lvmd.deviceClasses[0].spare-gb=10
```

```yaml
spec:
  ceph:
    osdsPerNode: 4
    osdSize: 100Gi
    osdStorageClass: topolvm-provisioner
```

**Note the layering:** Ceph's `ceph-volume` puts LVM on the device it is given,
so you end up with LVM-on-LVM. It works, and TopoLVM is widely used this way,
but it is a layer of indirection that does not exist on bare metal — expect a
small performance cost and one more place to look when debugging.

---

## D. Loop devices — no spare disks

For CI or a laptop. Fabricate block devices from files.

```bash
# On the Linux host (or inside each kind node container).
sudo mkdir -p /var/lib/ceph-loop
for i in 0 1 2; do
  sudo truncate -s 20G /var/lib/ceph-loop/osd${i}.img
  sudo losetup -f --show /var/lib/ceph-loop/osd${i}.img   # → /dev/loopN
done
```

Then declare static local PVs (option A) pointing at the `/dev/loopN` paths.

For kind specifically, the node is a container, so the loop devices have to be
visible inside it:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: worker
    extraMounts:
      - hostPath: /dev/loop0
        containerPath: /dev/loop0
      - hostPath: /dev/loop1
        containerPath: /dev/loop1
```

### Verified

The core mechanic was tested in a privileged Debian container:

```
/dev/loop0 attached from a 2G file
  [ -b /dev/loop0 ]                     → true (a real block device to the kernel)
  blockdev --getsize64 /dev/loop0       → 2147483648
  pvcreate /dev/loop0                   → OK
  vgcreate ceph-test-vg /dev/loop0      → OK
  lvcreate -l 100%FREE -n osd-block     → OK  (with the udev fix below)
  pvs reports a ceph VG on the device   → ceph-osd.sh idempotency guard fires
```

A loop device is not "fake" in any way that matters to Ceph — the kernel exposes
it through the same block layer as a disk, so `ceph-volume` treats it
identically.

### The gotcha this surfaced

Without disabling LVM's udev interaction, `lvcreate` fails inside a container:

```
/dev/ceph-test-vg/osd-block: not found: device not cleared
Aborting. Failed to wipe start of new LV.
```

LVM asks udev to create the LV device node and waits for it; there is no udev in
the container. The node image ships `/etc/lvm/lvmlocal.conf` with `udev_sync=0`,
`udev_rules=0`, `verify_udev_operations=0` and
`obtain_device_list_from_udev=0`, plus `DM_DISABLE_UDEV=1`. Both were verified to
fix it independently.

> This is exactly how a real bug was found: the Dockerfile originally tried to
> patch `lvm.conf` with `sed`, but Debian ships those settings **commented out**
> (`# udev_sync = 1`), so the pattern matched nothing and the fix was a silent
> no-op. `pveceph osd create` would have failed on any Linux host with real
> disks. Hence the drop-in file.

### Remaining caveats

- **Not verified end-to-end.** The block-device and LVM mechanics above are
  tested; the full chain (kind `extraMounts` → static local PV → PVC binding →
  `pveceph osd create` → a healthy Ceph cluster) has not been, because Proxmox
  packages are amd64-only and this was checked on arm64.
- Loop devices are host-kernel objects. They do not survive a reboot unless you
  re-run `losetup`, and a PV pointing at a stale `/dev/loopN` is worse than a
  missing one — it may point at someone else's data.
- Performance is unrepresentative. Fine for "does the operator work", useless for
  "how does Ceph perform".
- If you have real spare disks, use them. Loop devices are the fallback for when
  you do not, or when you want more OSDs than you have spindles.

---

## Verifying before you deploy

```bash
# Does the class exist and what provisioner backs it?
kubectl get sc

# Do PVs actually offer Block mode?
kubectl get pv -o custom-columns=NAME:.metadata.name,MODE:.spec.volumeMode,SC:.spec.storageClassName,NODE:'.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[0].values[0]'
```

The decisive check is a throwaway PVC — cheaper than discovering the truth via a
half-built Ceph cluster:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: block-probe
spec:
  volumeMode: Block
  accessModes: [ReadWriteOnce]
  storageClassName: ceph-osd-local     # the class you intend to use
  resources:
    requests:
      storage: 1Gi
```

`WaitForFirstConsumer` classes stay `Pending` until a pod consumes them, so bind
a probe pod with `volumeDevices` to confirm the device actually appears:

```bash
kubectl get pvc block-probe   # Bound (immediate) or Pending (WaitForFirstConsumer)
```

## How the operator behaves when this is wrong

By design it fails visibly rather than pretending:

- OSD PVCs stay `Pending`, so pods never become Ready and the cluster stays
  `phase: Pending`.
- If pods do start but the device is missing, `ceph-osd.sh` logs
  `WARNING: <dev> is not a block device ... Check that the OSD PVC was
  provisioned with volumeMode: Block` and creates no OSD.
- `CephOSDsReady` and `CephHealthy` stay `False`; `status.ceph.osdSummary` shows
  the shortfall (e.g. `0/12`), and the cluster never reports `Ready`.

This is covered by an e2e test that asserts the operator does **not** report
`Ready` or `CephHealthy=True` when OSDs are impossible — see
[../test/e2e/README.md](../test/e2e/README.md).
