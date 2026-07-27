#!/usr/bin/env bash
##############################################################################
# multiprox operator — e2e test suite
#
# SAFETY
#   Operates exclusively on the kind cluster named multiprox-e2e, addressed by
#   the explicit context kind-multiprox-e2e on every single command. It never
#   reads or relies on kubectl's current-context, so other clusters on the host
#   cannot be touched. Teardown targets that one cluster by name.
#
# SCOPE
#   Tests the OPERATOR. The PVE node image is a stub (see stub-node/Dockerfile):
#   Proxmox packages are amd64-only, so a real node cannot run on arm64 hosts,
#   and kind's local-path StorageClass cannot provide the raw block volumes
#   Ceph OSDs require. The operator's own generated scripts run for real; the
#   Proxmox CLIs beneath them are stubbed.
#
#   Consequence, stated plainly: this suite proves the operator reconciles
#   correctly. It does NOT prove Proxmox clustering or Ceph work — only a real
#   amd64 cluster with block storage can do that.
#
# USAGE
#   ./run-e2e.sh              run all tests
#   ./run-e2e.sh --keep       leave the cluster up afterwards for inspection
##############################################################################

set -uo pipefail

CTX="kind-multiprox-e2e"
CLUSTER="multiprox-e2e"
NS="pxe2e"
OPERATOR_NS="multiprox-system"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

PASS=0
FAIL=0
declare -a FAILURES

# ── Output helpers ───────────────────────────────────────────────────────────
c_reset=$'\033[0m'; c_red=$'\033[31m'; c_grn=$'\033[32m'
c_yel=$'\033[33m'; c_blu=$'\033[36m'; c_bold=$'\033[1m'

section() { printf '\n%s━━━ %s %s\n' "${c_blu}${c_bold}" "$*" "${c_reset}"; }
ok()      { printf '  %sPASS%s  %s\n' "${c_grn}" "${c_reset}" "$*"; PASS=$((PASS+1)); }
bad()     { printf '  %sFAIL%s  %s\n' "${c_red}" "${c_reset}" "$*"; FAIL=$((FAIL+1)); FAILURES+=("$*"); }
info()    { printf '  %s·%s     %s\n' "${c_yel}" "${c_reset}" "$*"; }

k() { kubectl --context "${CTX}" "$@"; }

# assert_eq <description> <expected> <actual>
assert_eq() {
  local desc="$1" want="$2" got="$3"
  if [ "${want}" = "${got}" ]; then
    ok "${desc} (= ${got})"
  else
    bad "${desc} — expected [${want}], got [${got}]"
  fi
}

# assert_contains <description> <needle> <haystack>
assert_contains() {
  local desc="$1" needle="$2" hay="$3"
  if printf '%s' "${hay}" | grep -q -- "${needle}"; then
    ok "${desc}"
  else
    bad "${desc} — [${needle}] not found in: $(printf '%s' "${hay}" | head -c 300)"
  fi
}

# assert_fails <description> <command...>  — command must exit non-zero
assert_fails() {
  local desc="$1"; shift
  local out
  if out="$("$@" 2>&1)"; then
    bad "${desc} — command unexpectedly succeeded: $(printf '%s' "${out}" | head -c 200)"
    return 1
  fi
  ok "${desc}"
  printf '%s' "${out}" > /tmp/multiprox_last_err.txt
  return 0
}

# wait_for <timeout_sec> <description> <command...> — poll until command succeeds
wait_for() {
  local timeout="$1" desc="$2"; shift 2
  local elapsed=0
  while [ "${elapsed}" -lt "${timeout}" ]; do
    if "$@" >/dev/null 2>&1; then
      ok "${desc} (after ${elapsed}s)"
      return 0
    fi
    sleep 3
    elapsed=$((elapsed+3))
  done
  bad "${desc} — timed out after ${timeout}s"
  return 1
}

# wait_for_jsonpath <timeout> <desc> <resource> <jsonpath> <expected>
wait_for_jsonpath() {
  local timeout="$1" desc="$2" res="$3" path="$4" want="$5"
  local elapsed=0 got=""
  while [ "${elapsed}" -lt "${timeout}" ]; do
    got="$(k -n "${NS}" get ${res} -o jsonpath="${path}" 2>/dev/null)"
    if [ "${got}" = "${want}" ]; then
      ok "${desc} (= ${got}, after ${elapsed}s)"
      return 0
    fi
    sleep 3
    elapsed=$((elapsed+3))
  done
  bad "${desc} — expected [${want}], last saw [${got}] after ${timeout}s"
  return 1
}

