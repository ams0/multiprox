package resources

// Ceph, configured natively from inside the Proxmox cluster.
//
// There is no external storage operator involved. The disks are ordinary
// Kubernetes PersistentVolumeClaims provisioned with volumeMode: Block by the
// StatefulSet's volumeClaimTemplates (see statefulset.go), surfaced into each
// PVE pod as raw devices via volumeDevices. Proxmox's own `pveceph` tooling
// then claims those devices and manages the Ceph cluster.
//
// Bootstrap order — each step is idempotent and driven by the controller via
// exec into the pods:
//
//	1. ceph-init.sh    on pod-0            → pveceph init --network <cidr>
//	                                          writes /etc/pve/ceph.conf into pmxcfs
//	2. ceph-mon.sh     on pods 0..MonCount → pveceph mon create
//	3. ceph-mgr.sh     on pods 0..MgrCount → pveceph mgr create
//	4. ceph-osd.sh     on every pod        → pveceph osd create /dev/ceph-osd-N
//	5. ceph-pool.sh    on pod-0            → pveceph pool create + pvesm add rbd
//	6. ceph-status.sh  on pod-0            → poll health for status.ceph
//
// Steps 2 and 3 must precede step 4: OSDs cannot register without a monitor.
// Step 5 must follow step 4: a pool with size=N needs at least N OSDs to
// reach active+clean.

