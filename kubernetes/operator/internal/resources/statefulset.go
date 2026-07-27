package resources

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// OSDPVCName returns the volumeClaimTemplate name for OSD disk i.
// The resulting per-pod PVC is <name>-<cluster>-<ordinal>, e.g.
// ceph-osd-0-mycluster-2 (OSD disk 0 on PVE node 2).
func OSDPVCName(i int32) string {
	return fmt.Sprintf("ceph-osd-%d", i)
}

// OSDDevicePath returns the in-container block device path for OSD disk i,
// e.g. /dev/ceph-osd-0.
func OSDDevicePath(cluster *pxv1.ProxmoxCluster, i int32) string {
	prefix := "/dev/ceph-osd-"
	if cluster.Spec.Ceph != nil && cluster.Spec.Ceph.OSDDevicePrefix != "" {
		prefix = cluster.Spec.Ceph.OSDDevicePrefix
	}
	return fmt.Sprintf("%s%d", prefix, i)
}

// StatefulSet returns the StatefulSet for the Proxmox VE cluster nodes.
//
// Design:
//   - Each pod is one PVE node. Pod names are <cluster>-0, <cluster>-1, …
//   - Pod hostnames map to the headless Service, giving stable DNS.
//   - Pods run systemd as PID 1 (privileged, tmpfs on /run, /tmp).
//   - System PVCs: cluster-data (/var/lib/pve-cluster), vm-storage (/var/lib/vz).
//   - Ceph OSD PVCs: raw-block volumes surfaced as /dev/ceph-osd-N via
//     volumeDevices. Proxmox's pveceph consumes them as whole disks.
func StatefulSet(cluster *pxv1.ProxmoxCluster) *appsv1.StatefulSet {
	labels := clusterLabels(cluster)
	podLbls := podLabels(cluster)

	replicas := cluster.Spec.Nodes
	image := cluster.Spec.Image
	pullPolicy := cluster.Spec.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}

	// Default resource requests — operator is usable without explicit limits.
	resources := cluster.Spec.Resources
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
	}

	cfgMap := ConfigMapName(cluster)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            &replicas,
			ServiceName:         HeadlessSvcName(cluster),
			PodManagementPolicy: appsv1.OrderedReadyPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: podLbls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLbls,
				},
				Spec: corev1.PodSpec{
					// Stable hostname within the headless service domain.
					Subdomain:        HeadlessSvcName(cluster),
					NodeSelector:     cluster.Spec.NodeSelector,
					Tolerations:      cluster.Spec.Tolerations,
					Affinity:         podAffinity(cluster),
					SecurityContext:  &corev1.PodSecurityContext{},
					ImagePullSecrets: cluster.Spec.ImagePullSecrets,

					// Init container: install the bootstrap script over the image's entrypoint.
					// This lets us inject node-bootstrap.sh from the ConfigMap without rebuilding.
					InitContainers: []corev1.Container{
						{
							Name:            "install-scripts",
							Image:           "busybox:stable",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"sh", "-c",
								"cp /scripts/* /usr/local/bin/ && chmod +x /usr/local/bin/*.sh",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "scripts", MountPath: "/scripts"},
								{Name: "bin-overlay", MountPath: "/usr/local/bin"},
							},
						},
					},

					Containers: []corev1.Container{
						{
							Name:            "pve",
							Image:           image,
							ImagePullPolicy: pullPolicy,
							Resources:       resources,

							// systemd as PID 1 + Ceph OSD LVM operations require privileged.
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},

							// node-bootstrap.sh is injected by the init container.
							Command: []string{"/usr/local/bin/node-bootstrap.sh"},

							Env: nodeEnv(cluster),

							Ports: []corev1.ContainerPort{
								{Name: "pve-ui", ContainerPort: 8006, Protocol: corev1.ProtocolTCP},
								{Name: "ssh", ContainerPort: 22, Protocol: corev1.ProtocolTCP},
								{Name: "corosync-0", ContainerPort: 5405, Protocol: corev1.ProtocolUDP},
								{Name: "corosync-1", ContainerPort: 5406, Protocol: corev1.ProtocolUDP},
							},

							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{
											"bash", "-c",
											"systemctl is-active pve-cluster && systemctl is-active pveproxy",
										},
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       15,
								FailureThreshold:    10,
							},

							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{
											"bash", "-c", "systemctl is-active pve-cluster",
										},
									},
								},
								InitialDelaySeconds: 120,
								PeriodSeconds:       30,
								FailureThreshold:    5,
							},

							VolumeMounts: []corev1.VolumeMount{
								// System PVCs (via volumeClaimTemplates below)
								{Name: "cluster-data", MountPath: "/var/lib/pve-cluster"},
								{Name: "vm-storage", MountPath: "/var/lib/vz"},
								// Scripts from ConfigMap
								{Name: "scripts", MountPath: "/scripts"},
								{Name: "bin-overlay", MountPath: "/usr/local/bin"},
								// Corosync + datacenter config
								{Name: "pve-config", MountPath: "/etc/pve-bootstrap"},
								// systemd needs tmpfs
								{Name: "run", MountPath: "/run"},
								{Name: "run-lock", MountPath: "/run/lock"},
								{Name: "tmp", MountPath: "/tmp"},
								// cgroup v2
								{Name: "cgroup", MountPath: "/sys/fs/cgroup"},
							},

							// Ceph OSD raw-block devices — NOT filesystem mounts.
							// Each appears as a whole block device inside the container.
							VolumeDevices: osdVolumeDevices(cluster),
						},
					},

					Volumes: []corev1.Volume{
						{
							Name: "scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: cfgMap},
									DefaultMode:          int32Ptr(0755),
								},
							},
						},
						{
							Name: "pve-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: cfgMap},
									Items: []corev1.KeyToPath{
										{Key: "datacenter.cfg", Path: "datacenter.cfg"},
										{Key: "corosync.conf", Path: "corosync.conf"},
									},
								},
							},
						},
						// Shared emptyDir for the init container to drop scripts into /usr/local/bin
						{Name: "bin-overlay", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						// tmpfs volumes required by systemd
						{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
						{Name: "run-lock", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
						// cgroup v2 from the host
						{
							Name: "cgroup",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys/fs/cgroup",
									Type: hostPathTypePtr(corev1.HostPathDirectory),
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates(cluster),
		},
	}

	return sts
}

// nodeEnv builds the environment for the PVE container.
func nodeEnv(cluster *pxv1.ProxmoxCluster) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{
			Name: "NODE_ORDINAL",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.labels['apps.kubernetes.io/pod-index']",
				},
			},
		},
		{
			Name: "ROOT_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cluster.Name + "-secret",
					},
					Key:      "root-password",
					Optional: boolPtr(true),
				},
			},
		},
	}

	if cluster.Spec.Ceph != nil {
		env = append(env,
			corev1.EnvVar{
				Name:  "CEPH_OSDS_PER_NODE",
				Value: fmt.Sprintf("%d", cluster.Spec.Ceph.OSDsPerNode),
			},
			corev1.EnvVar{
				Name:  "CEPH_OSD_DEVICE_PREFIX",
				Value: cephDevicePrefix(cluster),
			},
			corev1.EnvVar{Name: "CEPH_NETWORK", Value: cluster.Spec.Ceph.Network},
			corev1.EnvVar{Name: "CEPH_CLUSTER_NETWORK", Value: cluster.Spec.Ceph.ClusterNetwork},
		)
	}

	return env
}

