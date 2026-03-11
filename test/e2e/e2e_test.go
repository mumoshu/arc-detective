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
	runKubectl := func(args ...string) (string, error) {
		return utils.Run(exec.Command("kubectl", args...))
	}

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
			// Also dump the specific test pod
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

	getInvField := func(ns, name, jsonpath string) string {
		out, err := runKubectl("get", "investigation.detective.arcdetective.io", name,
			"-n", ns, "-o", fmt.Sprintf("jsonpath=%s", jsonpath))
		Expect(err).NotTo(HaveOccurred(), "failed to get investigation field: %s", out)
		return out
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
      command: ["sh", "-c", "sleep 2; exit 1"]
`, podName, testNS))
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "pod", podName, "-n", testNS, "--ignore-not-found")
		})

		invNS, invName := waitForInvestigation("pod-crash", 30*time.Second)
		DeferCleanup(func() {
			_, _ = runKubectl("delete", "investigation", invName, "-n", invNS, "--ignore-not-found")
		})

		Expect(getInvField(invNS, invName, "{.spec.diagnosis.failureType}")).To(Equal("pod-crashed"))
		Expect(getInvField(invNS, invName, "{.spec.diagnosis.remediation}")).To(ContainSubstring("crash reason"))
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

		Expect(getInvField(invNS, invName, "{.spec.diagnosis.failureType}")).To(Equal("pod-oomkilled"))
		Expect(getInvField(invNS, invName, "{.spec.diagnosis.remediation}")).To(ContainSubstring("memory"))
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

		Expect(getInvField(invNS, invName, "{.spec.diagnosis.failureType}")).To(Equal("pod-init-timeout"))
		Expect(getInvField(invNS, invName, "{.spec.diagnosis.remediation}")).To(ContainSubstring("image"))
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
			phase := getInvField(testNS, invName, "{.status.phase}")
			g.Expect(phase).To(Equal("Complete"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		Expect(getInvField(testNS, invName, "{.spec.diagnosis.failureType}")).To(Equal("runner-stuck-running"))
		Expect(getInvField(testNS, invName, "{.spec.diagnosis.remediation}")).To(ContainSubstring("EphemeralRunner"))
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
