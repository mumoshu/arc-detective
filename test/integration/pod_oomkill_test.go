package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	gh "github.com/mumoshu/arc-detective/internal/github"
)

func TestPodOOMKillCreatesInvestigation(t *testing.T) {
	ns := createTestNamespace(t)

	// 1. Create a Pod that looks like an ARC runner pod
	pod := newRunnerPod(ns, "runner-oom-test", arcLabels("my-scale-set"))
	require.NoError(t, k8sClient.Create(ctx, pod))

	// 2. Wait for the pod watcher to add its finalizer
	waitForFinalizer(t, ns, "runner-oom-test", "detective.arcdetective.io/log-collector", 10*time.Second)

	// 3. Update pod status to simulate OOMKill
	updatePodStatus(t, ns, "runner-oom-test", func(status *corev1.PodStatus) {
		status.Phase = corev1.PodFailed
		status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "runner",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137,
					Reason:   "OOMKilled",
				},
			},
			RestartCount: 0,
		}}
	})

	// 4. Configure mock GitHub to return matching job info
	mockGitHub.SetWorkflowRuns("myorg", "myrepo", []gh.WorkflowRun{
		{ID: 1000, Name: "CI", Status: "completed", Conclusion: "failure"},
	})
	mockGitHub.SetJobsForRun(1000, []gh.Job{
		{ID: 2000, Name: "build", Status: "completed", Conclusion: "failure",
			RunnerName: "runner-oom-test"},
	})

	// 5. Wait for an Investigation CR to be created
	inv := waitForInvestigation(t, ns, 15*time.Second)
	assert.NotNil(t, inv)
	assert.Equal(t, "pod-oomkill", inv.Spec.Trigger.Type)
	assert.NotNil(t, inv.Spec.Pod)

	// Verify pod info was captured
	if inv.Spec.Pod != nil {
		assert.Equal(t, "runner-oom-test", inv.Spec.Pod.Name)
		assert.NotEmpty(t, inv.Spec.Pod.ContainerStatuses)
		if len(inv.Spec.Pod.ContainerStatuses) > 0 {
			cs := inv.Spec.Pod.ContainerStatuses[0]
			assert.True(t, cs.OOMKilled)
			assert.Equal(t, int32(137), *cs.ExitCode)
		}
	}
}
