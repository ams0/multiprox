package resources

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// ConfigMap contents
// ─────────────────────────────────────────────────────────────────────────────

func TestConfigMap_AlwaysPresentKeys(t *testing.T) {
	cm, err := ConfigMap(testCluster())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cm.Name != "mp-config" || cm.Namespace != "ns" {
		t.Errorf("name/namespace: %s/%s", cm.Namespace, cm.Name)
	}

	for _, key := range []string{
		"datacenter.cfg", "corosync.conf",
		"cluster-init.sh", "cluster-join.sh", "node-bootstrap.sh",
		"cluster-inventory.sh",
	} {
		if _, ok := cm.Data[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
}

func TestConfigMap_CephScriptsOnlyWhenCephEnabled(t *testing.T) {
	cephKeys := []string{
		"ceph-init.sh", "ceph-mon.sh", "ceph-mgr.sh",
		"ceph-osd.sh", "ceph-pool.sh", "ceph-status.sh",
	}

	// Shipping ceph scripts to a non-Ceph cluster is harmless but misleading;
	// their absence is a cheap signal that Ceph really is off.
	withoutCeph, err := ConfigMap(testCluster())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range cephKeys {
		if _, ok := withoutCeph.Data[key]; ok {
			t.Errorf("%q should not be present when ceph is disabled", key)
		}
	}

	withCephCM, err := ConfigMap(testCluster(withCeph(2, 3, 2)))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range cephKeys {
		if _, ok := withCephCM.Data[key]; !ok {
			t.Errorf("missing %q when ceph is enabled", key)
		}
	}
}

func TestConfigMap_ScriptsAreShebangedAndNonEmpty(t *testing.T) {
	cm, err := ConfigMap(testCluster(withCeph(1, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	for key, body := range cm.Data {
		if !strings.HasSuffix(key, ".sh") {
			continue
		}
		if len(strings.TrimSpace(body)) == 0 {
			t.Errorf("%s is empty", key)
			continue
		}
		if !strings.HasPrefix(body, "#!/usr/bin/env bash") {
			t.Errorf("%s missing bash shebang, starts with %q", key, firstLine(body))
		}
		// Every generated script sets a strict mode; a script that silently
		// continues past a failed pveceph call would report false success.
		if !strings.Contains(body, "set -e") && !strings.Contains(body, "set -u") {
			t.Errorf("%s does not set a strict shell mode", key)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestConfigMap_ClusterInitCarriesClusterName(t *testing.T) {
	c := testCluster(func(c *pxv1.ProxmoxCluster) { c.Spec.ClusterName = "prod-px" })
	cm, err := ConfigMap(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["cluster-init.sh"], `CLUSTER_NAME="prod-px"`) {
		t.Error("cluster-init.sh does not carry the configured cluster name")
	}
	// Idempotency guard: re-running must not try to recreate the cluster.
	if !strings.Contains(cm.Data["cluster-init.sh"], "pvecm status") {
		t.Error("cluster-init.sh lacks an idempotency guard on pvecm status")
	}
}

func TestConfigMap_ClusterJoinTargetsNodeZeroByDNS(t *testing.T) {
	cm, err := ConfigMap(testCluster())
	if err != nil {
		t.Fatal(err)
	}
	join := cm.Data["cluster-join.sh"]

	// Must address node-0 by its stable headless-Service FQDN, not a pod IP,
	// which changes on every restart.
	wantFQDN := "mp-0.mp-headless.ns.svc.cluster.local"
	if !strings.Contains(join, wantFQDN) {
		t.Errorf("cluster-join.sh should target %q", wantFQDN)
	}
	// pvecm add exchanges credentials over SSH, so waiting for :22 is required.
	if !strings.Contains(join, "22") {
		t.Error("cluster-join.sh should wait for SSH before running pvecm add")
	}
	if !strings.Contains(join, "pvecm status") {
		t.Error("cluster-join.sh lacks an idempotency guard")
	}
}

func TestConfigMap_NodeBootstrapUsesRoutableIP(t *testing.T) {
	cm, err := ConfigMap(testCluster())
	if err != nil {
		t.Fatal(err)
	}
	boot := cm.Data["node-bootstrap.sh"]

	// Corosync binding to loopback is a classic cluster-wide failure, so the
	// self entry in /etc/hosts must come from the pod's real address.
	if !strings.Contains(boot, "hostname -I") {
		t.Error("node-bootstrap.sh should derive the self IP from hostname -I")
	}
	if !strings.Contains(boot, "exec /lib/systemd/systemd") {
		t.Error("node-bootstrap.sh should exec systemd as the final step")
	}
}

func TestConfigMap_CephOSDScriptGuardsOnBlockDevices(t *testing.T) {
	cm, err := ConfigMap(testCluster(withCeph(2, 3, 2)))
	if err != nil {
		t.Fatal(err)
	}
	osd := cm.Data["ceph-osd.sh"]

	// The most likely misconfiguration is a filesystem StorageClass, which
	// leaves no block device. The script must detect that explicitly rather
	// than letting pveceph fail obscurely.
	if !strings.Contains(osd, "-b ") {
		t.Error("ceph-osd.sh should test that each device is a block device")
	}
	if !strings.Contains(osd, "volumeMode: Block") {
		t.Error("ceph-osd.sh should name volumeMode: Block in its diagnostic")
	}
	// Idempotency: an existing OSD must not be recreated on a later reconcile.
	if !strings.Contains(osd, "pvs") {
		t.Error("ceph-osd.sh should check for an existing LVM signature before creating an OSD")
	}
}

func TestConfigMap_CephPoolScriptEmitsOnePoolPerSpec(t *testing.T) {
	c := testCluster(withCeph(2, 3, 2), func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph.Pools = []pxv1.CephPoolSpec{
			{Name: "vmpool", Size: 3, MinSize: 2, Content: []string{"images"}},
			{Name: "ctpool", Size: 2, MinSize: 1, Content: []string{"rootdir"}},
			{Name: "hidden", Size: 2, MinSize: 1, AddAsStorage: bPtr(false)},
		}
	})
	cm, err := ConfigMap(c)
	if err != nil {
		t.Fatal(err)
	}
	pool := cm.Data["ceph-pool.sh"]

	for _, want := range []string{
		`create_pool "vmpool" 3 2 0 "true" "images"`,
		`create_pool "ctpool" 2 1 0 "true" "rootdir"`,
		`create_pool "hidden" 2 1 0 "false" "images,rootdir"`,
	} {
		if !strings.Contains(pool, want) {
			t.Errorf("ceph-pool.sh missing expected call:\n  %s", want)
		}
	}
	// addAsStorage=false must not register PVE storage.
	if !strings.Contains(pool, "pvesm add rbd") {
		t.Error("ceph-pool.sh should register pools as PVE storage")
	}
}

func TestConfigMap_DefaultPoolWhenNoneSpecified(t *testing.T) {
	cm, err := ConfigMap(testCluster(withCeph(1, 3, 2)))
	if err != nil {
		t.Fatal(err)
	}
	pool := cm.Data["ceph-pool.sh"]
	want := fmt.Sprintf(`create_pool "%s"`, pxv1.DefaultPoolName)
	if !strings.Contains(pool, want) {
		t.Errorf("expected a default pool call %q", want)
	}
}

func TestConfigMap_InventoryScriptIsReadOnly(t *testing.T) {
	cm, err := ConfigMap(testCluster())
	if err != nil {
		t.Fatal(err)
	}
	inv := cm.Data["cluster-inventory.sh"]

	// The inventory feeds a read-only projection; it must never mutate state.
	for _, forbidden := range []string{"pvecm create", "pvecm add", "pveceph osd create", "pvesm add"} {
		if strings.Contains(inv, forbidden) {
			t.Errorf("cluster-inventory.sh must not mutate state, found %q", forbidden)
		}
	}
	if !strings.Contains(inv, "pvecm nodes") {
		t.Error("cluster-inventory.sh should read membership from pvecm nodes")
	}
	// Output format must match what ParseInventory expects.
	if !strings.Contains(inv, "node=%s online=%s") {
		t.Error("cluster-inventory.sh output format does not match ParseInventory")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Corosync rendering
// ─────────────────────────────────────────────────────────────────────────────

func TestRenderCorosync_AllNodesAndDefaults(t *testing.T) {
	c := testCluster(withNodes(3))
	out, err := renderCorosync(c)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	if !strings.Contains(out, "cluster_name: mp-test") {
		t.Error("missing cluster_name")
	}
	// Defaults must be materialised even though the spec left them empty,
	// otherwise corosync.conf has blank values.
	for _, want := range []string{"transport:    knet", "crypto_cipher: aes256", "crypto_hash:  sha256"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing default %q in:\n%s", want, out)
		}
	}

	// Every node needs a nodelist entry with a 1-based nodeid and stable DNS.
	for i := 0; i < 3; i++ {
		addr := fmt.Sprintf("ring0_addr: mp-%d.mp-headless.ns.svc.cluster.local", i)
		if !strings.Contains(out, addr) {
			t.Errorf("missing %q", addr)
		}
		if !strings.Contains(out, fmt.Sprintf("nodeid:     %d", i+1)) {
			t.Errorf("missing nodeid %d", i+1)
		}
	}
	if strings.Contains(out, "nodeid:     0") {
		t.Error("corosync nodeids must be 1-based; found 0")
	}
}

func TestRenderCorosync_HonoursExplicitTransportSettings(t *testing.T) {
	c := testCluster(func(c *pxv1.ProxmoxCluster) {
		c.Spec.Corosync = pxv1.CorosyncSpec{Transport: "udpu", Crypto: "aes128", Hash: "sha512"}
	})
	out, err := renderCorosync(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"transport:    udpu", "crypto_cipher: aes128", "crypto_hash:  sha512"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRenderCorosync_SingleNode(t *testing.T) {
	out, err := renderCorosync(testCluster(withNodes(1)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "ring0_addr:") != 1 {
		t.Errorf("expected exactly one node entry, got:\n%s", out)
	}
}

func TestNodeDNS(t *testing.T) {
	c := testCluster()
	want := "mp-2.mp-headless.ns.svc.cluster.local"
	if got := nodeDNS(c, 2); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Services
// ─────────────────────────────────────────────────────────────────────────────

func TestHeadlessService(t *testing.T) {
	c := testCluster()
	svc := HeadlessService(c)

	if svc.Name != "mp-headless" {
		t.Errorf("name: got %q", svc.Name)
	}
	// ClusterIP None is what produces per-pod DNS records; anything else
	// load-balances and breaks Corosync addressing.
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("clusterIP: got %q, want None", svc.Spec.ClusterIP)
	}
	// Pods must resolve before they are Ready, or node-0 cannot be reached
	// during bootstrap and joins deadlock.
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("publishNotReadyAddresses must be true so peers resolve during bootstrap")
	}

	ports := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		ports[p.Name] = p.Port
	}
	if ports["ssh"] != 22 {
		t.Errorf("ssh port: got %d", ports["ssh"])
	}
	if ports["corosync-0"] != 5405 {
		t.Errorf("corosync-0 port: got %d, want 5405", ports["corosync-0"])
	}
	for _, p := range svc.Spec.Ports {
		if strings.HasPrefix(p.Name, "corosync") && p.Protocol != corev1.ProtocolUDP {
			t.Errorf("%s must be UDP, got %s", p.Name, p.Protocol)
		}
	}
}

func TestUIService(t *testing.T) {
	svc := UIService(testCluster())
	if svc.Name != "mp-ui" {
		t.Errorf("name: got %q", svc.Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("type: got %q, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8006 {
		t.Errorf("expected a single port 8006, got %v", svc.Spec.Ports)
	}
}

func TestServicesSelectTheSamePods(t *testing.T) {
	// If the two Services disagree on their selector, the UI can point at pods
	// that are not cluster members.
	c := testCluster()
	h := HeadlessService(c).Spec.Selector
	u := UIService(c).Spec.Selector

	if len(h) != len(u) {
		t.Fatalf("selectors differ in size: %v vs %v", h, u)
	}
	for k, v := range h {
		if u[k] != v {
			t.Errorf("selector mismatch on %q: headless=%q ui=%q", k, v, u[k])
		}
	}
	// And the StatefulSet's pod labels must actually match that selector.
	podLabels := StatefulSet(c).Spec.Template.ObjectMeta.Labels
	for k, v := range h {
		if podLabels[k] != v {
			t.Errorf("service selector %q=%q does not match pod label %q", k, v, podLabels[k])
		}
	}
}
