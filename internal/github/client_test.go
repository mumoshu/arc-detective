package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	oauth2githubapp "github.com/int128/oauth2-github-app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListWorkflowRuns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs", r.URL.Path)
		assert.Equal(t, "in_progress", r.URL.Query().Get("status"))
		assert.Equal(t, "Bearer fake-token", r.Header.Get("Authorization"))
		w.Header().Set("X-RateLimit-Remaining", "100")
		_ = json.NewEncoder(w).Encode(listRunsResponse{
			TotalCount:   1,
			WorkflowRuns: []WorkflowRun{{ID: 123, Name: "CI", Status: "in_progress"}},
		})
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithPAT("fake-token"))
	runs, err := c.ListWorkflowRuns(context.Background(), "myorg", "myrepo", ListRunsOpts{Status: "in_progress"})
	require.NoError(t, err)
	assert.Len(t, runs, 1)
	assert.Equal(t, int64(123), runs[0].ID)
	assert.Equal(t, "CI", runs[0].Name)
}

func TestListJobsForRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/123/jobs", r.URL.Path)
		w.Header().Set("X-RateLimit-Remaining", "50")
		_ = json.NewEncoder(w).Encode(listJobsResponse{
			TotalCount: 1,
			Jobs:       []Job{{ID: 456, Name: "build", Status: "completed", Conclusion: "failure"}},
		})
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithPAT("fake-token"))
	jobs, err := c.ListJobsForRun(context.Background(), "myorg", "myrepo", 123)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
	assert.Equal(t, int64(456), jobs[0].ID)
	assert.Equal(t, "failure", jobs[0].Conclusion)
}

func TestGetJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/myorg/myrepo/actions/jobs/456", r.URL.Path)
		w.Header().Set("X-RateLimit-Remaining", "90")
		_ = json.NewEncoder(w).Encode(Job{
			ID: 456, Name: "build", Status: "completed", RunnerName: "runner-abc",
		})
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithPAT("fake-token"))
	job, err := c.GetJob(context.Background(), "myorg", "myrepo", 456)
	require.NoError(t, err)
	assert.Equal(t, int64(456), job.ID)
	assert.Equal(t, "runner-abc", job.RunnerName)
}

func TestRateLimitTracking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "5")
		_ = json.NewEncoder(w).Encode(listRunsResponse{})
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithPAT("fake-token"))
	_, err := c.ListWorkflowRuns(context.Background(), "myorg", "myrepo", ListRunsOpts{})
	require.NoError(t, err)
	assert.True(t, c.IsRateLimited())
}

func TestRateLimitNotTriggeredWithHighRemaining(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		_ = json.NewEncoder(w).Encode(listRunsResponse{})
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithPAT("fake-token"))
	_, _ = c.ListWorkflowRuns(context.Background(), "myorg", "myrepo", ListRunsOpts{})
	assert.False(t, c.IsRateLimited())
}

func TestAPIErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"rate limit exceeded"}`))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithPAT("fake-token"))
	_, err := c.ListWorkflowRuns(context.Background(), "myorg", "myrepo", ListRunsOpts{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestWithGitHubApp(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	parsedKey, err := oauth2githubapp.ParsePrivateKey(pemBytes)
	require.NoError(t, err)

	var mintedToken bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/12345/access_tokens":
			mintedToken = true
			assert.Contains(t, r.Header.Get("Authorization"), "Bearer ")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "installation-token",
				"expires_at": time.Now().Add(time.Hour),
			})
		case r.URL.Path == "/repos/myorg/myrepo/actions/runs":
			assert.Equal(t, "token installation-token", r.Header.Get("Authorization"))
			w.Header().Set("X-RateLimit-Remaining", "100")
			_ = json.NewEncoder(w).Encode(listRunsResponse{})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithGitHubApp(context.Background(), oauth2githubapp.Config{
		PrivateKey:     parsedKey,
		AppID:          "1",
		InstallationID: "12345",
		BaseURL:        srv.URL,
	}))

	_, err = c.ListWorkflowRuns(context.Background(), "myorg", "myrepo", ListRunsOpts{})
	require.NoError(t, err)
	assert.True(t, mintedToken)
}

func TestListWorkflowRunsWithoutStatusFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs", r.URL.Path)
		assert.Empty(t, r.URL.Query().Get("status"))
		w.Header().Set("X-RateLimit-Remaining", "100")
		_ = json.NewEncoder(w).Encode(listRunsResponse{})
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithPAT("fake-token"))
	runs, err := c.ListWorkflowRuns(context.Background(), "myorg", "myrepo", ListRunsOpts{})
	require.NoError(t, err)
	assert.Empty(t, runs)
}
