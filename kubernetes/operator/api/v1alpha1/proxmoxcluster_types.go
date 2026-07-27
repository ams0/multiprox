package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// ProxmoxCluster
// ─────────────────────────────────────────────────────────────────────────────

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.nodes,statuspath=.status.joinedNodes
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.spec.nodes`
// +kubebuilder:printcolumn:name="Joined",type=integer,JSONPath=`.status.joinedNodes`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ceph",type=string,JSONPath=`.status.ceph.health`
// +kubebuilder:printcolumn:name="OSDs",type=string,JSONPath=`.status.ceph.osdSummary`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=pxc;proxmox

// ProxmoxCluster is the Schema for the proxmoxclusters API.
// It describes a multi-node Proxmox VE cluster deployed as a Kubernetes
// StatefulSet, optionally with a hyper-converged Ceph cluster configured
// natively from inside Proxmox via pveceph, backed by raw-block PVCs.
type ProxmoxCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxmoxClusterSpec   `json:"spec,omitempty"`
	Status ProxmoxClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProxmoxClusterList contains a list of ProxmoxCluster.
type ProxmoxClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProxmoxCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProxmoxCluster{}, &ProxmoxClusterList{})
}

// ─────────────────────────────────────────────────────────────────────────────
// Spec
// ─────────────────────────────────────────────────────────────────────────────

// ProxmoxClusterSpec defines the desired state of a ProxmoxCluster.
type ProxmoxClusterSpec struct {
	// Nodes is the total number of Proxmox VE nodes. Minimum 1; 3+ recommended for quorum.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Nodes int32 `json:"nodes"`

	// ClusterName is the Proxmox cluster name passed to `pvecm create`.
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9\-]{0,14}$`
	// +kubebuilder:default="multiprox"
	ClusterName string `json:"clusterName,omitempty"`

	// Image is the container image for each PVE node.
	// Must include the Proxmox Ceph packages when spec.ceph is set
	// (build docker/Dockerfile with --build-arg ENABLE_CEPH=1).
	// +kubebuilder:default="ghcr.io/feynman/multiprox-node:latest"
	Image string `json:"image"`

	// ImagePullPolicy for the PVE node image.
	// +kubebuilder:default=IfNotPresent
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecrets for private registries.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Resources applied to each PVE node container.
	// Recommended: at least 2 CPU and 4 Gi memory per node, plus ~1 CPU
	// and 4 Gi per OSD when Ceph is enabled.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Storage configuration for each node's system PersistentVolumeClaims.
	// +kubebuilder:default={}
	Storage StorageSpec `json:"storage,omitempty"`

	// Corosync networking and encryption options.
	// +kubebuilder:default={}
	Corosync CorosyncSpec `json:"corosync,omitempty"`

	// Ceph enables a hyper-converged Ceph cluster managed by Proxmox itself.
	// The operator attaches raw-block PVCs to each PVE pod as OSD devices,
	// then drives `pveceph` inside the cluster to create mons, mgrs, OSDs
	// and pools. No external storage operator (Rook) is involved.
	Ceph *CephSpec `json:"ceph,omitempty"`

	// NodeSelector constrains which Kubernetes nodes the PVE pods may land on.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations for the PVE node pods.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity rules for the PVE node pods.
	// The operator sets a default anti-affinity rule so PVE nodes are spread
	// across Kubernetes nodes; this field overrides that default.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// StorageSpec defines PVC templates for each PVE node's system volumes.
type StorageSpec struct {
	// ClusterData backs /var/lib/pve-cluster (Corosync DB, auth keys, pmxcfs).
	// Small volume; should be on fast storage.
	// +kubebuilder:default={size: "2Gi"}
	ClusterData PVCSpec `json:"clusterData,omitempty"`

	// VMStorage backs /var/lib/vz (ISO images, CT templates, local VM disks,
	// backups). When Ceph is enabled, VM disks normally live on Ceph RBD
	// instead, so this can stay small.
	// +kubebuilder:default={size: "100Gi"}
	VMStorage PVCSpec `json:"vmStorage,omitempty"`
}

