package resources

// ProxmoxNode projection.
//
// These objects are a read-only view of per-node reality, written by the
// operator and never read back by it. See the doc comment on the ProxmoxNode
// type for the full contract; the short version is that the authoritative
// state stays in ProxmoxCluster.spec (desired) plus the live Kubernetes and
// Proxmox APIs (observed), and this is only a projection for humans and
// external tooling.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// NodeInventory is one node's worth of facts parsed out of cluster-inventory.sh.
type NodeInventory struct {
	Name      string
	Online    bool
	NodeID    int32
	Monitor   bool
	Manager   bool
	OSDsUp    int32
	OSDsTotal int32
}

// ParseInventory turns the output of cluster-inventory.sh into a map keyed by
// Proxmox node name. Malformed lines are skipped rather than failing the whole
// projection — a partial view is more useful than none, and this data is
// advisory by construction.
func ParseInventory(out string) map[string]NodeInventory {
	inv := map[string]NodeInventory{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "node=") {
			continue
		}

		var n NodeInventory
		for _, field := range strings.Fields(line) {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch k {
			case "node":
				n.Name = v
			case "online":
				n.Online = v == "1"
			case "nodeid":
				n.NodeID = atoi32(v)
			case "mon":
				n.Monitor = v == "1"
			case "mgr":
				n.Manager = v == "1"
			case "osds_up":
				n.OSDsUp = atoi32(v)
			case "osds_total":
				n.OSDsTotal = atoi32(v)
			}
		}

		if n.Name != "" {
			inv[n.Name] = n
		}
	}

	return inv
}

// NodeObjectName returns the ProxmoxNode object name for a given ordinal.
// It matches the pod name so the two are trivially correlatable.
func NodeObjectName(cluster *pxv1.ProxmoxCluster, ordinal int32) string {
	return fmt.Sprintf("%s-%d", cluster.Name, ordinal)
}

// ProxmoxNodeFor builds the desired ProxmoxNode object for one ordinal.
//
// pod may be nil when the pod does not exist yet (cluster scaling up, or the
// StatefulSet has not created it). inv may be absent from the map when the node
// is not yet known to Proxmox. Both cases are normal and produce a valid object
// in an early phase rather than an error.
func ProxmoxNodeFor(
	cluster *pxv1.ProxmoxCluster,
	ordinal int32,
	pod *corev1.Pod,
	inv *NodeInventory,
	now metav1.Time,
) *pxv1.ProxmoxNode {
	name := NodeObjectName(cluster, ordinal)

	node := &pxv1.ProxmoxNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "proxmox-node",
				"app.kubernetes.io/instance":   cluster.Name,
				"app.kubernetes.io/managed-by": "multiprox-operator",
				"multiprox.io/cluster":         cluster.Name,
				"multiprox.io/ordinal":         strconv.FormatInt(int64(ordinal), 10),
			},
			Annotations: map[string]string{
				// Make the contract discoverable from the object itself.
				"multiprox.io/projection": "observed-state; written by the operator, " +
					"edits are overwritten on the next reconcile",
			},
		},
		Spec: pxv1.ProxmoxNodeSpec{
			ClusterRef: cluster.Name,
			NodeName:   name,
			Ordinal:    ordinal,
		},
	}

	st := pxv1.ProxmoxNodeStatus{
		ObservedAt: &now,
	}

	// ── Kubernetes-side facts (free; from the cached informer) ───────────────
	podReady := false
	if pod != nil {
		st.PodName = pod.Name
		st.PodIP = pod.Status.PodIP
		st.KubernetesNode = pod.Spec.NodeName

		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				podReady = true
			}
		}
	}

	// ── Proxmox-side facts ──────────────────────────────────────────────────
	if inv != nil {
		st.InCluster = inv.NodeID > 0
		st.Online = inv.Online
		st.CorosyncNodeID = inv.NodeID
	}

	// ── Ceph roles ──────────────────────────────────────────────────────────
	if cluster.Spec.Ceph != nil {
		cs := &pxv1.ProxmoxNodeCephStatus{}
		if inv != nil {
			cs.Monitor = inv.Monitor
			cs.Manager = inv.Manager
			cs.OSDsUp = inv.OSDsUp
			cs.OSDsTotal = inv.OSDsTotal
		}
		// Expected devices come from the spec, so they are reported even before
		// Ceph knows about them. A device listed here with OSDsTotal below the
		// device count usually means the OSD PVC was not provisioned as a
		// raw block volume.
		perNode := maxInt32(cluster.Spec.Ceph.OSDsPerNode, 1)
		for i := int32(0); i < perNode; i++ {
			cs.Devices = append(cs.Devices, OSDDevicePath(cluster, i))
		}
		cs.OSDSummary = fmt.Sprintf("%d/%d", cs.OSDsUp, perNode)
		st.Ceph = cs
	}

	// ── Phase ───────────────────────────────────────────────────────────────
	st.Phase = nodePhase(pod, podReady, st.InCluster, st.Online)

	// ── Conditions ──────────────────────────────────────────────────────────
	setNodeCondition(&st, pxv1.NodeConditionPodReady, podReady,
		"PodReady", "PodNotReady", podReadyMessage(pod, podReady), now)

	setNodeCondition(&st, pxv1.NodeConditionClusterMember, st.InCluster,
		"IsClusterMember", "NotClusterMember",
		clusterMemberMessage(st.InCluster, st.Online, st.CorosyncNodeID), now)

	if cluster.Spec.Ceph != nil {
		expected := maxInt32(cluster.Spec.Ceph.OSDsPerNode, 1)
		osdsUp := int32(0)
		if st.Ceph != nil {
			osdsUp = st.Ceph.OSDsUp
		}
		allUp := osdsUp >= expected
		setNodeCondition(&st, pxv1.NodeConditionCephOSDsUp, allUp,
			"AllOSDsUp", "OSDsNotUp",
			fmt.Sprintf("%d/%d OSDs up on this node", osdsUp, expected), now)
	}

	node.Status = st
	return node
}

