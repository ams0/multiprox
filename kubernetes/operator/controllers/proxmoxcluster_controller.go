package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
	"github.com/feynman/multiprox/operator/internal/exec"
	"github.com/feynman/multiprox/operator/internal/resources"
)

const (
	finalizerName = "proxmox.multiprox.io/finalizer"
	requeueShort  = 15 * time.Second
	requeueMedium = 30 * time.Second
	requeueLong   = 2 * time.Minute
)

// ProxmoxClusterReconciler reconciles a ProxmoxCluster object.
//
// Reconcile loop overview:
//  1. Fetch the ProxmoxCluster CR.
//  2. Handle deletion (finalizer cleanup).
//  3. Validate the Ceph spec against the cluster shape.
//  4. Ensure headless Service (Corosync DNS) + UI Service.
//  5. Ensure ConfigMap with configs and scripts.
//  6. Ensure StatefulSet exists and matches spec.
//  7. Gate on StatefulSet readiness.
//  8. Bootstrap the Proxmox cluster on node-0 (pvecm create).
//  9. Join nodes 1..N (pvecm add).
// 10. If Ceph requested: walk the pveceph plan, then poll health.
// 11. Update status phase and conditions.

// +kubebuilder:rbac:groups=proxmox.multiprox.io,resources=proxmoxclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxmox.multiprox.io,resources=proxmoxclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=proxmox.multiprox.io,resources=proxmoxclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=proxmox.multiprox.io,resources=proxmoxnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxmox.multiprox.io,resources=proxmoxnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

type ProxmoxClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RestConfig and KubeClient power pod exec, which is how the operator
	// drives pvecm and pveceph inside the PVE containers.
	RestConfig *rest.Config
	KubeClient kubernetes.Interface
}

