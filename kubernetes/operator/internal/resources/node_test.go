package resources

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// ParseInventory
// ─────────────────────────────────────────────────────────────────────────────

func TestParseInventory(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]NodeInventory
	}{
		{
			name: "empty input yields empty map",
			in:   "",
			want: map[string]NodeInventory{},
		},
		{
			name: "single fully-populated node",
			in:   "node=mp-0 online=1 nodeid=1 mon=1 mgr=1 osds_up=2 osds_total=2\n",
			want: map[string]NodeInventory{
				"mp-0": {Name: "mp-0", Online: true, NodeID: 1, Monitor: true, Manager: true, OSDsUp: 2, OSDsTotal: 2},
			},
		},
		{
			name: "degraded node reports fewer OSDs up than total",
			in: "node=mp-0 online=1 nodeid=1 mon=1 mgr=1 osds_up=2 osds_total=2\n" +
				"node=mp-1 online=1 nodeid=2 mon=1 mgr=0 osds_up=1 osds_total=2\n",
			want: map[string]NodeInventory{
				"mp-0": {Name: "mp-0", Online: true, NodeID: 1, Monitor: true, Manager: true, OSDsUp: 2, OSDsTotal: 2},
				"mp-1": {Name: "mp-1", Online: true, NodeID: 2, Monitor: true, Manager: false, OSDsUp: 1, OSDsTotal: 2},
			},
		},
		{
			name: "offline node",
			in:   "node=mp-2 online=0 nodeid=3 mon=0 mgr=0 osds_up=0 osds_total=1\n",
			want: map[string]NodeInventory{
				"mp-2": {Name: "mp-2", Online: false, NodeID: 3, OSDsTotal: 1},
			},
		},
		{
			// The script runs inside a container where systemd or pveceph may
			// emit warnings first. Non-node lines must be ignored, not fatal.
			name: "ignores unrelated leading output",
			in: "WARNING: something unrelated\n" +
				"some other noise\n" +
				"node=mp-0 online=1 nodeid=1 mon=0 mgr=0 osds_up=0 osds_total=0\n",
			want: map[string]NodeInventory{
				"mp-0": {Name: "mp-0", Online: true, NodeID: 1},
			},
		},
		{
			name: "skips malformed numeric fields rather than failing",
			in:   "node=mp-0 online=1 nodeid=abc mon=1 mgr=1 osds_up=xyz osds_total=3\n",
			want: map[string]NodeInventory{
				"mp-0": {Name: "mp-0", Online: true, NodeID: 0, Monitor: true, Manager: true, OSDsUp: 0, OSDsTotal: 3},
			},
		},
		{
			name: "line without a node= key is skipped entirely",
			in:   "online=1 nodeid=1 mon=1\n",
			want: map[string]NodeInventory{},
		},
		{
			name: "tolerates extra whitespace and blank lines",
			in:   "\n   node=mp-0 online=1 nodeid=1 mon=0 mgr=0 osds_up=0 osds_total=0   \n\n",
			want: map[string]NodeInventory{
				"mp-0": {Name: "mp-0", Online: true, NodeID: 1},
			},
		},
		{
			name: "unknown keys are ignored so the script can grow",
			in:   "node=mp-0 online=1 nodeid=1 future_field=xyz mon=1 mgr=0 osds_up=1 osds_total=1\n",
			want: map[string]NodeInventory{
				"mp-0": {Name: "mp-0", Online: true, NodeID: 1, Monitor: true, OSDsUp: 1, OSDsTotal: 1},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseInventory(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("node count: got %d %v, want %d %v", len(got), keysOf(got), len(tc.want), keysOf(tc.want))
			}
			for name, want := range tc.want {
				g, ok := got[name]
				if !ok {
					t.Fatalf("missing node %q in result", name)
				}
				if g != want {
					t.Errorf("node %q:\n  got  %+v\n  want %+v", name, g, want)
				}
			}
		})
	}
}

func keysOf(m map[string]NodeInventory) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// nodePhase
// ─────────────────────────────────────────────────────────────────────────────

