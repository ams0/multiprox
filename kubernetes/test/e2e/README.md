# multiprox operator — e2e tests

```bash
# From kubernetes/:
make e2e            # create kind cluster, build + load images, install, run tests
make e2e-run        # re-run tests against an existing cluster
make e2e-teardown   # delete the multiprox-e2e cluster (and nothing else)
```

## What these tests do and do not prove

**They validate the operator.** Reconciliation, resource generation, status and
condition transitions, the ProxmoxNode projection, pruning, garbage collection,
CRD schema behaviour, and the exec plumbing that drives Proxmox CLIs all run for
real against a live API server.

**They do not validate Proxmox or Ceph.** Two hard environmental limits:

1. **Proxmox packages are amd64-only.** On an arm64 host (Apple silicon,
   Graviton) the real node image cannot be built or run at all.
2. **kind's `local-path` StorageClass cannot serve `volumeMode: Block`.** Ceph
   OSDs need whole raw devices, so no OSD can ever come up here.

So the PVE node image is a stub. Confirming that Proxmox clustering and Ceph
actually work needs a real amd64 cluster with a block-capable StorageClass —
that is a separate exercise this suite deliberately does not pretend to cover.

## What is real vs stubbed in the node image

The stub (`stub-node/`) is thin on purpose. The operator's **own generated
scripts run verbatim** from the ConfigMap — `node-bootstrap.sh`,
`cluster-init.sh`, `cluster-join.sh`, `cluster-inventory.sh` and the `ceph-*.sh`
set. Their `/etc/hosts` construction, idempotency guards, control flow, waits
and output formats are all genuinely exercised.

Only the layer beneath them is faked:

| Faked | Why |
|---|---|
| `/lib/systemd/systemd` | Sleeps instead of booting an init system. Also opens a listener on `:22`, because `cluster-join.sh` correctly gates on `nc -z <node-0> 22` before running `pvecm add`. |
| `systemctl` | Reports the PVE units active so readiness gating behaves normally. |
| `pvecm` | Persists membership on the cluster-data PVC (mirroring pmxcfs persistence) and discovers peers over the headless Service's DNS, so it adapts to any `spec.nodes`. |
| `pveceph`, `ceph`, `pvesm` | Report "no OSDs / unreachable" **honestly**. Faking a healthy Ceph would hide exactly the failure the Ceph tests exist to catch. |

That last row matters. Test 13 asserts the operator does **not** report `Ready`
or `CephHealthy=True` when OSDs are impossible. A stub that lied about Ceph
health would turn that test into a rubber stamp.

## Safety

Other kind clusters and Docker containers commonly share the host, so:

- The cluster is named **`multiprox-e2e`** and every `kubectl` call passes
  `--context kind-multiprox-e2e` explicitly. The suite never reads or depends on
  `current-context`.
- A **preflight check refuses to run** if any node in that context is not named
  `multiprox-e2e-*`, so a mis-pointed context aborts instead of mutating a
  stranger's cluster.
- `make e2e-teardown` deletes only that one cluster, by name. Nothing in the
  suite runs `kind delete clusters --all` or bulk `docker rm`.

## Test coverage

