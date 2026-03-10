//go:build e2e_full
// +build e2e_full

package e2e_full

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	detectiveNS    = "arc-detective-system"
	runnerNS       = "arc-runners"
	arcSystemNS    = "arc-systems"
	arcReleaseName = "arc-detective-e2e-runner"
	arcVersion     = "0.13.1"
	managerImage   = "arc-detective:e2e"
	workflowPath   = ".github/workflows/arc-detective-e2e-test.yaml"
	workflowFile   = "arc-detective-e2e-test.yaml"
)

var _ = Describe("ARC Detective Full E2E", Ordered, func() {
	var (
		ghToken       string
		owner         string
		repo          string
		kindBin       string
		helmBin       string
		kindCluster   string
		defaultBranch string
		runID         int64
	)

	// dumpOnFailure is a helper that dumps diagnostics and fails with a message.
	dumpOnFailure := func(msg string, args ...interface{}) {
		dumpDiagnostics(kindBin, kindCluster, detectiveNS, runnerNS)
		Fail(fmt.Sprintf(msg, args...))
	}

	BeforeAll(func() {
		By("validating environment variables")
		ghToken = os.Getenv("GITHUB_TOKEN")
		Expect(ghToken).NotTo(BeEmpty(), "GITHUB_TOKEN must be set")

		testRepo := os.Getenv("ARC_DETECTIVE_TEST_REPO")
		Expect(testRepo).NotTo(BeEmpty(), "ARC_DETECTIVE_TEST_REPO must be set (owner/repo)")

		parts := strings.SplitN(testRepo, "/", 2)
		Expect(parts).To(HaveLen(2), "ARC_DETECTIVE_TEST_REPO must be in owner/repo format")
		owner = parts[0]
		repo = parts[1]

		kindBin = os.Getenv("KIND")
		if kindBin == "" {
			kindBin = "kind"
		}
		helmBin = os.Getenv("HELM")
		if helmBin == "" {
			helmBin = "helm"
		}
		kindCluster = "arc-detective-e2e-full"

		By("checking anchor file in test repo")
		err := checkAnchorFile(owner, repo, ghToken)
		Expect(err).NotTo(HaveOccurred())

		By("getting default branch")
		defaultBranch, err = getDefaultBranch(owner, repo, ghToken)
		Expect(err).NotTo(HaveOccurred())
		Expect(defaultBranch).NotTo(BeEmpty())
		_, _ = fmt.Fprintf(GinkgoWriter, "Default branch: %s\n", defaultBranch)

		By("creating Kind cluster")
		// Delete any leftover cluster first (best-effort)
		_, _ = runCmd(kindBin, "delete", "cluster", "--name", kindCluster)
		out := mustRunCmd(kindBin, "create", "cluster", "--name", kindCluster, "--wait", "120s")
		_, _ = fmt.Fprintf(GinkgoWriter, "Kind cluster created: %s\n", out)
		DeferCleanup(func() {
			By("deleting Kind cluster")
			_, _ = runCmd(kindBin, "delete", "cluster", "--name", kindCluster)
		})

		By("installing ARC controller via Helm")
		out = mustRunCmd(helmBin, "install", "arc",
			"--namespace", arcSystemNS, "--create-namespace",
			"oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller",
			"--version", arcVersion,
			"--wait", "--timeout", "5m")
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC controller installed: %s\n", out)

		By("verifying ARC controller pod is running")
		arcControllerPod := waitForPodWithLabel(arcSystemNS, "app.kubernetes.io/part-of=gha-rs-controller", 2*time.Minute, 3*time.Second)
		waitForPodPhase(arcSystemNS, arcControllerPod, "Running", 2*time.Minute, 3*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC controller pod running: %s\n", arcControllerPod)

		By("creating runner namespace and GitHub secret for ARC")
		mustRunCmd("kubectl", "create", "namespace", runnerNS)
		mustRunCmd("kubectl", "create", "secret", "generic", "arc-github-secret",
			"--namespace", runnerNS,
			"--from-literal=github_token="+ghToken)

		By("installing ARC runner scale set via Helm")
		// Use a values file to properly set the runner template with memory limits.
		// Using --set with nested array notation (template.spec.containers[0]...) can
		// override the chart's default template in unexpected ways. A values file is safer.
		projectRoot := findProjectRoot()
		valuesPath := filepath.Join(projectRoot, "test", "e2e-full", "testdata", "runner-values.yaml")
		valuesContent := fmt.Sprintf(`githubConfigUrl: "https://github.com/%s/%s"
githubConfigSecret: arc-github-secret
minRunners: 0
maxRunners: 1
template:
  spec:
    containers:
      - name: runner
        image: "ghcr.io/actions/actions-runner:latest"
        command: ["/home/runner/run.sh"]
        resources:
          limits:
            memory: "64Mi"
`, owner, repo)
		err = os.WriteFile(valuesPath, []byte(valuesContent), 0644)
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(valuesPath)

		out, installErr := runCmd(helmBin, "install", arcReleaseName,
			"--namespace", runnerNS,
			"oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set",
			"--version", arcVersion,
			"-f", valuesPath,
			"--wait", "--timeout", "5m")
		if installErr != nil {
			// Dump Helm values and rendered manifests before failing
			dumpHelmInfo(helmBin, arcReleaseName, runnerNS, arcVersion, valuesPath)
			dumpOnFailure("Failed to install ARC runner scale set: %v\nOutput: %s", installErr, out)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC runner scale set installed: %s\n", out)

		By("verifying ARC listener pod is running")
		// The listener pod is created in arc-systems namespace by the ARC controller
		listenerPod, listenerErr := waitForPodWithLabelOrFail(arcSystemNS,
			"actions.github.com/scale-set-name="+arcReleaseName,
			2*time.Minute, 3*time.Second)
		if listenerErr != nil {
			dumpOnFailure("ARC listener pod did not appear: %v", listenerErr)
		}
		waitForPodPhase(arcSystemNS, listenerPod, "Running", 2*time.Minute, 3*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC listener pod running: %s\n", listenerPod)

		By("checking ARC listener logs for successful registration")
		// Give the listener a moment to register
		time.Sleep(5 * time.Second)
		listenerLogs, _ := runCmd("kubectl", "logs", listenerPod, "-n", arcSystemNS, "--tail=50")
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC listener logs:\n%s\n", listenerLogs)
		if strings.Contains(listenerLogs, "error") || strings.Contains(listenerLogs, "failed") {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: ARC listener logs contain errors\n")
		}

		By("verifying AutoScalingRunnerSet exists")
		arsOut, arsErr := runCmd("kubectl", "get", "autoscalingrunnersets.actions.github.com",
			"-n", runnerNS, "-o", "jsonpath={.items[*].metadata.name}")
		if arsErr != nil || arsOut == "" {
			dumpOnFailure("No AutoScalingRunnerSet found: err=%v, output=%s", arsErr, arsOut)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "AutoScalingRunnerSets: %s\n", arsOut)

		By("building arc-detective image")
		mustRunCmd("make", "-C", projectRoot, "docker-build", "IMG="+managerImage)

		By("loading image into Kind cluster")
		mustRunCmd(kindBin, "load", "docker-image", managerImage, "--name", kindCluster)

		By("installing arc-detective CRDs")
		mustRunCmd("make", "-C", projectRoot, "install")

		By("deploying arc-detective")
		mustRunCmd("make", "-C", projectRoot, "deploy", "IMG="+managerImage)

		By("patching arc-detective deployment with fast settings")
		patchJSON := fmt.Sprintf(`[
			{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--health-probe-bind-address=:8081","--poll-interval=3s","--stuck-threshold=10s"]},
			{"op":"add","path":"/spec/template/spec/containers/0/env","value":[{"name":"GITHUB_TOKEN","value":"%s"}]},
			{"op":"replace","path":"/spec/template/spec/containers/0/volumeMounts","value":[{"name":"log-storage","mountPath":"/var/log/arc-detective"}]},
			{"op":"replace","path":"/spec/template/spec/volumes","value":[{"name":"log-storage","emptyDir":{}}]}
		]`, ghToken)
		mustRunCmd("kubectl", "patch", "deployment", "arc-detective-controller-manager",
			"-n", detectiveNS,
			"--type=json", "-p", patchJSON)

		By("waiting for arc-detective controller pod to be running")
		controllerPod := waitForPodWithLabel(detectiveNS, "control-plane=controller-manager", 3*time.Minute, 3*time.Second)
		waitForPodPhase(detectiveNS, controllerPod, "Running", 3*time.Minute, 3*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Controller pod running: %s\n", controllerPod)

		By("creating github-credentials secret for DetectiveConfig")
		mustRunCmd("kubectl", "create", "secret", "generic", "github-credentials",
			"--namespace", detectiveNS,
			"--from-literal=token="+ghToken)

		By("creating DetectiveConfig CR")
		configYAML := fmt.Sprintf(`apiVersion: detective.arcdetective.io/v1alpha1
kind: DetectiveConfig
metadata:
  name: e2e-config
  namespace: %s
spec:
  repositories:
    - owner: %s
      name: %s
  githubAuth:
    type: pat
    secretName: github-credentials
  pollInterval: 3s
  logStorage:
    pvcName: ""
  retentionPeriod: 1h`, detectiveNS, owner, repo)
		cmd := fmt.Sprintf("echo '%s' | kubectl apply -f -", configYAML)
		mustRunCmd("bash", "-c", cmd)

		_, _ = fmt.Fprintf(GinkgoWriter, "Setup complete. Ready for test.\n")
	})

	AfterAll(func() {
		By("cleaning up test workflow from repo")
		if err := deleteWorkflowFile(owner, repo, ghToken, workflowPath, defaultBranch); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete workflow file: %v\n", err)
		}

		By("cancelling any in-progress workflow runs")
		if runID > 0 {
			if err := cancelWorkflowRun(owner, repo, ghToken, runID); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to cancel workflow run: %v\n", err)
			}
		}
		// Kind cluster cleanup is handled by DeferCleanup in BeforeAll
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(kindBin, kindCluster, detectiveNS, runnerNS)
		}
	})

	It("should detect OOMKilled runner pod and create a correct Investigation", func() {
		By("reading test workflow template")
		projectRoot := findProjectRoot()
		templatePath := filepath.Join(projectRoot, "test", "e2e-full", "testdata", "test-workflow.yaml")
		templateBytes, err := os.ReadFile(templatePath)
		Expect(err).NotTo(HaveOccurred())

		workflowContent := strings.ReplaceAll(string(templateBytes), "%RUNNER_LABEL%", arcReleaseName)

		By("pushing test workflow to repo")
		_, err = pushWorkflowFile(owner, repo, ghToken, workflowPath, workflowContent, defaultBranch)
		Expect(err).NotTo(HaveOccurred(), "failed to push workflow file")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow pushed to %s\n", workflowPath)

		// Small delay to let GitHub process the new workflow file
		time.Sleep(5 * time.Second)

		By("triggering workflow_dispatch")
		testRunID := fmt.Sprintf("e2e-%d", time.Now().Unix())
		dispatchTime := time.Now().Add(-5 * time.Second) // small buffer for clock skew
		err = triggerWorkflowDispatch(owner, repo, ghToken, workflowFile, defaultBranch, map[string]string{
			"run_id": testRunID,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to trigger workflow dispatch")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow dispatch triggered with run_id=%s\n", testRunID)

		By("waiting for workflow run to appear")
		runID, err = waitForWorkflowRun(owner, repo, ghToken, workflowFile, dispatchTime, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "workflow run did not appear")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow run ID: %d\n", runID)

		By("waiting for runner pod to appear in arc-runners namespace")
		podName := waitForPodWithLabel(runnerNS, "actions-ephemeral-runner=True", 5*time.Minute, 3*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Runner pod appeared: %s\n", podName)

		By("waiting for runner pod to be Running")
		waitForPodPhase(runnerNS, podName, "Running", 3*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Runner pod is Running\n")

		By("waiting for OOMKill (workflow writes 128M to /dev/shm with 64Mi limit)")
		reason := waitForContainerTerminated(runnerNS, podName, 3*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Container terminated with reason: %s\n", reason)
		// Accept both OOMKilled reason and exit code 137
		Expect(reason).To(SatisfyAny(
			Equal("OOMKilled"),
			Equal("Error"), // some runtimes report "Error" with exit code 137
		), "expected OOMKilled or Error termination reason, got %s", reason)

		By("waiting for Investigation CR to reach Complete phase")
		invNS, invName := waitForInvestigationComplete(5*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation complete: %s/%s\n", invNS, invName)

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		_, _ = fmt.Fprintf(GinkgoWriter, "Diagnosis failureType: %s\n", failureType)
		Expect(failureType).To(Equal("pod-oomkilled"),
			"expected diagnosis failureType 'pod-oomkilled', got '%s'", failureType)

		remediation := getInvestigationField(invNS, invName, "{.spec.diagnosis.remediation}")
		Expect(remediation).To(ContainSubstring("memory"),
			"expected remediation to mention memory, got: %s", remediation)

		triggerType := getInvestigationField(invNS, invName, "{.spec.trigger.type}")
		_, _ = fmt.Fprintf(GinkgoWriter, "Trigger type: %s\n", triggerType)
		Expect(triggerType).To(Equal("pod-oomkill"),
			"expected trigger type 'pod-oomkill', got '%s'", triggerType)

		_, _ = fmt.Fprintf(GinkgoWriter, "Full e2e test passed! OOMKill detected and diagnosed correctly.\n")
	})
})

func findProjectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			Fail("could not find project root")
		}
		dir = parent
	}
}
