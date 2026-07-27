package resources

// Shared test helpers.
//
// These tests are in-package (not _test package) deliberately: the parsing and
// phase-derivation logic worth testing hardest is unexported, and exporting it
// purely for tests would widen the API surface for no benefit.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// testCluster returns a minimal valid ProxmoxCluster, optionally mutated.
func testCluster(mutators ...func(*pxv1.ProxmoxCluster)) *pxv1.ProxmoxCluster {
	c := &pxv1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mp",
			Namespace: "ns",
		},
		Spec: pxv1.ProxmoxClusterSpec{
			Nodes:       3,
			ClusterName: "mp-test",
			Image:       "example/node:test",
		},
	}
	for _, m := range mutators {
		m(c)
	}
	return c
}

// withCeph enables Ceph with the given per-node OSD count and mon/mgr counts.
func withCeph(osdsPerNode, monCount, mgrCount int32) func(*pxv1.ProxmoxCluster) {
	return func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph = &pxv1.CephSpec{
			OSDsPerNode: osdsPerNode,
			MonCount:    monCount,
			MgrCount:    mgrCount,
			OSDSize:     "10Gi",
		}
	}
}

func withNodes(n int32) func(*pxv1.ProxmoxCluster) {
	return func(c *pxv1.ProxmoxCluster) { c.Spec.Nodes = n }
}

func strPtr(s string) *string { return &s }
func bPtr(b bool) *bool       { return &b }