##############################################################################
# Preflight — refuse to run anywhere but the dedicated cluster
##############################################################################
section "Preflight: cluster isolation"

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  printf '%sFATAL%s kind cluster %s does not exist. Run: make -C kubernetes e2e-setup\n' \
    "${c_red}" "${c_reset}" "${CLUSTER}"
  exit 1
fi
ok "kind cluster ${CLUSTER} exists"

# Every node in this cluster must be a multiprox-e2e node. If not, the context
# is pointing somewhere unexpected and we stop rather than mutate it.
NODES="$(k get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
STRAY=0
for n in ${NODES}; do
  case "${n}" in
    ${CLUSTER}-*) ;;
    *) STRAY=1 ;;
  esac
done
if [ "${STRAY}" -eq 1 ]; then
  printf '%sFATAL%s context %s contains foreign nodes: %s\n' \
    "${c_red}" "${c_reset}" "${CTX}" "${NODES}"
  exit 1
fi
ok "all nodes belong to ${CLUSTER} — safe to proceed"

##############################################################################
# 1. CRD installation
##############################################################################
section "1. CRDs installed and structurally valid"

for crd in proxmoxclusters.proxmox.multiprox.io proxmoxnodes.proxmox.multiprox.io; do
  if k get crd "${crd}" >/dev/null 2>&1; then
    ok "CRD present: ${crd}"
  else
    bad "CRD missing: ${crd}"
  fi
done

# The API server only reports Established once it has accepted the schema.
for crd in proxmoxclusters.proxmox.multiprox.io proxmoxnodes.proxmox.multiprox.io; do
  est="$(k get crd "${crd}" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null)"
  assert_eq "CRD Established: ${crd}" "True" "${est}"
done

# Short names must resolve, since the docs tell users to use them.
if k get pxc -A >/dev/null 2>&1; then ok "short name 'pxc' resolves"; else bad "short name 'pxc' does not resolve"; fi
if k get pxn -A >/dev/null 2>&1; then ok "short name 'pxn' resolves"; else bad "short name 'pxn' does not resolve"; fi

##############################################################################
# 2. Operator health
##############################################################################
section "2. Operator running"

wait_for 120 "operator deployment available" \
  k -n "${OPERATOR_NS}" wait --for=condition=Available deployment/multiprox-operator --timeout=5s

READY="$(k -n "${OPERATOR_NS}" get deploy multiprox-operator -o jsonpath='{.status.readyReplicas}')"
assert_eq "operator readyReplicas" "1" "${READY}"

# A crash-looping operator can still report Available briefly; check restarts.
RESTARTS="$(k -n "${OPERATOR_NS}" get pods -l app.kubernetes.io/name=multiprox-operator \
  -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)"
assert_eq "operator container restarts" "0" "${RESTARTS:-0}"

##############################################################################
# 3. Schema validation — defaults
##############################################################################
section "3. CRD defaulting"

k create namespace "${NS}" >/dev/null 2>&1 || true

cat <<'YAML' | k -n "${NS}" apply -f - >/dev/null
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: defaults-probe
spec:
  nodes: 1
  image: multiprox-node:e2e-stub
YAML

DEF_CLUSTERNAME="$(k -n "${NS}" get pxc defaults-probe -o jsonpath='{.spec.clusterName}')"
assert_eq "spec.clusterName defaulted" "multiprox" "${DEF_CLUSTERNAME}"

DEF_TRANSPORT="$(k -n "${NS}" get pxc defaults-probe -o jsonpath='{.spec.corosync.transport}')"
assert_eq "spec.corosync.transport defaulted" "knet" "${DEF_TRANSPORT}"

DEF_CRYPTO="$(k -n "${NS}" get pxc defaults-probe -o jsonpath='{.spec.corosync.crypto}')"
assert_eq "spec.corosync.crypto defaulted" "aes256" "${DEF_CRYPTO}"

DEF_CD_SIZE="$(k -n "${NS}" get pxc defaults-probe -o jsonpath='{.spec.storage.clusterData.size}')"
assert_eq "spec.storage.clusterData.size defaulted" "2Gi" "${DEF_CD_SIZE}"