func (r *ProxmoxClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("proxmoxcluster", req.NamespacedName)

	// ── 1. Fetch ──────────────────────────────────────────────────────────────
	cluster := &pxv1.ProxmoxCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ── 2. Deletion / finalizer ───────────────────────────────────────────────
	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster)
	}
	if !controllerutil.ContainsFinalizer(cluster, finalizerName) {
		if err := r.addFinalizer(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ── 3. Validate Ceph spec ─────────────────────────────────────────────────
	// Catches configurations that can never converge (e.g. pool size greater
	// than the total OSD count) before we provision anything.
	if err := resources.ValidateCeph(cluster); err != nil {
		logger.Error(err, "invalid ceph configuration")
		r.setCondition(cluster, pxv1.ConditionCephInitialized, false,
			"", "InvalidCephSpec", err.Error())
		if err2 := r.updateStatus(ctx, cluster, pxv1.PhaseFailed); err2 != nil {
			return ctrl.Result{}, err2
		}
		// Spec is wrong — requeueing won't help until the user edits it.
		return ctrl.Result{}, nil
	}

	// ── 4. Services ───────────────────────────────────────────────────────────
	if err := r.ensureService(ctx, cluster, resources.HeadlessService(cluster)); err != nil {
		return ctrl.Result{}, fmt.Errorf("headless service: %w", err)
	}
	if err := r.ensureService(ctx, cluster, resources.UIService(cluster)); err != nil {
		return ctrl.Result{}, fmt.Errorf("ui service: %w", err)
	}

	// ── 5. ConfigMap ──────────────────────────────────────────────────────────
	cm, err := resources.ConfigMap(cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build configmap: %w", err)
	}
	if err := r.ensureConfigMap(ctx, cluster, cm); err != nil {
		return ctrl.Result{}, fmt.Errorf("configmap: %w", err)
	}

	// ── 6. StatefulSet ────────────────────────────────────────────────────────
	if err := r.ensureStatefulSet(ctx, cluster, resources.StatefulSet(cluster)); err != nil {
		return ctrl.Result{}, fmt.Errorf("statefulset: %w", err)
	}

	// ── Refresh the ProxmoxNode projection on EVERY return path from here on ──
	//
	// Deferred rather than done at the end of the happy path, because the
	// projection is most valuable exactly when the cluster is NOT healthy:
	// "which node is stuck joining?", "which pod never became Ready?". Updating
	// it only once the cluster reached Ready would leave it stale throughout
	// scale-ups and outages — the moments you actually go looking at it.
	//
	// Best-effort by design: these objects are a read-only view, so a failure
	// here must never fail or block the cluster reconcile. Nothing in the
	// reconcile path reads them.
	defer func() {
		if err := r.reconcileNodes(ctx, cluster); err != nil {
			logger.Error(err, "failed to refresh ProxmoxNode projection (non-fatal)")
		}
	}()

	// ── 7. Gate on StatefulSet readiness ──────────────────────────────────────
	// The client is cache-backed, so a StatefulSet created moments ago in step 6
	// may not be visible yet. That is expected, not an error: treat NotFound as
	// "not observable yet" and requeue, otherwise every fresh cluster logs a
	// spurious reconcile error on its first pass.
	currentSTS := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, currentSTS); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("StatefulSet not yet visible in cache; requeueing")
			if err := r.updateStatus(ctx, cluster, pxv1.PhasePending); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		return ctrl.Result{}, err
	}
	stsReady := currentSTS.Status.ReadyReplicas == cluster.Spec.Nodes
	r.setCondition(cluster, pxv1.ConditionStatefulSetReady, stsReady,
		"AllPodsReady", "NotAllPodsReady",
		fmt.Sprintf("%d/%d pods ready", currentSTS.Status.ReadyReplicas, cluster.Spec.Nodes))

	if !stsReady {
		logger.Info("waiting for StatefulSet pods",
			"ready", currentSTS.Status.ReadyReplicas, "desired", cluster.Spec.Nodes)
		if err := r.updateStatus(ctx, cluster, pxv1.PhasePending); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	// ── 8. Cluster initialisation on node-0 ───────────────────────────────────
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, pxv1.ConditionClusterInitialized) {
		if err := r.updateStatus(ctx, cluster, pxv1.PhaseInitializing); err != nil {
			return ctrl.Result{}, err
		}
		out, err := r.execScript(ctx, cluster, 0, "/scripts/cluster-init.sh")
		if err != nil {
			logger.Error(err, "cluster-init failed", "output", out)
			r.setCondition(cluster, pxv1.ConditionClusterInitialized, false,
				"", "InitFailed", truncate(err.Error(), 400))
			if err2 := r.updateStatus(ctx, cluster, pxv1.PhaseFailed); err2 != nil {
				return ctrl.Result{}, err2
			}
			return ctrl.Result{RequeueAfter: requeueLong}, nil
		}
		r.setCondition(cluster, pxv1.ConditionClusterInitialized, true,
			"ClusterCreated", "", "pvecm create succeeded on node-0")
		cluster.Status.JoinedNodes = 1
		if err := r.updateStatus(ctx, cluster, pxv1.PhaseJoining); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	// ── 9. Join remaining nodes ───────────────────────────────────────────────
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, pxv1.ConditionAllNodesJoined) {
		if err := r.updateStatus(ctx, cluster, pxv1.PhaseJoining); err != nil {
			return ctrl.Result{}, err
		}
		joined := int32(1) // node-0 is in the cluster after init
		for i := int32(1); i < cluster.Spec.Nodes; i++ {
			out, err := r.execScript(ctx, cluster, i, "/scripts/cluster-join.sh")
			if err != nil {
				logger.Error(err, "cluster-join failed", "ordinal", i, "output", out)
				cluster.Status.JoinedNodes = joined
				if err2 := r.updateStatus(ctx, cluster, pxv1.PhaseJoining); err2 != nil {
					return ctrl.Result{}, err2
				}
				return ctrl.Result{RequeueAfter: requeueMedium}, nil
			}
			joined++
		}
		cluster.Status.JoinedNodes = joined
		r.setCondition(cluster, pxv1.ConditionAllNodesJoined, true,
			"AllNodesJoined", "", fmt.Sprintf("all %d nodes joined", cluster.Spec.Nodes))
		r.setCondition(cluster, pxv1.ConditionQuorumHealthy, true,
			"QuorumOK", "", "Corosync quorum established")
		if err := r.updateStatus(ctx, cluster, pxv1.PhaseJoining); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	// ── 10. Ceph via pveceph ──────────────────────────────────────────────────
	if cluster.Spec.Ceph != nil {
		done, res, err := r.reconcileCeph(ctx, cluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return res, nil
		}
	}

	// ── 11. Ready ─────────────────────────────────────────────────────────────
	r.setCondition(cluster, pxv1.ConditionQuorumHealthy, true,
		"QuorumOK", "", "Corosync quorum is healthy")
	if err := r.updateStatus(ctx, cluster, pxv1.PhaseReady); err != nil {
		return ctrl.Result{}, err
	}

	// The ProxmoxNode projection is refreshed by the deferred call registered
	// after step 6, so it stays current on every return path — not just this one.

	logger.Info("cluster is Ready", "nodes", cluster.Spec.Nodes)
	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ProxmoxNode projection
// ─────────────────────────────────────────────────────────────────────────────

// reconcileNodes maintains one ProxmoxNode object per PVE node.
//
// These objects are OBSERVED STATE ONLY. The operator writes them and never
// reads them back to make decisions — desired state lives in
// ProxmoxCluster.spec, and observed state is re-derived here from the
// Kubernetes API and from Proxmox itself. That discipline is what stops this
// projection from becoming a second, drift-prone source of truth.
//
// Cost control: per-node facts come from ONE exec on node-0, not one exec per
// node, and each object is patched only when its content actually changed.
func (r *ProxmoxClusterReconciler) reconcileNodes(
	ctx context.Context,
	cluster *pxv1.ProxmoxCluster,
) error {
	logger := log.FromContext(ctx)
	now := metav1.Now()

	// ── Proxmox-side facts: a single exec for the whole cluster ──────────────
	inventory := map[string]resources.NodeInventory{}
	out, err := r.execScript(ctx, cluster, 0, "/scripts/cluster-inventory.sh")
	if err != nil {
		// Degrade gracefully: still project pod-level state so the objects
		// exist and show something useful.
		logger.V(1).Info("cluster-inventory failed; projecting pod state only",
			"error", err.Error())
	} else {
		inventory = resources.ParseInventory(out)
	}

	// ── Kubernetes-side facts: from the cached informer, effectively free ────
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":     "proxmox-cluster",
			"app.kubernetes.io/instance": cluster.Name,
		},
	); err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	podsByName := make(map[string]*corev1.Pod, len(podList.Items))
	for i := range podList.Items {
		podsByName[podList.Items[i].Name] = &podList.Items[i]
	}

	// ── Create or update one object per desired node ─────────────────────────
	desired := make(map[string]struct{}, cluster.Spec.Nodes)
	for ordinal := int32(0); ordinal < cluster.Spec.Nodes; ordinal++ {
		name := resources.NodeObjectName(cluster, ordinal)
		desired[name] = struct{}{}

		var invPtr *resources.NodeInventory
		if inv, ok := inventory[name]; ok {
			invPtr = &inv
		}

		node := resources.ProxmoxNodeFor(cluster, ordinal, podsByName[name], invPtr, now)
		if err := controllerutil.SetControllerReference(cluster, node, r.Scheme); err != nil {
			return fmt.Errorf("set owner on ProxmoxNode %s: %w", name, err)
		}

		if err := r.upsertNode(ctx, node); err != nil {
			// One bad node should not abort the rest of the projection.
			logger.Error(err, "failed to upsert ProxmoxNode", "node", name)
		}
	}

	// ── Prune objects for nodes that no longer exist ─────────────────────────
	// OwnerReferences garbage-collect these when the whole cluster is deleted,
	// but scale-down needs explicit cleanup or stale nodes linger indefinitely
	// and misreport a cluster shape that is no longer real.
	existing := &pxv1.ProxmoxNodeList{}
	if err := r.List(ctx, existing,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"multiprox.io/cluster": cluster.Name},
	); err != nil {
		return fmt.Errorf("list ProxmoxNodes: %w", err)
	}
	for i := range existing.Items {
		obj := &existing.Items[i]
		if _, keep := desired[obj.Name]; keep {
			continue
		}
		logger.Info("pruning ProxmoxNode for removed node", "node", obj.Name)
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to prune ProxmoxNode", "node", obj.Name)
		}
	}

	return nil
}

