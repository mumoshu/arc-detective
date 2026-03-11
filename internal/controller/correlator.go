package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/mumoshu/arc-detective/internal/diagnosis"
	gh "github.com/mumoshu/arc-detective/internal/github"
)

// Correlator watches Investigation CRs in "Collecting" phase and enriches them
// with data from both sides (GitHub + K8s), then runs diagnosis.
type Correlator struct {
	client.Client
	ghClient gh.Client
}

func NewCorrelator(c client.Client, ghClient gh.Client) *Correlator {
	return &Correlator{Client: c, ghClient: ghClient}
}

func (r *Correlator) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var inv v1alpha1.Investigation
	if err := r.Get(ctx, req.NamespacedName, &inv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if inv.Status.Phase != phaseCollecting {
		return ctrl.Result{}, nil
	}

	logger.Info("Correlating investigation", "trigger", inv.Spec.Trigger.Type)

	// Enrich K8s side if missing
	if inv.Spec.EphemeralRunner == nil || inv.Spec.Pod == nil {
		r.enrichFromK8s(ctx, &inv)
	}

	// Enrich GitHub side if missing
	if inv.Spec.Job == nil && r.ghClient != nil {
		if err := r.enrichFromGitHub(ctx, &inv); err != nil {
			logger.Error(err, "Failed to enrich from GitHub")
		}
	}

	// Collect K8s events for the pod
	if inv.Spec.Pod != nil && len(inv.Spec.Pod.Events) == 0 {
		r.collectPodEvents(ctx, &inv)
	}

	// Build timeline
	inv.Spec.Timeline = BuildTimeline(inv.Spec.Timeline)

	// Run diagnosis
	if inv.Spec.Diagnosis == nil {
		inv.Spec.Diagnosis = diagnosis.Classify(&inv.Spec)
	}

	// Mark complete
	inv.Status.Phase = "Complete"
	if inv.Spec.Diagnosis == nil {
		inv.Spec.Diagnosis = &v1alpha1.Diagnosis{
			FailureType: "unknown",
			Summary:     "No known failure pattern matched the collected evidence.",
		}
	}

	if err := r.Update(ctx, &inv); err != nil {
		return ctrl.Result{}, err
	}
	// Re-fetch after spec update to get the latest resourceVersion for status update
	if err := r.Get(ctx, req.NamespacedName, &inv); err != nil {
		return ctrl.Result{}, err
	}
	inv.Status.Phase = "Complete"
	if err := r.Status().Update(ctx, &inv); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Investigation complete", "failureType", inv.Spec.Diagnosis.FailureType)
	return ctrl.Result{}, nil
}

func (r *Correlator) enrichFromK8s(ctx context.Context, inv *v1alpha1.Investigation) {
	// Try to find matching EphemeralRunner by runner name from the Job
	if inv.Spec.Job != nil && inv.Spec.Job.RunnerName != "" && inv.Spec.EphemeralRunner == nil {
		var erList unstructured.UnstructuredList
		erList.SetGroupVersionKind(ephemeralRunnerGVK)
		if err := r.List(ctx, &erList, client.InNamespace(inv.Namespace)); err == nil {
			for _, er := range erList.Items {
				runnerName := getNestedString(er.Object, "status", "runnerName")
				if runnerName == inv.Spec.Job.RunnerName {
					phase := getNestedString(er.Object, "status", "phase")
					inv.Spec.EphemeralRunner = &v1alpha1.EphemeralRunnerInfo{
						Name:      er.GetName(),
						Namespace: er.GetNamespace(),
						Phase:     phase,
					}
					break
				}
			}
		}
	}

	// Try to find matching Pod by name or labels
	if inv.Spec.EphemeralRunner != nil && inv.Spec.Pod == nil {
		var podList corev1.PodList
		if err := r.List(ctx, &podList,
			client.InNamespace(inv.Spec.EphemeralRunner.Namespace),
			client.MatchingLabels{arcRunnerLabel: "True"},
		); err == nil {
			// Match pod by owner reference or naming convention
			for _, pod := range podList.Items {
				// ARC names pods after the EphemeralRunner
				if pod.Name == inv.Spec.EphemeralRunner.Name || hasOwnerRef(pod, inv.Spec.EphemeralRunner.Name) {
					inv.Spec.Pod = buildPodInfoFromPod(&pod)
					break
				}
			}
		}
	}
}

