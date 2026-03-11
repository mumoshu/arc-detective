//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/mumoshu/arc-detective/test/utils"
)

const namespace = "arc-detective-system"
const serviceAccountName = "arc-detective-controller-manager"
const metricsServiceName = "arc-detective-controller-manager-metrics-service"
const metricsRoleBindingName = "arc-detective-metrics-binding"
const testNS = "arc-detective-e2e-test"

// dumpPodDetails prints pod spec, status, and logs for debugging.
func dumpPodDetails(ns string, labelOrName string, isLabel bool) {
	var args []string
	if isLabel {
		args = []string{"get", "pods", "-l", labelOrName, "-n", ns, "-o", "yaml"}
	} else {
		args = []string{"get", "pod", labelOrName, "-n", ns, "-o", "yaml"}
	}
	out, _ := utils.Run(exec.Command("kubectl", args...))
	_, _ = fmt.Fprintf(GinkgoWriter, "Pod details (%s):\n%s\n", labelOrName, out)

	if isLabel {
		args = []string{"logs", "-l", labelOrName, "-n", ns, "--tail=100", "--all-containers"}
	} else {
		args = []string{"logs", labelOrName, "-n", ns, "--all-containers"}
	}
	out, _ = utils.Run(exec.Command("kubectl", args...))
	_, _ = fmt.Fprintf(GinkgoWriter, "Pod logs (%s):\n%s\n", labelOrName, out)
}

func dumpDiagnostics() {
	_, _ = fmt.Fprintf(GinkgoWriter, "\n=== DIAGNOSTIC DUMP ===\n")

	dumpPodDetails(namespace, "control-plane=controller-manager", true)

	out, _ := utils.Run(exec.Command("kubectl", "get", "investigations.detective.arcdetective.io", "-A", "-o", "yaml"))
	_, _ = fmt.Fprintf(GinkgoWriter, "Investigations:\n%s\n", out)

	out, _ = utils.Run(exec.Command("kubectl", "get", "events", "-n", testNS, "--sort-by=.lastTimestamp"))
	_, _ = fmt.Fprintf(GinkgoWriter, "Test namespace events:\n%s\n", out)

	out, _ = utils.Run(exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp"))
	_, _ = fmt.Fprintf(GinkgoWriter, "Manager namespace events:\n%s\n", out)

	out, _ = utils.Run(exec.Command("kubectl", "get", "pods", "-A", "-o", "wide"))
	_, _ = fmt.Fprintf(GinkgoWriter, "All pods:\n%s\n", out)
}

// runKubectl is a top-level helper for Detection tests.
func runKubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

// getInvestigation fetches the full Investigation CR as a typed struct.
func getInvestigation(ns, name string) (*v1alpha1.Investigation, error) {
	out, err := runKubectl("get", "investigation.detective.arcdetective.io", name,
		"-n", ns, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("kubectl get investigation: %w: %s", err, out)
	}
	var inv v1alpha1.Investigation
	if err := json.Unmarshal([]byte(out), &inv); err != nil {
		return nil, fmt.Errorf("unmarshal investigation: %w", err)
	}
	return &inv, nil
}

// getControllerLogs returns the last N lines of the controller pod logs.
func getControllerLogs(tailLines int) string {
	out, _ := runKubectl("logs", "-l", "control-plane=controller-manager",
		"-n", namespace, "--tail", fmt.Sprintf("%d", tailLines), "--all-containers")
	return out
}

// readCollectedLog reads a log file from inside the controller pod.
func readCollectedLog(logPath string) (string, error) {
	podName, err := runKubectl("get", "pods", "-l", "control-plane=controller-manager",
		"-n", namespace, "-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", fmt.Errorf("get controller pod name: %w", err)
	}
	fullPath := fmt.Sprintf("/var/log/arc-detective/%s", logPath)
	out, err := runKubectl("exec", podName, "-n", namespace, "--", "cat", fullPath)
	if err != nil {
		return "", fmt.Errorf("cat %s: %w: %s", fullPath, err, out)
	}
	return out, nil
}

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics()
		}
	})

	It("should run successfully", func() {
		verifyControllerUp := func(g Gomega) {
			cmd := exec.Command("kubectl", "get",
				"pods", "-l", "control-plane=controller-manager",
				"-o", "go-template={{ range .items }}"+
					"{{ if not .metadata.deletionTimestamp }}"+
					"{{ .metadata.name }}"+
					"{{ \"\\n\" }}{{ end }}{{ end }}",
				"-n", namespace,
			)
			podOutput, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			podNames := utils.GetNonEmptyLines(podOutput)
			g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
			controllerPodName = podNames[0]
			g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

			cmd = exec.Command("kubectl", "get",
				"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
		}
		Eventually(verifyControllerUp).Should(Succeed())
	})

	It("should ensure the metrics endpoint is serving metrics", func() {
		By("creating a ClusterRoleBinding for the service account to allow access to metrics")
		cmd := exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
			"--clusterrole=arc-detective-metrics-reader",
			fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

		By("validating that the metrics service is available")
		cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

		By("getting the service account token")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("ensuring the controller pod is ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
				"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"), "Controller pod not ready")
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying that the controller manager is serving the metrics server")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Serving metrics server"),
				"Metrics server not yet started")
		}, 30*time.Second, time.Second).Should(Succeed())

		// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

		By("cleaning up any previous curl-metrics pod")
		cmd = exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("creating the curl-metrics pod to access the metrics endpoint")
		cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
			"--namespace", namespace,
			"--image=curlimages/curl:latest",
			"--overrides",
			fmt.Sprintf(`{
				"spec": {
					"containers": [{
						"name": "curl",
						"image": "curlimages/curl:latest",
						"command": ["/bin/sh", "-c"],
						"args": [
							"curl -v -H 'Authorization: Bearer %s' http://%s.%s.svc.cluster.local:8443/metrics"
						],
						"securityContext": {
							"readOnlyRootFilesystem": true,
							"allowPrivilegeEscalation": false,
							"capabilities": {
								"drop": ["ALL"]
							},
							"runAsNonRoot": true,
							"runAsUser": 1000,
							"seccompProfile": {
								"type": "RuntimeDefault"
							}
						}
					}],
					"serviceAccountName": "%s"
				}
			}`, token, metricsServiceName, namespace, serviceAccountName))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

		By("waiting for the curl-metrics pod to complete")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
				"-o", "jsonpath={.status.phase}",
				"-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(SatisfyAny(Equal("Succeeded"), Equal("Failed")),
				"curl pod still running")
		}, 30*time.Second).Should(Succeed())

		By("checking curl-metrics result")
		metricsOutput, err := getMetricsOutput()
		Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		if !strings.Contains(metricsOutput, "200 OK") {
			dumpPodDetails(namespace, "curl-metrics", false)
		}
		Expect(metricsOutput).To(ContainSubstring("200 OK"))
	})

	// +kubebuilder:scaffold:e2e-webhooks-checks
})

