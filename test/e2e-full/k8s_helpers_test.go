//go:build e2e_full
// +build e2e_full

package e2e_full

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// runCmd executes a command and returns its combined stdout/stderr.
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// runCmdWithTimeout executes a command with a timeout.
func runCmdWithTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// mustRunCmd runs a command and fails the test if it errors.
func mustRunCmd(name string, args ...string) string {
	out, err := runCmd(name, args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(),
		fmt.Sprintf("Command failed: %s %s\nOutput: %s", name, strings.Join(args, " "), out))
	return out
}

// waitForPodWithLabel waits for a pod matching the label selector to exist and returns its name.
// If createdAfter is non-zero, only pods created after that time are considered.
func waitForPodWithLabel(namespace, labelSelector string, timeout, poll time.Duration, createdAfter ...time.Time) string {
	var podName string
	EventuallyWithOffset(1, func(g Gomega) {
		// Get pod names and creation timestamps
		out, err := runCmd("kubectl", "get", "pods",
			"-l", labelSelector,
			"-n", namespace,
			"-o", "jsonpath={range .items[*]}{.metadata.name},{.metadata.creationTimestamp}{\"\\n\"}{end}",
			"--field-selector=status.phase!=Succeeded,status.phase!=Failed")
		g.Expect(err).NotTo(HaveOccurred(), "kubectl get pods failed: %s", out)
		var found bool
		for _, line := range strings.Split(out, "\n") {
			parts := strings.SplitN(line, ",", 2)
			if len(parts) != 2 || parts[0] == "" {
				continue
			}
			name := parts[0]
			if len(createdAfter) > 0 && !createdAfter[0].IsZero() {
				ts, parseErr := time.Parse(time.RFC3339, parts[1])
				if parseErr != nil {
					continue
				}
				if ts.Before(createdAfter[0]) {
					continue // skip pods created before our dispatch
				}
			}
			podName = name
			found = true
			break
		}
		g.Expect(found).To(BeTrue(), "no pods found with label %s (created after filter applied)", labelSelector)
	}, timeout, poll).Should(Succeed())
	return podName
}

// waitForPodWithLabelOrFail is like waitForPodWithLabel but returns an error instead of calling Fail.
func waitForPodWithLabelOrFail(namespace, labelSelector string, timeout, poll time.Duration) (string, error) {
	var podName string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := runCmd("kubectl", "get", "pods",
			"-l", labelSelector,
			"-n", namespace,
			"-o", "jsonpath={.items[*].metadata.name}",
			"--field-selector=status.phase!=Succeeded,status.phase!=Failed")
		if err == nil {
			names := strings.Fields(out)
			if len(names) > 0 {
				podName = names[0]
				return podName, nil
			}
		}
		time.Sleep(poll)
	}
	return "", fmt.Errorf("timed out after %v waiting for pod with label %s in %s", timeout, labelSelector, namespace)
}

