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
	)

	// dumpOnFailure dumps diagnostics and fails with a message.
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

		By("cancelling any leftover workflow runs from previous test executions")
		_ = cancelAllInProgressRuns(owner, repo, ghToken, workflowFile)

		By("creating Kind cluster")
		_, _ = runCmdWithTimeout(10*time.Second, kindBin, "delete", "cluster", "--name", kindCluster)
		out := mustRunCmd(kindBin, "create", "cluster", "--name", kindCluster, "--wait", "120s")
		_, _ = fmt.Fprintf(GinkgoWriter, "Kind cluster created: %s\n", out)
		DeferCleanup(func() {
			By("uninstalling ARC runner scale set for graceful runner deregistration")
			_, _ = runCmdWithTimeout(30*time.Second, helmBin, "uninstall", arcReleaseName, "-n", runnerNS)
			By("deleting Kind cluster")
			_, _ = runCmdWithTimeout(10*time.Second, kindBin, "delete", "cluster", "--name", kindCluster)
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
            memory: "256Mi"
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
			dumpHelmInfo(helmBin, arcReleaseName, runnerNS, arcVersion, valuesPath)
			dumpOnFailure("Failed to install ARC runner scale set: %v\nOutput: %s", installErr, out)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC runner scale set installed: %s\n", out)

		By("verifying ARC listener pod is running")
		listenerPod, listenerErr := waitForPodWithLabelOrFail(arcSystemNS,
			"actions.github.com/scale-set-name="+arcReleaseName,
			2*time.Minute, 3*time.Second)
		if listenerErr != nil {
			dumpOnFailure("ARC listener pod did not appear: %v", listenerErr)
		}
		waitForPodPhase(arcSystemNS, listenerPod, "Running", 2*time.Minute, 3*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC listener pod running: %s\n", listenerPod)

		By("checking ARC listener logs for successful registration")
		time.Sleep(5 * time.Second)
		listenerLogs, _ := runCmd("kubectl", "logs", listenerPod, "-n", arcSystemNS, "--tail=50")
		_, _ = fmt.Fprintf(GinkgoWriter, "ARC listener logs:\n%s\n", listenerLogs)

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
			{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--health-probe-bind-address=:8081","--poll-interval=3s","--stuck-threshold=10s","--running-threshold=10s"]},
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

		By("pushing test workflow to repo")
		templatePath := filepath.Join(projectRoot, "test", "e2e-full", "testdata", "test-workflow.yaml")
		templateBytes, readErr := os.ReadFile(templatePath)
		Expect(readErr).NotTo(HaveOccurred())
		workflowContent := strings.ReplaceAll(string(templateBytes), "%RUNNER_LABEL%", arcReleaseName)
		_, err = pushWorkflowFile(owner, repo, ghToken, workflowPath, workflowContent, defaultBranch)
		Expect(err).NotTo(HaveOccurred(), "failed to push workflow file")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow pushed to %s\n", workflowPath)

		// Let GitHub process the new workflow file
		time.Sleep(5 * time.Second)

		_, _ = fmt.Fprintf(GinkgoWriter, "Setup complete. Ready for tests.\n")
	})

	AfterAll(func() {
		By("cleaning up test workflow from repo")
		if err := deleteWorkflowFile(owner, repo, ghToken, workflowPath, defaultBranch); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete workflow file: %v\n", err)
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(kindBin, kindCluster, detectiveNS, runnerNS)
		}
	})

	// dispatchAndWaitForRun triggers a workflow dispatch and returns the run ID and the time just before dispatch.
	dispatchAndWaitForRun := func() (int64, time.Time) {
		testRunID := fmt.Sprintf("e2e-%d", time.Now().Unix())
		dispatchTime := time.Now().Add(-5 * time.Second)
		err := triggerWorkflowDispatch(owner, repo, ghToken, workflowFile, defaultBranch, map[string]string{
			"run_id": testRunID,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to trigger workflow dispatch")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow dispatch triggered: run_id=%s\n", testRunID)

		id, err := waitForWorkflowRun(owner, repo, ghToken, workflowFile, dispatchTime, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "workflow run did not appear")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow run ID: %d\n", id)
		return id, dispatchTime
	}

	It("should detect OOMKilled runner pod and create Investigation with pod-oomkilled diagnosis", func() {
		By("triggering workflow_dispatch")
		runID, dispatchTime := dispatchAndWaitForRun()
		DeferCleanup(func() {
			By("cancelling OOM test workflow run")
			if err := cancelWorkflowRun(owner, repo, ghToken, runID); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to cancel workflow run %d: %v\n", runID, err)
			}
		})

		By("waiting for runner pod to appear")
		podName := waitForPodWithLabel(runnerNS, "actions-ephemeral-runner=True", 5*time.Minute, 3*time.Second, dispatchTime)
		_, _ = fmt.Fprintf(GinkgoWriter, "Runner pod appeared: %s\n", podName)

		By("waiting for runner pod to be Running")
		waitForPodPhase(runnerNS, podName, "Running", 3*time.Minute, 2*time.Second)

		By("waiting for OOMKill (workflow allocates memory to exceed 256Mi limit)")
		reason := waitForContainerTerminated(runnerNS, podName, 3*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Container terminated with reason: %s\n", reason)
		Expect(reason).To(SatisfyAny(
			Equal("OOMKilled"),
			Equal("Error"),
		), "expected OOMKilled or Error termination reason, got %s", reason)

		By("waiting for Investigation with trigger pod-oomkill to complete")
		invNS, invName := waitForInvestigationWithTrigger("pod-oomkill", 5*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation complete: %s/%s\n", invNS, invName)
		DeferCleanup(func() {
			By("cleaning up OOM Investigation CR")
			_, _ = runCmd("kubectl", "delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("pod-oomkilled"))

		remediation := getInvestigationField(invNS, invName, "{.spec.diagnosis.remediation}")
		Expect(remediation).To(ContainSubstring("memory"))

		_, _ = fmt.Fprintf(GinkgoWriter, "OOMKill test passed.\n")
	})

	It("should detect crashed runner pod and create Investigation with pod-crashed diagnosis", func() {
		// In real ARC, pod-crash (non-zero exit, non-OOM) happens when:
		// - The runner image is misconfigured (bad entrypoint)
		// - The node evicts the pod
		// - The kubelet kills the container
		// Since we can't simulate these from within a workflow step (the
		// runner agent always exits 0), we create a synthetic pod with the
		// ARC runner label that exits with a non-zero code. This directly
		// tests PodWatcher's crash detection path.

		By("creating a synthetic runner pod that exits with code 1")
		crashPodName := fmt.Sprintf("crash-test-%d", time.Now().Unix())
		podYAML := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    actions-ephemeral-runner: "True"
spec:
  restartPolicy: Never
  containers:
    - name: runner
      image: busybox
      command: ["sh", "-c", "echo 'Simulating runner crash'; sleep 2; exit 1"]
`, crashPodName, runnerNS)
		cmd := fmt.Sprintf("echo '%s' | kubectl apply -f -", podYAML)
		mustRunCmd("bash", "-c", cmd)
		_, _ = fmt.Fprintf(GinkgoWriter, "Synthetic crash pod created: %s\n", crashPodName)
		DeferCleanup(func() {
			By("cleaning up synthetic crash pod")
			_, _ = runCmd("kubectl", "delete", "pod", crashPodName, "-n", runnerNS, "--ignore-not-found")
		})

		By("waiting for container to terminate with non-zero exit")
		reason := waitForContainerTerminated(runnerNS, crashPodName, 2*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Container terminated with reason: %s\n", reason)

		By("waiting for Investigation with trigger pod-crash to complete")
		invNS, invName := waitForInvestigationWithTrigger("pod-crash", 3*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation complete: %s/%s\n", invNS, invName)
		DeferCleanup(func() {
			By("cleaning up crash Investigation CR")
			_, _ = runCmd("kubectl", "delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("pod-crashed"))

		remediation := getInvestigationField(invNS, invName, "{.spec.diagnosis.remediation}")
		Expect(remediation).To(ContainSubstring("crash reason"))

		_, _ = fmt.Fprintf(GinkgoWriter, "Pod-crash test passed.\n")
	})

	It("should detect ImagePullBackOff and create Investigation with pod-init-timeout diagnosis", func() {
		// When a runner pod references a non-existent image, the kubelet
		// reports ErrImagePull then ImagePullBackOff. PodWatcher detects
		// this and creates an Investigation classified as pod-init-timeout.

		By("creating a pod with a non-existent image")
		badImagePod := fmt.Sprintf("bad-image-test-%d", time.Now().Unix())
		podYAML := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    actions-ephemeral-runner: "True"
spec:
  restartPolicy: Never
  containers:
    - name: runner
      image: "nonexistent-registry.invalid/no-such-image:latest"
      command: ["echo", "never runs"]
`, badImagePod, runnerNS)
		cmd := fmt.Sprintf("echo '%s' | kubectl apply -f -", podYAML)
		mustRunCmd("bash", "-c", cmd)
		_, _ = fmt.Fprintf(GinkgoWriter, "Bad-image pod created: %s\n", badImagePod)
		DeferCleanup(func() {
			By("cleaning up bad-image pod")
			_, _ = runCmd("kubectl", "delete", "pod", badImagePod, "-n", runnerNS, "--ignore-not-found", "--force", "--grace-period=0")
		})

		By("waiting for Investigation with trigger pod-init-timeout to complete")
		invNS, invName := waitForInvestigationWithTrigger("pod-init-timeout", 3*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation complete: %s/%s\n", invNS, invName)
		DeferCleanup(func() {
			By("cleaning up pod-init-timeout Investigation CR")
			_, _ = runCmd("kubectl", "delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("pod-init-timeout"))

		remediation := getInvestigationField(invNS, invName, "{.spec.diagnosis.remediation}")
		Expect(remediation).To(ContainSubstring("image"))

		_, _ = fmt.Fprintf(GinkgoWriter, "Pod-init-timeout test passed.\n")
	})

	It("should classify runner-stuck-running when EphemeralRunner is Running but job completed", func() {
		// In real ARC, this happens when the EphemeralRunner CR stays in
		// Running phase after the GitHub job has already completed. We
		// can't easily trigger this condition in ARC itself, so we create
		// an Investigation CR directly with the pre-populated evidence
		// and verify the Correlator classifies it correctly.

		By("creating Investigation CR with pre-populated ER and job data")
		invName := fmt.Sprintf("er-stuck-test-%d", time.Now().Unix())
		invYAML := fmt.Sprintf(`apiVersion: detective.arcdetective.io/v1alpha1
kind: Investigation
metadata:
  name: %s
  namespace: %s
spec:
  trigger:
    type: runner-stuck
    source: %s/synthetic-er
  ephemeralRunner:
    name: synthetic-er
    namespace: %s
    phase: Running
  job:
    id: 99999
    name: test-job
    status: completed
    conclusion: failure
  timeline:
    - timestamp: "2024-01-01T00:00:00Z"
      source: test
      type: runner-stuck
      message: Synthetic stuck-running test
`, invName, runnerNS, runnerNS, runnerNS)
		cmd := fmt.Sprintf("echo '%s' | kubectl apply -f -", invYAML)
		mustRunCmd("bash", "-c", cmd)
		_, _ = fmt.Fprintf(GinkgoWriter, "Synthetic runner-stuck Investigation created: %s\n", invName)
		DeferCleanup(func() {
			By("cleaning up runner-stuck Investigation CR")
			_, _ = runCmd("kubectl", "delete", "investigation", invName, "-n", runnerNS, "--ignore-not-found")
		})

		By("setting status to Collecting so Correlator processes it")
		mustRunCmd("kubectl", "patch", "investigation.detective.arcdetective.io", invName,
			"-n", runnerNS,
			"--subresource=status",
			"--type=merge",
			"-p", `{"status":{"phase":"Collecting"}}`)

		By("waiting for Investigation to be classified as runner-stuck-running")
		Eventually(func(g Gomega) {
			phase := getInvestigationField(runnerNS, invName, "{.status.phase}")
			g.Expect(phase).To(Equal("Complete"), "investigation not yet complete, phase=%s", phase)
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(runnerNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("runner-stuck-running"))

		remediation := getInvestigationField(runnerNS, invName, "{.spec.diagnosis.remediation}")
		Expect(remediation).To(ContainSubstring("EphemeralRunner"))

		_, _ = fmt.Fprintf(GinkgoWriter, "Runner-stuck-running test passed.\n")
	})

	It("should detect EphemeralRunner stuck Running past threshold", func() {
		// Variant 2 of runner-stuck-running: the ER has been Running
		// longer than the stuck-threshold (10s in e2e config), regardless
		// of whether the GitHub job has completed. This catches hung
		// runners even when we can't correlate with a GitHub job.

		By("creating a synthetic EphemeralRunner CR in Running state")
		erName := fmt.Sprintf("stuck-runner-%d", time.Now().Unix())
		erYAML := fmt.Sprintf(`apiVersion: actions.github.com/v1alpha1
kind: EphemeralRunner
metadata:
  name: %s
  namespace: %s
spec:
  githubConfigUrl: "https://github.com/fake/repo"
  githubConfigSecret: arc-github-secret
  runnerScaleSetId: 99999
`, erName, runnerNS)
		cmd := fmt.Sprintf("echo '%s' | kubectl apply -f -", erYAML)
		mustRunCmd("bash", "-c", cmd)
		_, _ = fmt.Fprintf(GinkgoWriter, "Synthetic EphemeralRunner created: %s\n", erName)
		DeferCleanup(func() {
			By("cleaning up synthetic EphemeralRunner")
			// Remove finalizers first to avoid ARC controller blocking deletion
			_, _ = runCmd("kubectl", "patch", "ephemeralrunner.actions.github.com", erName,
				"-n", runnerNS, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
			_, _ = runCmdWithTimeout(10*time.Second, "kubectl", "delete", "ephemeralrunner.actions.github.com", erName, "-n", runnerNS, "--ignore-not-found")
		})

		By("patching ER status to Running")
		mustRunCmd("kubectl", "patch", "ephemeralrunner.actions.github.com", erName,
			"-n", runnerNS,
			"--subresource=status",
			"--type=merge",
			"-p", `{"status":{"phase":"Running"}}`)

		By("waiting for Investigation to be created after running-threshold (10s)")
		// The EphemeralRunnerWatcher creates investigation named "er-{erName}".
		// With --running-threshold=10s, the ER must be Running for 10s before triggering.
		expectedInvName := fmt.Sprintf("er-%s", erName)
		waitForInvestigationByName(runnerNS, expectedInvName, 30*time.Second, 2*time.Second)
		invNS, invName := runnerNS, expectedInvName
		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation created: %s/%s\n", invNS, invName)
		DeferCleanup(func() {
			By("cleaning up runner-stuck Investigation CR")
			_, _ = runCmd("kubectl", "delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("runner-stuck-running"))

		_, _ = fmt.Fprintf(GinkgoWriter, "Runner-stuck-running (threshold) test passed.\n")
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
