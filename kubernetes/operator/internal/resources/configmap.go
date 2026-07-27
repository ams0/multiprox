package resources

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// ConfigMap returns the ConfigMap that holds cluster-wide configuration and
// the scripts the operator exec's inside the PVE pods.
func ConfigMap(cluster *pxv1.ProxmoxCluster) (*corev1.ConfigMap, error) {
	corosyncCfg, err := renderCorosync(cluster)
	if err != nil {
		return nil, fmt.Errorf("render corosync template: %w", err)
	}

	data := map[string]string{
		"datacenter.cfg":       datacenterCfg(cluster),
		"corosync.conf":        corosyncCfg,
		"cluster-init.sh":      clusterInitScript(cluster),
		"cluster-join.sh":      clusterJoinScript(cluster),
		"node-bootstrap.sh":    nodeBootstrapScript(cluster),
		"cluster-inventory.sh": clusterInventoryScript(cluster),
	}

	// Ceph scripts are only shipped when Ceph is requested.
	if cluster.Spec.Ceph != nil {
		data["ceph-init.sh"] = cephInitScript(cluster)
		data["ceph-mon.sh"] = cephMonScript(cluster)
		data["ceph-mgr.sh"] = cephMgrScript(cluster)
		data["ceph-osd.sh"] = cephOSDScript(cluster)
		data["ceph-pool.sh"] = cephPoolScript(cluster)
		data["ceph-status.sh"] = cephStatusScript(cluster)
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName(cluster),
			Namespace: cluster.Namespace,
			Labels:    clusterLabels(cluster),
		},
		Data: data,
	}, nil
}

// ConfigMapName returns the ConfigMap name for the cluster.
func ConfigMapName(cluster *pxv1.ProxmoxCluster) string {
	return cluster.Name + "-config"
}

// ─────────────────────────────────────────────────────────────────────────────
// Corosync config template
// ─────────────────────────────────────────────────────────────────────────────

var corosyncTmpl = template.Must(template.New("corosync").Parse(`
totem {
  version:      2
  cluster_name: {{ .ClusterName }}
  transport:    {{ .Transport }}
  crypto_cipher: {{ .Crypto }}
  crypto_hash:  {{ .Hash }}
}

nodelist {
{{ range .Nodes }}  node {
    ring0_addr: {{ .Address }}
    name:       {{ .Name }}
    nodeid:     {{ .ID }}
  }
{{ end }}}

quorum {
  provider: corosync_votequorum
}

logging {
  to_syslog: yes
}
`))

type corosyncNode struct {
	Name    string
	Address string
	ID      int
}

type corosyncData struct {
	ClusterName string
	Transport   string
	Crypto      string
	Hash        string
	Nodes       []corosyncNode
}