// upsertNode creates the ProxmoxNode or patches it only when something changed.
//
// Skipping no-op writes matters: this runs for every node on every reconcile,
// so unconditional patching would multiply API server write load by the node
// count for no benefit.
func (r *ProxmoxClusterReconciler) upsertNode(ctx context.Context, desired *pxv1.ProxmoxNode) error {
	existing := &pxv1.ProxmoxNode{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desired.Name,
		Namespace: desired.Namespace,
	}, existing)

	if apierrors.IsNotFound(err) {
		status := desired.Status // Create drops status; it is a subresource
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		// desired now carries the server-assigned UID/resourceVersion.
		desired.Status = status
		return r.Status().Update(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}

	// Spec holds immutable identity; it should never need updating, but keep
	// metadata (labels/annotations) current in case the operator adds new ones.
	if !equality.Semantic.DeepEqual(existing.Labels, desired.Labels) ||
		!equality.Semantic.DeepEqual(existing.Annotations, desired.Annotations) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Labels = desired.Labels
		existing.Annotations = desired.Annotations
		if err := r.Patch(ctx, existing, patch); err != nil {
			return fmt.Errorf("patch metadata: %w", err)
		}
	}

	// Merge fixes up condition transition times against the stored object and
	// tells us whether anything meaningful actually differs.
	merged, changed := resources.MergeNodeStatus(existing.Status, desired.Status)
	if !changed {
		return nil // skip the write entirely
	}

	existing.Status = merged
	return r.Status().Update(ctx, existing)
}