// PVCSpec defines the StorageClass and size for a filesystem PersistentVolumeClaim.
type PVCSpec struct {
	// StorageClass to use. Omit to use the cluster default.
	StorageClass *string `json:"storageClass,omitempty"`

	// Size of the volume, e.g. "50Gi".
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E)?$`
	Size string `json:"size"`
}

// Quantity converts the size string to a resource.Quantity.
func (p PVCSpec) Quantity() resource.Quantity {
	return resource.MustParse(p.Size)
}

// CorosyncSpec controls Corosync cluster transport and encryption.
type CorosyncSpec struct {
	// Transport protocol for Corosync.
	// knet (default) supports multiple redundant rings and encryption.
	// udpu is simpler but unicast-only and deprecated in PVE 8.
	// +kubebuilder:validation:Enum=knet;udpu
	// +kubebuilder:default=knet
	Transport string `json:"transport,omitempty"`

	// Crypto is the encryption cipher for Corosync traffic.
	// +kubebuilder:validation:Enum=aes256;aes128;none
	// +kubebuilder:default=aes256
	Crypto string `json:"crypto,omitempty"`

	// HashAlgorithm for Corosync message authentication.
	// +kubebuilder:validation:Enum=sha256;sha384;sha512;none
	// +kubebuilder:default=sha256
	Hash string `json:"hash,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Ceph (native pveceph, backed by raw-block PVCs)
// ─────────────────────────────────────────────────────────────────────────────

// CephSpec configures a hyper-converged Ceph cluster driven by `pveceph`
// from inside the Proxmox cluster.
//
// Disk model: each PVE pod receives OSDsPerNode raw-block PVCs via the
// StatefulSet's volumeClaimTemplates. They appear inside the container as
// block devices at <OSDDevicePrefix><index>, e.g. /dev/ceph-osd-0. The
// operator then runs `pveceph osd create <device>` for each one.
//
// Total OSD count = spec.nodes × spec.ceph.osdsPerNode.
type CephSpec struct {
	// OSDsPerNode is the number of raw-block PVCs attached to each PVE node
	// as Ceph OSD devices. One OSD daemon is created per device.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	// +kubebuilder:default=1
	OSDsPerNode int32 `json:"osdsPerNode,omitempty"`

	// OSDSize is the capacity requested for each OSD block PVC.
	// Ceph requires at least ~5Gi per OSD; 100Gi+ is typical.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E)?$`
	// +kubebuilder:default="100Gi"
	OSDSize string `json:"osdSize,omitempty"`

	// OSDStorageClass provisions the OSD PVCs. It MUST support
	// volumeMode: Block (raw block volumes) — e.g. local-path-provisioner
	// with block support, OpenEBS, TopoLVM, Longhorn, or a CSI driver
	// exposing raw devices. Omit to use the cluster default StorageClass,
	// which may not support block mode.
	OSDStorageClass *string `json:"osdStorageClass,omitempty"`

	// OSDDevicePrefix is the in-container device path prefix for OSD block
	// devices. The device index is appended, giving /dev/ceph-osd-0,
	// /dev/ceph-osd-1, and so on.
	// +kubebuilder:validation:Pattern=`^/dev/[a-zA-Z0-9._-]+$`
	// +kubebuilder:default="/dev/ceph-osd-"
	OSDDevicePrefix string `json:"osdDevicePrefix,omitempty"`

	// Network is the Ceph public network CIDR passed to `pveceph init
	// --network`. It must cover every PVE pod IP. Leave empty to let the
	// bootstrap script derive a /16 from the pod's own address, which
	// matches most CNI pod CIDRs.
	// +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	Network string `json:"network,omitempty"`

	// ClusterNetwork optionally separates Ceph replication traffic from
	// client traffic (`pveceph init --cluster-network`). Requires a second
	// pod interface (e.g. Multus); leave empty to share one network.
	// +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	ClusterNetwork string `json:"clusterNetwork,omitempty"`

	// MonCount is how many PVE nodes host a Ceph Monitor. Must be odd and
	// no greater than spec.nodes. 3 is standard.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	MonCount int32 `json:"monCount,omitempty"`

	// MgrCount is how many PVE nodes host a Ceph Manager. One is active,
	// the rest stand by. Must be no greater than spec.nodes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	MgrCount int32 `json:"mgrCount,omitempty"`

	// Pools are the Ceph RBD pools to create and register as Proxmox
	// storage backends. If empty, the operator creates a single pool named
	// after DefaultPoolName holding VM disks and container volumes.
	// +listType=map
	// +listMapKey=name
	Pools []CephPoolSpec `json:"pools,omitempty"`
}

// DefaultPoolName is used when spec.ceph.pools is empty.
const DefaultPoolName = "ceph-vm"

