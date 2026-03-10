package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DetectiveConfigSpec struct {
	// Repositories lists the GitHub repos to monitor for failed workflow jobs.
	Repositories []RepositoryRef `json:"repositories"`
	// GitHubAuth references a Secret containing GitHub credentials.
	GitHubAuth GitHubAuthRef `json:"githubAuth"`
	// PollInterval controls how often the GitHub API is polled. Default: 30s.
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`
	// LogStorage configures where collected pod logs are stored.
	LogStorage LogStorageSpec `json:"logStorage"`
	// RetentionPeriod controls how long Investigation CRs are kept. Default: 7d.
	// +optional
	RetentionPeriod *metav1.Duration `json:"retentionPeriod,omitempty"`
}

type RepositoryRef struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type GitHubAuthRef struct {
	// Type is "app" or "pat".
	// +kubebuilder:validation:Enum=app;pat
	Type string `json:"type"`
	// SecretName references the Secret containing credentials.
	SecretName string `json:"secretName"`
}

type LogStorageSpec struct {
	// PVCName is the PersistentVolumeClaim used for log storage.
	PVCName string `json:"pvcName"`
	// MaxSizeMB is the maximum storage usage before oldest logs are pruned.
	// +optional
	MaxSizeMB *int `json:"maxSizeMB,omitempty"`
}

type DetectiveConfigStatus struct {
	// Phase indicates the current state of the config: "Ready", "Error".
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message provides additional detail when Phase is Error.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// DetectiveConfig is the singleton configuration for ARC Detective.
type DetectiveConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec   DetectiveConfigSpec   `json:"spec"`
	Status DetectiveConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DetectiveConfigList contains a list of DetectiveConfig.
type DetectiveConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DetectiveConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DetectiveConfig{}, &DetectiveConfigList{})
}