// ─────────────────────────────────────────────────────────────────────────────
// Ceph reconciliation
// ─────────────────────────────────────────────────────────────────────────────

// reconcileCeph walks the pveceph bootstrap plan, then polls Ceph health.
//
// Returns done=true only when Ceph is usable (all expected OSDs up and health
// is not HEALTH_ERR). Every script in the plan is idempotent, so the whole plan
// is replayed on each pass; completed steps exit early and cost almost nothing.
func (r *ProxmoxClusterReconciler) reconcileCeph(
	ctx context.Context,
	cluster *pxv1.ProxmoxCluster,
) (done bool, res ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	if err := r.updateStatus(ctx, cluster, pxv1.PhaseConfiguringCeph); err != nil {
		return false, ctrl.Result{}, err
	}

	// Walk the plan in order. Stop at the first failure and requeue — the next
	// pass resumes from the same point because the earlier steps no-op.
	for _, task := range resources.CephPlan(cluster) {
		out, execErr := r.execScript(ctx, cluster, task.Ordinal, task.Script)
		if execErr != nil {
			logger.Error(execErr, "ceph step failed",
				"step", task.Step, "ordinal", task.Ordinal, "output", truncate(out, 800))

			condType := cephStepCondition(task.Step)
			r.setCondition(cluster, condType, false, "", string(task.Step)+"Failed",
				truncate(fmt.Sprintf("pod-%d: %v", task.Ordinal, execErr), 400))
			if err := r.updateStatus(ctx, cluster, pxv1.PhaseConfiguringCeph); err != nil {
				return false, ctrl.Result{}, err
			}
			return false, ctrl.Result{RequeueAfter: requeueMedium}, nil
		}
		logger.V(1).Info("ceph step ok", "step", task.Step, "ordinal", task.Ordinal)
	}

	// Plan completed — mark the structural conditions true.
	r.setCondition(cluster, pxv1.ConditionCephInitialized, true,
		"CephInitialized", "", "pveceph init completed; /etc/pve/ceph.conf present")
	// Report the EFFECTIVE counts, not the requested ones. monCount/mgrCount are
	// capped at the node count and monitors snapped down to an odd number, so a
	// 2-node cluster asking for the default 3 monitors gets 1. Echoing the
	// request would hide that.
	effMons := resources.EffectiveMonCount(cluster)
	effMgrs := resources.EffectiveMgrCount(cluster)
	monMsg := fmt.Sprintf("%d monitor(s) and %d manager(s) created", effMons, effMgrs)
	if cluster.Spec.Ceph.MonCount != effMons {
		monMsg += fmt.Sprintf(" (monCount %d adjusted to %d: capped at %d nodes, kept odd for quorum)",
			cluster.Spec.Ceph.MonCount, effMons, cluster.Spec.Nodes)
	}
	r.setCondition(cluster, pxv1.ConditionCephMonsReady, true,
		"MonsCreated", "", monMsg)
	r.setCondition(cluster, pxv1.ConditionCephPoolsReady, true,
		"PoolsCreated", "",
		fmt.Sprintf("pools reconciled: %v", resources.PoolNames(cluster)))

	// Poll health and OSD counts.
	statusTask := resources.StatusTask()
	out, execErr := r.execScript(ctx, cluster, statusTask.Ordinal, statusTask.Script)
	if execErr != nil {
		logger.Error(execErr, "ceph-status failed", "output", truncate(out, 400))
		return false, ctrl.Result{RequeueAfter: requeueMedium}, nil
	}

	cephStatus := resources.ParseCephStatus(cluster, out)
	cluster.Status.Ceph = cephStatus

	osdsReady := cephStatus.OSDsReady >= cephStatus.OSDsExpected
	r.setCondition(cluster, pxv1.ConditionCephOSDsReady, osdsReady,
		"AllOSDsUp", "OSDsNotReady",
		fmt.Sprintf("%d/%d OSDs up", cephStatus.OSDsReady, cephStatus.OSDsExpected))

	usable := resources.CephUsable(cephStatus)
	healthMsg := cephStatus.Health
	if cephStatus.HealthDetail != "" {
		healthMsg = cephStatus.Health + ": " + cephStatus.HealthDetail
	}
	r.setCondition(cluster, pxv1.ConditionCephHealthy, usable,
		"CephUsable", "CephNotUsable", healthMsg)

	if !usable {
		logger.Info("waiting for Ceph to converge",
			"health", cephStatus.Health,
			"osds", cephStatus.OSDSummary)
		if err := r.updateStatus(ctx, cluster, pxv1.PhaseConfiguringCeph); err != nil {
			return false, ctrl.Result{}, err
		}
		return false, ctrl.Result{RequeueAfter: requeueMedium}, nil
	}

	logger.Info("Ceph is usable",
		"health", cephStatus.Health,
		"osds", cephStatus.OSDSummary,
		"pools", cephStatus.Pools)
	return true, ctrl.Result{}, nil
}

