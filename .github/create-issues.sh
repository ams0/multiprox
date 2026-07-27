#!/usr/bin/env bash
##############################################################################
# Backfill GitHub issues for every bug found while building and testing
# multiprox, closing the ones already fixed.
#
#   gh auth login                    # must be able to write to $REPO
#   DRY_RUN=1 ./create-issues.sh     # preview
#   ./create-issues.sh               # do it
#
# One-shot backfill: re-running creates duplicates.
#
# Issue bodies are passed via stdin heredocs rather than shell arguments.
# Quoting them inline is a trap — the bodies contain apostrophes and parentheses,
# and a bare apostrophe silently terminates a single-quoted string.
##############################################################################

set -uo pipefail

REPO="${REPO:-ams0/multiprox}"
DRY_RUN="${DRY_RUN:-0}"

created=0
closed=0
failed=0

label() {
  [ "$DRY_RUN" = "1" ] && return 0
  gh label create "$1" --repo "$REPO" --color "$2" --description "$3" >/dev/null 2>&1 || true
}

# issue <state> <labels-csv> <title> [closing-comment]   — body on stdin
issue() {
  local state="$1" labels="$2" title="$3" comment="${4:-}"
  local body_file
  body_file=$(mktemp)
  cat > "$body_file"

  if [ "$DRY_RUN" = "1" ]; then
    printf '[%-6s] %s\n           labels: %s  (body %s lines)\n' \
      "$state" "$title" "$labels" "$(wc -l < "$body_file" | tr -d ' ')"
    rm -f "$body_file"
    return 0
  fi

  local url
  url=$(gh issue create --repo "$REPO" --title "$title" \
          --body-file "$body_file" --label "$labels" 2>&1 | tail -1)
  rm -f "$body_file"

  if [[ "$url" != https://* ]]; then
    printf '  FAILED  %s\n            %s\n' "$title" "$url"
    failed=$((failed + 1))
    return 1
  fi
  created=$((created + 1))
  printf '  created  %s\n' "$url"

  if [ "$state" = "closed" ]; then
    if gh issue close "$url" --repo "$REPO" --comment "$comment" --reason completed >/dev/null 2>&1; then
      closed=$((closed + 1))
    fi
  fi
}

echo "==> labels"
label "bug"              "d73a4a" "Something is broken"
label "node-image"       "5319e7" "docker/Dockerfile - the Proxmox VE node image"
label "compose"          "0e8a16" "docker/ - Docker Compose deployment path"
label "operator"         "1d76db" "kubernetes/operator - the Kubernetes operator"
label "testing"          "fbca04" "Test coverage and test infrastructure"
label "security"         "b60205" "Vulnerabilities and hardening"
label "found-by-testing" "c5def5" "Only surfaced by running the thing for real"

echo "==> issues"

# ── Node image ──────────────────────────────────────────────────────────────

issue closed "bug,node-image,found-by-testing" \
  "pve-manager install fails on debian:bookworm-slim (aplinfo.dat discarded)" \
  "Fixed in 9826b92." <<'BODY'
## Symptom

Building the node image with `ENABLE_CEPH=1` aborts partway through:

```
Setting up pve-manager (8.4.19) ...
cp: cannot stat '/usr/share/doc/pve-manager/aplinfo.dat': No such file or directory
dpkg: error processing package pve-manager (--configure):
 installed pve-manager package post-installation script subprocess returned error exit status 1
```

## Root cause

`debian:*-slim` ships `/etc/dpkg/dpkg.cfg.d/docker` containing
`path-exclude /usr/share/doc/*`. `pve-manager` keeps a **data** file there —
`aplinfo.dat`, the appliance-template index — so dpkg discarded it at unpack and
the postinst then failed on it.

## Fix

Base image changed to the full `debian:bookworm` (later `debian:trixie`). A
targeted `path-include` would also work, but Proxmox assumes an ordinary Debian
across ~350 packages and many postinst scripts, and slim also strips man pages
and locales. Costs ~100 MB on a ~2 GB image.

## Why it was not caught earlier

Proxmox packages are amd64-only, so the image had never been built at all until
an x86 host was available.
BODY

issue closed "bug,node-image,found-by-testing" \
  "pve-manager installs the enterprise apt repo, breaking every later apt-get update" \
  "Fixed in 9826b92." <<'BODY'
## Symptom

The Ceph stage of the image build fails:

```
Err https://enterprise.proxmox.com/debian/pve bookworm InRelease
  401  Unauthorized
E: The repository 'https://enterprise.proxmox.com/debian/pve bookworm InRelease' is not signed.
```

## Root cause

Installing `pve-manager` writes `/etc/apt/sources.list.d/pve-enterprise.list`:

```
deb https://enterprise.proxmox.com/debian/pve bookworm pve-enterprise
```

That repository requires a paid subscription and returns 401, so the **next**
`apt-get update` in the build fails hard. Nothing in the Dockerfile added it.

## Fix

Remove any enterprise entry after the PVE install, matching on file **content**
rather than filename so both the `.list` and deb822 `.sources` layouts used by
different Proxmox versions are covered.
BODY

issue closed "bug,node-image,found-by-testing" \
  "LVM udev workaround silently produced an empty file on the legacy Docker builder" \
  "Fixed in 9826b92." <<'BODY'
## Symptom

The image built successfully and looked correct, but `/etc/lvm/lvmlocal.conf`
was **empty**, so `lvmconfig` reported none of the intended settings. The failure
would only appear much later, at `pveceph osd create`.

## Root cause

The config was written with a Dockerfile heredoc. Dockerfile heredocs require
**BuildKit**; on the legacy builder `cat > file <<EOF` creates an empty file and
the build still reports success. Docker Desktop defaults to BuildKit, so this
passed on macOS and produced a broken image on a stock Ubuntu host.

## Fix

Rewritten with `printf`, which behaves identically on both builders. The build
step now prints the resulting config and runs `lvmconfig`, so a silent regression
fails loudly.

## Note

This is the *second* incarnation of the same class of bug. The original
workaround was a `sed` against `lvm.conf` that matched nothing, because Debian
ships those settings commented out (`# udev_sync = 1`). Both versions ran,
reported success, and did nothing.
BODY

# ── Compose / runtime ───────────────────────────────────────────────────────

issue closed "bug,compose,found-by-testing" \
  "systemd cannot boot: host cgroup bind-mount shadows the container hierarchy" \
  "Fixed in 73780af." <<'BODY'
## Symptom

Every node container crash-looped. systemd exited 255 immediately, with no output
unless a TTY was attached:

```
Failed to create /init.scope control group: No such file or directory
Failed to allocate manager object.
Exiting PID 1...
```

## Root cause

The compose file bind-mounted the host hierarchy:

```yaml
volumes:
  - /sys/fs/cgroup:/sys/fs/cgroup:ro
```

That is cgroup-v1 era advice. Under cgroup v2 Docker gives the container its own
namespaced hierarchy, and bind-mounting the host over it means systemd cannot
create `/init.scope`.

## Measured

| config | result |
|---|---|
| `cgroupns=private`, no bind mount | boots |
| `cgroupns=private` + bind mount (ro or rw) | exit 255 |
| `cgroupns=host` + bind mount rw | boots |

The compose file was doing bind-mount + Docker's cgroup-v2 default of private —
the one broken combination.

## Fix

`cgroup: private` and no bind mount at all.
BODY

issue closed "bug,node-image,compose,found-by-testing" \
  "pmxcfs cannot mount /etc/pve because the image stages a file there" \
  "Fixed in 73780af." <<'BODY'
## Symptom

`pve-cluster.service` failed on every node, leaving `/etc/pve` empty and every
`pvecm` command broken:

```
fuse: mountpoint is not empty
fuse: if you are sure this is safe, use the 'nonempty' mount option
[main] crit: fuse_mount error: File exists
pve-cluster.service: Failed with result 'exit-code'
```

## Root cause

The Dockerfile staged the datacenter defaults **into** the very directory pmxcfs
mounts over:

```dockerfile
COPY config/datacenter.cfg /etc/pve/datacenter.cfg.default
```

The entrypoint also wrote there before systemd started. FUSE refuses to mount
over a non-empty directory.

## Fix

Defaults now live at `/usr/share/multiprox/datacenter.cfg`; `/etc/pve` is kept
empty in the image and treated purely as a mountpoint. `cluster-init.sh`
installs the file once pmxcfs is mounted **and** writable.
BODY

issue closed "bug,compose,found-by-testing" \
  "Compose never provided /dev/fuse, which pmxcfs requires" \
  "Fixed in 73780af." <<'BODY'
The node containers had no `/dev/fuse`, so pmxcfs could not mount the cluster
filesystem. The entrypoint even warned about this, but nothing supplied the
device.

Fixed by adding to the shared service definition:

```yaml
devices:
  - /dev/fuse:/dev/fuse
```
BODY

issue closed "bug,compose,operator,found-by-testing" \
  "cluster-join used nc, which is not installed in the Proxmox node image" \
  "Fixed in 73780af." <<'BODY'
## Symptom

Joining always failed, claiming node1 was unreachable:

```
[cluster-join/pve2] Waiting for SSH on node1 (10.10.0.11)...
[cluster-join/pve2] ERROR: SSH on 10.10.0.11:22 not reachable after 60s.
```

sshd was in fact listening and answering the whole time:

```
$ echo > /dev/tcp/10.10.0.11/22   # succeeds
SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4
```

## Root cause

The wait loop used `nc -z <host> 22`. **netcat is not installed** in the Proxmox
node image, so every iteration failed with command-not-found and the loop simply
timed out. The operator generates its own copy of this script and had the same
bug.

## Fix

Use bash's built-in `/dev/tcp`, which needs no package.

See also the companion issue about the stub image hiding this.
BODY

issue closed "bug,testing,found-by-testing" \
  "E2E stub image was more capable than the real image, hiding a shipped bug" \
  "Fixed in 73780af." <<'BODY'
## What happened

The e2e suite passed 122 assertions while `cluster-join.sh` was completely broken
on real nodes. The stub node image installed `netcat-openbsd`; the real Proxmox
image does not have it. The stub was **more capable than the image it stands in
for**, so `nc -z` worked in tests and failed everywhere else.

This is the failure mode that turns a stub from a test into a lie.

## Fix

1. The stub no longer installs netcat, and runs a **real sshd** instead of a fake
   listener — closer to the real image, not further from it.
2. Added `TestConfigMap_ScriptsOnlyUseToolsTheRealImageHas`, which fails if any
   generated script invokes a binary absent from the node image
   (`nc`, `socat`, `jq`, `python3`, `curl`, ...).

## Principle

A stub must never be more capable than what it substitutes for. Where it must
differ, the difference should make tests *harder* to pass, not easier.
BODY

issue closed "bug,compose,operator,found-by-testing" \
  "pvecm add has no --password option; joins fail with unable to copy ssh ID" \
  "Fixed in 73780af." <<'BODY'
## Symptom

```
Unknown option: password
400 unable to parse option
```

and after removing the flag:

```
unable to copy ssh ID: exit code 1
```

## Root cause

`pvecm add` in PVE 9 accepts only `--fingerprint`, `--force`, `--link[n]`,
`--nodeid`, `--use_ssh`, `--votes`. There is no `--password`. It expects to
already be able to reach the peer as root over SSH, and its first action is to
push the local key across:

```perl
PVE::Cluster::Setup::ssh_unmerge_known_hosts();
my $cmd = ['ssh-copy-id', '-i', '/root/.ssh/id_rsa', "root\@$host"];
run_command($cmd, ..., 'errmsg' => "unable to copy ssh ID");
```

Note it does **not** pass `StrictHostKeyChecking=no`, so an unknown host key
becomes an interactive prompt that cannot be answered.

## Fix

`cluster-join.sh` now:

1. records node1's host key with `ssh-keyscan` into a persistent `known_hosts`
   (seeding with `UserKnownHostsFile=/dev/null` authenticates fine but leaves
   pvecm facing the prompt anyway),
2. seeds key trust using `sshpass` with the configured root password (`sshpass`
   added to the image),
3. verifies key auth works non-interactively before calling
   `pvecm add <host> --use_ssh`.
BODY

issue closed "bug,compose,found-by-testing" \
  "Race: pmxcfs is read-only until quorum, so early writes fail silently" \
  "Fixed in 73780af." <<'BODY'
## Symptom

Immediately after `pvecm create`:

```
cp: cannot create regular file '/etc/pve/datacenter.cfg': Permission denied
```

and joining nodes failed with a message blaming the password, because
`/root/.ssh/authorized_keys` on node1 is a symlink into `/etc/pve/priv/` — also
pmxcfs, also read-only.

## Root cause

pmxcfs is read-only until corosync establishes quorum. `pvecm create` returns as
soon as the config is written, well before that.

**Checking `pvecm status` for `Quorate: Yes` is not sufficient.** pmxcfs flips to
read-write later still, via its own CPG callback. Measured on a 3-node cluster:

| event | elapsed |
|---|---|
| corosync reports `Quorate: Yes` | 4s |
| `/etc/pve` actually writable | 8s |

A first attempt at fixing this waited for quorum and still lost the race.

## Fix

Poll for the **operation succeeding**, not for a proxy signal:

- `cluster-init.sh` writes a probe file until it succeeds, then installs
  `datacenter.cfg`.
- `cluster-join.sh` retries `ssh-copy-id` until key auth genuinely works.

## Lesson

Waiting on a proxy signal instead of the thing you actually need is a losing
pattern.
BODY

issue closed "bug,compose,found-by-testing" \
  "Corosync exhausts Docker 64 MB /dev/shm; cluster looks healthy but all commands fail" \
  "Fixed in 73780af." <<'BODY'
## Symptom

The cluster was quorate and healthy, yet every CLI call failed:

```
[QB] couldn't allocate file /dev/shm/qb-610-.../qb-response-cfg-data: No space left on device (28)
[QB] shm connection FAILED: No space left on device (28)
Cannot initialise CFG service
```

`/dev/shm` was at **89% of 64 MB**.

## Root cause

Corosync's IPC layer (libqb) allocates POSIX shared memory per client connection.
Docker's default `/dev/shm` is 64 MB. Proxmox on bare metal gets half of RAM, so
this never occurs there.

Membership is carried over the network, so the cluster stays quorate — the
symptom is "the cluster is fine but every command is broken", which is a
confusing thing to diagnose.

## Fix

`shm_size: 1gb` on the node services.
BODY

issue closed "bug,compose,found-by-testing" \
  "Corosync authkey lost on container recreate: /etc/corosync was not persisted" \
  "Fixed in 73780af." <<'BODY'
## Symptom

After `docker compose up --force-recreate`, corosync failed to start on every
node:

```
[MAIN] Could not open /etc/corosync/authkey: No such file or directory
Corosync Cluster Engine exiting with status 8 at main.c:1428
```

`pve-cluster` still started, so the cluster appeared half-alive: `/etc/pve` was
mounted, but every `pvecm` command failed with `Cannot initialize CMAP service`.

## Root cause

`pvecm create` writes the Corosync authkey to `/etc/corosync/`, which lives on
the container filesystem. Only `/var/lib/pve-cluster`, `/var/lib/vz` and
`/var/log/pve` had volumes, so recreating a container destroyed the key.
`corosync.conf` itself was safe — it lives in `/etc/pve` (pmxcfs).

## Fix

A named volume per node for `/etc/corosync`.
BODY

issue closed "bug,compose" \
  "Default SSH host port 2222 collides with the host's own sshd" \
  "Fixed in 73780af." <<'BODY'
The compose defaults mapped pve2's SSH to host port 2222, the most common
alternate sshd port. On a host whose sshd had been moved there, startup failed:

```
failed to bind host port 0.0.0.0:2222/tcp: address already in use
```

Defaults moved to 22201-22203, with a comment explaining why.
BODY

# ── Operator ────────────────────────────────────────────────────────────────

issue closed "bug,operator" \
  "make install fails: RBAC applied before the namespace exists" \
  "Fixed in 52c5f36." <<'BODY'
The documented `make install && make deploy` sequence failed:

```
Error from server (NotFound): error when creating "config/rbac/serviceaccount.yaml":
namespaces "multiprox-system" not found
```

`config/rbac/serviceaccount.yaml` is namespaced, but only
`config/manager/deployment.yaml` created the namespace — and that runs in the
later target.

Fixed by extracting `config/namespace.yaml`, applied first by both targets.
BODY

issue closed "bug,operator,found-by-testing" \
  "Spurious reconcile error on every new cluster from a cache read-after-write" \
  "Fixed in 52c5f36." <<'BODY'
Every freshly created ProxmoxCluster logged a hard reconcile error:

```
"msg":"Reconciler error","error":"StatefulSet.apps \"defaults-probe\" not found"
```

The controller created the StatefulSet and immediately `Get`-ed it through the
cache-backed client. Before the informer catches up this returns `NotFound`,
which was surfaced as a failure rather than treated as "not observable yet".

Now treated as a normal condition and requeued.
BODY

issue closed "bug,operator,found-by-testing" \
  "Conflict-driven reconcile churn from repeated status writes" \
  "Fixed in 52c5f36." <<'BODY'
The operator logged a steady stream of:

```
Operation cannot be fulfilled on proxmoxclusters.proxmox.multiprox.io "mp":
the object has been modified; please apply your changes to the latest version and try again
```

A single Reconcile pass legitimately writes status several times as it moves
through phases. Each write bumps `resourceVersion` server-side, leaving the
in-memory object stale so the next write conflicts.

Status and finalizer writes now re-fetch and retry with `retry.RetryOnConflict`.
After the fix the operator log contains **zero** reconcile errors across a full
e2e run.
BODY

issue closed "bug,operator" \
  "Operator image hardcoded GOARCH=amd64 and an outdated Go toolchain" \
  "Fixed in 52c5f36 and 86fdec9." <<'BODY'
The operator Dockerfile pinned `GOARCH=amd64`, producing a binary that cannot run
on an arm64 cluster (Apple silicon, Graviton, kind on an M-series Mac). It also
pinned `golang:1.23-alpine`, which cannot build the module after
controller-runtime v0.24 raised the requirement:

```
go.mod requires go >= 1.26.0
```

Now uses BuildKit's `TARGETARCH` and `golang:1.26-alpine`.
BODY

issue closed "bug,operator,found-by-testing" \
  "ProxmoxNode projection was stale exactly when it is most useful" \
  "Fixed in 52c5f36." <<'BODY'
`reconcileNodes` ran only at the end of the happy path, so the projection was
refreshed only once a cluster reached `Ready`. Observed live during a scale-up:
`spec.nodes` was 4 while `kubectl get pxn` still showed 3.

That inverts the feature's purpose — the projection exists to answer "which node
is stuck joining?", a question only ever asked mid-transition.

Now registered as a `defer` immediately after the StatefulSet is ensured, so
every return path refreshes it, including error and requeue paths. It is
best-effort and cannot fail the cluster reconcile.

After the fix the projection populates while the cluster is still `Pending`,
showing per-node `Pending`/`Starting`/`Joining` states.
BODY

issue closed "bug,operator" \
  "ProxmoxNode was rewritten on every reconcile (write amplification)" \
  "Fixed in 52c5f36." <<'BODY'
`ProxmoxNodeFor` builds a fresh status each pass, stamping every condition with
the current time. A naive comparison therefore always differed, so every
ProxmoxNode would be patched on every reconcile — multiplying API-server write
load by the node count for no benefit.

`MergeNodeStatus` now carries `LastTransitionTime` forward from the stored object
when a condition's status is unchanged, and computes its changed flag while
ignoring timestamps entirely.

Verified against a live API server: `resourceVersion` unchanged (4301 -> 4301)
across 45s spanning several reconciles.

Consequence worth knowing: `observedAt` only advances when something else also
changed. It means "state last confirmed to differ at", not "last polled at".
BODY

issue closed "bug,operator" \
  "ValidateCeph rejected a monCount the user never set" \
  "Fixed in 52c5f36." <<'BODY'
A 1- or 2-node cluster with Ceph enabled and `monCount` left unset failed
immediately:

```
spec.ceph.monCount (3) exceeds spec.nodes (1): a monitor needs its own node
phase: Failed
```

The CRD defaults `monCount` to 3 server-side, so the operator rejected a value
the user never wrote — while `CephPlan` already clamped it correctly. Validation
was stricter than the executor.

Naively capping at the node count is also wrong: a 2-node cluster would get 2
monitors, an even quorum tolerating zero failures, which is exactly what the
odd-count rule exists to prevent.

`EffectiveMonCount` now caps **and snaps down to odd** (2 nodes -> 1 mon),
`ValidateCeph` rejects only an explicitly even `monCount`, and the
`CephMonsReady` condition reports the effective numbers so the clamping is not
silent. Also fixed `monCount: 0` tripping the even check.
BODY

issue closed "bug,operator" \
  "Helm chart installed the CRD twice, breaking helm install" \
  "Fixed in 52c5f36." <<'BODY'
`templates/crd.yaml` rendered the CRD while Helm also auto-installs everything in
`crds/`, so `helm install` would fail on a duplicate resource.

Removed the redundant template and documented `--skip-crds` as the opt-out, since
Helm never templates or upgrades `crds/`.
BODY

issue closed "security,operator" \
  "Four reachable CVEs in Go dependencies" \
  "Fixed in 86fdec9." <<'BODY'
`govulncheck` reported four vulnerabilities reachable from our own code, all via
the SPDY exec path in `internal/exec` (how the operator drives pvecm and pveceph
inside the containers):

| ID | Module |
|---|---|
| GO-2026-5970 | golang.org/x/text - infinite loop on invalid input |
| GO-2026-5026 | golang.org/x/net/idna - ASCII-only Punycode labels not rejected |
| GO-2026-4958 | github.com/moby/spdystream - unbounded resource use parsing frames |
| GO-2026-4918 | golang.org/x/net - infinite loop in HTTP/2 transport |

Upgrading k8s and controller-runtime alone was **not** sufficient: k8s.io v0.36
still pins `x/net` v0.49.0 and `x/text` v0.33.0, both below the fixed versions.
Those were raised explicitly, which Go's minimal version selection permits.

`govulncheck` now reports: **No vulnerabilities found.**
BODY

# ── Ceph on real disks (b171857) ────────────────────────────────────────────

issue closed "bug,compose,found-by-testing" \
  "Proxmox cannot use a block device mapped under a custom name" \
  "Fixed in b171857." <<'BODY'
## Symptom

```
# pveceph osd create /dev/ceph-osd-0
unable to get device info for '/dev/ceph-osd-0'
```

The device node exists and is a valid block device:

```
$ ls -l /dev/ceph-osd-0     -> brw-rw---- 8, 0
$ lsblk /dev/ceph-osd-0     -> resolves to sda
```

## Root cause

`docker --device /dev/disk/by-id/scsi-XXX:/dev/ceph-osd-0` creates a device
**node** at the container path, but sysfs belongs to the host and still only
knows the disk by its kernel name:

```
$ ls /sys/block/    -> sda sdb sdc sdd      (no ceph-osd-0)
```

`PVE::Diskmanage` resolves disks through `/sys/block/<name>`, so it rejects any
path whose basename is not a kernel device name.

## Fix

`scripts/ceph-osd-create.sh` derives the kernel name from the device major:minor
via `/sys/dev/block/<maj>:<min>/uevent`, recreates the node under that name, and
passes it to pveceph. Host-side selection stays on stable
`/dev/disk/by-id/...` paths.

The script also refuses any device that is mounted or has mounted partitions —
`pveceph osd create` destroys its target with no confirmation.
BODY

issue closed "bug,compose,found-by-testing" \
  "ceph-volume fails without the udev database: No udev data could be retrieved" \
  "Fixed in b171857." <<'BODY'
## Symptom

```
Running command: ceph-volume lvm create --data /dev/sda
-->  RuntimeError: No udev data could be retrieved for /sys/block/sda
command 'ceph-volume lvm create --data /dev/sda' failed: exit code 1
```

## Root cause

`ceph-volume` reads device properties from the udev database through pyudev.
There is no udevd running in the container and no `/run/udev` database, so the
lookup fails outright.

## Fix

Bind-mount the host database read-only:

```yaml
volumes:
  - /run/udev:/run/udev:ro
```

Read-only is deliberate: the container consumes the host's data and never writes
to it.
BODY

issue closed "bug,compose,found-by-testing" \
  "/var/lib/ceph not persisted: recreating a container destroys all monitors" \
  "Fixed in b171857." <<'BODY'
## Symptom

After a `docker compose up -d` that recreated containers, the Ceph cluster
became unreachable:

```
monclient(hunting): authenticate timed out after 300
[errno 110] RADOS timed out (error connecting to the cluster)
```

`ceph.client.bootstrap-osd.keyring` was also missing, and OSD creation failed
with `Unable to create a new OSD id`.

## Root cause

`pveceph mon create` writes monitor, manager and OSD state to `/var/lib/ceph`,
which was on the container filesystem with no volume. Recreating a container
destroyed every monitor.

This is the same trap as the Corosync authkey in `/etc/corosync` — Ceph state
looks like it lives in the cluster filesystem, but most of it does not.

## Fix

A named volume per node for `/var/lib/ceph`.
BODY

issue closed "bug,compose" \
  "Ceph pools are created in a permanent HEALTH_WARN" \
  "Fixed in b171857." <<'BODY'
After `pveceph pool create`, a small cluster sits at HEALTH_WARN indefinitely:

```
HEALTH_WARN 1 pools have too many placement groups
[WRN] POOL_TOO_MANY_PGS: Pool ceph-vm has 128 placement groups, should have 32
```

PVE creates pools with `pg_autoscale_mode=warn` and a default `pg_num` of 128.
On three OSDs that is far too many, and in `warn` mode Ceph reports the problem
but will not correct it.

`make ceph-pool` now sets `pg_autoscale_mode on` after creating the pool, and
the cluster converges to HEALTH_OK.
BODY

# ── Still open ──────────────────────────────────────────────────────────────

issue open "testing,operator" \
  "No unit tests for controllers/ and internal/exec/" <<'BODY'
`internal/resources` is at 97.5% statement coverage (126 tests). The other two
packages have **zero**:

```
github.com/feynman/multiprox/operator/controllers    [no test files]
github.com/feynman/multiprox/operator/internal/exec  [no test files]
```

Both are covered only end-to-end, so a regression there surfaces in ~12 minutes
rather than under a second. Testing them needs `envtest` or a fake client, which
is a larger lift than the pure-function tests.

Worth covering:

- the reconcile phase machine and its conditions
- `RetryOnConflict` paths for status and finalizer writes
- ProxmoxNode pruning on scale-down
- `execScript` error handling (pod not Running, non-zero exit)
BODY

issue open "operator" \
  "Scaling down requires a manual pvecm delnode" <<'BODY'
Reducing `spec.nodes` scales the StatefulSet and prunes the ProxmoxNode
projection, but nothing removes the node from the Corosync cluster. The removed
node stays in the membership list and counts toward expected votes, degrading
quorum.

`pvecm delnode <name>` must be run manually before scaling down. The operator
should do this as part of a controlled scale-down.
BODY

issue open "operator" \
  "osdsPerNode and osdSize are immutable after creation" <<'BODY'
`volumeClaimTemplates` cannot be patched on an existing StatefulSet, so changing
`spec.ceph.osdsPerNode` or `osdSize` has no effect on a running cluster. The
controller deliberately patches only replicas and the pod template, because
recreating the StatefulSet would orphan OSD data.

Currently documented rather than handled. A real implementation would need an
explicit, opt-in migration path.
BODY

echo ""
echo "==> ${created} created, ${closed} closed, ${failed} failed"
[ "$DRY_RUN" = "1" ] && echo "    (dry run - nothing was created)"
exit 0