DEF_VM_SIZE="$(k -n "${NS}" get pxc defaults-probe -o jsonpath='{.spec.storage.vmStorage.size}')"
assert_eq "spec.storage.vmStorage.size defaulted" "100Gi" "${DEF_VM_SIZE}"

k -n "${NS}" delete pxc defaults-probe --wait=false >/dev/null 2>&1

##############################################################################
# 4. Schema validation — rejections
##############################################################################
section "4. CRD validation rejects bad input"

reject() {
  local desc="$1" manifest="$2"
  local out
  if out="$(printf '%s' "${manifest}" | k -n "${NS}" apply -f - 2>&1)"; then
    bad "${desc} — was accepted but should have been rejected"
    printf '%s' "${manifest}" | k -n "${NS}" delete -f - >/dev/null 2>&1
  else
    ok "${desc}"
  fi
}

reject "rejects nodes: 0 (below minimum)" '
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata: {name: bad-nodes}
spec: {nodes: 0, image: x}'

reject "rejects invalid corosync.transport" '
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata: {name: bad-transport}
spec: {nodes: 3, image: x, corosync: {transport: sctp}}'

reject "rejects malformed storage size" '
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata: {name: bad-size}
spec: {nodes: 3, image: x, storage: {vmStorage: {size: "100 gigabytes"}}}'

reject "rejects clusterName with uppercase" '
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata: {name: bad-cname}
spec: {nodes: 3, image: x, clusterName: BadName}'

reject "rejects osdsPerNode above maximum (32)" '
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata: {name: bad-osds}
spec: {nodes: 3, image: x, ceph: {osdsPerNode: 99}}'

reject "rejects malformed ceph.network CIDR" '
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata: {name: bad-cidr}
spec: {nodes: 3, image: x, ceph: {network: "not-a-cidr"}}'

reject "rejects invalid pool content type" '
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata: {name: bad-content}
spec:
  nodes: 3
  image: x
  ceph:
    pools:
      - {name: p1, content: [snippets]}'

##############################################################################
# 5. Operator-level Ceph validation (ValidateCeph)
##############################################################################
section "5. Operator rejects unconvergeable Ceph specs"

# Pool size 5 with only 3 total OSDs can never reach active+clean. The CRD
# cannot express this (it spans two fields), so the controller must catch it.
cat <<'YAML' | k -n "${NS}" apply -f - >/dev/null
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: ceph-oversized-pool
spec:
  nodes: 3
  image: multiprox-node:e2e-stub
  ceph:
    osdsPerNode: 1
    monCount: 3
    pools:
      - name: too-big
        size: 5
YAML

wait_for_jsonpath 90 "oversized pool → phase Failed" \
  "pxc/ceph-oversized-pool" '{.status.phase}' "Failed"

MSG="$(k -n "${NS}" get pxc ceph-oversized-pool \
  -o jsonpath='{.status.conditions[?(@.type=="CephInitialized")].message}' 2>/dev/null)"
assert_contains "failure explains the OSD shortfall" "only provides 3 OSDs" "${MSG}"

REASON="$(k -n "${NS}" get pxc ceph-oversized-pool \
  -o jsonpath='{.status.conditions[?(@.type=="CephInitialized")].reason}' 2>/dev/null)"
assert_eq "condition reason" "InvalidCephSpec" "${REASON}"

# Even monCount cannot form a monitor quorum.
cat <<'YAML' | k -n "${NS}" apply -f - >/dev/null
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: ceph-even-mons
spec:
  nodes: 4
  image: multiprox-node:e2e-stub
  ceph:
    osdsPerNode: 1
    monCount: 4
YAML

wait_for_jsonpath 90 "even monCount → phase Failed" \
  "pxc/ceph-even-mons" '{.status.phase}' "Failed"

MSG2="$(k -n "${NS}" get pxc ceph-even-mons \
  -o jsonpath='{.status.conditions[?(@.type=="CephInitialized")].message}' 2>/dev/null)"
assert_contains "failure explains the quorum problem" "must be odd" "${MSG2}"

# A Failed spec must not leave a StatefulSet behind.
if k -n "${NS}" get statefulset ceph-oversized-pool >/dev/null 2>&1; then
  bad "invalid spec created a StatefulSet — validation should gate provisioning"
else
  ok "invalid spec created no StatefulSet"
fi

k -n "${NS}" delete pxc ceph-oversized-pool ceph-even-mons --wait=false >/dev/null 2>&1

