package resources

import (
	"fmt"
	"strings"
	"testing"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// ParseCephStatus
// ─────────────────────────────────────────────────────────────────────────────

func TestParseCephStatus(t *testing.T) {
	c := testCluster(withCeph(2, 3, 2)) // 3 nodes × 2 OSDs = 6 expected

	tests := []struct {
		name             string
		in               string
		wantHealth       string
		wantInitialized  bool
		wantOSDsReady    int32
		wantSummary      string
		wantMons         []string
		wantPools        []string
		wantDetailPrefix string
	}{
		{
			name:            "healthy cluster",
			in:              "health=HEALTH_OK osds_up=6 osds_in=6 mons=mp-0,mp-1,mp-2 mgrs=mp-0 pools=ceph-vm detail=",
			wantHealth:      "HEALTH_OK",
			wantInitialized: true,
			wantOSDsReady:   6,
			wantSummary:     "6/6",
			wantMons:        []string{"mp-0", "mp-1", "mp-2"},
			wantPools:       []string{"ceph-vm"},
		},
		{
			name:            "uninitialized reports not initialized",
			in:              "health=UNINITIALIZED osds_up=0 osds_in=0 mons= mgrs= pools= detail=ceph.conf missing",
			wantHealth:      "UNINITIALIZED",
			wantInitialized: false,
			wantOSDsReady:   0,
			wantSummary:     "0/6",
		},
		{
			name:             "warn carries a detail message with spaces",
			in:               "health=HEALTH_WARN osds_up=5 osds_in=5 mons=mp-0 mgrs=mp-0 pools=ceph-vm detail=Degraded data redundancy: 12 pgs undersized",
			wantHealth:       "HEALTH_WARN",
			wantInitialized:  true,
			wantOSDsReady:    5,
			wantSummary:      "5/6",
			wantMons:         []string{"mp-0"},
			wantDetailPrefix: "Degraded data redundancy",
		},
		{
			// pveceph and systemd routinely print warnings before the payload;
			// only the final line is the status record.
			name:            "takes the last line when preceded by noise",
			in:              "WARNING: unrelated chatter\nanother line\nhealth=HEALTH_OK osds_up=6 osds_in=6 mons=mp-0 mgrs=mp-0 pools=p1,p2 detail=",
			wantHealth:      "HEALTH_OK",
			wantInitialized: true,
			wantOSDsReady:   6,
			wantSummary:     "6/6",
			wantMons:        []string{"mp-0"},
			wantPools:       []string{"p1", "p2"},
		},
		{
			name:            "trailing newline is tolerated",
			in:              "health=HEALTH_OK osds_up=6 osds_in=6 mons=mp-0 mgrs=mp-0 pools=p1 detail=\n",
			wantHealth:      "HEALTH_OK",
			wantInitialized: true,
			wantOSDsReady:   6,
			wantSummary:     "6/6",
		},
		{
			name:            "unreachable cluster",
			in:              "health=UNREACHABLE osds_up=0 osds_in=0 mons= mgrs= pools= detail=cannot reach cluster",
			wantHealth:      "UNREACHABLE",
			wantInitialized: true, // ceph.conf exists; only reachability failed
			wantOSDsReady:   0,
			wantSummary:     "0/6",
		},
		{
			name:            "garbage input degrades gracefully",
			in:              "totally unexpected output",
			wantHealth:      "",
			wantInitialized: false,
			wantOSDsReady:   0,
			wantSummary:     "0/6",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseCephStatus(c, tc.in)

			if got.Health != tc.wantHealth {
				t.Errorf("health: got %q, want %q", got.Health, tc.wantHealth)
			}
			if got.Initialized != tc.wantInitialized {
				t.Errorf("initialized: got %v, want %v", got.Initialized, tc.wantInitialized)
			}
			if got.OSDsReady != tc.wantOSDsReady {
				t.Errorf("osdsReady: got %d, want %d", got.OSDsReady, tc.wantOSDsReady)
			}
			if got.OSDsExpected != 6 {
				t.Errorf("osdsExpected: got %d, want 6", got.OSDsExpected)
			}
			if got.OSDSummary != tc.wantSummary {
				t.Errorf("osdSummary: got %q, want %q", got.OSDSummary, tc.wantSummary)
			}
			if tc.wantMons != nil && !equalStrings(got.Monitors, tc.wantMons) {
				t.Errorf("monitors: got %v, want %v", got.Monitors, tc.wantMons)
			}
			if tc.wantPools != nil && !equalStrings(got.Pools, tc.wantPools) {
				t.Errorf("pools: got %v, want %v", got.Pools, tc.wantPools)
			}
			if tc.wantDetailPrefix != "" && !strings.HasPrefix(got.HealthDetail, tc.wantDetailPrefix) {
				t.Errorf("healthDetail: got %q, want prefix %q", got.HealthDetail, tc.wantDetailPrefix)
			}
			// Detail is only meaningful when not OK; it must not leak otherwise.
			if got.Health == "HEALTH_OK" && got.HealthDetail != "" {
				t.Errorf("healthDetail should be empty when HEALTH_OK, got %q", got.HealthDetail)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// CephUsable
// ─────────────────────────────────────────────────────────────────────────────

func TestCephUsable(t *testing.T) {
	tests := []struct {
		name string
		st   *pxv1.CephStatus
		want bool
	}{
		{"nil is not usable", nil, false},
		{
			"not initialized is not usable",
			&pxv1.CephStatus{Initialized: false, Health: "UNINITIALIZED", OSDsExpected: 3, OSDsReady: 0},
			false,
		},
		{
			"HEALTH_ERR is not usable even with all OSDs up",
			&pxv1.CephStatus{Initialized: true, Health: "HEALTH_ERR", OSDsExpected: 3, OSDsReady: 3},
			false,
		},
		{
			// A fresh cluster commonly warns about PG autoscaling or clock skew
			// while being entirely usable, so WARN must not block readiness.
			"HEALTH_WARN with all OSDs up is usable",
			&pxv1.CephStatus{Initialized: true, Health: "HEALTH_WARN", OSDsExpected: 3, OSDsReady: 3},
			true,
		},
		{
			"HEALTH_OK with all OSDs up is usable",
			&pxv1.CephStatus{Initialized: true, Health: "HEALTH_OK", OSDsExpected: 3, OSDsReady: 3},
			true,
		},
		{
			// This is the case that catches a filesystem StorageClass being
			// used for OSDs: health looks fine but no OSD ever came up.
			"missing OSDs is not usable",
			&pxv1.CephStatus{Initialized: true, Health: "HEALTH_OK", OSDsExpected: 6, OSDsReady: 5},
			false,
		},
		{
			"zero expected OSDs is not usable",
			&pxv1.CephStatus{Initialized: true, Health: "HEALTH_OK", OSDsExpected: 0, OSDsReady: 0},
			false,
		},
		{
			"unreachable is not usable",
			&pxv1.CephStatus{Initialized: true, Health: "UNREACHABLE", OSDsExpected: 3, OSDsReady: 3},
			false,
		},
		{
			"more OSDs than expected still usable",
			&pxv1.CephStatus{Initialized: true, Health: "HEALTH_OK", OSDsExpected: 3, OSDsReady: 4},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CephUsable(tc.st); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ExpectedOSDs
// ─────────────────────────────────────────────────────────────────────────────

func TestExpectedOSDs(t *testing.T) {
	if got := ExpectedOSDs(testCluster()); got != 0 {
		t.Errorf("no ceph: got %d, want 0", got)
	}
	if got := ExpectedOSDs(testCluster(withCeph(4, 3, 2))); got != 12 {
		t.Errorf("3 nodes x 4 osds: got %d, want 12", got)
	}
	// osdsPerNode 0 is treated as 1 so the count never collapses to zero and
	// silently make every pool look satisfiable.
	if got := ExpectedOSDs(testCluster(withCeph(0, 3, 2))); got != 3 {
		t.Errorf("osdsPerNode 0 should floor to 1 per node: got %d, want 3", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CephPlan
// ─────────────────────────────────────────────────────────────────────────────

func TestCephPlan_NilWhenCephDisabled(t *testing.T) {
	if plan := CephPlan(testCluster()); plan != nil {
		t.Errorf("expected nil plan without ceph, got %d tasks", len(plan))
	}
}

func TestCephPlan_OrderIsLoadBearing(t *testing.T) {
	// Ordering is not cosmetic: OSDs cannot register without a monitor, and a
	// pool with size=N cannot go active+clean before N OSDs exist.
	c := testCluster(withCeph(2, 3, 2))
	plan := CephPlan(c)

	var order []pxv1.Phase // placeholder to avoid unused import concerns
	_ = order

	steps := make([]CephStep, 0, len(plan))
	for _, task := range plan {
		steps = append(steps, task.Step)
	}

	// init, then 3 mons, then 2 mgrs, then 3 osds, then pool
	want := []CephStep{
		StepCephInit,
		StepCephMon, StepCephMon, StepCephMon,
		StepCephMgr, StepCephMgr,
		StepCephOSD, StepCephOSD, StepCephOSD,
		StepCephPool,
	}
	if len(steps) != len(want) {
		t.Fatalf("plan length: got %d %v, want %d %v", len(steps), steps, len(want), want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("step %d: got %q, want %q\nfull plan: %v", i, steps[i], want[i], steps)
		}
	}

	// init and pool must run on node-0, since they write to pmxcfs which then
	// replicates cluster-wide; running them per-node would duplicate work.
	if plan[0].Ordinal != 0 || !plan[0].Step0 {
		t.Errorf("init must target ordinal 0: %+v", plan[0])
	}
	last := plan[len(plan)-1]
	if last.Ordinal != 0 || !last.Step0 {
		t.Errorf("pool creation must target ordinal 0: %+v", last)
	}

	// Every node must get an OSD task — OSD disks are per-node.
	osdOrdinals := map[int32]bool{}
	for _, task := range plan {
		if task.Step == StepCephOSD {
			osdOrdinals[task.Ordinal] = true
		}
	}
	for i := int32(0); i < c.Spec.Nodes; i++ {
		if !osdOrdinals[i] {
			t.Errorf("no OSD task for ordinal %d", i)
		}
	}
}

func TestCephPlan_ClampsMonAndMgrToNodeCount(t *testing.T) {
	// A single-node cluster cannot host 3 monitors. The plan must clamp rather
	// than emit tasks for pods that will never exist.
	c := testCluster(withNodes(1), withCeph(1, 3, 2))
	plan := CephPlan(c)

	mons, mgrs := 0, 0
	for _, task := range plan {
		switch task.Step {
		case StepCephMon:
			mons++
		case StepCephMgr:
			mgrs++
		}
		if task.Ordinal >= c.Spec.Nodes {
			t.Errorf("task targets ordinal %d but cluster has %d nodes: %+v",
				task.Ordinal, c.Spec.Nodes, task)
		}
	}
	if mons != 1 {
		t.Errorf("mons: got %d, want 1 (clamped to node count)", mons)
	}
	if mgrs != 1 {
		t.Errorf("mgrs: got %d, want 1 (clamped to node count)", mgrs)
	}
}

func TestCephPlan_ScriptPathsAndCommand(t *testing.T) {
	c := testCluster(withCeph(1, 1, 1))
	for _, task := range CephPlan(c) {
		if !strings.HasPrefix(task.Script, "/scripts/") {
			t.Errorf("script must live under /scripts (mounted from the ConfigMap): %q", task.Script)
		}
		cmd := task.Command()
		if len(cmd) != 2 || cmd[0] != "bash" || cmd[1] != task.Script {
			t.Errorf("command: got %v, want [bash %s]", cmd, task.Script)
		}
	}
	st := StatusTask()
	if st.Ordinal != 0 || st.Script != "/scripts/ceph-status.sh" {
		t.Errorf("status task wrong: %+v", st)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// clampCount
// ─────────────────────────────────────────────────────────────────────────────

func TestClampCount(t *testing.T) {
	tests := []struct {
		v, def, max, want int32
	}{
		{0, 3, 5, 3},  // unset takes the default
		{-1, 3, 5, 3}, // negative takes the default
		{7, 3, 5, 5},  // above max clamps to max
		{2, 3, 5, 2},  // in range passes through
		{3, 3, 1, 1},  // max below default wins
		{0, 3, 1, 1},  // default above max clamps
		{1, 3, 5, 1},
	}
	for _, tc := range tests {
		if got := clampCount(tc.v, tc.def, tc.max); got != tc.want {
			t.Errorf("clampCount(%d, %d, %d) = %d, want %d", tc.v, tc.def, tc.max, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ValidateCeph
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateCeph_AcceptsSaneConfigs(t *testing.T) {
	tests := []struct {
		name string
		c    *pxv1.ProxmoxCluster
	}{
		{"ceph disabled", testCluster()},
		{"3 nodes, 1 osd each, 3 mons", testCluster(withCeph(1, 3, 2))},
		{"3 nodes, 4 osds each", testCluster(withCeph(4, 3, 2))},
		{"single node single mon", testCluster(withNodes(1), withCeph(1, 1, 1))},
		{"5 nodes, 5 mons", testCluster(withNodes(5), withCeph(2, 5, 2))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCeph(tc.c); err != nil {
				t.Errorf("unexpected rejection: %v", err)
			}
		})
	}
}

// TestValidateCeph_ToleratesDefaultMonCountOnSmallClusters covers the case a
// user actually hits first: `nodes: 1` (or 2) with ceph enabled and monCount
// left unset. The CRD defaults monCount to 3 server-side, so the operator sees
// a value the user never wrote. Rejecting it produces a baffling
// "monCount (3) exceeds spec.nodes (1)" for a spec that says nothing about
// monitors — and CephPlan already clamps, so validation would be stricter than
// the executor.
func TestValidateCeph_ToleratesDefaultMonCountOnSmallClusters(t *testing.T) {
	for _, nodes := range []int32{1, 2} {
		t.Run(fmt.Sprintf("nodes=%d", nodes), func(t *testing.T) {
			// monCount/mgrCount 3/2 as the CRD would default them.
			c := testCluster(withNodes(nodes), withCeph(1, 3, 2))

			if err := ValidateCeph(c); err != nil {
				t.Fatalf("default monCount on a %d-node cluster was rejected: %v", nodes, err)
			}

			// The effective monitor count must be odd, or the quorum is broken.
			// Clamping 3 down to 2 on a 2-node cluster would be worse than
			// clamping to 1.
			mons := 0
			for _, task := range CephPlan(c) {
				if task.Step == StepCephMon {
					mons++
				}
			}
			if mons%2 == 0 {
				t.Errorf("effective mon count %d is even — cannot form a quorum", mons)
			}
			if int32(mons) > nodes {
				t.Errorf("effective mon count %d exceeds %d nodes", mons, nodes)
			}
		})
	}
}

func TestValidateCeph_RejectsUnconvergeableConfigs(t *testing.T) {
	tests := []struct {
		name        string
		c           *pxv1.ProxmoxCluster
		wantMessage string
	}{
		{
			name:        "even mon count cannot form a quorum",
			c:           testCluster(withNodes(4), withCeph(1, 4, 2)),
			wantMessage: "must be odd",
		},
		// mon/mgr counts exceeding the node count are deliberately NOT rejected
		// — see TestEffectiveMonMgrCounts_ClampInsteadOfReject.
		{
			name: "pool replica count above total OSDs can never go clean",
			c: testCluster(withNodes(3), withCeph(1, 3, 2), func(c *pxv1.ProxmoxCluster) {
				c.Spec.Ceph.Pools = []pxv1.CephPoolSpec{{Name: "big", Size: 5, MinSize: 2}}
			}),
			wantMessage: "only provides 3 OSDs",
		},
		{
			name: "minSize above size",
			c: testCluster(withNodes(3), withCeph(2, 3, 2), func(c *pxv1.ProxmoxCluster) {
				c.Spec.Ceph.Pools = []pxv1.CephPoolSpec{{Name: "p", Size: 2, MinSize: 3}}
			}),
			wantMessage: "minSize",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCeph(tc.c)
			if err == nil {
				t.Fatal("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("message should explain the problem\n  got:  %v\n  want substring: %q", err, tc.wantMessage)
			}
		})
	}
}

// TestEffectiveMonMgrCounts_ClampInsteadOfReject pins the clamping contract.
//
// The key row is nodes=2: capping the default 3 to the node count would give 2
// monitors — an even quorum that tolerates zero failures, which is precisely
// what the odd-count rule prevents. It must snap down to 1.
func TestEffectiveMonMgrCounts_ClampInsteadOfReject(t *testing.T) {
	tests := []struct {
		nodes, monReq, mgrReq int32
		wantMon, wantMgr      int32
		why                   string
	}{
		{1, 3, 2, 1, 1, "single node: everything collapses to 1"},
		{2, 3, 2, 1, 2, "two nodes: mons must snap to 1, not an even 2"},
		{3, 3, 2, 3, 2, "typical case passes through untouched"},
		{4, 3, 2, 3, 2, "4 nodes keeps 3 mons (odd) not 4"},
		{5, 5, 3, 5, 3, "larger explicit counts honoured"},
		{3, 9, 9, 3, 3, "counts above the node count clamp down"},
		{4, 0, 0, 3, 2, "unset uses defaults 3/2"},
		{2, 0, 0, 1, 2, "unset on 2 nodes still yields an odd mon count"},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			c := testCluster(withNodes(tc.nodes), withCeph(1, tc.monReq, tc.mgrReq))

			if got := EffectiveMonCount(c); got != tc.wantMon {
				t.Errorf("mons: got %d, want %d", got, tc.wantMon)
			}
			if got := EffectiveMgrCount(c); got != tc.wantMgr {
				t.Errorf("mgrs: got %d, want %d", got, tc.wantMgr)
			}

			// Invariants that must hold for every input.
			mons := EffectiveMonCount(c)
			if mons%2 == 0 {
				t.Errorf("effective mon count %d is even", mons)
			}
			if mons > tc.nodes {
				t.Errorf("effective mon count %d exceeds %d nodes", mons, tc.nodes)
			}
			if EffectiveMgrCount(c) > tc.nodes {
				t.Errorf("effective mgr count exceeds node count")
			}

			// The plan must emit exactly the effective number of tasks.
			planMons, planMgrs := 0, 0
			for _, task := range CephPlan(c) {
				switch task.Step {
				case StepCephMon:
					planMons++
				case StepCephMgr:
					planMgrs++
				}
			}
			if int32(planMons) != tc.wantMon || int32(planMgrs) != tc.wantMgr {
				t.Errorf("plan disagrees with effective counts: mons=%d mgrs=%d, want %d/%d",
					planMons, planMgrs, tc.wantMon, tc.wantMgr)
			}
		})
	}
}

// A zero monCount reaches the operator only when a CR bypasses CRD defaulting,
// but 0 % 2 == 0 would have tripped the even-count check and rejected a spec
// that simply left the field alone.
func TestValidateCeph_ZeroMonCountIsNotTreatedAsEven(t *testing.T) {
	c := testCluster(withNodes(3), withCeph(1, 0, 0))
	if err := ValidateCeph(c); err != nil {
		t.Errorf("unset monCount must not be rejected as even: %v", err)
	}
}

func TestValidateCeph_DefaultPoolSizeNeverExceedsOSDs(t *testing.T) {
	// When the user specifies no pools, the default pool must be sized to fit
	// the cluster. A 1-node/1-OSD cluster would otherwise get a size-3 pool
	// that can never reach active+clean.
	for _, nodes := range []int32{1, 2, 3, 5} {
		c := testCluster(withNodes(nodes), withCeph(1, 1, 1))
		if err := ValidateCeph(c); err != nil {
			t.Errorf("nodes=%d: default pool rejected: %v", nodes, err)
		}
		pools := effectivePools(c)
		if len(pools) != 1 {
			t.Fatalf("nodes=%d: expected 1 default pool, got %d", nodes, len(pools))
		}
		if pools[0].Size > nodes {
			t.Errorf("nodes=%d: default pool size %d exceeds OSD count %d",
				nodes, pools[0].Size, nodes)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// effectivePools
// ─────────────────────────────────────────────────────────────────────────────

func TestEffectivePools_Defaults(t *testing.T) {
	c := testCluster(withCeph(2, 3, 2)) // 6 OSDs
	pools := effectivePools(c)

	if len(pools) != 1 {
		t.Fatalf("expected one default pool, got %d", len(pools))
	}
	p := pools[0]
	if p.Name != pxv1.DefaultPoolName {
		t.Errorf("name: got %q, want %q", p.Name, pxv1.DefaultPoolName)
	}
	if p.Size != 3 {
		t.Errorf("size: got %d, want 3", p.Size)
	}
	if p.MinSize != 2 {
		t.Errorf("minSize: got %d, want 2", p.MinSize)
	}
	if !equalStrings(p.Content, []string{"images", "rootdir"}) {
		t.Errorf("content: got %v, want [images rootdir]", p.Content)
	}
}

func TestEffectivePools_FillsPerPoolDefaults(t *testing.T) {
	c := testCluster(withCeph(2, 3, 2), func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph.Pools = []pxv1.CephPoolSpec{
			{Name: "explicit", Size: 2, MinSize: 1, Content: []string{"images"}},
			{Name: "bare"}, // everything should be defaulted
		}
	})
	pools := effectivePools(c)
	if len(pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(pools))
	}

	// Explicit values must be preserved exactly.
	if pools[0].Size != 2 || pools[0].MinSize != 1 || !equalStrings(pools[0].Content, []string{"images"}) {
		t.Errorf("explicit pool was altered: %+v", pools[0])
	}
	// Bare pool gets sensible defaults.
	if pools[1].Size != 3 {
		t.Errorf("bare pool size: got %d, want 3", pools[1].Size)
	}
	if pools[1].MinSize != 2 {
		t.Errorf("bare pool minSize: got %d, want 2 (size-1)", pools[1].MinSize)
	}
	if !equalStrings(pools[1].Content, []string{"images", "rootdir"}) {
		t.Errorf("bare pool content: got %v", pools[1].Content)
	}
}

func TestEffectivePools_MinSizeNeverZero(t *testing.T) {
	// size=1 would give minSize = size-1 = 0, which Ceph rejects.
	c := testCluster(withCeph(1, 1, 1), func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph.Pools = []pxv1.CephPoolSpec{{Name: "single", Size: 1}}
	})
	pools := effectivePools(c)
	if pools[0].MinSize < 1 {
		t.Errorf("minSize must be at least 1, got %d", pools[0].MinSize)
	}
}

func TestPoolNames(t *testing.T) {
	c := testCluster(withCeph(1, 1, 1), func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph.Pools = []pxv1.CephPoolSpec{{Name: "a"}, {Name: "b"}}
	})
	if got := PoolNames(c); !equalStrings(got, []string{"a", "b"}) {
		t.Errorf("got %v, want [a b]", got)
	}
	if got := PoolNames(testCluster()); len(got) != 0 {
		t.Errorf("no ceph should yield no pools, got %v", got)
	}
}