func (r *Correlator) enrichFromGitHub(ctx context.Context, inv *v1alpha1.Investigation) error {
	if inv.Spec.EphemeralRunner == nil {
		return nil
	}

	// Get repo info from configs
	configs := &v1alpha1.DetectiveConfigList{}
	if err := r.List(ctx, configs); err != nil {
		return err
	}
	if len(configs.Items) == 0 {
		return nil
	}

	for _, config := range configs.Items {
		for _, repo := range config.Spec.Repositories {
			runs, err := r.ghClient.ListWorkflowRuns(ctx, repo.Owner, repo.Name, gh.ListRunsOpts{})
			if err != nil {
				continue
			}
			for _, run := range runs {
				jobs, err := r.ghClient.ListJobsForRun(ctx, repo.Owner, repo.Name, run.ID)
				if err != nil {
					continue
				}
				for _, job := range jobs {
					if job.RunnerName == inv.Spec.EphemeralRunner.Name {
						repoFullName := fmt.Sprintf("%s/%s", repo.Owner, repo.Name)
						inv.Spec.WorkflowRun = &v1alpha1.WorkflowRunInfo{
							ID:         run.ID,
							Name:       run.Name,
							Status:     run.Status,
							Conclusion: run.Conclusion,
							HTMLURL:    run.HTMLURL,
							Repository: repoFullName,
						}
						var startedAt, completedAt *metav1.Time
						if !job.StartedAt.IsZero() {
							t := metav1.NewTime(job.StartedAt)
							startedAt = &t
						}
						if !job.CompletedAt.IsZero() {
							t := metav1.NewTime(job.CompletedAt)
							completedAt = &t
						}
						steps := make([]v1alpha1.JobStep, len(job.Steps))
						for i, s := range job.Steps {
							steps[i] = v1alpha1.JobStep{Name: s.Name, Status: s.Status, Conclusion: s.Conclusion}
						}
						inv.Spec.Job = &v1alpha1.JobInfo{
							ID:          job.ID,
							Name:        job.Name,
							Status:      job.Status,
							Conclusion:  job.Conclusion,
							RunnerName:  job.RunnerName,
							StartedAt:   startedAt,
							CompletedAt: completedAt,
							Steps:       steps,
						}
						return nil
					}
				}
			}
		}
	}
	return nil
}

func (r *Correlator) collectPodEvents(ctx context.Context, inv *v1alpha1.Investigation) {
	if inv.Spec.Pod == nil {
		return
	}

	var eventList corev1.EventList
	if err := r.List(ctx, &eventList,
		client.InNamespace(inv.Spec.Pod.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("involvedObject.name", inv.Spec.Pod.Name)},
	); err != nil {
		return
	}

	for _, evt := range eventList.Items {
		inv.Spec.Pod.Events = append(inv.Spec.Pod.Events, v1alpha1.EventSnapshot{
			Type:      evt.Type,
			Reason:    evt.Reason,
			Message:   evt.Message,
			Count:     evt.Count,
			Source:    evt.Source.Component,
			FirstSeen: evt.FirstTimestamp,
			LastSeen:  evt.LastTimestamp,
		})

		// Also add to timeline
		inv.Spec.Timeline = append(inv.Spec.Timeline, v1alpha1.TimelineEvent{
			Timestamp: evt.LastTimestamp,
			Source:    "event",
			Type:      evt.Reason,
			Message:   fmt.Sprintf("[%s] %s", evt.Source.Component, evt.Message),
		})
	}
}

// BuildTimeline merges and sorts timeline events by timestamp.
func BuildTimeline(events ...[]v1alpha1.TimelineEvent) []v1alpha1.TimelineEvent {
	total := 0
	for _, evts := range events {
		total += len(evts)
	}
	merged := make([]v1alpha1.TimelineEvent, 0, total)
	for _, evts := range events {
		merged = append(merged, evts...)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Timestamp.Before(&merged[j].Timestamp)
	})
	return merged
}

func (r *Correlator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Investigation{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			inv, ok := obj.(*v1alpha1.Investigation)
			if !ok {
				return false
			}
			return inv.Status.Phase == phaseCollecting
		})).
		Named("correlator").
		Complete(r)
}

func hasOwnerRef(pod corev1.Pod, ownerName string) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Name == ownerName {
			return true
		}
	}
	return false
}

func buildPodInfoFromPod(pod *corev1.Pod) *v1alpha1.PodInfo {
	info := &v1alpha1.PodInfo{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Phase:     string(pod.Status.Phase),
		NodeName:  pod.Spec.NodeName,
	}
	for _, c := range pod.Status.Conditions {
		info.Conditions = append(info.Conditions, v1alpha1.PodConditionSnapshot{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	for _, cs := range pod.Status.ContainerStatuses {
		csi := v1alpha1.ContainerStatusInfo{
			Name:         cs.Name,
			RestartCount: cs.RestartCount,
		}
		if cs.State.Waiting != nil {
			csi.State = "waiting"
			csi.Reason = cs.State.Waiting.Reason
		} else if cs.State.Running != nil {
			csi.State = "running"
		} else if cs.State.Terminated != nil {
			csi.State = "terminated"
			csi.Reason = cs.State.Terminated.Reason
			csi.ExitCode = &cs.State.Terminated.ExitCode
			csi.OOMKilled = cs.State.Terminated.Reason == reasonOOMKilled
		}
		info.ContainerStatuses = append(info.ContainerStatuses, csi)
	}
	return info
}