// cephStepCondition maps a plan step to the status condition it affects.
func cephStepCondition(step resources.CephStep) string {
	switch step {
	case resources.StepCephInit:
		return pxv1.ConditionCephInitialized
	case resources.StepCephMon, resources.StepCephMgr:
		return pxv1.ConditionCephMonsReady
	case resources.StepCephOSD:
		return pxv1.ConditionCephOSDsReady
	case resources.StepCephPool:
		return pxv1.ConditionCephPoolsReady
	default:
		return pxv1.ConditionCephHealthy
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Pod exec
// ─────────────────────────────────────────────────────────────────────────────

// execScript runs a script inside the pve container of pod <ordinal> and
// returns its combined output. An error is returned when the pod is not
// running or the command exits non-zero.
func (r *ProxmoxClusterReconciler) execScript(
	ctx context.Context,
	cluster *pxv1.ProxmoxCluster,
	ordinal int32,
	script string,
) (string, error) {
	podName := fmt.Sprintf("%s-%d", cluster.Name, ordinal)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: podName}, pod); err != nil {
		return "", fmt.Errorf("get pod %s: %w", podName, err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return "", fmt.Errorf("pod %s is %s, not Running", podName, pod.Status.Phase)
	}

	return exec.Run(ctx, exec.Request{
		RestConfig: r.RestConfig,
		KubeClient: r.KubeClient,
		Namespace:  cluster.Namespace,
		Pod:        podName,
		Container:  "pve",
		Command:    []string{"bash", script},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Ensure helpers (create-or-patch pattern)
// ─────────────────────────────────────────────────────────────────────────────

func (r *ProxmoxClusterReconciler) ensureService(ctx context.Context, cluster *pxv1.ProxmoxCluster, desired *corev1.Service) error {
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Only patch fields the operator owns; leave clusterIP and friends alone.
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	return r.Patch(ctx, existing, patch)
}

func (r *ProxmoxClusterReconciler) ensureConfigMap(ctx context.Context, cluster *pxv1.ProxmoxCluster, desired *corev1.ConfigMap) error {
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	return r.Patch(ctx, existing, patch)
}

func (r *ProxmoxClusterReconciler) ensureStatefulSet(ctx context.Context, cluster *pxv1.ProxmoxCluster, desired *appsv1.StatefulSet) error {
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Patch replicas and pod template only. volumeClaimTemplates are immutable
	// on an existing StatefulSet — changing osdsPerNode or osdSize requires
	// recreating the StatefulSet, which we deliberately do not do implicitly
	// because it would orphan OSD data.
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	return r.Patch(ctx, existing, patch)
}

// ─────────────────────────────────────────────────────────────────────────────
// Deletion
// ─────────────────────────────────────────────────────────────────────────────

func (r *ProxmoxClusterReconciler) reconcileDelete(ctx context.Context, cluster *pxv1.ProxmoxCluster) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("deleting ProxmoxCluster", "name", cluster.Name)
	// StatefulSet, Services, ConfigMap and ProxmoxNodes are garbage-collected
	// via ownerReferences. PVCs created from volumeClaimTemplates are
	// deliberately retained by Kubernetes so OSD data survives accidental CR
	// deletion; delete them manually to reclaim the space.
	if err := r.removeFinalizer(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// addFinalizer and removeFinalizer re-fetch before mutating metadata, for the
// same staleness reason as updateStatus: status writes elsewhere in the pass
// bump resourceVersion, so a metadata update built on the original copy would
// conflict.
func (r *ProxmoxClusterReconciler) addFinalizer(ctx context.Context, cluster *pxv1.ProxmoxCluster) error {
	key := client.ObjectKeyFromObject(cluster)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &pxv1.ProxmoxCluster{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		if !controllerutil.AddFinalizer(latest, finalizerName) {
			return nil // already present
		}
		if err := r.Update(ctx, latest); err != nil {
			return err
		}
		cluster.ResourceVersion = latest.ResourceVersion
		cluster.Finalizers = latest.Finalizers
		return nil
	})
}

func (r *ProxmoxClusterReconciler) removeFinalizer(ctx context.Context, cluster *pxv1.ProxmoxCluster) error {
	key := client.ObjectKeyFromObject(cluster)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &pxv1.ProxmoxCluster{}
		if err := r.Get(ctx, key, latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // already gone
			}
			return err
		}
		if !controllerutil.RemoveFinalizer(latest, finalizerName) {
			return nil // already removed
		}
		return r.Update(ctx, latest)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Status helpers
// ─────────────────────────────────────────────────────────────────────────────

// updateStatus writes the accumulated status, tolerating concurrent modification.
//
// A single Reconcile pass legitimately writes status several times as it moves
// through phases (Pending → Initializing → Joining → …). Each write bumps
// resourceVersion on the server, which leaves the in-memory object stale and
// makes the next write in the same pass fail with a conflict. Re-fetch the
// latest object and re-apply the status we computed, retrying on conflict.
func (r *ProxmoxClusterReconciler) updateStatus(ctx context.Context, cluster *pxv1.ProxmoxCluster, phase pxv1.Phase) error {
	cluster.Status.Phase = phase
	cluster.Status.ObservedGeneration = cluster.Generation

	desired := cluster.Status.DeepCopy()
	key := client.ObjectKeyFromObject(cluster)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &pxv1.ProxmoxCluster{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		latest.Status = *desired.DeepCopy()
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}
		// Keep the caller's copy usable for subsequent writes in this pass.
		cluster.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

func (r *ProxmoxClusterReconciler) setCondition(
	cluster *pxv1.ProxmoxCluster,
	condType string,
	status bool,
	trueReason, falseReason string,
	message string,
) {
	condStatus := metav1.ConditionTrue
	reason := trueReason
	if !status {
		condStatus = metav1.ConditionFalse
		reason = falseReason
	}
	if reason == "" {
		reason = "Unknown"
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cluster.Generation,
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ─────────────────────────────────────────────────────────────────────────────
// SetupWithManager
// ─────────────────────────────────────────────────────────────────────────────

func (r *ProxmoxClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pxv1.ProxmoxCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
