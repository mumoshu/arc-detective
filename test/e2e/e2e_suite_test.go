//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mumoshu/arc-detective/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/arc-detective:v0.0.1"
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind and CertManager.
//
// To skip CertManager installation, set: CERT_MANAGER_INSTALL_SKIP=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting arc-detective e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	setupCertManager()

	By("creating manager namespace")
	cmd = exec.Command("bash", "-c", fmt.Sprintf("kubectl create ns %s --dry-run=client -o yaml | kubectl apply -f -", namespace))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create namespace")

	By("labeling the namespace to enforce the restricted security policy")
	cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

	By("installing ARC EphemeralRunner CRD (stub)")
	cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/ephemeralrunner-crd.yaml")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install EphemeralRunner CRD")

	By("installing CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("deploying the controller-manager")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

	By("patching controller deployment with log storage volume")
	patchJSON := `[
		{"op":"replace","path":"/spec/template/spec/containers/0/volumeMounts","value":[{"name":"log-storage","mountPath":"/var/log/arc-detective"}]},
		{"op":"replace","path":"/spec/template/spec/volumes","value":[{"name":"log-storage","emptyDir":{}}]}
	]`
	cmd = exec.Command("kubectl", "patch", "deployment", "arc-detective-controller-manager",
		"-n", namespace, "--type=json", "-p", patchJSON)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to patch deployment")

	By("creating test namespace for synthetic resources")
	cmd = exec.Command("bash", "-c", fmt.Sprintf("kubectl create ns %s --dry-run=client -o yaml | kubectl apply -f -", testNS))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create test namespace")
})

var _ = AfterSuite(func() {
	By("cleaning up test namespace")
	cmd := exec.Command("kubectl", "delete", "ns", testNS, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	By("cleaning up the curl pod for metrics")
	cmd = exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	By("undeploying the controller-manager")
	cmd = exec.Command("make", "undeploy")
	_, _ = utils.Run(cmd)

	By("uninstalling CRDs")
	cmd = exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)

	By("removing ARC EphemeralRunner CRD (stub)")
	cmd = exec.Command("kubectl", "delete", "-f", "test/e2e/testdata/ephemeralrunner-crd.yaml", "--ignore-not-found")
	_, _ = utils.Run(cmd)

	By("removing manager namespace")
	cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	teardownCertManager()
})

// setupCertManager installs CertManager if needed for webhook tests.
// Skips installation if CERT_MANAGER_INSTALL_SKIP=true or if already present.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// Mark for cleanup before installation to handle interruptions and partial installs.
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by setupCertManager.
// This ensures we only remove what we installed.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