##############################################################################
# 6. Full lifecycle — 3-node cluster, no Ceph
##############################################################################
section "6. Full lifecycle: 3-node cluster reaches Ready"

cat <<'YAML' | k -n "${NS}" apply -f - >/dev/null
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: mp
spec:
  nodes: 3
  clusterName: mp-test
  image: multiprox-node:e2e-stub
  imagePullPolicy: Never
  resources:
    requests:
      cpu: 50m
      memory: 96Mi
  storage:
    clusterData:
      size: 128Mi
    vmStorage:
      size: 256Mi
YAML
info "created ProxmoxCluster mp (3 nodes, no ceph)"

wait_for 60 "StatefulSet created" k -n "${NS}" get statefulset mp
wait_for 60 "headless Service created" k -n "${NS}" get svc mp-headless
wait_for 60 "UI Service created" k -n "${NS}" get svc mp-ui
wait_for 60 "ConfigMap created" k -n "${NS}" get configmap mp-config

# Headless Service must have no ClusterIP — that is what gives per-pod DNS.
CIP="$(k -n "${NS}" get svc mp-headless -o jsonpath='{.spec.clusterIP}')"
assert_eq "headless Service has no ClusterIP" "None" "${CIP}"

PNRA="$(k -n "${NS}" get svc mp-headless -o jsonpath='{.spec.publishNotReadyAddresses}')"
assert_eq "headless publishNotReadyAddresses" "true" "${PNRA}"

# ConfigMap must carry the scripts the operator execs.
CM_KEYS="$(k -n "${NS}" get configmap mp-config -o jsonpath='{.data}' | tr ',' '\n')"
for key in cluster-init.sh cluster-join.sh node-bootstrap.sh cluster-inventory.sh corosync.conf datacenter.cfg; do
  assert_contains "ConfigMap contains ${key}" "${key}" "${CM_KEYS}"
done

# Ceph scripts must be ABSENT when Ceph is not requested.
if printf '%s' "${CM_KEYS}" | grep -q 'ceph-init.sh'; then
  bad "ceph-init.sh present in ConfigMap despite spec.ceph being unset"
else
  ok "ceph scripts absent when spec.ceph unset"
fi

# Corosync config must name all 3 nodes with per-pod DNS.
COROSYNC="$(k -n "${NS}" get configmap mp-config -o jsonpath='{.data.corosync\.conf}')"
assert_contains "corosync.conf has cluster_name" "cluster_name: mp-test" "${COROSYNC}"
for i in 0 1 2; do
  assert_contains "corosync.conf has ring0_addr for mp-${i}" \
    "mp-${i}.mp-headless.${NS}.svc.cluster.local" "${COROSYNC}"
done

# The projection must be live DURING startup, not only once the cluster is
# healthy. Its whole value is answering "which node is stuck?" mid-transition,
# so a projection that only appears at phase=Ready is useless exactly when
# needed. Check before the cluster has converged.
EARLY_PHASE="$(k -n "${NS}" get pxc mp -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "${EARLY_PHASE}" != "Ready" ]; then
  if wait_for 90 "ProxmoxNodes exist before cluster is Ready (phase=${EARLY_PHASE})" \
      bash -c "[ \"\$(kubectl --context ${CTX} -n ${NS} get pxn --no-headers 2>/dev/null | wc -l | tr -d ' ')\" -ge 1 ]"; then
    EARLY_NODE_PHASE="$(k -n "${NS}" get pxn -o jsonpath='{.items[0].status.phase}' 2>/dev/null)"
    info "earliest projected node phase: ${EARLY_NODE_PHASE}"
  fi
else
  info "cluster converged too fast to sample the transitional projection"
fi

info "waiting for 3 stub pods to become Ready..."
wait_for 300 "all 3 pods Ready" \
  k -n "${NS}" wait --for=condition=Ready pod -l app.kubernetes.io/instance=mp --timeout=5s

wait_for_jsonpath 300 "cluster reaches phase Ready" "pxc/mp" '{.status.phase}' "Ready"

JOINED="$(k -n "${NS}" get pxc mp -o jsonpath='{.status.joinedNodes}')"
assert_eq "status.joinedNodes" "3" "${JOINED}"

