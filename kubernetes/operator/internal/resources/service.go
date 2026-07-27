package resources

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
)

// HeadlessService returns the headless Service that gives each StatefulSet pod
// a stable DNS name: <pod>.<svc>.<namespace>.svc.cluster.local
// Corosync uses these names as knet ring0 addresses.
func HeadlessService(cluster *pxv1.ProxmoxCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HeadlessSvcName(cluster),
			Namespace: cluster.Namespace,
			Labels:    clusterLabels(cluster),
			Annotations: map[string]string{
				"multiprox.io/purpose": "corosync-headless",
			},
		},
		Spec: corev1.ServiceSpec{
			// ClusterIP: None → DNS returns all pod IPs, no load-balancing.
			ClusterIP: "None",
			Selector:  podLabels(cluster),
			Ports: []corev1.ServicePort{
				{Name: "corosync-0", Port: 5405, Protocol: corev1.ProtocolUDP},
				{Name: "corosync-1", Port: 5406, Protocol: corev1.ProtocolUDP},
				{Name: "ssh", Port: 22, Protocol: corev1.ProtocolTCP},
			},
			// Required so that pod hostnames resolve before readiness passes.
			PublishNotReadyAddresses: true,
		},
	}
}

// UIService returns a ClusterIP Service for the PVE web UI / REST API.
// Expose externally via an Ingress or LoadBalancer as needed.
func UIService(cluster *pxv1.ProxmoxCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      UISvcName(cluster),
			Namespace: cluster.Namespace,
			Labels:    clusterLabels(cluster),
			Annotations: map[string]string{
				"multiprox.io/purpose": "pve-ui",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: podLabels(cluster),
			Ports: []corev1.ServicePort{
				{
					Name:       "pve-ui",
					Port:       8006,
					TargetPort: intstr.FromInt(8006),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// HeadlessSvcName returns the name for the headless Service.
func HeadlessSvcName(cluster *pxv1.ProxmoxCluster) string {
	return cluster.Name + "-headless"
}

// UISvcName returns the name for the UI ClusterIP Service.
func UISvcName(cluster *pxv1.ProxmoxCluster) string {
	return cluster.Name + "-ui"
}

func clusterLabels(cluster *pxv1.ProxmoxCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "proxmox-cluster",
		"app.kubernetes.io/instance":   cluster.Name,
		"app.kubernetes.io/managed-by": "multiprox-operator",
	}
}

func podLabels(cluster *pxv1.ProxmoxCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "proxmox-cluster",
		"app.kubernetes.io/instance": cluster.Name,
	}
}
