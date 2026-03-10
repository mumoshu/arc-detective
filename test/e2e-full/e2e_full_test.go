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
		runIDs        []int64
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

		By("creating Kind cluster")
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

		By("cancelling any in-progress workflow runs")
		for _, id := range runIDs {
			if err := cancelWorkflowRun(owner, repo, ghToken, id); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to cancel workflow run %d: %v\n", id, err)
			}
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(kindBin, kindCluster, detectiveNS, runnerNS)
		}
	})

	// dispatchAndWaitForRun triggers a workflow dispatch and returns the run ID.
	dispatchAndWaitForRun := func(failureMode string) int64 {
		testRunID := fmt.Sprintf("e2e-%s-%d", failureMode, time.Now().Unix())
		dispatchTime := time.Now().Add(-5 * time.Second)
		err := triggerWorkflowDispatch(owner, repo, ghToken, workflowFile, defaultBranch, map[string]string{
			"run_id":       testRunID,
			"failure_mode": failureMode,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to trigger workflow dispatch")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow dispatch triggered: run_id=%s failure_mode=%s\n", testRunID, failureMode)

		id, err := waitForWorkflowRun(owner, repo, ghToken, workflowFile, dispatchTime, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "workflow run did not appear")
		_, _ = fmt.Fprintf(GinkgoWriter, "Workflow run ID: %d\n", id)
		runIDs = append(runIDs, id)
		return id
	}

	// knownPodNames tracks pods from previous tests so we can wait for new ones.
	var knownPodNames []string

	// waitForNewRunnerPod waits for a runner pod that is NOT in knownPodNames.
	waitForNewRunnerPod := func(timeout, poll time.Duration) string {
		var podName string
		EventuallyWithOffset(1, func(g Gomega) {
			out, err := runCmd("kubectl", "get", "pods",
				"-l", "actions-ephemeral-runner=True",
				"-n", runnerNS,
				"-o", "jsonpath={.items[*].metadata.name}",
				"--field-selector=status.phase!=Succeeded,status.phase!=Failed")
			g.Expect(err).NotTo(HaveOccurred())
			for _, name := range strings.Fields(out) {
				known := false
				for _, k := range knownPodNames {
					if name == k {
						known = true
						break
					}
				}
				if !known {
					podName = name
					return
				}
			}
			g.Expect(false).To(BeTrue(), "no new runner pod found (known: %v, current: %s)", knownPodNames, out)
		}, timeout, poll).Should(Succeed())
		knownPodNames = append(knownPodNames, podName)
		return podName
	}

	It("should detect OOMKilled runner pod and create Investigation with pod-oomkilled diagnosis", func() {
		By("triggering workflow_dispatch with failure_mode=oom")
		dispatchAndWaitForRun("oom")

		By("waiting for runner pod to appear")
		podName := waitForNewRunnerPod(5*time.Minute, 3*time.Second)
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

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("pod-oomkilled"))

		remediation := getInvestigationField(invNS, invName, "{.spec.diagnosis.remediation}")
		Expect(remediation).To(ContainSubstring("memory"))

		By("cancelling OOM workflow run on GitHub")
		// The OOM-killed runner never reported back to GitHub, so the run
		// is stuck in_progress. Cancel it so GitHub frees the runner slot.
		for _, id := range runIDs {
			_, _ = fmt.Fprintf(GinkgoWriter, "Cancelling workflow run %d\n", id)
			_ = cancelWorkflowRun(owner, repo, ghToken, id)
		}

		By("cleaning up stuck EphemeralRunner to free runner slot")
		// ARC doesn't auto-clean up failed EphemeralRunners, and they have
		// finalizers that block deletion. Remove finalizers then delete.
		erOut, _ := runCmd("kubectl", "get", "ephemeralrunners.actions.github.com",
			"-n", runnerNS, "-o", "jsonpath={.items[*].metadata.name}")
		for _, erName := range strings.Fields(erOut) {
			_, _ = fmt.Fprintf(GinkgoWriter, "Removing finalizers and deleting EphemeralRunner %s\n", erName)
			_, _ = runCmd("kubectl", "patch", "ephemeralrunner.actions.github.com", erName,
				"-n", runnerNS, "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
			_, _ = runCmd("kubectl", "delete", "ephemeralrunner.actions.github.com", erName,
				"-n", runnerNS, "--wait=false")
		}
		// Also clean up the pod directly (it may have its own finalizers)
		_, _ = runCmd("kubectl", "delete", "pods", "-l", "actions-ephemeral-runner=True",
			"-n", runnerNS, "--force", "--grace-period=0")
		// Wait for the pod to actually be gone
		Eventually(func() string {
			out, _ := runCmd("kubectl", "get", "pods",
				"-l", "actions-ephemeral-runner=True",
				"-n", runnerNS,
				"-o", "jsonpath={.items[*].metadata.name}")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 3*time.Second).Should(BeEmpty(), "runner pod not cleaned up")

		// Give ARC listener time to reconcile runner slot availability.
		// After force-deleting the ER and pod, ARC needs to re-sync with
		// GitHub and mark the runner slot as free. This takes 30-60s.
		By("waiting for ARC to reconcile after cleanup")
		time.Sleep(30 * time.Second)

		// Verify no EphemeralRunners remain (confirms ARC reconciled)
		erCheck, _ := runCmd("kubectl", "get", "ephemeralrunners.actions.github.com",
			"-n", runnerNS, "-o", "jsonpath={.items[*].metadata.name}")
		_, _ = fmt.Fprintf(GinkgoWriter, "EphemeralRunners after cleanup: %q\n", erCheck)

		_, _ = fmt.Fprintf(GinkgoWriter, "OOMKill test passed.\n")
	})

	It("should detect failed GitHub Actions job via GitHubPoller and create Investigation with job-failed trigger", func() {
		By("triggering workflow_dispatch with failure_mode=exit1")
		dispatchAndWaitForRun("exit1")

		By("waiting for new runner pod to appear")
		podName := waitForNewRunnerPod(5*time.Minute, 3*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Runner pod appeared: %s\n", podName)

		By("waiting for runner pod to be Running")
		waitForPodPhase(runnerNS, podName, "Running", 3*time.Minute, 2*time.Second)

		By("waiting for Investigation with trigger job-failed to complete")
		// The runner agent handles step failures gracefully — it reports the
		// failure to GitHub and exits with code 0. ARC then deletes the pod
		// (it may never reach Succeeded/Failed phase). The GitHubPoller
		// independently detects the job failure on GitHub's side.
		// Use 7-minute timeout since the runner may need time to pull its
		// image and execute the step.
		invNS, invName := waitForInvestigationWithTrigger("job-failed", 7*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation complete: %s/%s\n", invNS, invName)

		By("verifying Investigation has workflow run and job details")
		workflowRunID := getInvestigationField(invNS, invName, "{.spec.workflowRun.id}")
		Expect(workflowRunID).NotTo(BeEmpty(), "expected workflowRun.id to be set")
		_, _ = fmt.Fprintf(GinkgoWriter, "WorkflowRun ID in investigation: %s\n", workflowRunID)

		jobConclusion := getInvestigationField(invNS, invName, "{.spec.job.conclusion}")
		Expect(jobConclusion).To(Equal("failure"),
			"expected job conclusion 'failure', got '%s'", jobConclusion)

		jobName := getInvestigationField(invNS, invName, "{.spec.job.name}")
		_, _ = fmt.Fprintf(GinkgoWriter, "Job name: %s, conclusion: %s\n", jobName, jobConclusion)

		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("job-failed"),
			"expected diagnosis.failureType 'job-failed', got '%s'", failureType)

		triggerSource := getInvestigationField(invNS, invName, "{.spec.trigger.source}")
		Expect(triggerSource).To(ContainSubstring("github/"),
			"expected trigger source to contain 'github/', got '%s'", triggerSource)

		_, _ = fmt.Fprintf(GinkgoWriter, "Job-failed test passed.\n")
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

		By("waiting for container to terminate with non-zero exit")
		reason := waitForContainerTerminated(runnerNS, crashPodName, 2*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Container terminated with reason: %s\n", reason)

		By("waiting for Investigation with trigger pod-crash to complete")
		invNS, invName := waitForInvestigationWithTrigger("pod-crash", 3*time.Minute, 2*time.Second)
		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation complete: %s/%s\n", invNS, invName)

		By("verifying Investigation diagnosis")
		failureType := getInvestigationField(invNS, invName, "{.spec.diagnosis.failureType}")
		Expect(failureType).To(Equal("pod-crashed"))

		remediation := getInvestigationField(invNS, invName, "{.spec.diagnosis.remediation}")
		Expect(remediation).To(ContainSubstring("crash reason"))

		_, _ = fmt.Fprintf(GinkgoWriter, "Pod-crash test passed.\n")
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