var _ = Describe("Detection", Ordered, func() {
	BeforeAll(func() {
		By("cleaning up leftover investigations from previous runs")
		_, _ = runKubectl("delete", "investigations.detective.arcdetective.io", "--all", "-n", testNS, "--ignore-not-found")
		_, _ = runKubectl("delete", "pods", "--all", "-n", testNS, "--ignore-not-found", "--force", "--grace-period=0")

		By("ensuring controller pod is ready")
		Eventually(func(g Gomega) {
			out, err := runKubectl("get", "pods", "-l", "control-plane=controller-manager",
				"-n", namespace, "-o", "jsonpath={.items[0].status.conditions[?(@.type==\"Ready\")].status}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"), "controller not ready: %s", out)
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics()
			out, _ := runKubectl("get", "pods", "-n", testNS, "-o", "yaml")
			_, _ = fmt.Fprintf(GinkgoWriter, "Test pods:\n%s\n", out)
		}
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	waitForInvestigation := func(triggerType string, timeout time.Duration) (string, string) {
		var ns, name string
		Eventually(func(g Gomega) {
			out, err := runKubectl("get", "investigations.detective.arcdetective.io",
				"-A", "-o", "jsonpath={range .items[*]}{.status.phase},{.spec.trigger.type},{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}")
			g.Expect(err).NotTo(HaveOccurred(), "failed to list investigations: %s", out)
			var found bool
			for _, line := range strings.Split(out, "\n") {
				parts := strings.SplitN(line, ",", 3)
				if len(parts) != 3 {
					continue
				}
				if parts[0] == "Complete" && parts[1] == triggerType {
					nsParts := strings.SplitN(parts[2], "/", 2)
					g.Expect(nsParts).To(HaveLen(2))
					ns = nsParts[0]
					name = nsParts[1]
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "no completed investigation with trigger %s", triggerType)
		}, timeout, 2*time.Second).Should(Succeed())
		return ns, name
	}

	applyYAML := func(yaml string) {
		cmd := exec.Command("bash", "-c", fmt.Sprintf("echo '%s' | kubectl apply -f -", yaml))
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply YAML")
	}

	It("should detect crashed runner pod", func() {
		podName := fmt.Sprintf("crash-test-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: v1
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
      command: ["sh", "-c", "echo crash-test-output; sleep 2; exit 1"]
`, podName, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "pod", podName, "-n", testNS, "--ignore-not-found")
		})

		invNS, invName := waitForInvestigation("pod-crash", 30*time.Second)
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		inv, err := getInvestigation(invNS, invName)
		Expect(err).NotTo(HaveOccurred(), "failed to fetch investigation as JSON")

		_, _ = fmt.Fprintf(GinkgoWriter, "Investigation %s/%s:\n", invNS, invName)

		By("verifying status.phase")
		Expect(inv.Status.Phase).To(Equal("Complete"))

		By("verifying trigger")
		Expect(inv.Spec.Trigger.Type).To(Equal("pod-crash"))
		Expect(inv.Spec.Trigger.Source).To(Equal(fmt.Sprintf("%s/%s", testNS, podName)))

		By("verifying pod info")
		Expect(inv.Spec.Pod).NotTo(BeNil(), "spec.pod must be populated")
		Expect(inv.Spec.Pod.Name).To(Equal(podName))
		Expect(inv.Spec.Pod.Namespace).To(Equal(testNS))
		Expect(inv.Spec.Pod.NodeName).NotTo(BeEmpty(), "pod.nodeName must be set")

		By("verifying container statuses")
		Expect(inv.Spec.Pod.ContainerStatuses).To(HaveLen(1))
		cs := inv.Spec.Pod.ContainerStatuses[0]
		Expect(cs.Name).To(Equal("runner"))
		Expect(cs.State).To(Equal("terminated"))
		Expect(cs.ExitCode).NotTo(BeNil())
		Expect(*cs.ExitCode).To(Equal(int32(1)))
		Expect(cs.OOMKilled).To(BeFalse())

		By("verifying pod conditions exist")
		Expect(inv.Spec.Pod.Conditions).NotTo(BeEmpty(), "pod.conditions should be populated")

		By("verifying pod events were collected by Correlator")
		// K8s events should be present for the pod (at minimum: Scheduled, Pulling, Pulled, Created, Started)
		Expect(inv.Spec.Pod.Events).NotTo(BeEmpty(), "pod.events should be populated by Correlator")
		var hasKubeletEvent bool
		for _, evt := range inv.Spec.Pod.Events {
			if evt.Source == "kubelet" {
				hasKubeletEvent = true
			}
		}
		Expect(hasKubeletEvent).To(BeTrue(), "expected at least one kubelet event")

		By("verifying timeline")
		Expect(inv.Spec.Timeline).NotTo(BeEmpty(), "timeline should have events")
		var hasPodTrigger bool
		for _, evt := range inv.Spec.Timeline {
			if evt.Source == "pod" && evt.Type == "pod-crash" {
				hasPodTrigger = true
			}
		}
		Expect(hasPodTrigger).To(BeTrue(), "timeline should contain the pod-crash trigger event")

		By("verifying diagnosis")
		Expect(inv.Spec.Diagnosis).NotTo(BeNil())
		Expect(inv.Spec.Diagnosis.FailureType).To(Equal("pod-crashed"))
		Expect(inv.Spec.Diagnosis.Summary).To(ContainSubstring(podName))
		Expect(inv.Spec.Diagnosis.Remediation).To(ContainSubstring("crash reason"))
	})

	It("should detect OOMKilled runner pod", func() {
		podName := fmt.Sprintf("oom-test-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: v1
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
      command: ["sh", "-c", "x=''; while true; do x=\"${x}$(dd if=/dev/urandom bs=1M count=1 2>/dev/null | od)\"; done"]
      resources:
        limits:
          memory: "32Mi"
`, podName, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "pod", podName, "-n", testNS, "--ignore-not-found")
		})

		invNS, invName := waitForInvestigation("pod-oomkill", 30*time.Second)
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		inv, err := getInvestigation(invNS, invName)
		Expect(err).NotTo(HaveOccurred())

		By("verifying status.phase")
		Expect(inv.Status.Phase).To(Equal("Complete"))

		By("verifying trigger")
		Expect(inv.Spec.Trigger.Type).To(Equal("pod-oomkill"))
		Expect(inv.Spec.Trigger.Source).To(Equal(fmt.Sprintf("%s/%s", testNS, podName)))

		By("verifying pod info")
		Expect(inv.Spec.Pod).NotTo(BeNil())
		Expect(inv.Spec.Pod.Name).To(Equal(podName))
		Expect(inv.Spec.Pod.Namespace).To(Equal(testNS))

		By("verifying container statuses show OOMKill")
		Expect(inv.Spec.Pod.ContainerStatuses).To(HaveLen(1))
		cs := inv.Spec.Pod.ContainerStatuses[0]
		Expect(cs.Name).To(Equal("runner"))
		Expect(cs.State).To(Equal("terminated"))
		Expect(cs.Reason).To(Equal("OOMKilled"))
		Expect(cs.ExitCode).NotTo(BeNil())
		Expect(*cs.ExitCode).To(Equal(int32(137)))
		Expect(cs.OOMKilled).To(BeTrue())

		By("verifying pod conditions exist")
		Expect(inv.Spec.Pod.Conditions).NotTo(BeEmpty())

		By("verifying timeline")
		Expect(inv.Spec.Timeline).NotTo(BeEmpty())
		var hasPodTrigger bool
		for _, evt := range inv.Spec.Timeline {
			if evt.Source == "pod" && evt.Type == "pod-oomkill" {
				hasPodTrigger = true
			}
		}
		Expect(hasPodTrigger).To(BeTrue(), "timeline should contain the pod-oomkill trigger event")

		By("verifying diagnosis")
		Expect(inv.Spec.Diagnosis).NotTo(BeNil())
		Expect(inv.Spec.Diagnosis.FailureType).To(Equal("pod-oomkilled"))
		Expect(inv.Spec.Diagnosis.Summary).To(ContainSubstring(podName))
		Expect(inv.Spec.Diagnosis.Remediation).To(ContainSubstring("memory"))
	})

	It("should detect ImagePullBackOff", func() {
		podName := fmt.Sprintf("bad-image-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: v1
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
`, podName, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "pod", podName, "-n", testNS, "--ignore-not-found", "--force", "--grace-period=0")
		})

		invNS, invName := waitForInvestigation("pod-init-timeout", 30*time.Second)
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		inv, err := getInvestigation(invNS, invName)
		Expect(err).NotTo(HaveOccurred())

		By("verifying status.phase")
		Expect(inv.Status.Phase).To(Equal("Complete"))

		By("verifying trigger")
		Expect(inv.Spec.Trigger.Type).To(Equal("pod-init-timeout"))
		Expect(inv.Spec.Trigger.Source).To(Equal(fmt.Sprintf("%s/%s", testNS, podName)))

		By("verifying pod info")
		Expect(inv.Spec.Pod).NotTo(BeNil())
		Expect(inv.Spec.Pod.Name).To(Equal(podName))

		By("verifying container statuses show waiting/ImagePullBackOff")
		Expect(inv.Spec.Pod.ContainerStatuses).To(HaveLen(1))
		cs := inv.Spec.Pod.ContainerStatuses[0]
		Expect(cs.Name).To(Equal("runner"))
		Expect(cs.State).To(Equal("waiting"))
		Expect(cs.Reason).To(SatisfyAny(Equal("ImagePullBackOff"), Equal("ErrImagePull")))

		By("verifying pod events show image pull failure")
		Expect(inv.Spec.Pod.Events).NotTo(BeEmpty(), "pod.events should show image pull failure")
		var hasImagePullEvent bool
		for _, evt := range inv.Spec.Pod.Events {
			if evt.Reason == "Failed" && strings.Contains(evt.Message, "nonexistent-registry.invalid") {
				hasImagePullEvent = true
			}
		}
		Expect(hasImagePullEvent).To(BeTrue(), "expected an image pull failure event")

		By("verifying timeline")
		Expect(inv.Spec.Timeline).NotTo(BeEmpty())

		By("verifying diagnosis")
		Expect(inv.Spec.Diagnosis).NotTo(BeNil())
		Expect(inv.Spec.Diagnosis.FailureType).To(Equal("pod-init-timeout"))
		Expect(inv.Spec.Diagnosis.Summary).To(ContainSubstring(podName))
		Expect(inv.Spec.Diagnosis.Remediation).To(ContainSubstring("image"))
	})

	It("should collect logs and populate logPaths on pod deletion", func() {
		podName := fmt.Sprintf("log-test-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: v1
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
      command: ["sh", "-c", "echo collected-log-content-12345; sleep 2; exit 1"]
`, podName, testNS))

		By("waiting for anomaly investigation to be created")
		invNS, invName := waitForInvestigation("pod-crash", 30*time.Second)
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		By("deleting the pod to trigger log collection via finalizer")
		_, err := runKubectl("delete", "pod", podName, "-n", testNS, "--timeout=15s")
		Expect(err).NotTo(HaveOccurred(), "failed to delete pod")

		By("waiting for logPaths to be populated after pod deletion")
		Eventually(func(g Gomega) {
			inv, err := getInvestigation(invNS, invName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(inv.Spec.LogPaths).NotTo(BeEmpty(), "logPaths should be populated after pod deletion")
		}, 15*time.Second, 2*time.Second).Should(Succeed())

		inv, err := getInvestigation(invNS, invName)
		Expect(err).NotTo(HaveOccurred())

		By("verifying logPaths contain expected path structure")
		Expect(inv.Spec.LogPaths).To(HaveLen(1))
		logPath := inv.Spec.LogPaths[0]
		Expect(logPath).To(ContainSubstring(testNS))
		Expect(logPath).To(ContainSubstring(podName))
		Expect(logPath).To(ContainSubstring("runner"))
		_, _ = fmt.Fprintf(GinkgoWriter, "Log path: %s\n", logPath)

		By("reading collected log via kubectl exec (as documented in README)")
		content, err := readCollectedLog(logPath)
		Expect(err).NotTo(HaveOccurred(), "kubectl exec cat should work on the controller image")
		_, _ = fmt.Fprintf(GinkgoWriter, "Collected log content:\n%s\n", content)
		Expect(content).To(ContainSubstring("collected-log-content-12345"),
			"collected log should contain the pod's stdout output")
	})

	It("should classify runner-stuck-running when job completed", func() {
		invName := fmt.Sprintf("er-stuck-test-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: detective.arcdetective.io/v1alpha1
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
`, invName, testNS, testNS, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", testNS, "--ignore-not-found")
		})

		By("setting status to Collecting so Correlator processes it")
		cmd := exec.Command("kubectl", "patch", "investigation.detective.arcdetective.io", invName,
			"-n", testNS, "--subresource=status", "--type=merge",
			"-p", `{"status":{"phase":"Collecting"}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			inv, err := getInvestigation(testNS, invName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(inv.Status.Phase).To(Equal("Complete"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		inv, err := getInvestigation(testNS, invName)
		Expect(err).NotTo(HaveOccurred())

		By("verifying trigger preserved")
		Expect(inv.Spec.Trigger.Type).To(Equal("runner-stuck"))
		Expect(inv.Spec.Trigger.Source).To(Equal(fmt.Sprintf("%s/synthetic-er", testNS)))

		By("verifying ephemeralRunner info preserved")
		Expect(inv.Spec.EphemeralRunner).NotTo(BeNil())
		Expect(inv.Spec.EphemeralRunner.Name).To(Equal("synthetic-er"))
		Expect(inv.Spec.EphemeralRunner.Namespace).To(Equal(testNS))
		Expect(inv.Spec.EphemeralRunner.Phase).To(Equal("Running"))

		By("verifying job info preserved")
		Expect(inv.Spec.Job).NotTo(BeNil())
		Expect(inv.Spec.Job.ID).To(Equal(int64(99999)))
		Expect(inv.Spec.Job.Name).To(Equal("test-job"))
		Expect(inv.Spec.Job.Status).To(Equal("completed"))
		Expect(inv.Spec.Job.Conclusion).To(Equal("failure"))

		By("verifying timeline preserved and sorted")
		Expect(inv.Spec.Timeline).NotTo(BeEmpty())
		var hasStuckEvent bool
		for _, evt := range inv.Spec.Timeline {
			if evt.Type == "runner-stuck" {
				hasStuckEvent = true
			}
		}
		Expect(hasStuckEvent).To(BeTrue(), "timeline should contain the runner-stuck event")

		By("verifying diagnosis")
		Expect(inv.Spec.Diagnosis).NotTo(BeNil())
		Expect(inv.Spec.Diagnosis.FailureType).To(Equal("runner-stuck-running"))
		Expect(inv.Spec.Diagnosis.Summary).To(ContainSubstring("synthetic-er"))
		Expect(inv.Spec.Diagnosis.Remediation).To(ContainSubstring("EphemeralRunner"))
	})

	It("should classify runner-stuck-failed", func() {
		invName := fmt.Sprintf("er-failed-test-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: detective.arcdetective.io/v1alpha1
kind: Investigation
metadata:
  name: %s
  namespace: %s
spec:
  trigger:
    type: runner-stuck
    source: %s/failed-er
  ephemeralRunner:
    name: failed-er
    namespace: %s
    phase: Failed
  timeline:
    - timestamp: "2024-01-01T00:00:00Z"
      source: test
      type: runner-stuck
      message: Synthetic stuck-failed test
`, invName, testNS, testNS, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", testNS, "--ignore-not-found")
		})

		By("setting status to Collecting so Correlator processes it")
		cmd := exec.Command("kubectl", "patch", "investigation.detective.arcdetective.io", invName,
			"-n", testNS, "--subresource=status", "--type=merge",
			"-p", `{"status":{"phase":"Collecting"}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			inv, err := getInvestigation(testNS, invName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(inv.Status.Phase).To(Equal("Complete"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		inv, err := getInvestigation(testNS, invName)
		Expect(err).NotTo(HaveOccurred())

		Expect(inv.Spec.Diagnosis).NotTo(BeNil())
		Expect(inv.Spec.Diagnosis.FailureType).To(Equal("runner-stuck-failed"))
		Expect(inv.Spec.Diagnosis.Summary).To(ContainSubstring("failed-er"))
		Expect(inv.Spec.Diagnosis.Remediation).To(ContainSubstring("EphemeralRunner"))
	})

	It("should classify job-stuck-queued", func() {
		invName := fmt.Sprintf("job-queued-test-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: detective.arcdetective.io/v1alpha1
kind: Investigation
metadata:
  name: %s
  namespace: %s
spec:
  trigger:
    type: job-stuck-queued
    source: test-org/test-repo
  job:
    id: 88888
    name: stuck-job
    status: queued
  timeline:
    - timestamp: "2024-01-01T00:00:00Z"
      source: github
      type: job-stuck-queued
      message: Job queued with no runner available
`, invName, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", testNS, "--ignore-not-found")
		})

		By("setting status to Collecting so Correlator processes it")
		cmd := exec.Command("kubectl", "patch", "investigation.detective.arcdetective.io", invName,
			"-n", testNS, "--subresource=status", "--type=merge",
			"-p", `{"status":{"phase":"Collecting"}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			inv, err := getInvestigation(testNS, invName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(inv.Status.Phase).To(Equal("Complete"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		inv, err := getInvestigation(testNS, invName)
		Expect(err).NotTo(HaveOccurred())

		Expect(inv.Spec.Diagnosis).NotTo(BeNil())
		Expect(inv.Spec.Diagnosis.FailureType).To(Equal("job-stuck-queued"))
		Expect(inv.Spec.Diagnosis.Remediation).To(ContainSubstring("AutoScalingRunnerSet"))
	})

	It("should classify pod-stuck-terminating", func() {
		invName := fmt.Sprintf("pod-terminating-test-%d", time.Now().Unix())
		applyYAML(fmt.Sprintf(`apiVersion: detective.arcdetective.io/v1alpha1
kind: Investigation
metadata:
  name: %s
  namespace: %s
spec:
  trigger:
    type: pod-deletion
    source: %s/terminating-pod
  pod:
    name: terminating-pod
    namespace: %s
    phase: Terminating
    nodeName: test-node
    containerStatuses:
      - name: runner
        state: running
        restartCount: 0
  timeline:
    - timestamp: "2024-01-01T00:00:00Z"
      source: pod
      type: pod-deletion
      message: Pod stuck in Terminating phase
`, invName, testNS, testNS, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", testNS, "--ignore-not-found")
		})

		By("setting status to Collecting so Correlator processes it")
		cmd := exec.Command("kubectl", "patch", "investigation.detective.arcdetective.io", invName,
			"-n", testNS, "--subresource=status", "--type=merge",
			"-p", `{"status":{"phase":"Collecting"}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			inv, err := getInvestigation(testNS, invName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(inv.Status.Phase).To(Equal("Complete"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		inv, err := getInvestigation(testNS, invName)
		Expect(err).NotTo(HaveOccurred())

		Expect(inv.Spec.Diagnosis).NotTo(BeNil())
		Expect(inv.Spec.Diagnosis.FailureType).To(Equal("pod-stuck-terminating"))
		Expect(inv.Spec.Diagnosis.Remediation).To(ContainSubstring("Force-delete"))
	})
})

func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)
		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())
		out = token.Status.Token
	}
	Eventually(verifyTokenCreation, 10*time.Second).Should(Succeed())

	return out, err
}

func getMetricsOutput() (string, error) {
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