for cond in StatefulSetReady ClusterInitialized AllNodesJoined QuorumHealthy; do
  st="$(k -n "${NS}" get pxc mp -o jsonpath="{.status.conditions[?(@.type==\"${cond}\")].status}")"
  assert_eq "condition ${cond}" "True" "${st}"
done

##############################################################################
# 7. Cluster formation actually happened inside the pods
##############################################################################
section "7. pvecm was really driven inside the pods"

# pod-0 should have been bootstrapped via cluster-init.sh (pvecm create).
if k -n "${NS}" exec mp-0 -c pve -- test -f /var/lib/pve-cluster/.clustered >/dev/null 2>&1; then
  ok "mp-0 is a cluster member (cluster-init.sh ran)"
else
  bad "mp-0 has no cluster marker — cluster-init.sh did not take effect"
fi

CNAME="$(k -n "${NS}" exec mp-0 -c pve -- cat /var/lib/pve-cluster/.cluster_name 2>/dev/null | tr -d '\r\n')"
assert_eq "pvecm create received the configured cluster name" "mp-test" "${CNAME}"

# pods 1 and 2 should have joined via cluster-join.sh (pvecm add).
for i in 1 2; do
  if k -n "${NS}" exec "mp-${i}" -c pve -- test -f /var/lib/pve-cluster/.clustered >/dev/null 2>&1; then
    ok "mp-${i} joined the cluster (cluster-join.sh ran)"
  else
    bad "mp-${i} did not join"
  fi
done

# The real node-bootstrap.sh must have written /etc/hosts with the routable pod
# IP, not loopback — Corosync binding to 127.0.0.1 is a classic failure.
HOSTS="$(k -n "${NS}" exec mp-1 -c pve -- cat /etc/hosts 2>/dev/null)"
POD1_IP="$(k -n "${NS}" get pod mp-1 -o jsonpath='{.status.podIP}')"
assert_contains "node-bootstrap.sh mapped FQDN to the pod IP (${POD1_IP})" \
  "${POD1_IP}" "${HOSTS}"
assert_contains "/etc/hosts contains the headless FQDN" \
  "mp-1.mp-headless.${NS}.svc.cluster.local" "${HOSTS}"

##############################################################################
# 8. ProxmoxNode projection
##############################################################################
section "8. ProxmoxNode projection"

wait_for 120 "ProxmoxNode objects created" \
  bash -c "[ \"\$(kubectl --context ${CTX} -n ${NS} get pxn -o name 2>/dev/null | wc -l | tr -d ' ')\" = 3 ]"

PXN_COUNT="$(k -n "${NS}" get pxn --no-headers 2>/dev/null | wc -l | tr -d ' ')"
assert_eq "one ProxmoxNode per PVE node" "3" "${PXN_COUNT}"

for i in 0 1 2; do
  # Immutable identity spec
  cref="$(k -n "${NS}" get pxn "mp-${i}" -o jsonpath='{.spec.clusterRef}' 2>/dev/null)"
  assert_eq "mp-${i} spec.clusterRef" "mp" "${cref}"
  ord="$(k -n "${NS}" get pxn "mp-${i}" -o jsonpath='{.spec.ordinal}' 2>/dev/null)"
  assert_eq "mp-${i} spec.ordinal" "${i}" "${ord}"
done

# Observed status must reflect reality gathered from pods + inventory.
wait_for_jsonpath 180 "mp-0 phase Ready" "pxn/mp-0" '{.status.phase}' "Ready"

for i in 0 1 2; do
  inc="$(k -n "${NS}" get pxn "mp-${i}" -o jsonpath='{.status.inCluster}' 2>/dev/null)"
  assert_eq "mp-${i} status.inCluster" "true" "${inc}"

  # Correlation with Kubernetes: the projection must name the real pod and host.
  pname="$(k -n "${NS}" get pxn "mp-${i}" -o jsonpath='{.status.podName}' 2>/dev/null)"
  assert_eq "mp-${i} status.podName" "mp-${i}" "${pname}"

  knode="$(k -n "${NS}" get pxn "mp-${i}" -o jsonpath='{.status.kubernetesNode}' 2>/dev/null)"
  realnode="$(k -n "${NS}" get pod "mp-${i}" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
  assert_eq "mp-${i} status.kubernetesNode matches pod" "${realnode}" "${knode}"

  oat="$(k -n "${NS}" get pxn "mp-${i}" -o jsonpath='{.status.observedAt}' 2>/dev/null)"
  if [ -n "${oat}" ]; then ok "mp-${i} status.observedAt set (${oat})"; else bad "mp-${i} observedAt empty"; fi
done

# The operator applies a default soft pod anti-affinity so PVE nodes spread
# across distinct Kubernetes hosts. With 3 nodes and 3 workers they should land
# one per host; if they pile onto a single host the anti-affinity is not working
# and a host failure would take out the whole Proxmox cluster at once.
HOSTS_USED="$(k -n "${NS}" get pxn -o jsonpath='{.items[*].status.kubernetesNode}' | tr ' ' '\n' | sort -u | grep -c .)"
assert_eq "PVE nodes spread across distinct hosts (anti-affinity)" "3" "${HOSTS_USED}"

# Ceph block must be absent when Ceph is disabled.
CEPHBLK="$(k -n "${NS}" get pxn mp-0 -o jsonpath='{.status.ceph}' 2>/dev/null)"
if [ -z "${CEPHBLK}" ]; then
  ok "status.ceph absent when spec.ceph unset"
else
  bad "status.ceph present despite Ceph being disabled: ${CEPHBLK}"
fi

# Owner reference must point at the cluster, so GC works.
OWNER="$(k -n "${NS}" get pxn mp-0 -o jsonpath='{.metadata.ownerReferences[0].name}' 2>/dev/null)"
assert_eq "ProxmoxNode ownerReference name" "mp" "${OWNER}"
OWNKIND="$(k -n "${NS}" get pxn mp-0 -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null)"
assert_eq "ProxmoxNode ownerReference kind" "ProxmoxCluster" "${OWNKIND}"

##############################################################################
# 9. ProxmoxNode immutability (CEL)
##############################################################################
section "9. ProxmoxNode spec immutability enforced by CEL"

assert_fails "rejects changing spec.ordinal" \
  k -n "${NS}" patch pxn mp-1 --type=merge -p '{"spec":{"ordinal":9}}'
assert_contains "rejection cites immutability" "immutable" "$(cat /tmp/multiprox_last_err.txt)"

assert_fails "rejects changing spec.clusterRef" \
  k -n "${NS}" patch pxn mp-1 --type=merge -p '{"spec":{"clusterRef":"somewhere-else"}}'

assert_fails "rejects changing spec.nodeName" \
  k -n "${NS}" patch pxn mp-1 --type=merge -p '{"spec":{"nodeName":"impostor"}}'

##############################################################################
# 10. No-op write suppression
##############################################################################
section "10. Projection does not rewrite on every reconcile"

RV_BEFORE="$(k -n "${NS}" get pxn mp-0 -o jsonpath='{.metadata.resourceVersion}')"
GEN_BEFORE="$(k -n "${NS}" get pxc mp -o jsonpath='{.metadata.resourceVersion}')"
info "mp-0 resourceVersion before: ${RV_BEFORE}"
info "sleeping 45s to span multiple reconciles..."
sleep 45
RV_AFTER="$(k -n "${NS}" get pxn mp-0 -o jsonpath='{.metadata.resourceVersion}')"
info "mp-0 resourceVersion after:  ${RV_AFTER}"

if [ "${RV_BEFORE}" = "${RV_AFTER}" ]; then
  ok "ProxmoxNode not rewritten while state was unchanged (no write amplification)"
else
  bad "ProxmoxNode resourceVersion changed ${RV_BEFORE} → ${RV_AFTER} with no state change"
fi

##############################################################################
# 11. Scale up, then scale down with pruning
##############################################################################
section "11. Scale up and down; projection prunes"

k -n "${NS}" patch pxc mp --type=merge -p '{"spec":{"nodes":4}}' >/dev/null
info "scaled to 4 nodes"

wait_for 240 "StatefulSet scaled to 4 replicas" \
  bash -c "[ \"\$(kubectl --context ${CTX} -n ${NS} get sts mp -o jsonpath='{.status.replicas}')\" = 4 ]"

wait_for 300 "4th ProxmoxNode appears" k -n "${NS}" get pxn mp-3

k -n "${NS}" patch pxc mp --type=merge -p '{"spec":{"nodes":2}}' >/dev/null
info "scaled down to 2 nodes"

wait_for 240 "ProxmoxNode mp-3 pruned" \
  bash -c "! kubectl --context ${CTX} -n ${NS} get pxn mp-3 >/dev/null 2>&1"
wait_for 120 "ProxmoxNode mp-2 pruned" \
  bash -c "! kubectl --context ${CTX} -n ${NS} get pxn mp-2 >/dev/null 2>&1"

REMAIN="$(k -n "${NS}" get pxn --no-headers 2>/dev/null | wc -l | tr -d ' ')"
assert_eq "2 ProxmoxNodes remain after scale-down" "2" "${REMAIN}"

# Surviving objects must be untouched.
for i in 0 1; do
  if k -n "${NS}" get pxn "mp-${i}" >/dev/null 2>&1; then
    ok "mp-${i} survived pruning"
  else
    bad "mp-${i} was pruned but should have been kept"
  fi
done

##############################################################################
# 12. Ceph: raw block PVC generation
##############################################################################
section "12. Ceph OSD disks are raw-block PVCs"

cat <<'YAML' | k -n "${NS}" apply -f - >/dev/null
apiVersion: proxmox.multiprox.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: cephy
spec:
  nodes: 3
  clusterName: cephy
  image: multiprox-node:e2e-stub
  imagePullPolicy: Never
  resources:
    requests:
      cpu: 50m
      memory: 96Mi
  storage:
    clusterData: {size: 128Mi}
    vmStorage: {size: 256Mi}
  ceph:
    osdsPerNode: 2
    osdSize: 512Mi
    monCount: 3
    mgrCount: 2
YAML
info "created cluster 'cephy' with osdsPerNode: 2"

wait_for 90 "StatefulSet created for ceph cluster" k -n "${NS}" get statefulset cephy

# volumeClaimTemplates must include both system PVCs and 2 OSD PVCs.
VCT_NAMES="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.volumeClaimTemplates[*].metadata.name}')"
assert_contains "volumeClaimTemplates include ceph-osd-0" "ceph-osd-0" "${VCT_NAMES}"
assert_contains "volumeClaimTemplates include ceph-osd-1" "ceph-osd-1" "${VCT_NAMES}"

# The critical assertion: OSD volumes must be Block mode, system volumes Filesystem.
OSD0_MODE="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.volumeClaimTemplates[?(@.metadata.name=="ceph-osd-0")].spec.volumeMode}')"
assert_eq "ceph-osd-0 volumeMode" "Block" "${OSD0_MODE}"
OSD1_MODE="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.volumeClaimTemplates[?(@.metadata.name=="ceph-osd-1")].spec.volumeMode}')"
assert_eq "ceph-osd-1 volumeMode" "Block" "${OSD1_MODE}"
CD_MODE="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.volumeClaimTemplates[?(@.metadata.name=="cluster-data")].spec.volumeMode}')"
assert_eq "cluster-data volumeMode" "Filesystem" "${CD_MODE}"

OSD_SIZE="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.volumeClaimTemplates[?(@.metadata.name=="ceph-osd-0")].spec.resources.requests.storage}')"
assert_eq "ceph-osd-0 size honours spec.ceph.osdSize" "512Mi" "${OSD_SIZE}"

# OSD disks must be attached as volumeDevices (raw), never volumeMounts.
VD_NAMES="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.template.spec.containers[0].volumeDevices[*].name}')"
assert_contains "container volumeDevices include ceph-osd-0" "ceph-osd-0" "${VD_NAMES}"
VD_PATHS="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.template.spec.containers[0].volumeDevices[*].devicePath}')"
assert_contains "devicePath /dev/ceph-osd-0" "/dev/ceph-osd-0" "${VD_PATHS}"
assert_contains "devicePath /dev/ceph-osd-1" "/dev/ceph-osd-1" "${VD_PATHS}"

VM_MOUNTS="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.template.spec.containers[0].volumeMounts[*].name}')"
if printf '%s' "${VM_MOUNTS}" | grep -q 'ceph-osd'; then
  bad "OSD volume appears in volumeMounts — it must be a raw volumeDevice"
else
  ok "OSD volumes absent from volumeMounts (correctly raw-block only)"
fi

# Ceph scripts must now be present in the ConfigMap.
CEPH_CM="$(k -n "${NS}" get configmap cephy-config -o jsonpath='{.data}' | tr ',' '\n')"
for key in ceph-init.sh ceph-mon.sh ceph-mgr.sh ceph-osd.sh ceph-pool.sh ceph-status.sh; do
  assert_contains "ConfigMap contains ${key}" "${key}" "${CEPH_CM}"
done

# Env wiring for the OSD scripts.
ENVS="$(k -n "${NS}" get sts cephy -o jsonpath='{.spec.template.spec.containers[0].env[*].name}')"
assert_contains "CEPH_OSDS_PER_NODE injected" "CEPH_OSDS_PER_NODE" "${ENVS}"
assert_contains "CEPH_OSD_DEVICE_PREFIX injected" "CEPH_OSD_DEVICE_PREFIX" "${ENVS}"

##############################################################################
# 13. Ceph on a filesystem-only StorageClass fails honestly
##############################################################################
section "13. Non-block StorageClass surfaces as a real failure"

# kind's default local-path provisioner cannot serve volumeMode: Block. The
# operator must NOT report a healthy Ceph cluster in that situation.
info "kind default StorageClass: $(k get sc -o jsonpath='{.items[?(@.metadata.annotations.storageclass\.kubernetes\.io/default-class=="true")].metadata.name}')"

sleep 20
OSD_PVC_PHASE="$(k -n "${NS}" get pvc ceph-osd-0-cephy-0 -o jsonpath='{.status.phase}' 2>/dev/null)"
info "OSD PVC ceph-osd-0-cephy-0 phase: ${OSD_PVC_PHASE:-<not created>}"

CEPHY_PHASE="$(k -n "${NS}" get pxc cephy -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "${CEPHY_PHASE}" = "Ready" ]; then
  bad "cluster reported Ready despite OSDs being impossible (false positive!)"
else
  ok "cluster did NOT report Ready without working OSDs (phase=${CEPHY_PHASE})"
fi

CEPH_HEALTHY="$(k -n "${NS}" get pxc cephy \
  -o jsonpath='{.status.conditions[?(@.type=="CephHealthy")].status}' 2>/dev/null)"
if [ "${CEPH_HEALTHY}" = "True" ]; then
  bad "CephHealthy=True with no OSDs (false positive!)"
else
  ok "CephHealthy is not True without OSDs (=${CEPH_HEALTHY:-<unset>})"
fi

##############################################################################
# 14. Deletion and garbage collection
##############################################################################
section "14. Deletion cascades"

k -n "${NS}" delete pxc cephy --timeout=90s >/dev/null 2>&1
wait_for 120 "cephy StatefulSet garbage-collected" \
  bash -c "! kubectl --context ${CTX} -n ${NS} get sts cephy >/dev/null 2>&1"

k -n "${NS}" delete pxc mp --timeout=120s >/dev/null 2>&1
wait_for 150 "mp StatefulSet garbage-collected" \
  bash -c "! kubectl --context ${CTX} -n ${NS} get sts mp >/dev/null 2>&1"
wait_for 120 "mp ProxmoxNodes garbage-collected via ownerRef" \
  bash -c "[ \"\$(kubectl --context ${CTX} -n ${NS} get pxn --no-headers 2>/dev/null | wc -l | tr -d ' ')\" = 0 ]"

# The finalizer must be removed so the CR actually disappears.
wait_for 90 "ProxmoxCluster finalizer released" \
  bash -c "! kubectl --context ${CTX} -n ${NS} get pxc mp >/dev/null 2>&1"

##############################################################################
# Summary
##############################################################################
section "Summary"

printf '  %s%d passed%s, %s%d failed%s\n' \
  "${c_grn}" "${PASS}" "${c_reset}" \
  "$( [ "${FAIL}" -gt 0 ] && printf '%s' "${c_red}" || printf '%s' "${c_grn}")" \
  "${FAIL}" "${c_reset}"

if [ "${FAIL}" -gt 0 ]; then
  printf '\n  Failures:\n'
  for f in "${FAILURES[@]}"; do printf '    - %s\n' "${f}"; done
fi

printf '\n  %sScope reminder:%s the PVE node image is a stub. This suite validates the\n' "${c_yel}" "${c_reset}"
printf '  operator only — Proxmox clustering and Ceph themselves require a real\n'
printf '  amd64 cluster with a block-capable StorageClass.\n'

if [ "${KEEP}" -eq 1 ]; then
  printf '\n  Cluster kept. Inspect with:\n'
  printf '    kubectl --context %s -n %s get pxc,pxn,sts,pods\n' "${CTX}" "${NS}"
  printf '  Tear down with:\n    kind delete cluster --name %s\n' "${CLUSTER}"
else
  printf '\n  Cleaning up namespace %s (cluster %s left running; delete with: kind delete cluster --name %s)\n' \
    "${NS}" "${CLUSTER}" "${CLUSTER}"
  k delete namespace "${NS}" --wait=false >/dev/null 2>&1
fi

[ "${FAIL}" -eq 0 ]
