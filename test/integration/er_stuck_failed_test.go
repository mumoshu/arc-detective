package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEphemeralRunnerStuckFailedCreatesInvestigation(t *testing.T) {
	ns := createTestNamespace(t)

	// Create EphemeralRunner first (status is ignored on Create due to status subresource)
	er := newEphemeralRunner(ns, "runner-stuck-test", nil)
	require.NoError(t, k8sClient.Create(ctx, er))

	// Then update status to Failed via status subresource
	updateERStatus(t, ns, "runner-stuck-test", map[string]interface{}{
		"phase":   "Failed",
		"reason":  "RunnerDeregistered",
		"message": "Runner deregistered from GitHub",
	})

	// Wait for the ER watcher to detect the stuck state and create an Investigation
	// (stuckThreshold is 1s in test setup)
	inv := waitForInvestigation(t, ns, 15*time.Second)
	assert.NotNil(t, inv)
	assert.Equal(t, "runner-stuck", inv.Spec.Trigger.Type)
	assert.NotNil(t, inv.Spec.EphemeralRunner)
	if inv.Spec.EphemeralRunner != nil {
		assert.Equal(t, "Failed", inv.Spec.EphemeralRunner.Phase)
		assert.Equal(t, "runner-stuck-test", inv.Spec.EphemeralRunner.Name)
	}
}
