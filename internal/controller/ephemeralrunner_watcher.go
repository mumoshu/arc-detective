package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var ephemeralRunnerGVK = schema.GroupVersionKind{
	Group:   "actions.github.com",
	Version: "v1alpha1",
	Kind:    "EphemeralRunner",
}

// EphemeralRunnerWatcher watches ARC EphemeralRunner CRs for stuck states.
type EphemeralRunnerWatcher struct {
	client.Client
	StuckThreshold   time.Duration // how long a Failed ER must exist before triggering
	RunningThreshold time.Duration // how long a Running ER must exist before triggering

	mu       sync.Mutex
	erStatus map[types.NamespacedName]*erTracker
}

type erTracker struct {
	lastPhase    string
	runningSince *time.Time
	transitions  []v1alpha1.StatusTransition
}

func NewEphemeralRunnerWatcher(c client.Client, stuckThreshold, runningThreshold time.Duration) *EphemeralRunnerWatcher {
	return &EphemeralRunnerWatcher{
		Client:           c,
		StuckThreshold:   stuckThreshold,
		RunningThreshold: runningThreshold,
		erStatus:         make(map[types.NamespacedName]*erTracker),
	}
}

func (r *EphemeralRunnerWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var er unstructured.Unstructured
	er.SetGroupVersionKind(ephemeralRunnerGVK)
	if err := r.Get(ctx, req.NamespacedName, &er); err != nil {
		if apierrors.IsNotFound(err) {
			r.mu.Lock()
			delete(r.erStatus, req.NamespacedName)
			r.mu.Unlock()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	phase := getNestedString(er.Object, "status", "phase")
	if phase == "" {
		return ctrl.Result{}, nil
	}

	r.recordTransition(req.NamespacedName, phase)

	switch phase {
	case "Failed":
		return r.handleFailed(ctx, req, &er, phase, logger)
	case "Running":
		return r.handleRunning(ctx, req, &er, phase, logger)
	}

	return ctrl.Result{}, nil
}

func (r *EphemeralRunnerWatcher) handleFailed(ctx context.Context, req ctrl.Request, er *unstructured.Unstructured, phase string, logger interface{ Info(string, ...any) }) (ctrl.Result, error) {
	age := time.Since(er.GetCreationTimestamp().Time)
	if age < r.StuckThreshold {
		return ctrl.Result{RequeueAfter: r.StuckThreshold - age}, nil
	}

	logger.Info("EphemeralRunner stuck in Failed state", "name", req.Name, "age", age)
	if err := r.createInvestigation(ctx, er, "runner-stuck", phase); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *EphemeralRunnerWatcher) handleRunning(ctx context.Context, req ctrl.Request, er *unstructured.Unstructured, phase string, logger interface{ Info(string, ...any) }) (ctrl.Result, error) {
	r.mu.Lock()
	tracker := r.erStatus[req.NamespacedName]
	r.mu.Unlock()

	if tracker == nil || tracker.runningSince == nil {
		return ctrl.Result{RequeueAfter: r.RunningThreshold}, nil
	}

	elapsed := time.Since(*tracker.runningSince)
	if elapsed < r.RunningThreshold {
		return ctrl.Result{RequeueAfter: r.RunningThreshold - elapsed}, nil
	}

	logger.Info("EphemeralRunner stuck in Running state", "name", req.Name, "runningSince", tracker.runningSince)
	if err := r.createInvestigation(ctx, er, "runner-stuck", phase); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *EphemeralRunnerWatcher) recordTransition(key types.NamespacedName, phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tracker, ok := r.erStatus[key]
	if !ok {
		tracker = &erTracker{}
		r.erStatus[key] = tracker
	}

	if tracker.lastPhase != phase {
		tracker.transitions = append(tracker.transitions, v1alpha1.StatusTransition{
			From:      tracker.lastPhase,
			To:        phase,
			Timestamp: metav1.Now(),
		})
		tracker.lastPhase = phase

		if phase == "Running" {
			now := time.Now()
			tracker.runningSince = &now
		} else {
			tracker.runningSince = nil
		}
	}
}

func (r *EphemeralRunnerWatcher) createInvestigation(ctx context.Context, er *unstructured.Unstructured, triggerType, phase string) error {
	invName := fmt.Sprintf("er-%s", er.GetName())
	inv := &v1alpha1.Investigation{}
	key := types.NamespacedName{Namespace: er.GetNamespace(), Name: invName}

	if err := r.Get(ctx, key, inv); err == nil {
		return nil // already exists
	}

	r.mu.Lock()
	erKey := types.NamespacedName{Namespace: er.GetNamespace(), Name: er.GetName()}
	tracker := r.erStatus[erKey]
	var transitions []v1alpha1.StatusTransition
	if tracker != nil {
		transitions = tracker.transitions
	}
	r.mu.Unlock()

	inv = &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      invName,
			Namespace: er.GetNamespace(),
		},
		Spec: v1alpha1.InvestigationSpec{
			Trigger: v1alpha1.InvestigationTrigger{
				Type:   triggerType,
				Source: fmt.Sprintf("%s/%s", er.GetNamespace(), er.GetName()),
			},
			EphemeralRunner: &v1alpha1.EphemeralRunnerInfo{
				Name:          er.GetName(),
				Namespace:     er.GetNamespace(),
				Phase:         phase,
				StatusHistory: transitions,
			},
			Timeline: []v1alpha1.TimelineEvent{
				{
					Timestamp: metav1.Now(),
					Source:    "ephemeralrunner",
					Type:      triggerType,
					Message:   fmt.Sprintf("EphemeralRunner %s in %s state", er.GetName(), phase),
				},
			},
		},
	}
	if err := r.Create(ctx, inv); err != nil {
		return err
	}
	// Status subresource requires a separate update after creation
	inv.Status.Phase = phaseCollecting
	return r.Status().Update(ctx, inv)
}

func (r *EphemeralRunnerWatcher) SetupWithManager(mgr ctrl.Manager) error {
	er := &unstructured.Unstructured{}
	er.SetGroupVersionKind(ephemeralRunnerGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(er).
		Named("ephemeralrunner-watcher").
		Complete(r)
}

func getNestedString(obj map[string]any, fields ...string) string {
	val, _, _ := unstructured.NestedString(obj, fields...)
	return val
}
