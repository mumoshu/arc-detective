package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	detectivev1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/mumoshu/arc-detective/internal/controller"
	gh "github.com/mumoshu/arc-detective/internal/github"
	"github.com/mumoshu/arc-detective/internal/logcollector"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(detectivev1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var logStoragePath string
	var ghToken string
	var ghBaseURL string
	var pollInterval time.Duration
	var stuckThreshold time.Duration
	var runningThreshold time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&logStoragePath, "log-storage-path", "/var/log/arc-detective", "Path to store collected pod logs.")
	flag.StringVar(&ghToken, "github-token", os.Getenv("GITHUB_TOKEN"), "GitHub PAT for API access.")
	flag.StringVar(&ghBaseURL, "github-base-url", "", "GitHub API base URL (for testing).")
	flag.DurationVar(&pollInterval, "poll-interval", 30*time.Second, "GitHub API poll interval.")
	flag.DurationVar(&stuckThreshold, "stuck-threshold", 5*time.Minute, "How long before a Failed EphemeralRunner triggers an investigation.")
	flag.DurationVar(&runningThreshold, "running-threshold", 30*time.Minute, "How long before a Running EphemeralRunner triggers an investigation.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "d2975cd7.arcdetective.io",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Register field index for Event.involvedObject.name so Correlator can
	// look up events by pod name through the cache.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Event{}, "involvedObject.name",
		func(obj client.Object) []string {
			evt := obj.(*corev1.Event)
			return []string{evt.InvolvedObject.Name}
		}); err != nil {
		setupLog.Error(err, "Failed to create field index for Events")
		os.Exit(1)
	}

	// Build dependencies
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create clientset")
		os.Exit(1)
	}

	storage := logcollector.NewDiskStorage(logStoragePath)
	collector := logcollector.NewPodLogCollector(clientset, storage)

	ghOpts := []gh.Option{gh.WithPAT(ghToken)}
	if ghBaseURL != "" {
		ghOpts = append(ghOpts, gh.WithBaseURL(ghBaseURL))
	}
	ghClient := gh.NewClient(ghOpts...)

	// Register controllers
	podWatcher := controller.NewPodWatcher(mgr.GetClient(), collector)
	if err := podWatcher.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "PodWatcher")
		os.Exit(1)
	}

	erWatcher := controller.NewEphemeralRunnerWatcher(mgr.GetClient(), stuckThreshold, runningThreshold)
	if err := erWatcher.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "EphemeralRunnerWatcher")
		os.Exit(1)
	}

	correlator := controller.NewCorrelator(mgr.GetClient(), ghClient)
	if err := correlator.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "Correlator")
		os.Exit(1)
	}

	// Register runnables (periodic timers)
	poller := controller.NewGitHubPoller(mgr.GetClient(), ghClient, pollInterval)
	if err := mgr.Add(poller); err != nil {
		setupLog.Error(err, "Failed to add GitHub poller")
		os.Exit(1)
	}

	cleanup := controller.NewCleanup(mgr.GetClient(), storage)
	if err := mgr.Add(cleanup); err != nil {
		setupLog.Error(err, "Failed to add cleanup controller")
		os.Exit(1)
	}

	// Health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// suppress unused import warnings for tls (needed for TLS config if enabled later)
var _ = tls.Config{}