// nodePhase derives the observed phase from pod and Proxmox state.
func nodePhase(pod *corev1.Pod, podReady, inCluster, online bool) pxv1.NodePhase {
	if pod == nil {
		return pxv1.NodePhasePending
	}

	switch pod.Status.Phase {
	case corev1.PodPending:
		return pxv1.NodePhasePending
	case corev1.PodFailed:
		return pxv1.NodePhaseFailed
	case corev1.PodSucceeded:
		// A PVE node pod exiting cleanly is still a failure of intent: the
		// node should run indefinitely.
		return pxv1.NodePhaseFailed
	}

	if !podReady {
		return pxv1.NodePhaseStarting
	}
	if !inCluster {
		return pxv1.NodePhaseJoining
	}
	if !online {
		return pxv1.NodePhaseOffline
	}
	return pxv1.NodePhaseReady
}

func podReadyMessage(pod *corev1.Pod, ready bool) string {
	if pod == nil {
		return "pod does not exist yet"
	}
	if ready {
		return fmt.Sprintf("pod %s is Running and Ready on %s", pod.Name, pod.Spec.NodeName)
	}
	return fmt.Sprintf("pod %s is %s", pod.Name, pod.Status.Phase)
}

func clusterMemberMessage(inCluster, online bool, nodeID int32) string {
	if !inCluster {
		return "node does not appear in `pvecm nodes`"
	}
	if !online {
		return fmt.Sprintf("node is a member (nodeid %d) but not currently reachable", nodeID)
	}
	return fmt.Sprintf("node is an online Corosync member (nodeid %d)", nodeID)
}

func setNodeCondition(
	st *pxv1.ProxmoxNodeStatus,
	condType string,
	ok bool,
	trueReason, falseReason, message string,
	now metav1.Time,
) {
	status := metav1.ConditionTrue
	reason := trueReason
	if !ok {
		status = metav1.ConditionFalse
		reason = falseReason
	}

	// LastTransitionTime is provisional here. ProxmoxNodeFor builds a fresh
	// status each reconcile, so there is no prior condition in scope to compare
	// against; MergeNodeStatus fixes the timestamp up against the stored object.
	st.Conditions = append(st.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// MergeNodeStatus reconciles a freshly-built status against the stored one and
// reports whether a write is actually warranted.
//
// Two things have to be true at once, and they pull against each other:
//
//   - LastTransitionTime must mean "when this condition last flipped", so it
//     has to be carried over from the stored object whenever Status is
//     unchanged. A freshly-built status cannot know this on its own.
//
//   - The projection must not be rewritten on every reconcile. Both
//     ObservedAt and the provisional transition timestamps change every pass,
//     so a naive DeepEqual would always report a difference and patch every
//     node every loop — multiplying write load by the node count for nothing.
//
// The merge therefore returns the status to store (with corrected timestamps)
// plus a changed flag computed while ignoring timestamps entirely.
//
// Consequence worth knowing: ObservedAt only advances when something else also
// changed. Read it as "state last confirmed to differ at", not "last polled
// at" — a healthy, unchanging node keeps an old ObservedAt by design. For
// liveness of the operator itself, watch the ProxmoxCluster or the operator's
// own metrics instead.
func MergeNodeStatus(existing, desired pxv1.ProxmoxNodeStatus) (merged pxv1.ProxmoxNodeStatus, changed bool) {
	merged = *desired.DeepCopy()

	prev := make(map[string]metav1.Condition, len(existing.Conditions))
	for _, c := range existing.Conditions {
		prev[c.Type] = c
	}

	for i := range merged.Conditions {
		if old, ok := prev[merged.Conditions[i].Type]; ok &&
			old.Status == merged.Conditions[i].Status {
			merged.Conditions[i].LastTransitionTime = old.LastTransitionTime
		}
	}

	changed = !nodeStatusEquivalent(existing, merged)

	// Only let ObservedAt advance when there is a real change to record;
	// otherwise keep the stored value so no write is triggered.
	if !changed {
		merged.ObservedAt = existing.ObservedAt
	}

	return merged, changed
}

// nodeStatusEquivalent compares two statuses ignoring every timestamp, so that
// clock movement alone never counts as a change.
func nodeStatusEquivalent(a, b pxv1.ProxmoxNodeStatus) bool {
	sa, sb := *a.DeepCopy(), *b.DeepCopy()

	sa.ObservedAt, sb.ObservedAt = nil, nil
	stripConditionTimes(sa.Conditions)
	stripConditionTimes(sb.Conditions)
	sortConditions(sa.Conditions)
	sortConditions(sb.Conditions)

	return equality.Semantic.DeepEqual(sa, sb)
}

func stripConditionTimes(cs []metav1.Condition) {
	for i := range cs {
		cs[i].LastTransitionTime = metav1.Time{}
	}
}

func sortConditions(cs []metav1.Condition) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Type < cs[j].Type })
}

func atoi32(s string) int32 {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return int32(n)
}
