package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	gh "github.com/mumoshu/arc-detective/internal/github"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const defaultQueuedThreshold = 10 * time.Minute

// GitHubPoller periodically polls GitHub Actions API for workflow/job state.
type GitHubPoller struct {
	client.Client
	ghClient     gh.Client
	pollInterval time.Duration
	queuedThresh time.Duration
	configName   string
	configNS     string

	mu    sync.Mutex
	cache map[int64]*gh.WorkflowRun // run ID -> last known state
}

func NewGitHubPoller(c client.Client, ghClient gh.Client, pollInterval time.Duration) *GitHubPoller {
	return &GitHubPoller{
		Client:       c,
		ghClient:     ghClient,
		pollInterval: pollInterval,
		queuedThresh: defaultQueuedThreshold,
		cache:        make(map[int64]*gh.WorkflowRun),
	}
}

// Start implements manager.Runnable so the poller is managed by controller-runtime.
func (p *GitHubPoller) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("github-poller")
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	logger.Info("Starting GitHub poller", "interval", p.pollInterval)

	for {
		select {
		case <-ctx.Done():
			logger.Info("Stopping GitHub poller")
			return nil
		case <-ticker.C:
			if err := p.poll(ctx); err != nil {
				logger.Error(err, "Poll cycle failed")
			}
		}
	}
}

func (p *GitHubPoller) poll(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("github-poller")

	if p.ghClient.IsRateLimited() {
		logger.V(1).Info("Skipping poll due to rate limiting")
		return nil
	}

	// Get the DetectiveConfig to know what repos to poll
	configs := &v1alpha1.DetectiveConfigList{}
	if err := p.List(ctx, configs); err != nil {
		return fmt.Errorf("listing DetectiveConfigs: %w", err)
	}

	for _, config := range configs.Items {
		for _, repo := range config.Spec.Repositories {
			if err := p.pollRepo(ctx, repo.Owner, repo.Name, config.Namespace); err != nil {
				logger.Error(err, "Failed to poll repo", "owner", repo.Owner, "repo", repo.Name)
			}
		}
	}
	return nil
}

func (p *GitHubPoller) pollRepo(ctx context.Context, owner, repo, namespace string) error {
	logger := log.FromContext(ctx).WithName("github-poller")

	// Fetch in-progress and queued runs
	for _, status := range []string{"in_progress", "queued"} {
		runs, err := p.ghClient.ListWorkflowRuns(ctx, owner, repo, gh.ListRunsOpts{Status: status})
		if err != nil {
			// Log and continue — don't let a transient error block completed run checks
			logger.Error(err, "Failed to list runs", "status", status)
			continue
		}

		for _, run := range runs {
			jobs, err := p.ghClient.ListJobsForRun(ctx, owner, repo, run.ID)
			if err != nil {
				logger.Error(err, "Failed to list jobs for run", "runID", run.ID)
				continue
			}

			for _, job := range jobs {
				p.checkJob(ctx, owner, repo, namespace, &run, &job)
			}
		}
	}

	// Also check completed/failed runs for recent failures
	runs, err := p.ghClient.ListWorkflowRuns(ctx, owner, repo, gh.ListRunsOpts{Status: "completed"})
	if err != nil {
		return fmt.Errorf("listing completed runs: %w", err)
	}
	for _, run := range runs {
		if run.Conclusion != "failure" {
			continue
		}
		// Only look at recent failures (last 5 minutes)
		if time.Since(run.UpdatedAt) > 5*time.Minute {
			continue
		}
		jobs, err := p.ghClient.ListJobsForRun(ctx, owner, repo, run.ID)
		if err != nil {
			continue
		}
		for _, job := range jobs {
			if job.Conclusion == "failure" {
				p.checkJob(ctx, owner, repo, namespace, &run, &job)
			}
		}
	}

	return nil
}

func (p *GitHubPoller) checkJob(ctx context.Context, owner, repo, namespace string, run *gh.WorkflowRun, job *gh.Job) {
	logger := log.FromContext(ctx).WithName("github-poller")

	// Detect stuck queued job
	if job.Status == "queued" && !job.StartedAt.IsZero() && time.Since(job.StartedAt) > p.queuedThresh {
		logger.Info("Detected stuck queued job", "jobID", job.ID, "name", job.Name)
		_ = p.createJobInvestigation(ctx, owner, repo, namespace, run, job, "job-stuck-queued")
		return
	}

	// Detect failed job
	if job.Status == "completed" && job.Conclusion == "failure" {
		_ = p.createJobInvestigation(ctx, owner, repo, namespace, run, job, "job-failed")
	}
}

func (p *GitHubPoller) createJobInvestigation(ctx context.Context, owner, repo, namespace string, run *gh.WorkflowRun, job *gh.Job, triggerType string) error {
	invName := fmt.Sprintf("gh-%d", job.ID)
	inv := &v1alpha1.Investigation{}
	key := types.NamespacedName{Namespace: namespace, Name: invName}

	if err := p.Get(ctx, key, inv); err == nil {
		return nil // already exists
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	repoFullName := fmt.Sprintf("%s/%s", owner, repo)
	steps := make([]v1alpha1.JobStep, len(job.Steps))
	for i, s := range job.Steps {
		steps[i] = v1alpha1.JobStep{Name: s.Name, Status: s.Status, Conclusion: s.Conclusion}
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

	inv = &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      invName,
			Namespace: namespace,
		},
		Spec: v1alpha1.InvestigationSpec{
			Trigger: v1alpha1.InvestigationTrigger{
				Type:   triggerType,
				Source: fmt.Sprintf("github/%s/jobs/%d", repoFullName, job.ID),
			},
			WorkflowRun: &v1alpha1.WorkflowRunInfo{
				ID:         run.ID,
				Name:       run.Name,
				Status:     run.Status,
				Conclusion: run.Conclusion,
				HTMLURL:    run.HTMLURL,
				Repository: repoFullName,
			},
			Job: &v1alpha1.JobInfo{
				ID:          job.ID,
				Name:        job.Name,
				Status:      job.Status,
				Conclusion:  job.Conclusion,
				RunnerName:  job.RunnerName,
				StartedAt:   startedAt,
				CompletedAt: completedAt,
				Steps:       steps,
			},
			Timeline: []v1alpha1.TimelineEvent{
				{
					Timestamp: metav1.Now(),
					Source:    "github",
					Type:      triggerType,
					Message:   fmt.Sprintf("Job %s (%s) in repo %s", job.Name, triggerType, repoFullName),
				},
			},
		},
	}
	if err := p.Create(ctx, inv); err != nil {
		return err
	}
	// Status subresource requires a separate update after creation
	inv.Status.Phase = "Collecting"
	return p.Status().Update(ctx, inv)
}

// Ensure GitHubPoller implements manager.Runnable
var _ manager.Runnable = &GitHubPoller{}