// waitForPodPhase waits until the named pod reaches the given phase.
func waitForPodPhase(namespace, podName, phase string, timeout, poll time.Duration) {
	EventuallyWithOffset(1, func(g Gomega) {
		out, err := runCmd("kubectl", "get", "pod", podName,
			"-n", namespace,
			"-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal(phase), "pod %s phase is %s, want %s", podName, out, phase)
	}, timeout, poll).Should(Succeed())
}

// waitForPodPhaseAny waits until the named pod reaches any of the given phases.
func waitForPodPhaseAny(namespace, podName string, phases []string, timeout, poll time.Duration) {
	EventuallyWithOffset(1, func(g Gomega) {
		out, err := runCmd("kubectl", "get", "pod", podName,
			"-n", namespace,
			"-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		matched := false
		for _, p := range phases {
			if out == p {
				matched = true
				break
			}
		}
		g.Expect(matched).To(BeTrue(), "pod %s phase is %s, want one of %v", podName, out, phases)
	}, timeout, poll).Should(Succeed())
}

// waitForContainerTerminated waits until the first container in the pod is terminated.
// Returns the termination reason.
func waitForContainerTerminated(namespace, podName string, timeout, poll time.Duration) string {
	var reason string
	EventuallyWithOffset(1, func(g Gomega) {
		out, err := runCmd("kubectl", "get", "pod", podName,
			"-n", namespace,
			"-o", "jsonpath={.status.containerStatuses[0].state.terminated.reason}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).NotTo(BeEmpty(), "container not yet terminated")
		reason = out
	}, timeout, poll).Should(Succeed())
	return reason
}

// waitForInvestigationComplete waits for an Investigation CR with phase "Complete"
// in any namespace and returns its namespace and name.
func waitForInvestigationComplete(timeout, poll time.Duration) (string, string) {
	var ns, name string
	EventuallyWithOffset(1, func(g Gomega) {
		// Get all investigations across namespaces
		out, err := runCmd("kubectl", "get", "investigations.detective.arcdetective.io",
			"-A", "-o", "jsonpath={range .items[?(@.status.phase==\"Complete\")]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}")
		g.Expect(err).NotTo(HaveOccurred(), "failed to list investigations: %s", out)
		lines := strings.Fields(out)
		g.Expect(lines).NotTo(BeEmpty(), "no completed investigations found")
		parts := strings.SplitN(lines[0], "/", 2)
		g.Expect(parts).To(HaveLen(2))
		ns = parts[0]
		name = parts[1]
	}, timeout, poll).Should(Succeed())
	return ns, name
}

// waitForInvestigationWithTrigger waits for a completed Investigation with a specific trigger type.
func waitForInvestigationWithTrigger(triggerType string, timeout, poll time.Duration) (string, string) {
	var ns, name string
	EventuallyWithOffset(1, func(g Gomega) {
		// List all completed investigations and find one with the matching trigger type
		out, err := runCmd("kubectl", "get", "investigations.detective.arcdetective.io",
			"-A", "-o", "jsonpath={range .items[*]}{.status.phase},{.spec.trigger.type},{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}")
		g.Expect(err).NotTo(HaveOccurred(), "failed to list investigations: %s", out)
		var found bool
		for _, line := range strings.Split(out, "\n") {
			parts := strings.SplitN(line, ",", 3)
			if len(parts) != 3 {
				continue
			}
			phase, trigger, nsName := parts[0], parts[1], parts[2]
			if phase == "Complete" && trigger == triggerType {
				nsParts := strings.SplitN(nsName, "/", 2)
				g.Expect(nsParts).To(HaveLen(2))
				ns = nsParts[0]
				name = nsParts[1]
				found = true
				break
			}
		}
		g.Expect(found).To(BeTrue(), "no completed investigation with trigger type %s found", triggerType)
	}, timeout, poll).Should(Succeed())
	return ns, name
}

// waitForInvestigationByName waits for a specific Investigation CR to reach Complete status.
func waitForInvestigationByName(namespace, name string, timeout, poll time.Duration) {
	EventuallyWithOffset(1, func(g Gomega) {
		out, err := runCmd("kubectl", "get", "investigation.detective.arcdetective.io", name,
			"-n", namespace,
			"-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred(), "failed to get investigation %s/%s: %s", namespace, name, out)
		g.Expect(out).To(Equal("Complete"), "investigation %s/%s phase is %s, want Complete", namespace, name, out)
	}, timeout, poll).Should(Succeed())
}

// getInvestigationField retrieves a jsonpath field from an Investigation CR.
func getInvestigationField(namespace, name, jsonpath string) string {
	out, err := runCmd("kubectl", "get", "investigation.detective.arcdetective.io", name,
		"-n", namespace,
		"-o", fmt.Sprintf("jsonpath=%s", jsonpath))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to get investigation field: %s", out)
	return out
}

// dumpHelmInfo dumps Helm release values and rendered manifests for debugging.
func dumpHelmInfo(helmBin, releaseName, namespace, version, valuesPath string) {
	fmt.Fprintf(GinkgoWriter, "\n=== HELM DEBUG INFO ===\n")

	// Dump the values file used
	if valuesPath != "" {
		fmt.Fprintf(GinkgoWriter, "\n--- values file: %s ---\n", valuesPath)
		content, err := os.ReadFile(valuesPath)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "ERROR reading values file: %v\n", err)
		} else {
			fmt.Fprintf(GinkgoWriter, "%s\n", string(content))
		}
	}

	// Dump computed values of the installed release
	fmt.Fprintf(GinkgoWriter, "\n--- helm get values %s ---\n", releaseName)
	out, err := runCmd(helmBin, "get", "values", releaseName, "-n", namespace, "-a")
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "ERROR: %v\n%s\n", err, out)
	} else {
		fmt.Fprintf(GinkgoWriter, "%s\n", out)
	}

	// Dump rendered manifests
	fmt.Fprintf(GinkgoWriter, "\n--- helm get manifest %s ---\n", releaseName)
	out, err = runCmd(helmBin, "get", "manifest", releaseName, "-n", namespace)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "ERROR: %v\n%s\n", err, out)
	} else {
		fmt.Fprintf(GinkgoWriter, "%s\n", out)
	}
}

