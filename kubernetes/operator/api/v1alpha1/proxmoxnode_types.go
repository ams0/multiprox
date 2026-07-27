package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// ProxmoxNode
//
// ProxmoxNode is an OBSERVED-STATE mirror of one Proxmox VE node, written
// exclusively by the operator and intended to be read — never authored — by
// users and external tooling.
//
// Contract, and the reason this object is safe to have:
//
//	• The operator WRITES these objects. It never READS them back to make
//	  reconciliation decisions. All authoritative desired state lives in
//	  ProxmoxCluster.spec; all authoritative observed state is re-derived each
//	  reconcile from the Kubernetes API (pods) and from Proxmox itself
//	  (pvecm / ceph, via exec). That keeps this from becoming a second source
//	  of truth that could drift or be hand-edited into causing real damage.
//
//	• Because it is a projection, it can be stale — most obviously when the
//	  operator is down. status.observedAt records when the projection was last
//	  refreshed so consumers can judge freshness themselves. Treat a
//	  ProxmoxNode whose observedAt is old as unknown, not as healthy.
//
//	• Editing a ProxmoxNode has no effect on the cluster; the next reconcile
//	  overwrites it.
//
// Why a separate object rather than a status.nodes[] array on ProxmoxCluster:
// printer columns cannot iterate an array, per-node Events need a per-node
// involvedObject, and a large cluster's nested array would make every parent
// status patch rewrite an ever-growing object.
// ─────────────────────────────────────────────────────────────────────────────

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef`
// +kubebuilder:printcolumn:name="Ordinal",type=integer,JSONPath=`.spec.ordinal`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="In-Cluster",type=boolean,JSONPath=`.status.inCluster`
// +kubebuilder:printcolumn:name="Mon",type=boolean,JSONPath=`.status.ceph.monitor`
// +kubebuilder:printcolumn:name="Mgr",type=boolean,JSONPath=`.status.ceph.manager`
// +kubebuilder:printcolumn:name="OSDs",type=string,JSONPath=`.status.ceph.osdSummary`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.status.kubernetesNode`,priority=1
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.status.observedAt`
// +kubebuilder:resource:shortName=pxn;pvenode

// ProxmoxNode is a read-only projection of one Proxmox VE node's observed state.
// It is created and maintained by the multiprox operator; user edits are
// overwritten on the next reconcile.
type ProxmoxNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxmoxNodeSpec   `json:"spec,omitempty"`
	Status ProxmoxNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProxmoxNodeList contains a list of ProxmoxNode.
type ProxmoxNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProxmoxNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProxmoxNode{}, &ProxmoxNodeList{})
}

// ─────────────────────────────────────────────────────────────────────────────
// Spec — immutable identity only
// ─────────────────────────────────────────────────────────────────────────────

// ProxmoxNodeSpec carries only the node's stable identity, declared by the
// owning ProxmoxCluster. There are no tunables here: everything configurable
// lives on ProxmoxCluster.spec. The fields are immutable because they identify
// which node this object projects — changing them would silently repoint the
// object at a different node.
type ProxmoxNodeSpec struct {
	// ClusterRef is the name of the owning ProxmoxCluster in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is immutable"
	ClusterRef string `json:"clusterRef"`

	// NodeName is the Proxmox node name, which equals the StatefulSet pod name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="nodeName is immutable"
	NodeName string `json:"nodeName"`

	// Ordinal is the StatefulSet ordinal index of this node (0-based).
	// Node 0 is the cluster bootstrap leader.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ordinal is immutable"
	Ordinal int32 `json:"ordinal"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Status — everything observed
// ─────────────────────────────────────────────────────────────────────────────

// NodePhase enumerates the observed lifecycle states of a single PVE node.
type NodePhase string

const (
	NodePhasePending  NodePhase = "Pending"  // pod not yet Running
	NodePhaseStarting NodePhase = "Starting" // pod Running, PVE services still coming up
	NodePhaseJoining  NodePhase = "Joining"  // PVE up, not yet a Corosync member
	NodePhaseReady    NodePhase = "Ready"    // Corosync member and online
	NodePhaseOffline  NodePhase = "Offline"  // known to the cluster but not currently reachable
	NodePhaseFailed   NodePhase = "Failed"   // pod failed or terminated unexpectedly
)

// ProxmoxNode condition types.
const (
	NodeConditionPodReady      = "PodReady"      // Kubernetes pod is Running and Ready
	NodeConditionClusterMember = "ClusterMember" // appears in `pvecm nodes`
	NodeConditionCephOSDsUp    = "CephOSDsUp"    // all OSDs on this node are up and in
)

// ProxmoxNodeStatus is the observed state of one Proxmox VE node.
// Every field is re-derived each reconcile; nothing here is authoritative.
type ProxmoxNodeStatus struct {
	// Phase is the observed lifecycle phase of this node.
	// +kubebuilder:validation:Enum=Pending;Starting;Joining;Ready;Offline;Failed
	Phase NodePhase `json:"phase,omitempty"`

	// Conditions carries detailed observations about this node.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// InCluster is true when this node appears as a member in `pvecm nodes`.
	InCluster bool `json:"inCluster,omitempty"`

	// Online is true when the Proxmox cluster currently considers the node reachable.
	Online bool `json:"online,omitempty"`

	// CorosyncNodeID is the Corosync nodeid assigned to this node (1-based).
	CorosyncNodeID int32 `json:"corosyncNodeID,omitempty"`

	// PodName is the Kubernetes pod backing this node.
	PodName string `json:"podName,omitempty"`

	// PodIP is the pod's current address — also its Corosync ring0 address.
	PodIP string `json:"podIP,omitempty"`

	// KubernetesNode is the cluster node the pod is scheduled on. Useful for
	// confirming that PVE nodes are spread across distinct failure domains.
	KubernetesNode string `json:"kubernetesNode,omitempty"`

	// Ceph reports this node's role in the Ceph cluster. Nil when Ceph is
	// not enabled on the owning ProxmoxCluster.
	Ceph *ProxmoxNodeCephStatus `json:"ceph,omitempty"`

	// ObservedAt is when the operator last refreshed this projection.
	//
	// This object is a cache of state owned elsewhere. If ObservedAt is old,
	// the operator has not run recently and every other field should be
	// treated as unknown rather than current.
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// ProxmoxNodeCephStatus describes one node's Ceph daemons.
type ProxmoxNodeCephStatus struct {
	// Monitor is true when a Ceph Monitor runs on this node.
	Monitor bool `json:"monitor,omitempty"`

	// Manager is true when a Ceph Manager runs on this node.
	Manager bool `json:"manager,omitempty"`

	// OSDsUp is the number of OSDs on this node that are up and in.
	OSDsUp int32 `json:"osdsUp,omitempty"`

	// OSDsTotal is the number of OSDs Ceph knows about on this node.
	OSDsTotal int32 `json:"osdsTotal,omitempty"`

	// OSDSummary is a "up/total" string for printer columns.
	OSDSummary string `json:"osdSummary,omitempty"`

	// Devices lists the block devices attached to this node for OSD use, in
	// the order they were requested. Useful for spotting a PVC that was
	// provisioned with a filesystem StorageClass and so never became a device.
	Devices []string `json:"devices,omitempty"`
}
