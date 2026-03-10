package diagnosis

import (
	"fmt"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
)

// Classify examines the collected evidence and returns a Diagnosis if a known
// failure pattern matches. Returns nil if no pattern matches.
func Classify(spec *v1alpha1.InvestigationSpec) *v1alpha1.Diagnosis {
	for _, pattern := range KnownPatterns {
		if pattern.Match(spec) {
			return &v1alpha1.Diagnosis{
				FailureType: pattern.Type,
				Summary:     buildSummary(spec, pattern),
				Remediation: pattern.Remediation,
			}
		}
	}
	return nil
}

func buildSummary(spec *v1alpha1.InvestigationSpec, pattern FailurePattern) string {
	summary := pattern.Description

	if spec.Pod != nil {
		summary = fmt.Sprintf("%s (pod %s/%s)", summary, spec.Pod.Namespace, spec.Pod.Name)
	}
	if spec.EphemeralRunner != nil {
		summary = fmt.Sprintf("%s (runner %s/%s)", summary, spec.EphemeralRunner.Namespace, spec.EphemeralRunner.Name)
	}
	if spec.Job != nil {
		summary = fmt.Sprintf("%s [job: %s]", summary, spec.Job.Name)
	}

	return summary
}