// dumpDiagnostics collects debug info when a test fails.
func dumpDiagnostics(kindBin, kindCluster, detectiveNS, runnerNS string) {
	fmt.Fprintf(GinkgoWriter, "\n=== DIAGNOSTIC DUMP (test failed) ===\n")

	dumps := []struct {
		label string
		args  []string
	}{
		{"all pods (wide)",
			[]string{"kubectl", "get", "pods", "-A", "-o", "wide"}},
		{"describe all pods in " + runnerNS,
			[]string{"kubectl", "describe", "pods", "-n", runnerNS}},
		{"describe all pods in " + detectiveNS,
			[]string{"kubectl", "describe", "pods", "-n", detectiveNS}},
		{"describe all pods in arc-systems",
			[]string{"kubectl", "describe", "pods", "-n", arcSystemNS}},
		{"arc-detective controller logs",
			[]string{"kubectl", "logs", "-l", "control-plane=controller-manager", "-n", detectiveNS, "--tail=200", "--all-containers"}},
		{"ARC controller logs (arc-systems)",
			[]string{"kubectl", "logs", "-n", arcSystemNS, "--tail=200", "--all-containers", "-l", "app.kubernetes.io/part-of=gha-rs-controller"}},
		{"ARC listener logs (arc-systems)",
			[]string{"kubectl", "logs", "-n", arcSystemNS, "--tail=200", "--all-containers", "-l", "actions.github.com/scale-set-name=" + arcReleaseName}},
		{"runner pod details",
			[]string{"kubectl", "describe", "pods", "-l", "actions-ephemeral-runner=True", "-A"}},
		{"investigations",
			[]string{"kubectl", "get", "investigations.detective.arcdetective.io", "-A", "-o", "yaml"}},
		{"detective configs",
			[]string{"kubectl", "get", "detectiveconfigs.detective.arcdetective.io", "-A", "-o", "yaml"}},
		{"events in " + runnerNS,
			[]string{"kubectl", "get", "events", "-n", runnerNS, "--sort-by=.lastTimestamp"}},
		{"events in " + detectiveNS,
			[]string{"kubectl", "get", "events", "-n", detectiveNS, "--sort-by=.lastTimestamp"}},
		{"events in arc-systems",
			[]string{"kubectl", "get", "events", "-n", arcSystemNS, "--sort-by=.lastTimestamp"}},
		{"ephemeral runners",
			[]string{"kubectl", "get", "ephemeralrunners.actions.github.com", "-A", "-o", "yaml"}},
		{"autoscaling runner sets",
			[]string{"kubectl", "get", "autoscalingrunnersets.actions.github.com", "-A", "-o", "yaml"}},
		{"autoscaling listeners",
			[]string{"kubectl", "get", "autoscalinglisteners.actions.github.com", "-A", "-o", "yaml"}},
	}

	for _, d := range dumps {
		fmt.Fprintf(GinkgoWriter, "\n--- %s ---\n", d.label)
		out, err := runCmd(d.args[0], d.args[1:]...)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "ERROR: %v\n%s\n", err, out)
		} else {
			fmt.Fprintf(GinkgoWriter, "%s\n", out)
		}
	}

	// Export Kind logs
	if kindBin != "" && kindCluster != "" {
		fmt.Fprintf(GinkgoWriter, "\n--- kind export logs ---\n")
		out, err := runCmd(kindBin, "export", "logs", "/tmp/kind-e2e-full-logs", "--name", kindCluster)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "ERROR: %v\n%s\n", err, out)
		} else {
			fmt.Fprintf(GinkgoWriter, "Kind logs exported to /tmp/kind-e2e-full-logs\n")
		}
	}
}