func TestNodePhase(t *testing.T) {
	pod := func(phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
	}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		podReady  bool
		inCluster bool
		online    bool
		want      pxv1.NodePhase
	}{
		{"nil pod is Pending", nil, false, false, false, pxv1.NodePhasePending},
		{"pending pod is Pending", pod(corev1.PodPending), false, false, false, pxv1.NodePhasePending},
		{"failed pod is Failed", pod(corev1.PodFailed), false, false, false, pxv1.NodePhaseFailed},
		{
			// A PVE node should run indefinitely; a clean exit still means
			// the node is gone, so Succeeded is a failure of intent.
			"succeeded pod is Failed", pod(corev1.PodSucceeded), false, false, false, pxv1.NodePhaseFailed,
		},
		{"running but not ready is Starting", pod(corev1.PodRunning), false, false, false, pxv1.NodePhaseStarting},
		{"ready but not in cluster is Joining", pod(corev1.PodRunning), true, false, false, pxv1.NodePhaseJoining},
		{"in cluster but not online is Offline", pod(corev1.PodRunning), true, true, false, pxv1.NodePhaseOffline},
		{"ready, in cluster, online is Ready", pod(corev1.PodRunning), true, true, true, pxv1.NodePhaseReady},
		{
			// Readiness gates the phase: a pod that is not ready cannot be
			// Ready even if Proxmox still lists it as an online member.
			"not ready wins over cluster membership", pod(corev1.PodRunning), false, true, true, pxv1.NodePhaseStarting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nodePhase(tc.pod, tc.podReady, tc.inCluster, tc.online)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProxmoxNodeFor
// ─────────────────────────────────────────────────────────────────────────────

func readyPod(name, ip, host string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       corev1.PodSpec{NodeName: host},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func TestProxmoxNodeFor_IdentityAndCorrelation(t *testing.T) {
	c := testCluster()
	now := metav1.NewTime(time.Unix(1700000000, 0))
	pod := readyPod("mp-1", "10.244.1.7", "worker2")
	inv := &NodeInventory{Name: "mp-1", Online: true, NodeID: 2}

	n := ProxmoxNodeFor(c, 1, pod, inv, now)

	if n.Name != "mp-1" {
		t.Errorf("object name: got %q, want mp-1", n.Name)
	}
	if n.Namespace != "ns" {
		t.Errorf("namespace: got %q, want ns", n.Namespace)
	}

	// Immutable identity spec.
	if n.Spec.ClusterRef != "mp" || n.Spec.NodeName != "mp-1" || n.Spec.Ordinal != 1 {
		t.Errorf("spec identity wrong: %+v", n.Spec)
	}

	// Kubernetes correlation — the projection must point at the real pod/host,
	// otherwise it is useless for debugging placement.
	if n.Status.PodName != "mp-1" {
		t.Errorf("podName: got %q", n.Status.PodName)
	}
	if n.Status.PodIP != "10.244.1.7" {
		t.Errorf("podIP: got %q", n.Status.PodIP)
	}
	if n.Status.KubernetesNode != "worker2" {
		t.Errorf("kubernetesNode: got %q", n.Status.KubernetesNode)
	}

	// Proxmox-side facts.
	if !n.Status.InCluster || !n.Status.Online || n.Status.CorosyncNodeID != 2 {
		t.Errorf("proxmox facts wrong: inCluster=%v online=%v nodeid=%d",
			n.Status.InCluster, n.Status.Online, n.Status.CorosyncNodeID)
	}
	if n.Status.Phase != pxv1.NodePhaseReady {
		t.Errorf("phase: got %q, want Ready", n.Status.Phase)
	}
	if n.Status.ObservedAt == nil || !n.Status.ObservedAt.Equal(&now) {
		t.Errorf("observedAt not set to now: %v", n.Status.ObservedAt)
	}

	// The annotation documents the contract on the object itself.
	if !strings.Contains(n.Annotations["multiprox.io/projection"], "overwritten") {
		t.Errorf("projection annotation missing or unhelpful: %q", n.Annotations["multiprox.io/projection"])
	}
}

func TestProxmoxNodeFor_CephAbsentWhenDisabled(t *testing.T) {
	c := testCluster() // no Ceph
	n := ProxmoxNodeFor(c, 0, readyPod("mp-0", "10.0.0.1", "w1"),
		&NodeInventory{Name: "mp-0", Online: true, NodeID: 1}, metav1.Now())

	if n.Status.Ceph != nil {
		t.Errorf("status.ceph must be nil when spec.ceph is unset, got %+v", n.Status.Ceph)
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == pxv1.NodeConditionCephOSDsUp {
			t.Errorf("CephOSDsUp condition must not be set when Ceph is disabled")
		}
	}
}

func TestProxmoxNodeFor_CephReporting(t *testing.T) {
	c := testCluster(withCeph(4, 3, 2))
	inv := &NodeInventory{
		Name: "mp-0", Online: true, NodeID: 1,
		Monitor: true, Manager: true, OSDsUp: 3, OSDsTotal: 4,
	}
	n := ProxmoxNodeFor(c, 0, readyPod("mp-0", "10.0.0.1", "w1"), inv, metav1.Now())

	if n.Status.Ceph == nil {
		t.Fatal("status.ceph must be populated when spec.ceph is set")
	}
	cs := n.Status.Ceph
	if !cs.Monitor || !cs.Manager {
		t.Errorf("mon/mgr flags: got mon=%v mgr=%v", cs.Monitor, cs.Manager)
	}
	// Summary is expected/spec-based, so a missing OSD is visible.
	if cs.OSDSummary != "3/4" {
		t.Errorf("osdSummary: got %q, want 3/4", cs.OSDSummary)
	}
	if len(cs.Devices) != 4 {
		t.Fatalf("devices: got %d, want 4", len(cs.Devices))
	}
	if cs.Devices[0] != "/dev/ceph-osd-0" || cs.Devices[3] != "/dev/ceph-osd-3" {
		t.Errorf("device paths wrong: %v", cs.Devices)
	}

	// With 3 of 4 OSDs up the condition must be False — this is the signal that
	// a PVC was provisioned as a filesystem instead of a raw block volume.
	cond := findCond(n.Status.Conditions, pxv1.NodeConditionCephOSDsUp)
	if cond == nil {
		t.Fatal("CephOSDsUp condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("CephOSDsUp: got %v, want False (3/4 up)", cond.Status)
	}
}

func TestProxmoxNodeFor_NilPodAndNilInventory(t *testing.T) {
	// Both are normal during scale-up: the pod does not exist yet and Proxmox
	// has never heard of the node. This must produce a valid early-phase object
	// rather than panicking or omitting the node.
	c := testCluster()
	n := ProxmoxNodeFor(c, 2, nil, nil, metav1.Now())

	if n.Status.Phase != pxv1.NodePhasePending {
		t.Errorf("phase: got %q, want Pending", n.Status.Phase)
	}
	if n.Status.PodName != "" || n.Status.InCluster {
		t.Errorf("expected empty pod/cluster facts, got %+v", n.Status)
	}
	if n.Spec.Ordinal != 2 || n.Spec.NodeName != "mp-2" {
		t.Errorf("identity must still be correct: %+v", n.Spec)
	}
	cond := findCond(n.Status.Conditions, pxv1.NodeConditionPodReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("PodReady should be False for a nonexistent pod")
	}
	if cond != nil && !strings.Contains(cond.Message, "does not exist") {
		t.Errorf("message should explain the pod is missing, got %q", cond.Message)
	}
}

func findCond(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// MergeNodeStatus — the write-amplification guard
// ─────────────────────────────────────────────────────────────────────────────

func TestMergeNodeStatus_NoChangeWhenOnlyTimestampsMove(t *testing.T) {
	// This is the regression test for the bug that would rewrite every
	// ProxmoxNode on every reconcile: a freshly built status stamps every
	// condition with time.Now(), so a naive comparison always differs.
	t0 := metav1.NewTime(time.Unix(1700000000, 0))
	t1 := metav1.NewTime(time.Unix(1700009999, 0))

	existing := pxv1.ProxmoxNodeStatus{
		Phase:      pxv1.NodePhaseReady,
		InCluster:  true,
		Online:     true,
		PodName:    "mp-0",
		ObservedAt: &t0,
		Conditions: []metav1.Condition{
			{Type: pxv1.NodeConditionPodReady, Status: metav1.ConditionTrue, Reason: "PodReady", Message: "ok", LastTransitionTime: t0},
		},
	}
	desired := pxv1.ProxmoxNodeStatus{
		Phase:      pxv1.NodePhaseReady,
		InCluster:  true,
		Online:     true,
		PodName:    "mp-0",
		ObservedAt: &t1, // moved
		Conditions: []metav1.Condition{
			{Type: pxv1.NodeConditionPodReady, Status: metav1.ConditionTrue, Reason: "PodReady", Message: "ok", LastTransitionTime: t1}, // moved
		},
	}

	merged, changed := MergeNodeStatus(existing, desired)
	if changed {
		t.Error("changed=true when only timestamps moved — this causes a write on every reconcile")
	}
	// Unchanged means no write, so ObservedAt must stay put rather than
	// advancing (which would itself be a change and defeat the whole point).
	if merged.ObservedAt == nil || !merged.ObservedAt.Equal(&t0) {
		t.Errorf("ObservedAt should be held at the stored value when nothing changed, got %v", merged.ObservedAt)
	}
	// The transition time must be preserved from the stored object, so it keeps
	// meaning "when this last flipped".
	if !merged.Conditions[0].LastTransitionTime.Equal(&t0) {
		t.Errorf("LastTransitionTime should be carried over, got %v", merged.Conditions[0].LastTransitionTime)
	}
}

func TestMergeNodeStatus_DetectsRealChanges(t *testing.T) {
	t0 := metav1.NewTime(time.Unix(1700000000, 0))
	t1 := metav1.NewTime(time.Unix(1700009999, 0))

	base := func() pxv1.ProxmoxNodeStatus {
		return pxv1.ProxmoxNodeStatus{
			Phase:      pxv1.NodePhaseReady,
			InCluster:  true,
			Online:     true,
			PodName:    "mp-0",
			ObservedAt: &t0,
			Conditions: []metav1.Condition{
				{Type: pxv1.NodeConditionPodReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: t0},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*pxv1.ProxmoxNodeStatus)
	}{
		{"phase change", func(s *pxv1.ProxmoxNodeStatus) { s.Phase = pxv1.NodePhaseOffline }},
		{"online flips", func(s *pxv1.ProxmoxNodeStatus) { s.Online = false }},
		{"inCluster flips", func(s *pxv1.ProxmoxNodeStatus) { s.InCluster = false }},
		{"pod rescheduled to another host", func(s *pxv1.ProxmoxNodeStatus) { s.KubernetesNode = "other" }},
		{"pod IP changes", func(s *pxv1.ProxmoxNodeStatus) { s.PodIP = "10.9.9.9" }},
		{"condition status flips", func(s *pxv1.ProxmoxNodeStatus) {
			s.Conditions[0].Status = metav1.ConditionFalse
		}},
		{"condition reason changes", func(s *pxv1.ProxmoxNodeStatus) {
			s.Conditions[0].Reason = "Different"
		}},
		{"new condition appears", func(s *pxv1.ProxmoxNodeStatus) {
			s.Conditions = append(s.Conditions, metav1.Condition{
				Type: pxv1.NodeConditionClusterMember, Status: metav1.ConditionTrue, Reason: "X", LastTransitionTime: t1,
			})
		}},
		{"ceph block appears", func(s *pxv1.ProxmoxNodeStatus) {
			s.Ceph = &pxv1.ProxmoxNodeCephStatus{Monitor: true, OSDSummary: "1/1"}
		}},
		{"osd count changes", func(s *pxv1.ProxmoxNodeStatus) {
			s.Ceph = &pxv1.ProxmoxNodeCephStatus{OSDsUp: 2, OSDsTotal: 4, OSDSummary: "2/4"}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			existing := base()
			desired := base()
			desired.ObservedAt = &t1
			tc.mutate(&desired)

			merged, changed := MergeNodeStatus(existing, desired)
			if !changed {
				t.Fatalf("changed=false but %s should be a real change", tc.name)
			}
			// A real change must let ObservedAt advance, otherwise consumers
			// cannot tell when state last differed.
			if merged.ObservedAt == nil || !merged.ObservedAt.Equal(&t1) {
				t.Errorf("ObservedAt should advance on a real change, got %v", merged.ObservedAt)
			}
		})
	}
}

func TestMergeNodeStatus_ConditionOrderIndependent(t *testing.T) {
	// Condition slice order is not semantically meaningful; a reordering must
	// not be mistaken for a change or the projection churns.
	t0 := metav1.NewTime(time.Unix(1700000000, 0))
	a := metav1.Condition{Type: pxv1.NodeConditionPodReady, Status: metav1.ConditionTrue, Reason: "A", LastTransitionTime: t0}
	b := metav1.Condition{Type: pxv1.NodeConditionClusterMember, Status: metav1.ConditionTrue, Reason: "B", LastTransitionTime: t0}

	existing := pxv1.ProxmoxNodeStatus{ObservedAt: &t0, Conditions: []metav1.Condition{a, b}}
	desired := pxv1.ProxmoxNodeStatus{ObservedAt: &t0, Conditions: []metav1.Condition{b, a}}

	if _, changed := MergeNodeStatus(existing, desired); changed {
		t.Error("reordered conditions reported as a change")
	}
}

func TestMergeNodeStatus_FirstObservation(t *testing.T) {
	// Empty stored status → anything is a change, and ObservedAt must be set.
	t1 := metav1.NewTime(time.Unix(1700009999, 0))
	desired := pxv1.ProxmoxNodeStatus{Phase: pxv1.NodePhasePending, ObservedAt: &t1}

	merged, changed := MergeNodeStatus(pxv1.ProxmoxNodeStatus{}, desired)
	if !changed {
		t.Error("first observation must count as a change")
	}
	if merged.ObservedAt == nil || !merged.ObservedAt.Equal(&t1) {
		t.Errorf("ObservedAt should be set on first observation, got %v", merged.ObservedAt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Naming
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeObjectNameMatchesPodName(t *testing.T) {
	// The projection object is named after the pod on purpose, so the two are
	// trivially correlatable by eye and by script.
	c := testCluster()
	for i := int32(0); i < 3; i++ {
		want := fmt.Sprintf("mp-%d", i)
		if got := NodeObjectName(c, i); got != want {
			t.Errorf("ordinal %d: got %q, want %q", i, got, want)
		}
	}
}
