package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InvestigationSpec is the forensic report content — all collected evidence.
// Controllers populate these fields as they gather data.
type InvestigationSpec struct {
	// Trigger describes what initiated this investigation.
	Trigger InvestigationTrigger `json:"trigger"`

	// WorkflowRun holds data collected from the GitHub Actions API.
	// +optional
	WorkflowRun *WorkflowRunInfo `json:"workflowRun,omitempty"`
	// Job holds data about the specific GitHub Actions job.
	// +optional
	Job *JobInfo `json:"job,omitempty"`

	// EphemeralRunner holds data collected from the ARC EphemeralRunner CR.
	// +optional
	EphemeralRunner *EphemeralRunnerInfo `json:"ephemeralRunner,omitempty"`
	// Pod holds data collected from the runner pod.
	// +optional
	Pod *PodInfo `json:"pod,omitempty"`

	// Timeline is a correlated sequence of events from all sources.
	// +optional
	Timeline []TimelineEvent `json:"timeline,omitempty"`

	// Diagnosis is the classified failure and suggested remediation.
	// +optional
	Diagnosis *Diagnosis `json:"diagnosis,omitempty"`

	// LogPaths lists paths to collected logs on the PV.
	// +optional
	LogPaths []string `json:"logPaths,omitempty"`
}

type InvestigationTrigger struct {
	// Type classifies the trigger: "runner-stuck", "job-failed", "pod-crash",
	// "pod-oomkill", "pod-stuck-terminating".
	Type string `json:"type"`
	// Source identifies the resource that triggered the investigation.
	Source string `json:"source"`
}

// InvestigationStatus is the lifecycle state of the investigation.
type InvestigationStatus struct {
	// Phase is "Collecting", "Complete", or "Failed".
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message provides detail when Phase is "Failed".
	// +optional
	Message string `json:"message,omitempty"`
}

type WorkflowRunInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	HTMLURL    string `json:"htmlURL"`
	Repository string `json:"repository"`
}

type JobInfo struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Status      string       `json:"status"`
	Conclusion  string       `json:"conclusion,omitempty"`
	RunnerName  string       `json:"runnerName,omitempty"`
	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	Steps       []JobStep    `json:"steps,omitempty"`
}

type JobStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

type EphemeralRunnerInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Phase is a snapshot of the EphemeralRunner phase at investigation time.
	Phase string `json:"phase"`
	// StatusHistory records observed phase transitions.
	// +optional
	StatusHistory []StatusTransition `json:"statusHistory,omitempty"`
}

type StatusTransition struct {
	From      string      `json:"from"`
	To        string      `json:"to"`
	Timestamp metav1.Time `json:"timestamp"`
}

type PodInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Phase     string `json:"phase"`
	// PhaseHistory records pod phase transitions (e.g. Pending→Running→Succeeded).
	// +optional
	PhaseHistory []StatusTransition `json:"phaseHistory,omitempty"`
	// ConditionTransitions records changes in pod conditions over time.
	// +optional
	ConditionTransitions []ConditionTransition `json:"conditionTransitions,omitempty"`
	// Conditions is a snapshot of current pod conditions.
	// +optional
	Conditions []PodConditionSnapshot `json:"conditions,omitempty"`
	// ContainerStatuses holds per-container status information.
	// +optional
	ContainerStatuses []ContainerStatusInfo `json:"containerStatuses,omitempty"`
	// Events holds Kubernetes events related to this pod.
	// +optional
	Events []EventSnapshot `json:"events,omitempty"`
	// NodeName is the node the pod was scheduled on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
}

type ConditionTransition struct {
	// Type is the condition type: "Ready", "PodScheduled", "ContainersReady", "Initialized".
	Type      string      `json:"type"`
	Status    string      `json:"status"` // "True", "False", "Unknown"
	Reason    string      `json:"reason,omitempty"`
	Message   string      `json:"message,omitempty"`
	Timestamp metav1.Time `json:"timestamp"`
}

type PodConditionSnapshot struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type EventSnapshot struct {
	// Type is "Normal" or "Warning".
	Type      string      `json:"type"`
	Reason    string      `json:"reason"`
	Message   string      `json:"message"`
	Count     int32       `json:"count,omitempty"`
	Source    string      `json:"source,omitempty"` // e.g. "kubelet", "default-scheduler"
	FirstSeen metav1.Time `json:"firstSeen"`
	LastSeen  metav1.Time `json:"lastSeen"`
}

type ContainerStatusInfo struct {
	Name         string `json:"name"`
	State        string `json:"state"` // "waiting", "running", "terminated"
	Reason       string `json:"reason,omitempty"`
	ExitCode     *int32 `json:"exitCode,omitempty"`
	RestartCount int32  `json:"restartCount"`
	OOMKilled    bool   `json:"oomKilled,omitempty"`
	// StateHistory records container state transitions.
	// +optional
	StateHistory []ContainerStateTransition `json:"stateHistory,omitempty"`
}

type ContainerStateTransition struct {
	From      string      `json:"from"` // "waiting", "running", "terminated"
	To        string      `json:"to"`
	Reason    string      `json:"reason,omitempty"` // e.g. "OOMKilled", "Completed", "Error"
	ExitCode  *int32      `json:"exitCode,omitempty"`
	Timestamp metav1.Time `json:"timestamp"`
}

type TimelineEvent struct {
	Timestamp metav1.Time `json:"timestamp"`
	Source    string      `json:"source"` // "github", "ephemeralrunner", "pod", "event"
	Type      string      `json:"type"`
	Message   string      `json:"message"`
}

type Diagnosis struct {
	// FailureType is the classified failure type, e.g. "pod-oomkilled".
	FailureType string `json:"failureType"`
	// Summary is a human-readable description of the failure.
	Summary string `json:"summary"`
	// Remediation suggests how to fix or prevent the failure.
	// +optional
	Remediation string `json:"remediation,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.trigger.type`
// +kubebuilder:printcolumn:name="Failure",type=string,JSONPath=`.spec.diagnosis.failureType`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Investigation is a forensic report created when a failure is detected.
type Investigation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec   InvestigationSpec   `json:"spec"`
	Status InvestigationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InvestigationList contains a list of Investigation.
type InvestigationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Investigation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Investigation{}, &InvestigationList{})
}
