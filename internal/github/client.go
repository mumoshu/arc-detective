package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is the interface for GitHub Actions API operations.
type Client interface {
	ListWorkflowRuns(ctx context.Context, owner, repo string, opts ListRunsOpts) ([]WorkflowRun, error)
	ListJobsForRun(ctx context.Context, owner, repo string, runID int64) ([]Job, error)
	GetJob(ctx context.Context, owner, repo string, jobID int64) (*Job, error)
	IsRateLimited() bool
}

type client struct {
	httpClient *http.Client
	baseURL    string
	token      string
	rateLimit  *RateLimitTracker
}

type Option func(*client)

func WithBaseURL(baseURL string) Option {
	return func(c *client) { c.baseURL = baseURL }
}

func WithPAT(token string) Option {
	return func(c *client) { c.token = token }
}

func WithHTTPClient(hc *http.Client) Option {
	return func(c *client) { c.httpClient = hc }
}

func NewClient(opts ...Option) Client {
	c := &client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://api.github.com",
		rateLimit:  NewRateLimitTracker(10),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *client) IsRateLimited() bool {
	return c.rateLimit.ShouldBackoff()
}

func (c *client) ListWorkflowRuns(ctx context.Context, owner, repo string, opts ListRunsOpts) ([]WorkflowRun, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs", c.baseURL, owner, repo)
	if opts.Status != "" {
		u += "?" + url.Values{"status": {opts.Status}}.Encode()
	}

	var result listRunsResponse
	if err := c.doGet(ctx, u, &result); err != nil {
		return nil, err
	}
	return result.WorkflowRuns, nil
}

func (c *client) ListJobsForRun(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs", c.baseURL, owner, repo, runID)

	var result listJobsResponse
	if err := c.doGet(ctx, u, &result); err != nil {
		return nil, err
	}
	return result.Jobs, nil
}

func (c *client) GetJob(ctx context.Context, owner, repo string, jobID int64) (*Job, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d", c.baseURL, owner, repo, jobID)

	var result Job
	if err := c.doGet(ctx, u, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *client) doGet(ctx context.Context, rawURL string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	c.rateLimit.Update(resp)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}
