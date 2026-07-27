package resources

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

func TestStatefulSet_BasicShape(t *testing.T) {
	c := testCluster()
	sts := StatefulSet(c)

	if sts.Name != "mp" || sts.Namespace != "ns" {
		t.Errorf("name/namespace: %s/%s", sts.Namespace, sts.Name)
	}
	if *sts.Spec.Replicas != 3 {
		t.Errorf("replicas: got %d, want 3", *sts.Spec.Replicas)
	}
	// The headless Service name must match, or per-pod DNS breaks and Corosync
	// cannot address peers.
	if sts.Spec.ServiceName != HeadlessSvcName(c) {
		t.Errorf("serviceName: got %q, want %q", sts.Spec.ServiceName, HeadlessSvcName(c))
	}
	// Ordered creation matters: node-0 must exist and bootstrap before others join.
	if sts.Spec.PodManagementPolicy != "OrderedReady" {
		t.Errorf("podManagementPolicy: got %q, want OrderedReady", sts.Spec.PodManagementPolicy)
	}
	if sts.Spec.Template.Spec.Subdomain != HeadlessSvcName(c) {
		t.Errorf("subdomain must match headless service for stable FQDNs, got %q",
			sts.Spec.Template.Spec.Subdomain)
	}
}

func TestStatefulSet_PrivilegedForSystemd(t *testing.T) {
	// PVE runs systemd as PID 1 and mounts /etc/pve via FUSE; both need
	// privileged. Ceph OSD creation additionally performs LVM operations.
	sts := StatefulSet(testCluster())
	sc := sts.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Fatal("pve container must be privileged")
	}
}

func TestStatefulSet_SystemVolumesArePresent(t *testing.T) {
	sts := StatefulSet(testCluster())
	ctr := sts.Spec.Template.Spec.Containers[0]

	mounts := map[string]string{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}

	want := map[string]string{
		"cluster-data": "/var/lib/pve-cluster",
		"vm-storage":   "/var/lib/vz",
		"run":          "/run",
		"run-lock":     "/run/lock",
		"tmp":          "/tmp",
		"cgroup":       "/sys/fs/cgroup",
	}
	for name, path := range want {
		got, ok := mounts[name]
		if !ok {
			t.Errorf("missing volumeMount %q", name)
			continue
		}
		if got != path {
			t.Errorf("mount %q: got %q, want %q", name, got, path)
		}
	}
}

func TestStatefulSet_NoOSDVolumesWhenCephDisabled(t *testing.T) {
	sts := StatefulSet(testCluster())

	if devs := sts.Spec.Template.Spec.Containers[0].VolumeDevices; len(devs) != 0 {
		t.Errorf("expected no volumeDevices without ceph, got %v", devs)
	}
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if strings.HasPrefix(vct.Name, "ceph-osd") {
			t.Errorf("unexpected OSD claim template %q without ceph", vct.Name)
		}
	}
	// Only the two system PVCs.
	if len(sts.Spec.VolumeClaimTemplates) != 2 {
		t.Errorf("claim templates: got %d, want 2", len(sts.Spec.VolumeClaimTemplates))
	}
}

func TestStatefulSet_OSDDisksAreRawBlock(t *testing.T) {
	// This is the single most important property of the Ceph disk model: OSDs
	// need whole raw devices. A filesystem volume silently produces a cluster
	// where no OSD can ever be created.
	c := testCluster(withCeph(3, 3, 2))
	sts := StatefulSet(c)

	byName := map[string]corev1.PersistentVolumeClaim{}
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		byName[vct.Name] = vct
	}

	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("ceph-osd-%d", i)
		vct, ok := byName[name]
		if !ok {
			t.Fatalf("missing claim template %q", name)
		}
		if vct.Spec.VolumeMode == nil {
			t.Fatalf("%s: volumeMode is nil — must be explicitly Block", name)
		}
		if *vct.Spec.VolumeMode != corev1.PersistentVolumeBlock {
			t.Errorf("%s: volumeMode got %q, want Block", name, *vct.Spec.VolumeMode)
		}
		if vct.Spec.AccessModes[0] != corev1.ReadWriteOnce {
			t.Errorf("%s: accessMode got %v, want RWO", name, vct.Spec.AccessModes)
		}
	}

	// System volumes must stay Filesystem.
	for _, name := range []string{"cluster-data", "vm-storage"} {
		vct := byName[name]
		if vct.Spec.VolumeMode == nil || *vct.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
			t.Errorf("%s: must be Filesystem, got %v", name, vct.Spec.VolumeMode)
		}
	}
}

