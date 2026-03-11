package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/mumoshu/arc-detective/internal/logcollector"
)

const (
	arcRunnerLabel        = "actions-ephemeral-runner"
	logCollectorFinalizer = "detective.arcdetective.io/log-collector"
)

// PodWatcher watches ARC runner pods for anomalies and collects logs before deletion.
type PodWatcher struct {
	client.Client
	Collector logcollector.Collector

	mu         sync.Mutex
	podHistory map[types.NamespacedName]*podTracker
}

type podTracker struct {
	lastPhase        corev1.PodPhase
	containerStates  map[string]string // container name -> last seen state
	phaseTransitions []v1alpha1.StatusTransition
}

func NewPodWatcher(c client.Client, collector logcollector.Collector) *PodWatcher {
	return &PodWatcher{
		Client:     c,
		Collector:  collector,
		podHistory: make(map[types.NamespacedName]*podTracker),
	}
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=detective.arcdetective.io,resources=investigations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=detective.arcdetective.io,resources=investigations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=detective.arcdetective.io,resources=detectiveconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=actions.github.com,resources=ephemeralrunners,verbs=get;list;watch

func (r *PodWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			r.mu.Lock()
			delete(r.podHistory, req.NamespacedName)
			r.mu.Unlock()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only handle ARC runner pods
	if pod.Labels[arcRunnerLabel] != "True" {
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present (but not if the pod is already being deleted)
	if pod.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&pod, logCollectorFinalizer) {
		controllerutil.AddFinalizer(&pod, logCollectorFinalizer)
		if err := r.Update(ctx, &pod); err != nil {
			return ctrl.Result{}, err
		}
		logger.V(1).Info("Added log-collector finalizer")
	}

	// Track state transitions
	r.recordTransition(req.NamespacedName, &pod)

	// Handle deletion — collect logs before allowing pod to be deleted
	if !pod.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(&pod, logCollectorFinalizer) {
		logger.Info("Pod is being deleted, collecting logs")
		logPaths, err := r.Collector.CollectLogs(ctx, &pod)
		if err != nil {
			logger.Error(err, "Failed to collect logs, proceeding with deletion anyway")
		}

		// Create or update Investigation with log paths
		if len(logPaths) > 0 {
			if err := r.ensureInvestigation(ctx, &pod, logPaths); err != nil {
				logger.Error(err, "Failed to create investigation for log collection")
			}
		}

		controllerutil.RemoveFinalizer(&pod, logCollectorFinalizer)
		if err := r.Update(ctx, &pod); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Removed log-collector finalizer")
		return ctrl.Result{}, nil
	}

	// Check for anomalies
	if anomaly := r.detectAnomaly(&pod); anomaly != "" {
		logger.Info("Detected anomaly", "type", anomaly)
		if err := r.createAnomalyInvestigation(ctx, &pod, anomaly); err != nil {
			logger.Error(err, "Failed to create investigation for anomaly")
		}
	}

	return ctrl.Result{}, nil
}

func (r *PodWatcher) recordTransition(key types.NamespacedName, pod *corev1.Pod) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tracker, ok := r.podHistory[key]
	if !ok {
		tracker = &podTracker{
			containerStates: make(map[string]string),
		}
		r.podHistory[key] = tracker
	}

	if pod.Status.Phase != "" && corev1.PodPhase(tracker.lastPhase) != pod.Status.Phase {
		tracker.phaseTransitions = append(tracker.phaseTransitions, v1alpha1.StatusTransition{
			From:      string(tracker.lastPhase),
			To:        string(pod.Status.Phase),
			Timestamp: metav1.Now(),
		})
		tracker.lastPhase = pod.Status.Phase
	}
}

func (r *PodWatcher) detectAnomaly(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
			return "pod-oomkill"
		}
		// Non-zero exit (excluding 137/OOMKill handled above).
		// ARC ephemeral runners use restartPolicy: Never, so they never
		// restart — a non-zero exit means the runner process crashed.
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return "pod-crash"
		}
		// Image pull failure — runner image doesn't exist or can't be pulled.
		if cs.State.Waiting != nil && (cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull") {
			return "pod-init-timeout"
		}
	}
	return ""
}

func (r *PodWatcher) buildPodInfo(pod *corev1.Pod) *v1alpha1.PodInfo {
	info := &v1alpha1.PodInfo{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Phase:     string(pod.Status.Phase),
		NodeName:  pod.Spec.NodeName,
	}

	// Snapshot conditions
	for _, c := range pod.Status.Conditions {
		info.Conditions = append(info.Conditions, v1alpha1.PodConditionSnapshot{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	// Container statuses
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
			csi.OOMKilled = cs.State.Terminated.Reason == "OOMKilled"
		}
		info.ContainerStatuses = append(info.ContainerStatuses, csi)
	}

	// Include phase history from tracker
	r.mu.Lock()
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	if tracker, ok := r.podHistory[key]; ok {
		info.PhaseHistory = tracker.phaseTransitions
	}
	r.mu.Unlock()

	return info
}

func (r *PodWatcher) ensureInvestigation(ctx context.Context, pod *corev1.Pod, logPaths []string) error {
	invName := fmt.Sprintf("pod-%s", pod.Name)
	inv := &v1alpha1.Investigation{}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: invName}

	err := r.Get(ctx, key, inv)
	if apierrors.IsNotFound(err) {
		inv = &v1alpha1.Investigation{
			ObjectMeta: metav1.ObjectMeta{
				Name:      invName,
				Namespace: pod.Namespace,
			},
			Spec: v1alpha1.InvestigationSpec{
				Trigger: v1alpha1.InvestigationTrigger{
					Type:   "pod-deletion",
					Source: fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
				},
				Pod:      r.buildPodInfo(pod),
				LogPaths: logPaths,
			},
		}
		if err := r.Create(ctx, inv); err != nil {
			return err
		}
		// Status subresource requires a separate update after creation
		inv.Status.Phase = "Collecting"
		return r.Status().Update(ctx, inv)
	}
	if err != nil {
		return err
	}

	// Update existing investigation with log paths
	inv.Spec.LogPaths = append(inv.Spec.LogPaths, logPaths...)
	inv.Spec.Pod = r.buildPodInfo(pod)
	return r.Update(ctx, inv)
}

func (r *PodWatcher) createAnomalyInvestigation(ctx context.Context, pod *corev1.Pod, anomalyType string) error {
	invName := fmt.Sprintf("pod-%s", pod.Name)
	inv := &v1alpha1.Investigation{}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: invName}

	if err := r.Get(ctx, key, inv); err == nil {
		// Already exists
		return nil
	}

	now := metav1.Now()
	_ = now
	inv = &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      invName,
			Namespace: pod.Namespace,
		},
		Spec: v1alpha1.InvestigationSpec{
			Trigger: v1alpha1.InvestigationTrigger{
				Type:   anomalyType,
				Source: fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			},
			Pod: r.buildPodInfo(pod),
			Timeline: []v1alpha1.TimelineEvent{
				{
					Timestamp: metav1.Now(),
					Source:    "pod",
					Type:      anomalyType,
					Message:   fmt.Sprintf("Anomaly detected on pod %s: %s", pod.Name, anomalyType),
				},
			},
		},
	}
	if err := r.Create(ctx, inv); err != nil {
		return err
	}
	// Status subresource requires a separate update after creation
	inv.Status.Phase = "Collecting"
	return r.Status().Update(ctx, inv)
}

func (r *PodWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetLabels()[arcRunnerLabel] == "True"
		})).
		Named("pod-watcher").
		Complete(r)
}

// Ensure compile-time interface compliance
var _ = time.Second