import (
	"fmt"
	"strconv"
	"strings"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// CephStep identifies one stage of the Ceph bootstrap sequence.
type CephStep string

const (
	StepCephInit   CephStep = "init"
	StepCephMon    CephStep = "mon"
	StepCephMgr    CephStep = "mgr"
	StepCephOSD    CephStep = "osd"
	StepCephPool   CephStep = "pool"
	StepCephStatus CephStep = "status"
)

// CephTask is a single script invocation on a single pod.
type CephTask struct {
	Step    CephStep
	Step0   bool   // true when this task must run on pod ordinal 0
	Ordinal int32  // pod ordinal to exec into
	Script  string // absolute path of the script inside the container
}

// Command returns the argv the controller should exec for this task.
func (t CephTask) Command() []string {
	return []string{"bash", t.Script}
}

// ─────────────────────────────────────────────────────────────────────────────
// Plan
// ─────────────────────────────────────────────────────────────────────────────

// CephPlan returns the ordered list of tasks required to bring Ceph up for
// this cluster. The controller walks the plan, stopping at the first task that
// has not yet succeeded, so a single Reconcile makes forward progress and the
// next Reconcile resumes where it left off.
//
// Every script is idempotent, so re-running a completed task is harmless.
func CephPlan(cluster *pxv1.ProxmoxCluster) []CephTask {
	if cluster.Spec.Ceph == nil {
		return nil
	}
	nodes := cluster.Spec.Nodes

	monCount := EffectiveMonCount(cluster)
	mgrCount := EffectiveMgrCount(cluster)

	tasks := []CephTask{
		// 1. Bootstrap ceph.conf (pmxcfs replicates it cluster-wide).
		{Step: StepCephInit, Step0: true, Ordinal: 0, Script: "/scripts/ceph-init.sh"},
	}

	// 2. Monitors — first monCount nodes.
	for i := int32(0); i < monCount; i++ {
		tasks = append(tasks, CephTask{
			Step: StepCephMon, Ordinal: i, Script: "/scripts/ceph-mon.sh",
		})
	}

	// 3. Managers — first mgrCount nodes.
	for i := int32(0); i < mgrCount; i++ {
		tasks = append(tasks, CephTask{
			Step: StepCephMgr, Ordinal: i, Script: "/scripts/ceph-mgr.sh",
		})
	}

	// 4. OSDs — every node consumes its own attached block devices.
	for i := int32(0); i < nodes; i++ {
		tasks = append(tasks, CephTask{
			Step: StepCephOSD, Ordinal: i, Script: "/scripts/ceph-osd.sh",
		})
	}

	// 5. Pools + Proxmox storage registration.
	tasks = append(tasks, CephTask{
		Step: StepCephPool, Step0: true, Ordinal: 0, Script: "/scripts/ceph-pool.sh",
	})

	return tasks
}

// StatusTask returns the task that polls Ceph health.
func StatusTask() CephTask {
	return CephTask{
		Step: StepCephStatus, Step0: true, Ordinal: 0, Script: "/scripts/ceph-status.sh",
	}
}

// ExpectedOSDs returns spec.nodes × spec.ceph.osdsPerNode.
func ExpectedOSDs(cluster *pxv1.ProxmoxCluster) int32 {
	if cluster.Spec.Ceph == nil {
		return 0
	}
	return cluster.Spec.Nodes * maxInt32(cluster.Spec.Ceph.OSDsPerNode, 1)
}

// EffectiveMonCount returns how many monitors will actually be created.
//
// The requested count is capped at the node count and then snapped DOWN to an
// odd number. Both steps matter:
//
//   - The CRD defaults monCount to 3, so a user who writes `nodes: 1` and says
//     nothing about monitors still arrives here with 3. Capping rather than
//     rejecting keeps that spec workable.
//   - Capping alone is not enough: a 2-node cluster would cap 3 → 2, an even
//     quorum that cannot tolerate any failure and is exactly what the odd-count
//     rule exists to prevent. Snapping down gives 1 instead.
func EffectiveMonCount(cluster *pxv1.ProxmoxCluster) int32 {
	if cluster.Spec.Ceph == nil {
		return 0
	}
	n := clampCount(cluster.Spec.Ceph.MonCount, 3, cluster.Spec.Nodes)
	if n%2 == 0 {
		n-- // snap down to odd
	}
	return maxInt32(n, 1)
}

// EffectiveMgrCount returns how many managers will actually be created.
// Managers have no quorum requirement, so this is a plain cap.
func EffectiveMgrCount(cluster *pxv1.ProxmoxCluster) int32 {
	if cluster.Spec.Ceph == nil {
		return 0
	}
	return clampCount(cluster.Spec.Ceph.MgrCount, 2, cluster.Spec.Nodes)
}

// PoolNames returns the effective pool names for this cluster.
func PoolNames(cluster *pxv1.ProxmoxCluster) []string {
	pools := effectivePools(cluster)
	names := make([]string, 0, len(pools))
	for _, p := range pools {
		names = append(names, p.Name)
	}
	return names
}

// ValidateCeph checks the Ceph spec against the cluster shape and returns a
// human-readable error when the configuration cannot converge. These are
// conditions the CRD's own validation cannot express.
func ValidateCeph(cluster *pxv1.ProxmoxCluster) error {
	if cluster.Spec.Ceph == nil {
		return nil
	}
	ceph := cluster.Spec.Ceph
	nodes := cluster.Spec.Nodes
	totalOSDs := ExpectedOSDs(cluster)

	// Only reject what is unambiguously the user's own mistake.
	//
	// monCount/mgrCount exceeding the node count is NOT rejected: the CRD
	// defaults them (3 and 2), so a spec that says nothing about monitors still
	// arrives here with 3, and failing a 1-node cluster over a value the user
	// never wrote is a bad trade. EffectiveMonCount caps and snaps to odd
	// instead, and the controller reports the effective numbers.
	//
	// An explicitly even monCount is different — no capping makes an even
	// quorum correct, so it is a real error worth surfacing.
	if ceph.MonCount > 0 && ceph.MonCount%2 == 0 {
		return fmt.Errorf(
			"spec.ceph.monCount (%d) must be odd to avoid split-brain in the monitor quorum",
			ceph.MonCount)
	}

	for _, p := range effectivePools(cluster) {
		if p.Size > totalOSDs {
			return fmt.Errorf(
				"pool %q has size %d but the cluster only provides %d OSDs "+
					"(spec.nodes %d × spec.ceph.osdsPerNode %d): the pool can never reach active+clean",
				p.Name, p.Size, totalOSDs, nodes, maxInt32(ceph.OSDsPerNode, 1))
		}
		if p.MinSize > p.Size {
			return fmt.Errorf("pool %q has minSize %d greater than size %d",
				p.Name, p.MinSize, p.Size)
		}
	}

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Status parsing
// ─────────────────────────────────────────────────────────────────────────────

// ParseCephStatus turns the single key=value line emitted by ceph-status.sh
// into a CephStatus. Unrecognised keys are ignored so the script can grow.
//
// Expected input:
//
//	health=HEALTH_OK osds_up=6 osds_in=6 mons=c-0,c-1,c-2 mgrs=c-0 pools=ceph-vm detail=
func ParseCephStatus(cluster *pxv1.ProxmoxCluster, out string) *pxv1.CephStatus {
	st := &pxv1.CephStatus{
		OSDsExpected: ExpectedOSDs(cluster),
	}

	line := strings.TrimSpace(out)
	// Take the last non-empty line — systemd/pveceph may print warnings first.
	if idx := strings.LastIndexByte(line, '\n'); idx >= 0 {
		line = strings.TrimSpace(line[idx+1:])
	}

	// detail= may contain spaces, so parse it separately and truncate the rest.
	detail := ""
	if i := strings.Index(line, "detail="); i >= 0 {
		detail = strings.TrimSpace(line[i+len("detail="):])
		line = line[:i]
	}

	for _, field := range strings.Fields(line) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch k {
		case "health":
			st.Health = v
			st.Initialized = v != "UNINITIALIZED"
		case "osds_up":
			if n, err := strconv.Atoi(v); err == nil {
				st.OSDsReady = int32(n)
			}
		case "mons":
			st.Monitors = splitCSV(v)
		case "mgrs":
			st.Managers = splitCSV(v)
		case "pools":
			st.Pools = splitCSV(v)
		}
	}

	if st.Health != "HEALTH_OK" {
		st.HealthDetail = detail
	}
	st.OSDSummary = fmt.Sprintf("%d/%d", st.OSDsReady, st.OSDsExpected)

	return st
}

// CephUsable reports whether Ceph has converged enough to serve VM disks:
// every expected OSD is up and health is not HEALTH_ERR.
//
// HEALTH_WARN is accepted because a fresh cluster commonly warns about PG
// autoscaling or clock skew while remaining fully usable.
func CephUsable(st *pxv1.CephStatus) bool {
	if st == nil || !st.Initialized {
		return false
	}
	if st.Health == "HEALTH_ERR" || st.Health == "UNREACHABLE" || st.Health == "UNINITIALIZED" {
		return false
	}
	return st.OSDsExpected > 0 && st.OSDsReady >= st.OSDsExpected
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// clampCount applies a default when zero and caps the value at max.
func clampCount(v, def, max int32) int32 {
	if v <= 0 {
		v = def
	}
	if v > max {
		v = max
	}
	if v < 1 {
		v = 1
	}
	return v
}
