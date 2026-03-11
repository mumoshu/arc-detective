package diagnosis

import (
	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
)

// FailurePattern describes a known ARC failure mode.
type FailurePattern struct {
	Type        string
	Description string
	Match       func(spec *v1alpha1.InvestigationSpec) bool
	Remediation string
}

// KnownPatterns is the ordered list of failure patterns to check.
// Patterns are evaluated in order; the first match wins.
var KnownPatterns = []FailurePattern{
	{
		Type:        "pod-oomkilled",
		Description: "Runner pod OOMKilled",
		Match: func(spec *v1alpha1.InvestigationSpec) bool {
			if spec.Pod == nil {
				return false
			}
			for _, cs := range spec.Pod.ContainerStatuses {
				if cs.OOMKilled {
					return true
				}
				if cs.State == "terminated" && cs.ExitCode != nil && *cs.ExitCode == 137 {
					return true
				}
			}
			return false
		},
		Remediation: "Increase memory limits on the runner pod template.",
	},
	{
		Type:        "pod-crashed",
		Description: "Runner pod exited with non-zero exit code",
		Match: func(spec *v1alpha1.InvestigationSpec) bool {
			if spec.Pod == nil {
				return false
			}
			for _, cs := range spec.Pod.ContainerStatuses {
				if cs.State == "terminated" && cs.ExitCode != nil && *cs.ExitCode != 0 && !cs.OOMKilled {
					return true
				}
			}
			return false
		},
		Remediation: "Check collected logs for the crash reason. Common causes: broken entrypoint scripts, missing binaries in the runner image, or incompatible runner versions.",
	},
	{
		Type:        "pod-stuck-terminating",
		Description: "Runner pod stuck Terminating",
		Match: func(spec *v1alpha1.InvestigationSpec) bool {
			if spec.Pod == nil {
				return false
			}
			return spec.Pod.Phase == "Terminating"
		},
		Remediation: "Force-delete the pod. Check node autoscaler configuration.",
	},
	{
		Type:        "pod-init-timeout",
		Description: "Runner pod killed during init",
		Match: func(spec *v1alpha1.InvestigationSpec) bool {
			if spec.Pod == nil {
				return false
			}
			// Check for terminated containers while pod was in Pending
			if spec.Pod.Phase != "Pending" && spec.Pod.Phase != "Failed" {
				return false
			}
			for _, cs := range spec.Pod.ContainerStatuses {
				if cs.State == "waiting" && (cs.Reason == "ImagePullBackOff" || cs.Reason == "ErrImagePull") {
					return true
				}
			}
			return false
		},
		Remediation: "Pre-pull runner images or use a registry mirror closer to the cluster.",
	},
	{
		Type:        "runner-stuck-running",
		Description: "EphemeralRunner stuck in Running state",
		Match: func(spec *v1alpha1.InvestigationSpec) bool {
			if spec.EphemeralRunner == nil || spec.EphemeralRunner.Phase != "Running" {
				return false
			}
			// Variant 1: job completed but runner still running (ARC cleanup failed)
			if spec.Job != nil && spec.Job.Status == "completed" {
				return true
			}
			// Variant 2: runner running past threshold (triggered by watcher)
			if spec.Trigger.Type == "runner-stuck" {
				return true
			}
			return false
		},
		Remediation: "Delete the stuck EphemeralRunner manually. Check ARC controller logs.",
	},
	{
		Type:        "runner-stuck-failed",
		Description: "EphemeralRunner stuck in Failed, not cleaned up",
		Match: func(spec *v1alpha1.InvestigationSpec) bool {
			if spec.EphemeralRunner == nil {
				return false
			}
			return spec.EphemeralRunner.Phase == "Failed"
		},
		Remediation: "Delete the stuck EphemeralRunner. This is a known ARC issue.",
	},
	{
		Type:        "job-stuck-queued",
		Description: "GitHub job stuck queued with no viable runner",
		Match: func(spec *v1alpha1.InvestigationSpec) bool {
			if spec.Job == nil {
				return false
			}
			return spec.Job.Status == "queued" && spec.EphemeralRunner == nil
		},
		Remediation: "Check AutoScalingRunnerSet status and listener pod health.",
	},
}