func TestStatefulSet_OSDsAttachedAsDevicesNotMounts(t *testing.T) {
	c := testCluster(withCeph(2, 3, 2))
	sts := StatefulSet(c)
	ctr := sts.Spec.Template.Spec.Containers[0]

	if len(ctr.VolumeDevices) != 2 {
		t.Fatalf("volumeDevices: got %d, want 2", len(ctr.VolumeDevices))
	}
	for i, dev := range ctr.VolumeDevices {
		wantName := fmt.Sprintf("ceph-osd-%d", i)
		wantPath := fmt.Sprintf("/dev/ceph-osd-%d", i)
		if dev.Name != wantName {
			t.Errorf("device %d name: got %q, want %q", i, dev.Name, wantName)
		}
		if dev.DevicePath != wantPath {
			t.Errorf("device %d path: got %q, want %q", i, dev.DevicePath, wantPath)
		}
	}

	// An OSD appearing in volumeMounts would mean kubelet formats/mounts it,
	// destroying its usefulness as a raw device.
	for _, m := range ctr.VolumeMounts {
		if strings.HasPrefix(m.Name, "ceph-osd") {
			t.Errorf("OSD volume %q must not be a filesystem mount", m.Name)
		}
	}
}

func TestStatefulSet_OSDDevicePrefixOverride(t *testing.T) {
	c := testCluster(withCeph(2, 3, 2), func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph.OSDDevicePrefix = "/dev/nvme-osd"
	})
	sts := StatefulSet(c)
	devs := sts.Spec.Template.Spec.Containers[0].VolumeDevices

	if devs[0].DevicePath != "/dev/nvme-osd0" || devs[1].DevicePath != "/dev/nvme-osd1" {
		t.Errorf("prefix override not honoured: %v", devs)
	}
	// The helper used by the scripts must agree with the pod spec, or
	// ceph-osd.sh looks for devices that are not there.
	if OSDDevicePath(c, 1) != devs[1].DevicePath {
		t.Errorf("OSDDevicePath(%d)=%q disagrees with pod spec %q",
			1, OSDDevicePath(c, 1), devs[1].DevicePath)
	}
}

func TestStatefulSet_OSDSizeAndStorageClass(t *testing.T) {
	c := testCluster(withCeph(1, 3, 2), func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph.OSDSize = "250Gi"
		c.Spec.Ceph.OSDStorageClass = strPtr("topolvm-provisioner")
	})
	sts := StatefulSet(c)

	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if vct.Name != "ceph-osd-0" {
			continue
		}
		got := vct.Spec.Resources.Requests[corev1.ResourceStorage]
		want := resource.MustParse("250Gi")
		if got.Cmp(want) != 0 {
			t.Errorf("osd size: got %s, want 250Gi", got.String())
		}
		if vct.Spec.StorageClassName == nil || *vct.Spec.StorageClassName != "topolvm-provisioner" {
			t.Errorf("storageClass: got %v, want topolvm-provisioner", vct.Spec.StorageClassName)
		}
		return
	}
	t.Fatal("ceph-osd-0 claim template not found")
}

