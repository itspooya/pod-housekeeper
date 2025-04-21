package main

import (
	"context"
	"flag"
	"os"

	"github.com/itspooya/pod-housekeeper/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// controllerOwnerIndexField is the field used for indexing Pods by their controller OwnerReference UID.
const controllerOwnerIndexField = ".metadata.controller"

var (
	// Define Prometheus metrics as CounterVecs to allow labeling
	podsMarkedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pod_housekeeper_marked_total",
			Help: "Total number of pods marked for deletion by pod-housekeeper",
		},
		[]string{"namespace"}, // Define labels
	)
	podsDeletedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pod_housekeeper_deleted_total",
			Help: "Total number of pods deleted by pod-housekeeper",
		},
		[]string{"namespace"}, // Define labels
	)
)

func init() {
	log.SetLogger(zap.New())
	// Register custom metrics with the global controller-runtime prometheus registry
	metrics.Registry.MustRegister(podsMarkedCounter)
	metrics.Registry.MustRegister(podsDeletedCounter)
}

func main() {
	entryLog := log.Log.WithName("entrypoint")

	// --- Define flags ---
	// Use pflag for the application-specific config flag
	configFile := pflag.String("config", "", "Path to the configuration file (e.g., config.yaml). If not set, configuration is read from environment variables and defaults.")
	// Add flag for max concurrent reconciles
	_ = pflag.Int("max-concurrent-reconciles", 0, "Maximum number of concurrent Reconciles for the controller. Overrides config file/env var if set. Default 0 means use value from config/env.")

	// Prepare zap options and bind standard flags (will be needed for Task 1.3)
	opts := zap.Options{
		Development: false, // Default to production logging
	}
	opts.BindFlags(flag.CommandLine) // Bind Zap flags to the *standard* flag set

	// Add pflag wrapper for standard flags
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	// Parse all flags (standard + pflag)
	pflag.Parse()

	// Bind pflags to Viper *after* parsing and *before* loading config
	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		entryLog.Error(err, "Failed to bind pflags to viper")
		os.Exit(1)
	}

	// Set the logger *after* parsing flags
	log.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// --- Get self Pod Name and Namespace from environment (set by Downward API) ---
	podName := os.Getenv("POD_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")
	podInfoPresent := podName != "" && podNamespace != ""
	if !podInfoPresent {
		// Log info/warning instead of exiting
		entryLog.Info("POD_NAME or POD_NAMESPACE environment variable not set. Self-exclusion default depends on these.")
	}

	// Determine Leader Election Namespace
	leaderElectionNS := podNamespace // Use pod's namespace if available
	if leaderElectionNS == "" {
		leaderElectionNS = "pod-housekeeper-system" // Fallback default
		entryLog.Info("POD_NAMESPACE not set, using default leader election namespace", "namespace", leaderElectionNS)
	}

	// Pass the config file path from the flag and pod info presence
	cfg, err := config.LoadConfig(*configFile, podInfoPresent)
	if err != nil {
		entryLog.Error(err, "Failed to load configuration")
		os.Exit(1) // Exit if config loading itself fails
	}

	// --- End Configuration ---

	// Setup a Manager
	entryLog.Info("setting up manager")

	// Get Kubernetes config
	restConfig := ctrlconfig.GetConfigOrDie()

	// Adjust client QPS and Burst based on MaxConcurrentReconciles
	// Rule of thumb: QPS = Reconciles * AvgAPICallsPerReconcile, Burst = QPS * 1.5-2
	// Estimate 3-4 API calls per reconcile (Get, maybe List, maybe Patch/Delete)
	clientQPS := float32(cfg.MaxConcurrentReconciles * 4)
	clientBurst := int(clientQPS * 2)
	// Ensure minimum reasonable values
	if clientQPS < 5 {
		clientQPS = 5
	}
	if clientBurst < 10 {
		clientBurst = 10
	}
	restConfig.QPS = clientQPS
	restConfig.Burst = clientBurst
	entryLog.Info("Adjusted Kubernetes client rate limits", "qps", restConfig.QPS, "burst", restConfig.Burst, "basedOnMaxConcurrentReconciles", cfg.MaxConcurrentReconciles)

	mgr, err := manager.New(restConfig, manager.Options{
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		HealthProbeBindAddress:  ":8081",
		LeaderElection:          true,
		LeaderElectionID:        "pod-housekeeper-lock",
		LeaderElectionNamespace: leaderElectionNS, // Set the leader election namespace
	})
	if err != nil {
		entryLog.Error(err, "unable to set up overall controller manager")
		os.Exit(1)
	}

	// Register corev1 types with the manager's scheme
	if err := corev1.AddToScheme(mgr.GetScheme()); err != nil {
		entryLog.Error(err, "unable to add corev1 scheme")
		os.Exit(1)
	}

	// --- Setup Field Indexer BEFORE Controller Setup ---
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, controllerOwnerIndexField, func(o client.Object) []string {
		pod := o.(*corev1.Pod)
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			return nil
		}
		// If the Pod is controlled by this Controller Kind, return its UID
		// Note: You might want to adjust this if you expect owners other than specific Kinds (e.g., ReplicaSet, StatefulSet, Job)
		// For simplicity, we'll index by any controller owner's UID.
		return []string{string(owner.UID)}
	}); err != nil {
		entryLog.Error(err, "failed to create field index", "key", controllerOwnerIndexField)
		os.Exit(1)
	}

	// Setup a new controller to reconcile Pods
	entryLog.Info("Setting up controller")
	c, err := controller.New("pod-housekeeper", mgr, controller.Options{
		Reconciler: &reconcilePods{
			client:                  mgr.GetClient(),
			markDuration:            cfg.MarkDuration,
			deleteDuration:          cfg.DeleteDuration,
			excludedNamespaces:      cfg.ExcludedNamespaces,
			maxMarkedPerOwner:       cfg.MaxMarkedPerOwner,       // Pass default limit
			maxMarkedPerOwnerByKind: cfg.MaxMarkedPerOwnerByKind, // Pass kind-specific limits
			CheckExcludeAnnotation:  cfg.CheckExcludeAnnotation,  // Pass annotation check flag
			Recorder:                mgr.GetEventRecorderFor("pod-housekeeper-controller"),
			ExcludeSelf:             cfg.ExcludeSelf, // Pass final ExcludeSelf config
			PodName:                 podName,         // Pass controller's pod name
			PodNamespace:            podNamespace,    // Pass controller's pod namespace
		},
		MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
	})
	if err != nil {
		entryLog.Error(err, "unable to set up individual controller")
		os.Exit(1)
	}

	// Define predicate to filter events for non-excluded namespaces
	// Use TypedFuncs specific to *corev1.Pod
	// This allows specific handling for different event types (e.g., always processing deletes).
	namespacePredicate := predicate.TypedFuncs[*corev1.Pod]{
		CreateFunc: func(e event.TypedCreateEvent[*corev1.Pod]) bool {
			return !cfg.IsExcluded(e.Object.GetNamespace())
		},
		UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Pod]) bool {
			// Process update only if the namespace is not excluded AND Pod phase has changed.
			// This prevents reconciles for status updates that don't change the phase.
			return !cfg.IsExcluded(e.ObjectNew.GetNamespace()) &&
				!equality.Semantic.DeepEqual(e.ObjectOld.Status.Phase, e.ObjectNew.Status.Phase)
		},
		DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Pod]) bool {
			// Allow deletes to be processed regardless of exclusion,
			// as the pod might have been marked before being moved to an excluded ns.
			// The reconciler handles exclusion checks anyway.
			return true
		},
		GenericFunc: func(e event.TypedGenericEvent[*corev1.Pod]) bool {
			return !cfg.IsExcluded(e.Object.GetNamespace())
		},
	}

	// Watch Pods and enqueue the Pod object key directly, applying the namespace predicate
	if err := c.Watch(
		// Source: Kind watches Pods, uses the handler, and applies the predicate
		source.Kind(
			mgr.GetCache(),
			&corev1.Pod{},
			&handler.TypedEnqueueRequestForObject[*corev1.Pod]{},
			namespacePredicate,
		),
	); err != nil {
		entryLog.Error(err, "unable to watch Pods")
		os.Exit(1)
	}

	// Add readiness probe
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		entryLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Add liveness probe
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		entryLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("livez", healthz.Ping); err != nil {
		entryLog.Error(err, "unable to set up live check")
		os.Exit(1)
	}

	entryLog.Info("starting manager")
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		entryLog.Error(err, "unable to run manager")
		os.Exit(1)
	}
}
