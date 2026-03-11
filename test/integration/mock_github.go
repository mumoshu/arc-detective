package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	gh "github.com/mumoshu/arc-detective/internal/github"
)

// MockGitHubServer is a configurable fake GitHub Actions API server.
type MockGitHubServer struct {
	*httptest.Server
	mu   sync.Mutex
	runs map[string][]gh.WorkflowRun // key: "owner/repo"
	jobs map[int64][]gh.Job          // key: run ID
}

func NewMockGitHubServer() *MockGitHubServer {
	m := &MockGitHubServer{
		runs: make(map[string][]gh.WorkflowRun),
		jobs: make(map[int64][]gh.Job),
	}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handler))
	return m
}

func (m *MockGitHubServer) SetWorkflowRuns(owner, repo string, runs []gh.WorkflowRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[owner+"/"+repo] = runs
}

func (m *MockGitHubServer) SetJobsForRun(runID int64, jobs []gh.Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[runID] = jobs
}

func (m *MockGitHubServer) handler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	w.Header().Set("X-RateLimit-Remaining", "1000")
	w.Header().Set("Content-Type", "application/json")

	path := r.URL.Path

	// GET /repos/{owner}/{repo}/actions/runs/{id}/jobs
	if strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs") {
		parts := strings.Split(path, "/")
		// /repos/owner/repo/actions/runs/{id}/jobs
		for i, p := range parts {
			if p == "runs" && i+1 < len(parts) {
				runID, err := strconv.ParseInt(parts[i+1], 10, 64)
				if err == nil {
					jobs := m.jobs[runID]
					_ = json.NewEncoder(w).Encode(map[string]any{
						"total_count": len(jobs),
						"jobs":        jobs,
					})
					return
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// GET /repos/{owner}/{repo}/actions/runs
	if strings.Contains(path, "/actions/runs") {
		parts := strings.Split(strings.TrimPrefix(path, "/repos/"), "/actions/runs")
		if len(parts) >= 1 {
			repoKey := parts[0] // "owner/repo"
			runs := m.runs[repoKey]
			// Filter by status if provided
			if status := r.URL.Query().Get("status"); status != "" {
				var filtered []gh.WorkflowRun
				for _, run := range runs {
					if run.Status == status {
						filtered = append(filtered, run)
					}
				}
				runs = filtered
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count":   len(runs),
				"workflow_runs": runs,
			})
			return
		}
	}

	// GET /repos/{owner}/{repo}/actions/jobs/{id}
	if strings.Contains(path, "/actions/jobs/") {
		parts := strings.Split(path, "/")
		for i, p := range parts {
			if p == "jobs" && i+1 < len(parts) {
				jobID, err := strconv.ParseInt(parts[i+1], 10, 64)
				if err == nil {
					for _, jobs := range m.jobs {
						for _, job := range jobs {
							if job.ID == jobID {
								_ = json.NewEncoder(w).Encode(job)
								return
							}
						}
					}
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}