| # | Area | Notable assertions |
|---|---|---|
| 1 | CRD install | both CRDs `Established`; `pxc`/`pxn` short names resolve |
| 2 | Operator health | Available, 1 ready replica, **0 restarts** (catches crashloops that briefly look healthy) |
| 3 | Defaulting | `clusterName`, `corosync.*`, storage sizes default server-side |
| 4 | Schema rejection | `nodes: 0`, bad transport enum, malformed size/CIDR, uppercase name, `osdsPerNode: 99`, bad pool content |
| 5 | Operator validation | pool `size` > total OSDs and even `monCount` → `phase: Failed` with an explanatory message, **and no StatefulSet created** |
| 6 | Lifecycle | STS/Services/ConfigMap created; headless has no ClusterIP + `publishNotReadyAddresses`; corosync.conf names every node by per-pod DNS; ceph scripts absent when Ceph is off; **projection is populated while still `Pending`** (not only at `Ready`); reaches `Ready` with all conditions true |
| 7 | In-pod effects | cluster markers exist on every pod; `pvecm create` got the configured cluster name; `/etc/hosts` maps the FQDN to the **routable pod IP, not loopback** |
| 8 | ProxmoxNode | one per node; immutable identity spec; `podName`/`kubernetesNode` match the real pod; `observedAt` set; `status.ceph` absent when Ceph off; ownerRef points at the cluster; **nodes spread across distinct hosts** (anti-affinity) |
| 9 | CEL immutability | patching `ordinal`/`clusterRef`/`nodeName` is rejected |
| 10 | Write suppression | `resourceVersion` unchanged across ~45s of reconciles — guards the no-op-skip that prevents write amplification |
| 11 | Scaling | scale up adds a node; scale down **prunes** the surplus ProxmoxNodes and keeps the rest |
| 12 | Ceph disks | OSD PVCs are `volumeMode: Block`, system PVCs `Filesystem`; attached via `volumeDevices` at `/dev/ceph-osd-N` and **absent from `volumeMounts`**; ceph scripts present; env injected |
| 13 | Honest failure | with a filesystem-only StorageClass the cluster does **not** reach `Ready` and `CephHealthy` is not `True` |
| 14 | Deletion | STS garbage-collected; ProxmoxNodes collected via ownerRef; finalizer released |

## Bugs this suite found

Worth recording, since they are the return on writing it:

1. **`make install` failed outright.** `config/rbac/serviceaccount.yaml` is
   namespaced, but only `config/manager/deployment.yaml` created the namespace —
   so the documented `make install && make deploy` order errored with
   `namespaces "multiprox-system" not found`. Fixed by extracting
   `config/namespace.yaml`, applied first by both targets.

2. **Spurious reconcile error on every new cluster.** The controller created the
   StatefulSet and immediately `Get`-ed it through the cache-backed client;
   before the informer caught up this returned `NotFound` and was surfaced as a
   hard reconcile error. Now treated as "not observable yet" and requeued.

3. **Conflict-driven reconcile churn.** A single pass writes status several times
   as it moves through phases; each write bumped `resourceVersion` server-side,
   leaving the in-memory object stale so the next write failed with
   `the object has been modified`. Status and finalizer writes now re-fetch and
   retry on conflict.

4. **Operator image was pinned to amd64.** The Dockerfile hardcoded
   `GOARCH=amd64`, producing a binary that cannot run on an arm64 cluster. Now
   uses BuildKit's `TARGETARCH`.

5. **The ProxmoxNode projection was stale during transitions.** `reconcileNodes`
   was called at the end of the happy path, so it only ran once the cluster
   reached `Ready`. Observed live during a scale-up: `spec.nodes` was 4 while
   `kubectl get pxn` still showed 3. That inverts the feature's purpose — the
   projection exists to answer "which node is stuck joining?", which is only
   ever asked mid-transition. Now registered as a `defer` immediately after the
   StatefulSet is ensured, so every return path refreshes it, including error
   and requeue paths.

   Two things make that safe: the refresh is best-effort and cannot fail the
   reconcile, and `cluster-inventory.sh` degrades to pod-only data when pod-0
   is not yet execable — so a starting cluster projects `Pending`/`Starting`
   nodes rather than nothing.

## Things the suite verified that were previously only asserted in prose

- **Pod anti-affinity actually spreads nodes.** The three PVE nodes landed on
  three distinct kind workers (`worker`, `worker2`, `worker3`). Now an explicit
  assertion rather than an incidental observation — piling all nodes onto one
  host would make a single host failure take out the whole Proxmox cluster.
- **No-op write suppression holds under a real API server.** `resourceVersion`
  was unchanged (`4301` → `4301`) across 45s spanning several reconciles,
  confirming `MergeNodeStatus` excludes timestamps from its change detection.
- **Ceph failure is reported honestly.** With kind's filesystem-only
  StorageClass the OSD PVCs stayed `Pending`, and the cluster correctly refused
  to report `Ready`, with `CephHealthy` never becoming `True`.