func TestStatefulSet_OSDsPerNodeZeroStillYieldsOneDisk(t *testing.T) {
	// Guard against a zero value producing a Ceph cluster with no disks at all,
	// which would look "configured" but never work.
	c := testCluster(withCeph(0, 1, 1))
	sts := StatefulSet(c)

	if got := len(sts.Spec.Template.Spec.Containers[0].VolumeDevices); got != 1 {
		t.Errorf("volumeDevices: got %d, want 1", got)
	}
	count := 0
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if strings.HasPrefix(vct.Name, "ceph-osd") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("osd claim templates: got %d, want 1", count)
	}
}

func TestStatefulSet_DefaultAntiAffinitySpreadsNodes(t *testing.T) {
	// Without this, all PVE nodes can land on one host and a single host
	// failure takes down the whole Proxmox cluster.
	sts := StatefulSet(testCluster())
	aff := sts.Spec.Template.Spec.Affinity

	if aff == nil || aff.PodAntiAffinity == nil {
		t.Fatal("expected a default pod anti-affinity")
	}
	terms := aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(terms) == 0 {
		t.Fatal("expected at least one preferred anti-affinity term")
	}
	if terms[0].PodAffinityTerm.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("topologyKey: got %q, want kubernetes.io/hostname",
			terms[0].PodAffinityTerm.TopologyKey)
	}
}

func TestStatefulSet_UserAffinityOverridesDefault(t *testing.T) {
	custom := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{TopologyKey: "topology.kubernetes.io/zone"},
			},
		},
	}
	c := testCluster(func(c *pxv1.ProxmoxCluster) { c.Spec.Affinity = custom })
	sts := StatefulSet(c)

	aff := sts.Spec.Template.Spec.Affinity
	if len(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("user affinity was not used verbatim")
	}
	if len(aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Error("default preferred term should not be merged into a user-supplied affinity")
	}
}

func TestStatefulSet_CephEnvOnlyWhenCephEnabled(t *testing.T) {
	envNames := func(c *pxv1.ProxmoxCluster) map[string]string {
		out := map[string]string{}
		for _, e := range StatefulSet(c).Spec.Template.Spec.Containers[0].Env {
			out[e.Name] = e.Value
		}
		return out
	}

	withoutCeph := envNames(testCluster())
	for _, key := range []string{"CEPH_OSDS_PER_NODE", "CEPH_OSD_DEVICE_PREFIX", "CEPH_NETWORK"} {
		if _, ok := withoutCeph[key]; ok {
			t.Errorf("%s must not be injected when ceph is disabled", key)
		}
	}
	// NODE_ORDINAL drives bootstrap-vs-join and is always required.
	if _, ok := withoutCeph["NODE_ORDINAL"]; !ok {
		t.Error("NODE_ORDINAL must always be injected")
	}

	c := testCluster(withCeph(4, 3, 2), func(c *pxv1.ProxmoxCluster) {
		c.Spec.Ceph.Network = "10.244.0.0/16"
	})
	withCephEnv := envNames(c)
	if withCephEnv["CEPH_OSDS_PER_NODE"] != "4" {
		t.Errorf("CEPH_OSDS_PER_NODE: got %q, want 4", withCephEnv["CEPH_OSDS_PER_NODE"])
	}
	if withCephEnv["CEPH_OSD_DEVICE_PREFIX"] != "/dev/ceph-osd-" {
		t.Errorf("CEPH_OSD_DEVICE_PREFIX: got %q", withCephEnv["CEPH_OSD_DEVICE_PREFIX"])
	}
	if withCephEnv["CEPH_NETWORK"] != "10.244.0.0/16" {
		t.Errorf("CEPH_NETWORK: got %q", withCephEnv["CEPH_NETWORK"])
	}
}

