package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	gh "github.com/mumoshu/arc-detective/internal/github"
)

func TestGitHubPollerDetectsStuckQueuedJob(t *testing.T) {
	ns := createTestNamespace(t)

	// 1. Create a DetectiveConfig pointing at mock GitHub
	pollInterval := metav1.Duration{Duration: 1 * time.Second}
	config := newDetectiveConfig(ns, "test-config", v1alpha1.DetectiveConfigSpec{
		Repositories: []v1alpha1.RepositoryRef{{Owner: "myorg", Name: "myrepo"}},
		PollInterval: &pollInterval,
		GitHubAuth:   v1alpha1.GitHubAuthRef{Type: "pat", SecretName: "gh-secret"},
		LogStorage:   v1alpha1.LogStorageSpec{PVCName: "test-pvc"},
	})
	require.NoError(t, k8sClient.Create(ctx, config))
	createSecret(t, ns, "gh-secret", map[string][]byte{"token": []byte("fake")})

	// 2. Configure mock GitHub with a queued run whose job has been queued for a long time
	longAgo := time.Now().Add(-15 * time.Minute)
	mockGitHub.SetWorkflowRuns("myorg", "myrepo", []gh.WorkflowRun{
		{ID: 5000, Name: "Deploy", Status: "queued", Conclusion: "",
			UpdatedAt: longAgo},
	})
	mockGitHub.SetJobsForRun(5000, []gh.Job{
		{ID: 6000, Name: "deploy-prod", Status: "queued", Conclusion: "",
			StartedAt: longAgo},
	})

	// 3. Wait for Investigation to be created by the poller
	inv := waitForInvestigation(t, ns, 15*time.Second)
	assert.NotNil(t, inv)
	assert.Equal(t, "job-stuck-queued", inv.Spec.Trigger.Type)
	assert.NotNil(t, inv.Spec.Job)
	if inv.Spec.Job != nil {
		assert.Equal(t, "queued", inv.Spec.Job.Status)
		assert.Equal(t, "deploy-prod", inv.Spec.Job.Name)
	}
}
