package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/mumoshu/arc-detective/internal/controller"
	gh "github.com/mumoshu/arc-detective/internal/github"
	"github.com/mumoshu/arc-detective/internal/logcollector"
)

var (
	k8sClient  client.Client
	testEnv    *envtest.Environment
	ctx        context.Context
	cancel     context.CancelFunc
	mockGitHub *MockGitHubServer
	logDir     string
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	// Find CRD paths
	projectRoot := findProjectRoot()
	crdPaths := []string{
		filepath.Join(projectRoot, "config", "crd", "bases"),
	}

	// Add ARC CRDs if available
	arcCRDPath := filepath.Join(projectRoot, "test", "testdata", "crds")
	if _, err := os.Stat(arcCRDPath); err == nil {
		crdPaths = append(crdPaths, arcCRDPath)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: crdPaths,
		Scheme:            scheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic(err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	// Setup mock GitHub
	mockGitHub = NewMockGitHubServer()

	// Setup log storage
	logDir, err = os.MkdirTemp("", "arc-detective-test-logs-*")
	if err != nil {
		panic(err)
	}

	// Build and start the manager
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics
		},
	})
	if err != nil {
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}

	storage := logcollector.NewDiskStorage(logDir)
	collector := logcollector.NewPodLogCollector(clientset, storage)
	ghClient := gh.NewClient(gh.WithBaseURL(mockGitHub.URL), gh.WithPAT("fake-token"))

	// Register controllers
	podWatcher := controller.NewPodWatcher(mgr.GetClient(), collector)
	if err := podWatcher.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	erWatcher := controller.NewEphemeralRunnerWatcher(mgr.GetClient(), 1*time.Second, 5*time.Second)
	if err := erWatcher.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	correlator := controller.NewCorrelator(mgr.GetClient(), ghClient)
	if err := correlator.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	poller := controller.NewGitHubPoller(mgr.GetClient(), ghClient, 1*time.Second)
	if err := mgr.Add(poller); err != nil {
		panic(err)
	}

	cleanup := controller.NewCleanup(mgr.GetClient(), storage)
	if err := mgr.Add(cleanup); err != nil {
		panic(err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()

	// Wait for cache sync
	time.Sleep(1 * time.Second)

	// Run tests
	code := m.Run()

	// Cleanup
	cancel()
	mockGitHub.Close()
	_ = testEnv.Stop()
	_ = os.RemoveAll(logDir)

	os.Exit(code)
}

func findProjectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find project root")
		}
		dir = parent
	}
}