func renderCorosync(cluster *pxv1.ProxmoxCluster) (string, error) {
	nodes := make([]corosyncNode, cluster.Spec.Nodes)
	headless := HeadlessSvcName(cluster)
	ns := cluster.Namespace
	for i := range nodes {
		nodes[i] = corosyncNode{
			Name:    fmt.Sprintf("%s-%d", cluster.Name, i),
			Address: fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", cluster.Name, i, headless, ns),
			ID:      i + 1,
		}
	}

	data := corosyncData{
		ClusterName: cluster.Spec.ClusterName,
		Transport:   coalesce(cluster.Spec.Corosync.Transport, "knet"),
		Crypto:      coalesce(cluster.Spec.Corosync.Crypto, "aes256"),
		Hash:        coalesce(cluster.Spec.Corosync.Hash, "sha256"),
		Nodes:       nodes,
	}

	var buf bytes.Buffer
	if err := corosyncTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Proxmox cluster scripts
// ─────────────────────────────────────────────────────────────────────────────

func clusterInitScript(cluster *pxv1.ProxmoxCluster) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# cluster-init.sh — exec'd by the operator on pod ordinal 0.
# Creates the Proxmox cluster. Idempotent.
set -euo pipefail

CLUSTER_NAME="%s"
NODE_IP="$(hostname -I | awk '{print $1}')"

log() { echo "[cluster-init] $*"; }

for i in $(seq 1 30); do
  systemctl is-active pve-cluster &>/dev/null && break
  sleep 3
  [ "$i" -eq 30 ] && { log "ERROR: pve-cluster never became active"; exit 1; }
done

if pvecm status &>/dev/null; then
  log "Cluster already exists — skipping init."
  exit 0
fi

log "Creating cluster '${CLUSTER_NAME}' on $(hostname) (link0=${NODE_IP})..."
pvecm create "${CLUSTER_NAME}" --link0 "address=${NODE_IP}"

log "Cluster created."
pvecm status
`, cluster.Spec.ClusterName)
}

func clusterJoinScript(cluster *pxv1.ProxmoxCluster) string {
	node0DNS := nodeDNS(cluster, 0)

	return fmt.Sprintf(`#!/usr/bin/env bash
# cluster-join.sh — exec'd by the operator on each non-zero pod.
# Joins this node to the cluster running on pod-0. Idempotent.
set -euo pipefail

NODE0="%s"
ROOT_PASSWORD="${ROOT_PASSWORD:-proxmox}"
NODE_IP="$(hostname -I | awk '{print $1}')"

log() { echo "[cluster-join/$(hostname)] $*"; }

if pvecm status &>/dev/null; then
  log "Already in a cluster — skipping join."
  exit 0
fi

for i in $(seq 1 30); do
  systemctl is-active pve-cluster &>/dev/null && break
  sleep 3
  [ "$i" -eq 30 ] && { log "ERROR: pve-cluster never became active"; exit 1; }
done

# Uses bash's /dev/tcp, not nc. netcat is NOT installed in the real Proxmox node
# image, so "nc -z" failed with command-not-found on every iteration and this
# loop always timed out — reporting node-0 unreachable while sshd was listening
# and answering. /dev/tcp is built into bash and needs no package.
log "Waiting for SSH on ${NODE0}..."
for i in $(seq 1 40); do
  if timeout 3 bash -c "echo > /dev/tcp/${NODE0}/22" 2>/dev/null; then
    log "node-0 SSH is up."
    break
  fi
  sleep 3
  [ "$i" -eq 40 ] && { log "ERROR: SSH on ${NODE0} not reachable"; exit 1; }
done

log "Joining cluster via ${NODE0} (link0=${NODE_IP})..."
pvecm add "${NODE0}" \
  --password "${ROOT_PASSWORD}" \
  --link0 "address=${NODE_IP}" \
  -o StrictHostKeyChecking=no

log "Joined."
pvecm status
`, node0DNS)
}

func nodeBootstrapScript(cluster *pxv1.ProxmoxCluster) string {
	headless := HeadlessSvcName(cluster)
	ns := cluster.Namespace

	return fmt.Sprintf(`#!/usr/bin/env bash
# node-bootstrap.sh — container entrypoint. Configures identity, execs systemd.
set -euo pipefail

CLUSTER_NAME="%s"
HEADLESS_SVC="%s"
NAMESPACE="%s"
FQDN="${HOSTNAME}.${HEADLESS_SVC}.${NAMESPACE}.svc.cluster.local"
ROOT_PASSWORD="${ROOT_PASSWORD:-proxmox}"

echo "root:${ROOT_PASSWORD}" | chpasswd
hostname "${HOSTNAME}"
echo "${HOSTNAME}" > /etc/hostname

# Self entry must resolve to a routable pod IP, not 127.x, or Corosync
# and the Ceph mon map will bind to loopback.
SELF_IP="$(hostname -I | awk '{print $1}')"
{
  echo "127.0.0.1 localhost"
  echo "::1       localhost ip6-localhost ip6-loopback"
  echo "${SELF_IP} ${FQDN} ${HOSTNAME}"
} > /etc/hosts

[ -f /etc/ssh/ssh_host_rsa_key ] || ssh-keygen -A

exec /lib/systemd/systemd --system --unit=multi-user.target
`, cluster.Spec.ClusterName, headless, ns)
}

// clusterInventoryScript gathers per-node facts in a SINGLE exec on node-0,
// rather than one exec per node per reconcile. It emits one line per Proxmox
// node in a stable key=value format:
//
//	node=<name> online=<0|1> nodeid=<n> mon=<0|1> mgr=<0|1> osds_up=<n> osds_total=<n>
//
// The operator merges this with pod data it already has cached from the
// Kubernetes API (pod IP, host, phase), which costs nothing extra.
func clusterInventoryScript(cluster *pxv1.ProxmoxCluster) string {
	_ = cluster
	return `#!/usr/bin/env bash
# cluster-inventory.sh — exec'd by the operator on node-0.
# Emits one key=value line per Proxmox node for the ProxmoxNode projection.
# Read-only: makes no changes to the cluster.
set -uo pipefail

# ── Corosync membership ──────────────────────────────────────────────────────
# "pvecm nodes" output:
#      Nodeid      Votes Name
#           1          1 cluster-0 (local)
#           2          1 cluster-1
# A node listed here is a member. Proxmox appends "(local)" to the node the
# command ran on.
declare -A NODEID ONLINE

if pvecm nodes &>/dev/null; then
  while read -r id votes name rest; do
    case "${id}" in ''|*[!0-9]*) continue ;; esac
    name="${name%% *}"
    NODEID["${name}"]="${id}"
    ONLINE["${name}"]=1
  done < <(pvecm nodes 2>/dev/null | tail -n +2)