func TestStatefulSet_DefaultResourceRequests(t *testing.T) {
	// The operator must be usable without the user specifying resources.
	sts := StatefulSet(testCluster())
	req := sts.Spec.Template.Spec.Containers[0].Resources.Requests
	if req == nil {
		t.Fatal("expected default resource requests")
	}
	if _, ok := req[corev1.ResourceCPU]; !ok {
		t.Error("missing default cpu request")
	}
	if _, ok := req[corev1.ResourceMemory]; !ok {
		t.Error("missing default memory request")
	}
}

func TestStatefulSet_UserResourcesPreserved(t *testing.T) {
	c := testCluster(func(c *pxv1.ProxmoxCluster) {
		c.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}
	})
	req := StatefulSet(c).Spec.Template.Spec.Containers[0].Resources.Requests
	cpu := req[corev1.ResourceCPU]
	if cpu.String() != "2" {
		t.Errorf("user cpu request overwritten: got %s, want 2", cpu.String())
	}
}

func TestStatefulSet_ScriptsInstalledByInitContainer(t *testing.T) {
	// The pve container runs node-bootstrap.sh from /usr/local/bin, which is an
	// emptyDir populated by the init container from the ConfigMap. If that wiring
	// breaks the pod runs the image's entrypoint instead and never joins.
	sts := StatefulSet(testCluster())
	spec := sts.Spec.Template.Spec

	if len(spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(spec.InitContainers))
	}
	cmd := strings.Join(spec.Containers[0].Command, " ")
	if !strings.Contains(cmd, "node-bootstrap.sh") {
		t.Errorf("pve command should run node-bootstrap.sh, got %q", cmd)
	}

	hasOverlay := false
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.Name == "bin-overlay" && m.MountPath == "/usr/local/bin" {
			hasOverlay = true
		}
	}
	if !hasOverlay {
		t.Error("pve container must mount bin-overlay at /usr/local/bin")
	}
}

func TestStatefulSet_SystemStorageSizesAndClasses(t *testing.T) {
	c := testCluster(func(c *pxv1.ProxmoxCluster) {
		c.Spec.Storage = pxv1.StorageSpec{
			ClusterData: pxv1.PVCSpec{Size: "5Gi", StorageClass: strPtr("fast-ssd")},
			VMStorage:   pxv1.PVCSpec{Size: "2Ti", StorageClass: strPtr("bulk")},
		}
	})
	sts := StatefulSet(c)

	want := map[string]struct {
		size  string
		class string
	}{
		"cluster-data": {"5Gi", "fast-ssd"},
		"vm-storage":   {"2Ti", "bulk"},
	}

	for _, vct := range sts.Spec.VolumeClaimTemplates {
		w, ok := want[vct.Name]
		if !ok {
			continue
		}
		got := vct.Spec.Resources.Requests[corev1.ResourceStorage]
		if got.Cmp(resource.MustParse(w.size)) != 0 {
			t.Errorf("%s size: got %s, want %s", vct.Name, got.String(), w.size)
		}
		if vct.Spec.StorageClassName == nil || *vct.Spec.StorageClassName != w.class {
			t.Errorf("%s storageClass: got %v, want %s", vct.Name, vct.Spec.StorageClassName, w.class)
		}
		delete(want, vct.Name)
	}
	if len(want) != 0 {
		t.Errorf("claim templates not found: %v", want)
	}
}

func TestStatefulSet_OmittedStorageClassMeansClusterDefault(t *testing.T) {
	// A nil StorageClassName tells Kubernetes to use the default class. An
	// empty string would instead mean "no dynamic provisioning", which would
	// leave the PVCs unbound forever.
	sts := StatefulSet(testCluster())
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if vct.Spec.StorageClassName != nil {
			t.Errorf("%s: storageClassName should be nil when unset, got %q",
				vct.Name, *vct.Spec.StorageClassName)
		}
	}
}

func TestOSDPVCName(t *testing.T) {
	for i := int32(0); i < 3; i++ {
		want := fmt.Sprintf("ceph-osd-%d", i)
		if got := OSDPVCName(i); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