// CephPoolSpec describes one Ceph pool and how Proxmox should consume it.
type CephPoolSpec struct {
	// Name of the Ceph pool. Also used as the Proxmox storage ID.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._-]{0,62}$`
	Name string `json:"name"`

	// Size is the number of replicas (`pveceph pool create --size`).
	// Must not exceed the total OSD count.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Size int32 `json:"size,omitempty"`

	// MinSize is the minimum replicas required for the pool to accept
	// writes (`--min_size`). Typically Size-1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	MinSize int32 `json:"minSize,omitempty"`

	// PGNum is the initial placement-group count (`--pg_num`). Leave zero
	// to let Ceph's PG autoscaler pick a value, which is recommended.
	// +kubebuilder:validation:Minimum=0
	PGNum int32 `json:"pgNum,omitempty"`

	// Content lists the Proxmox content types this storage accepts.
	// "images" = VM disks, "rootdir" = LXC container volumes.
	// +kubebuilder:validation:items:Enum=images;rootdir
	// +kubebuilder:default={"images","rootdir"}
	Content []string `json:"content,omitempty"`

	// AddAsStorage registers the pool in Proxmox via `pvesm add rbd`.
	// Set false to create the pool without exposing it as PVE storage.
	// +kubebuilder:default=true
	AddAsStorage *bool `json:"addAsStorage,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Status
// ─────────────────────────────────────────────────────────────────────────────

// Phase enumerates the lifecycle states of a ProxmoxCluster.
type Phase string

const (
	PhasePending         Phase = "Pending"         // waiting for StatefulSet pods to be Ready
	PhaseInitializing    Phase = "Initializing"    // running pvecm create on node-0
	PhaseJoining         Phase = "Joining"         // running pvecm add on remaining nodes
	PhaseConfiguringCeph Phase = "ConfiguringCeph" // driving pveceph init/mon/mgr/osd/pool
	PhaseReady           Phase = "Ready"           // all nodes joined, quorum healthy, Ceph usable
	PhaseDegraded        Phase = "Degraded"        // quorum intact but ≥1 node or OSD unhealthy
	PhaseFailed          Phase = "Failed"          // quorum lost or unrecoverable error
)

// Condition type constants.
const (
	ConditionStatefulSetReady   = "StatefulSetReady"
	ConditionClusterInitialized = "ClusterInitialized"
	ConditionAllNodesJoined     = "AllNodesJoined"
	ConditionQuorumHealthy      = "QuorumHealthy"
	ConditionCephInitialized    = "CephInitialized"
	ConditionCephMonsReady      = "CephMonsReady"
	ConditionCephOSDsReady      = "CephOSDsReady"
	ConditionCephPoolsReady     = "CephPoolsReady"
	ConditionCephHealthy        = "CephHealthy"
)

// ProxmoxClusterStatus defines the observed state of a ProxmoxCluster.
type ProxmoxClusterStatus struct {
	// Phase is the current lifecycle phase of the cluster.
	// +kubebuilder:validation:Enum=Pending;Initializing;Joining;ConfiguringCeph;Ready;Degraded;Failed
	Phase Phase `json:"phase,omitempty"`

	// Conditions contains detailed status for each aspect of the cluster.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// JoinedNodes is the number of nodes that have successfully joined the cluster.
	JoinedNodes int32 `json:"joinedNodes,omitempty"`

	// ClusterID is the Corosync-assigned cluster ID (visible in `pvecm status`).
	ClusterID string `json:"clusterID,omitempty"`

	// CorosyncConfigVersion is incremented each time a node joins or leaves.
	CorosyncConfigVersion int64 `json:"corosyncConfigVersion,omitempty"`

	// Ceph reports the observed state of the pveceph-managed Ceph cluster.
	// Nil when spec.ceph is unset.
	Ceph *CephStatus `json:"ceph,omitempty"`

	// ObservedGeneration is the .metadata.generation when this status was last updated.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// CephStatus reports progress and health of the in-Proxmox Ceph cluster.
type CephStatus struct {
	// Initialized is true once `pveceph init` has written /etc/pve/ceph.conf.
	Initialized bool `json:"initialized,omitempty"`

	// Monitors lists the PVE node names currently running a Ceph Monitor.
	Monitors []string `json:"monitors,omitempty"`

	// Managers lists the PVE node names currently running a Ceph Manager.
	Managers []string `json:"managers,omitempty"`

	// OSDsReady is the number of OSD daemons that are up and in.
	OSDsReady int32 `json:"osdsReady,omitempty"`

	// OSDsExpected is spec.nodes × spec.ceph.osdsPerNode.
	OSDsExpected int32 `json:"osdsExpected,omitempty"`

	// OSDSummary is a human-readable "ready/expected" string for printer columns.
	OSDSummary string `json:"osdSummary,omitempty"`

	// Pools lists the Ceph pools that exist and, where requested, have been
	// registered as Proxmox storage.
	Pools []string `json:"pools,omitempty"`

	// Health is the raw `ceph health` status: HEALTH_OK, HEALTH_WARN or HEALTH_ERR.
	Health string `json:"health,omitempty"`

	// HealthDetail carries the most recent `ceph health detail` summary line
	// when Health is not HEALTH_OK.
	HealthDetail string `json:"healthDetail,omitempty"`
}