fi

# ── Ceph daemon placement ────────────────────────────────────────────────────
declare -A ISMON ISMGR OSDUP OSDTOTAL

if [ -f /etc/pve/ceph.conf ] && ceph -s &>/dev/null; then
  # Monitors: "ceph mon dump" lists "0: [v2:ip:port/0,...] mon.<host>"
  while read -r m; do
    [ -n "${m}" ] && ISMON["${m}"]=1
  done < <(ceph mon dump 2>/dev/null | sed -n 's/.*mon\.\([^ ]*\).*/\1/p')

  # Managers: active plus standbys.
  ACTIVE_MGR="$(ceph mgr dump --format json 2>/dev/null \
    | grep -o '"active_name":"[^"]*"' | cut -d'"' -f4)"
  [ -n "${ACTIVE_MGR}" ] && ISMGR["${ACTIVE_MGR}"]=1
  while read -r m; do
    [ -n "${m}" ] && ISMGR["${m}"]=1
  done < <(ceph mgr dump --format json 2>/dev/null \
    | grep -o '"name":"[^"]*"' | cut -d'"' -f4)

  # OSDs per host, from the CRUSH tree. Track the current host bucket, then
  # count osd.N leaves under it and how many are marked up.
  while read -r host up total; do
    [ -n "${host}" ] || continue
    OSDUP["${host}"]="${up}"
    OSDTOTAL["${host}"]="${total}"
  done < <(ceph osd tree 2>/dev/null | awk '
    {
      for (i = 1; i <= NF; i++) {
        if ($i == "host") { host = $(i+1); next }
      }
    }
    /osd\./ {
      if (host != "") {
        total[host]++
        if ($0 ~ /[[:space:]]up[[:space:]]/) up[host]++
      }
    }
    END {
      for (h in total) printf "%s %d %d\n", h, (h in up ? up[h] : 0), total[h]
    }
  ')
fi

# ── Emit ─────────────────────────────────────────────────────────────────────
# Union of every node name we saw, so a node present in Ceph but not yet in
# Corosync (or vice versa) still gets a line.
for name in $(printf '%s\n' "${!NODEID[@]}" "${!ISMON[@]}" "${!OSDTOTAL[@]}" \
              | grep -v '^$' | sort -u); do
  printf 'node=%s online=%s nodeid=%s mon=%s mgr=%s osds_up=%s osds_total=%s\n' \
    "${name}" \
    "${ONLINE[$name]:-0}" \
    "${NODEID[$name]:-0}" \
    "${ISMON[$name]:-0}" \
    "${ISMGR[$name]:-0}" \
    "${OSDUP[$name]:-0}" \
    "${OSDTOTAL[$name]:-0}"
done
`
}

func datacenterCfg(cluster *pxv1.ProxmoxCluster) string {
	_ = cluster
	return strings.TrimSpace(`
bwlimit:
crs: ha=nodes
ha: shutdown_policy=conditional
keyboard: en-us
max_workers: 4
migration: type=insecure
`) + "\n"
}

// ─────────────────────────────────────────────────────────────────────────────
// Ceph scripts (pveceph, driven from inside the Proxmox cluster)
// ─────────────────────────────────────────────────────────────────────────────

// cephInitScript runs `pveceph init` on pod-0. This writes /etc/pve/ceph.conf,
// which lives in pmxcfs and is therefore immediately visible to every node.
func cephInitScript(cluster *pxv1.ProxmoxCluster) string {
	ceph := cluster.Spec.Ceph

	return fmt.Sprintf(`#!/usr/bin/env bash
# ceph-init.sh — exec'd by the operator on pod-0, once per cluster.
# Bootstraps Ceph configuration inside Proxmox. Idempotent.
set -euo pipefail

CEPH_NETWORK="${CEPH_NETWORK:-%s}"
CEPH_CLUSTER_NETWORK="${CEPH_CLUSTER_NETWORK:-%s}"

log() { echo "[ceph-init] $*"; }

# /etc/pve/ceph.conf is the marker that pveceph init has already run.
# It lives in pmxcfs, so it is cluster-wide.
if [ -f /etc/pve/ceph.conf ]; then
  log "Ceph already initialised (/etc/pve/ceph.conf exists) — skipping."
  exit 0
fi

# Derive a public network from the pod IP when not supplied. Pod CIDRs are
# usually a /16 (10.244.0.0/16 for Flannel, 10.42.0.0/16 for K3s, etc).
if [ -z "${CEPH_NETWORK}" ]; then
  SELF_IP="$(hostname -I | awk '{print $1}')"
  CEPH_NETWORK="$(echo "${SELF_IP}" | cut -d. -f1-2).0.0/16"
  log "No spec.ceph.network set — derived ${CEPH_NETWORK} from pod IP ${SELF_IP}."
fi

ARGS=(--network "${CEPH_NETWORK}")
if [ -n "${CEPH_CLUSTER_NETWORK}" ]; then
  ARGS+=(--cluster-network "${CEPH_CLUSTER_NETWORK}")
fi

log "Running: pveceph init ${ARGS[*]}"
pveceph init "${ARGS[@]}"

log "Ceph initialised. /etc/pve/ceph.conf:"
cat /etc/pve/ceph.conf
`, ceph.Network, ceph.ClusterNetwork)
}

// cephMonScript creates a Ceph Monitor on the node it runs on.
func cephMonScript(cluster *pxv1.ProxmoxCluster) string {
	_ = cluster
	return `#!/usr/bin/env bash
# ceph-mon.sh — exec'd by the operator on each node selected to host a monitor.
# Idempotent: skips if this host already appears in the mon map.
set -euo pipefail

log() { echo "[ceph-mon/$(hostname)] $*"; }

if [ ! -f /etc/pve/ceph.conf ]; then
  log "ERROR: /etc/pve/ceph.conf missing — run ceph-init.sh first."
  exit 1
fi

# If this host is already a monitor, /var/lib/ceph/mon/ceph-<host> exists.
if [ -d "/var/lib/ceph/mon/ceph-$(hostname)" ]; then
  log "Monitor already exists on this node — skipping."
  exit 0
fi

log "Creating Ceph monitor..."
pveceph mon create

log "Monitor created. Current mon list:"
ceph mon dump 2>/dev/null || true
`
}

// cephMgrScript creates a Ceph Manager on the node it runs on.
func cephMgrScript(cluster *pxv1.ProxmoxCluster) string {
	_ = cluster
	return `#!/usr/bin/env bash
# ceph-mgr.sh — exec'd by the operator on each node selected to host a manager.
# Idempotent.
set -euo pipefail

log() { echo "[ceph-mgr/$(hostname)] $*"; }

if [ -d "/var/lib/ceph/mgr/ceph-$(hostname)" ]; then
  log "Manager already exists on this node — skipping."
  exit 0
fi

log "Creating Ceph manager..."
pveceph mgr create

log "Manager created."
`
}

// cephOSDScript turns each attached raw block device into a Ceph OSD.
//
// The devices come from the StatefulSet's raw-block volumeClaimTemplates and
// appear at $CEPH_OSD_DEVICE_PREFIX<index>. `pveceph osd create` hands the
// device to ceph-volume, which creates an LVM PV/VG/LV on it and starts the
// OSD daemon.
func cephOSDScript(cluster *pxv1.ProxmoxCluster) string {
	_ = cluster
	return `#!/usr/bin/env bash
# ceph-osd.sh — exec'd by the operator on every PVE node.
# Creates one OSD per attached raw block device. Idempotent per device.
set -euo pipefail

OSDS_PER_NODE="${CEPH_OSDS_PER_NODE:-1}"
PREFIX="${CEPH_OSD_DEVICE_PREFIX:-/dev/ceph-osd-}"

log() { echo "[ceph-osd/$(hostname)] $*"; }

if [ ! -f /etc/pve/ceph.conf ]; then
  log "ERROR: /etc/pve/ceph.conf missing — run ceph-init.sh first."
  exit 1
fi

# A monitor must exist before OSDs can register.
if ! ceph -s &>/dev/null; then
  log "ERROR: cannot reach the Ceph cluster — is at least one monitor up?"
  exit 1
fi

created=0
skipped=0

for i in $(seq 0 $((OSDS_PER_NODE - 1))); do
  DEV="${PREFIX}${i}"

  if [ ! -b "${DEV}" ]; then
    log "WARNING: ${DEV} is not a block device — skipping. Check that the"
    log "         OSD PVC was provisioned with volumeMode: Block."
    continue
  fi

  # ceph-volume writes an LVM signature. If the device already has a
  # ceph_ VG on it, an OSD was already created here.
  if pvs --noheadings -o vg_name "${DEV}" 2>/dev/null | grep -q 'ceph'; then
    log "${DEV} already hosts an OSD — skipping."
    skipped=$((skipped + 1))
    continue
  fi

  log "Creating OSD on ${DEV}..."
  if pveceph osd create "${DEV}"; then
    created=$((created + 1))
    log "OSD created on ${DEV}."
  else
    log "ERROR: pveceph osd create ${DEV} failed."
    exit 1
  fi
done

log "Done. created=${created} skipped=${skipped}"
ceph osd tree 2>/dev/null || true
`
}

// cephPoolScript creates the requested pools and registers them as Proxmox
// storage backends. Runs on pod-0 only (pmxcfs replicates the result).
func cephPoolScript(cluster *pxv1.ProxmoxCluster) string {
	pools := effectivePools(cluster)

	var b strings.Builder
	b.WriteString(`#!/usr/bin/env bash
# ceph-pool.sh — exec'd by the operator on pod-0.
# Creates Ceph pools and registers them as Proxmox storage. Idempotent.
set -euo pipefail

log() { echo "[ceph-pool] $*"; }

if ! ceph -s &>/dev/null; then
  log "ERROR: cannot reach the Ceph cluster."
  exit 1
fi

create_pool() {
  local name="$1" size="$2" min_size="$3" pg_num="$4" add_storage="$5" content="$6"

  if ceph osd pool ls 2>/dev/null | grep -qx "${name}"; then
    log "Pool '${name}' already exists — skipping create."
  else
    local args=(--size "${size}" --min_size "${min_size}")
    # pg_num 0 means "let the autoscaler decide" — omit the flag entirely.
    if [ "${pg_num}" != "0" ]; then
      args+=(--pg_num "${pg_num}")
    fi
    log "Creating pool '${name}' (${args[*]})..."
    pveceph pool create "${name}" "${args[@]}"
  fi

  if [ "${add_storage}" = "true" ]; then
    if pvesm status 2>/dev/null | awk '{print $1}' | grep -qx "${name}"; then
      log "Proxmox storage '${name}' already registered — skipping."
    else
      log "Registering '${name}' as Proxmox RBD storage (content=${content})..."
      pvesm add rbd "${name}" \
        --pool "${name}" \
        --content "${content}" \
        --krbd 0
    fi
  fi
}

`)

	for _, p := range pools {
		add := "true"
		if p.AddAsStorage != nil && !*p.AddAsStorage {
			add = "false"
		}
		content := strings.Join(p.Content, ",")
		if content == "" {
			content = "images,rootdir"
		}
		b.WriteString(fmt.Sprintf(
			"create_pool %q %d %d %d %q %q\n",
			p.Name, p.Size, p.MinSize, p.PGNum, add, content,
		))
	}

	b.WriteString(`
log "All pools reconciled."
ceph osd pool ls detail 2>/dev/null || true
pvesm status 2>/dev/null || true
`)

	return b.String()
}

// cephStatusScript emits a single machine-readable line the operator parses
// into status.ceph. Format:
//
//	health=<HEALTH_*> osds_up=<n> osds_in=<n> mons=<a,b,c> mgrs=<a,b> pools=<x,y>
func cephStatusScript(cluster *pxv1.ProxmoxCluster) string {
	_ = cluster
	return `#!/usr/bin/env bash
# ceph-status.sh — exec'd by the operator to poll Ceph state.
# Emits one key=value line for the controller to parse.
set -uo pipefail

if [ ! -f /etc/pve/ceph.conf ]; then
  echo "health=UNINITIALIZED osds_up=0 osds_in=0 mons= mgrs= pools= detail=ceph.conf missing"
  exit 0
fi

if ! ceph -s &>/dev/null; then
  echo "health=UNREACHABLE osds_up=0 osds_in=0 mons= mgrs= pools= detail=cannot reach cluster"
  exit 0
fi

HEALTH="$(ceph health 2>/dev/null | awk '{print $1}')"
[ -z "${HEALTH}" ] && HEALTH="UNKNOWN"

OSDS_UP="$(ceph osd stat --format json 2>/dev/null \
  | grep -o '"num_up_osds":[0-9]*' | cut -d: -f2)"
OSDS_IN="$(ceph osd stat --format json 2>/dev/null \
  | grep -o '"num_in_osds":[0-9]*' | cut -d: -f2)"
[ -z "${OSDS_UP}" ] && OSDS_UP=0
[ -z "${OSDS_IN}" ] && OSDS_IN=0

MONS="$(ceph mon dump 2>/dev/null | awk '/^[0-9]+: /{print $3}' | paste -sd, -)"
[ -z "${MONS}" ] && MONS=""

MGRS="$(ceph mgr dump --format json 2>/dev/null \
  | grep -o '"active_name":"[^"]*"' | cut -d'"' -f4)"
[ -z "${MGRS}" ] && MGRS=""

POOLS="$(ceph osd pool ls 2>/dev/null | paste -sd, -)"
[ -z "${POOLS}" ] && POOLS=""

DETAIL=""
if [ "${HEALTH}" != "HEALTH_OK" ]; then
  DETAIL="$(ceph health detail 2>/dev/null | head -2 | tail -1 | tr -d '\n' | cut -c1-200)"
fi

echo "health=${HEALTH} osds_up=${OSDS_UP} osds_in=${OSDS_IN} mons=${MONS} mgrs=${MGRS} pools=${POOLS} detail=${DETAIL}"
`
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// effectivePools returns spec.ceph.pools with defaults applied, falling back
// to a single sensible pool when the user specified none.
func effectivePools(cluster *pxv1.ProxmoxCluster) []pxv1.CephPoolSpec {
	if cluster.Spec.Ceph == nil {
		return nil
	}

	pools := cluster.Spec.Ceph.Pools
	if len(pools) == 0 {
		// Replica count cannot exceed the total OSD count.
		totalOSDs := cluster.Spec.Nodes * maxInt32(cluster.Spec.Ceph.OSDsPerNode, 1)
		size := minInt32(3, totalOSDs)
		minSize := maxInt32(size-1, 1)
		pools = []pxv1.CephPoolSpec{{
			Name:    pxv1.DefaultPoolName,
			Size:    size,
			MinSize: minSize,
			Content: []string{"images", "rootdir"},
		}}
	}

	out := make([]pxv1.CephPoolSpec, 0, len(pools))
	for _, p := range pools {
		if p.Size == 0 {
			p.Size = 3
		}
		if p.MinSize == 0 {
			p.MinSize = maxInt32(p.Size-1, 1)
		}
		if len(p.Content) == 0 {
			p.Content = []string{"images", "rootdir"}
		}
		out = append(out, p)
	}
	return out
}

// nodeDNS returns the stable DNS name of pod <ordinal> via the headless Service.
func nodeDNS(cluster *pxv1.ProxmoxCluster, ordinal int32) string {
	return fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
		cluster.Name, ordinal, HeadlessSvcName(cluster), cluster.Namespace)
}

func coalesce(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

func minInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