// osdVolumeDevices maps each OSD block PVC into the container as a raw device.
// Returns nil when Ceph is disabled.
func osdVolumeDevices(cluster *pxv1.ProxmoxCluster) []corev1.VolumeDevice {
	if cluster.Spec.Ceph == nil {
		return nil
	}
	n := cluster.Spec.Ceph.OSDsPerNode
	if n < 1 {
		n = 1
	}
	devices := make([]corev1.VolumeDevice, 0, n)
	for i := int32(0); i < n; i++ {
		devices = append(devices, corev1.VolumeDevice{
			Name:       OSDPVCName(i),
			DevicePath: OSDDevicePath(cluster, i),
		})
	}
	return devices
}

// volumeClaimTemplates returns the system PVCs plus, when Ceph is enabled,
// one raw-block PVC per OSD disk.
func volumeClaimTemplates(cluster *pxv1.ProxmoxCluster) []corev1.PersistentVolumeClaim {
	storage := cluster.Spec.Storage

	clusterDataSize := resource.MustParse("2Gi")
	if storage.ClusterData.Size != "" {
		clusterDataSize = resource.MustParse(storage.ClusterData.Size)
	}

	vmStorageSize := resource.MustParse("100Gi")
	if storage.VMStorage.Size != "" {
		vmStorageSize = resource.MustParse(storage.VMStorage.Size)
	}

	fsMode := corev1.PersistentVolumeFilesystem

	pvcs := []corev1.PersistentVolumeClaim{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-data"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: storage.ClusterData.StorageClass,
				VolumeMode:       &fsMode,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: clusterDataSize},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-storage"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: storage.VMStorage.StorageClass,
				VolumeMode:       &fsMode,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: vmStorageSize},
				},
			},
		},
	}

	pvcs = append(pvcs, osdClaimTemplates(cluster)...)
	return pvcs
}

// osdClaimTemplates builds one raw-block PVC template per OSD disk.
// volumeMode: Block is what makes the volume appear as a whole device that
// ceph-volume (via pveceph osd create) can claim and LVM-partition itself.
func osdClaimTemplates(cluster *pxv1.ProxmoxCluster) []corev1.PersistentVolumeClaim {
	if cluster.Spec.Ceph == nil {
		return nil
	}
	ceph := cluster.Spec.Ceph

	n := ceph.OSDsPerNode
	if n < 1 {
		n = 1
	}

	size := resource.MustParse("100Gi")
	if ceph.OSDSize != "" {
		size = resource.MustParse(ceph.OSDSize)
	}

	blockMode := corev1.PersistentVolumeBlock

	out := make([]corev1.PersistentVolumeClaim, 0, n)
	for i := int32(0); i < n; i++ {
		out = append(out, corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: OSDPVCName(i),
				Labels: map[string]string{
					"multiprox.io/role":      "ceph-osd",
					"multiprox.io/osd-index": fmt.Sprintf("%d", i),
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: ceph.OSDStorageClass,
				// Raw block — no filesystem. Required for Ceph OSDs.
				VolumeMode: &blockMode,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: size},
				},
			},
		})
	}
	return out
}

func cephDevicePrefix(cluster *pxv1.ProxmoxCluster) string {
	if cluster.Spec.Ceph != nil && cluster.Spec.Ceph.OSDDevicePrefix != "" {
		return cluster.Spec.Ceph.OSDDevicePrefix
	}
	return "/dev/ceph-osd-"
}

// podAffinity builds a default anti-affinity rule to spread PVE pods
// across distinct Kubernetes nodes for availability.
// The cluster spec's Affinity field overrides this entirely.
func podAffinity(cluster *pxv1.ProxmoxCluster) *corev1.Affinity {
	if cluster.Spec.Affinity != nil {
		return cluster.Spec.Affinity
	}
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: podLabels(cluster),
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}
}

func boolPtr(b bool) *bool                                       { return &b }
func int32Ptr(i int32) *int32                                    { return &i }
func hostPathTypePtr(t corev1.HostPathType) *corev1.HostPathType { return &t }
