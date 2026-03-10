package diagnosis

import (
	"testing"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func ptr[T any](v T) *T { return &v }

func TestClassifyOOMKilled(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		Pod: &v1alpha1.PodInfo{
			Name: "runner-abc", Namespace: "ns1",
			ContainerStatuses: []v1alpha1.ContainerStatusInfo{{
				Name: "runner", State: "terminated",
				OOMKilled: true, ExitCode: ptr(int32(137)),
			}},
		},
	}
	diag := Classify(spec)
	assert.NotNil(t, diag)
	assert.Equal(t, "pod-oomkilled", diag.FailureType)
	assert.Contains(t, diag.Remediation, "memory limits")
}

func TestClassifyOOMKilledByExitCode(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		Pod: &v1alpha1.PodInfo{
			Name: "runner-xyz", Namespace: "ns1",
			ContainerStatuses: []v1alpha1.ContainerStatusInfo{{
				Name: "runner", State: "terminated",
				ExitCode: ptr(int32(137)),
			}},
		},
	}
	diag := Classify(spec)
	assert.NotNil(t, diag)
	assert.Equal(t, "pod-oomkilled", diag.FailureType)
}

func TestClassifyRunnerStuckRunning(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		EphemeralRunner: &v1alpha1.EphemeralRunnerInfo{
			Name: "runner-1", Namespace: "ns1", Phase: "Running",
		},
		Job: &v1alpha1.JobInfo{Status: "completed", Conclusion: "failure"},
	}
	diag := Classify(spec)
	assert.NotNil(t, diag)
	assert.Equal(t, "runner-stuck-running", diag.FailureType)
}

func TestClassifyRunnerStuckFailed(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		EphemeralRunner: &v1alpha1.EphemeralRunnerInfo{
			Name: "runner-2", Namespace: "ns1", Phase: "Failed",
		},
	}
	diag := Classify(spec)
	assert.NotNil(t, diag)
	assert.Equal(t, "runner-stuck-failed", diag.FailureType)
}

func TestClassifyJobStuckQueued(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		Job:             &v1alpha1.JobInfo{Status: "queued", Name: "deploy"},
		EphemeralRunner: nil,
	}
	diag := Classify(spec)
	assert.NotNil(t, diag)
	assert.Equal(t, "job-stuck-queued", diag.FailureType)
	assert.Contains(t, diag.Remediation, "AutoScalingRunnerSet")
}

func TestClassifyPodCrashLoop(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		Pod: &v1alpha1.PodInfo{
			Name: "runner-crash", Namespace: "ns1",
			ContainerStatuses: []v1alpha1.ContainerStatusInfo{{
				Name: "runner", RestartCount: 3,
				State: "waiting", Reason: "CrashLoopBackOff",
			}},
		},
		EphemeralRunner: &v1alpha1.EphemeralRunnerInfo{Phase: "Failed"},
	}
	diag := Classify(spec)
	assert.NotNil(t, diag)
	assert.Equal(t, "pod-crashloop", diag.FailureType)
}

func TestClassifyPodStuckTerminating(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		Pod: &v1alpha1.PodInfo{
			Name: "runner-term", Namespace: "ns1", Phase: "Terminating",
		},
	}
	diag := Classify(spec)
	assert.NotNil(t, diag)
	assert.Equal(t, "pod-stuck-terminating", diag.FailureType)
}

func TestClassifyNoMatch(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		Pod: &v1alpha1.PodInfo{Phase: "Succeeded"},
		Job: &v1alpha1.JobInfo{Status: "completed", Conclusion: "success"},
	}
	diag := Classify(spec)
	assert.Nil(t, diag)
}

func TestClassifyNilFields(t *testing.T) {
	spec := &v1alpha1.InvestigationSpec{
		Trigger: v1alpha1.InvestigationTrigger{Type: "unknown", Source: "test"},
	}
	diag := Classify(spec)
	assert.Nil(t, diag)
}
