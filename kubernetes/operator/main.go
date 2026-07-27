package main

import (
	"flag"
	"os"

	// Standard library
	_ "net/http/pprof"

	// Kubernetes
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	// Controller-runtime
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	// multiprox
	pxv1 "github.com/feynman/multiprox/operator/api/v1alpha1"
	"github.com/feynman/multiprox/operator/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(pxv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr             string
		probeAddr               string
		leaderElection          bool
		leaderElectionID        string
		maxConcurrentReconciles int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.BoolVar(&leaderElection, "leader-elect", true,
		"Enable leader election to ensure only one active controller instance.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "multiprox-operator.proxmox.multiprox.io",
		"Unique ID for leader election lease.")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 5,
		"Maximum number of ProxmoxClusters reconciled concurrently.")

	opts := zap.Options{Development: os.Getenv("DEV_MODE") == "true"}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// A typed clientset is required for pod exec — the controller-runtime
	// client has no exec subresource support. The operator drives pvecm and
	// pveceph inside the PVE containers over this path.
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to build kubernetes clientset")
		os.Exit(1)
	}

	if err = (&controllers.ProxmoxClusterReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		RestConfig: restConfig,
		KubeClient: kubeClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ProxmoxCluster")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting multiprox operator",
		"version", getVersion(),
		"leader-election", leaderElection,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func getVersion() string {
	if v := os.Getenv("OPERATOR_VERSION"); v != "" {
		return v
	}
	return "dev"
}
