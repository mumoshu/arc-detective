//go:build e2e_full
// +build e2e_full

package e2e_full

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const githubAPIBase = "https://api.github.com"

// checkAnchorFile verifies that the test repo contains .arc-detective-test as a safety check.
func checkAnchorFile(owner, repo, token string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/.arc-detective-test", githubAPIBase, owner, repo)
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	setGitHubHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("checking anchor file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("test repo %s/%s does not contain .arc-detective-test anchor file; "+
			"create this file in the repo before running the full e2e test", owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d checking anchor file", resp.StatusCode)
	}
	return nil
}

// getDefaultBranch returns the default branch name for the repo.
func getDefaultBranch(owner, repo, token string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", githubAPIBase, owner, repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	setGitHubHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GET repo returned %d: %s", resp.StatusCode, body)
	}

	var result struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.DefaultBranch, nil
}

// pushWorkflowFile creates or updates a file in the repo via the Contents API.
// Returns the file's blob SHA (needed for deletion).
func pushWorkflowFile(owner, repo, token, path, content, branch string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, owner, repo, path)

	// Check if file already exists (to get SHA for update)
	var existingSHA string
	req, _ := http.NewRequest(http.MethodGet, url+"?ref="+branch, nil)
	setGitHubHeaders(req, token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var existing struct {
				SHA string `json:"sha"`
			}
			json.NewDecoder(resp.Body).Decode(&existing)
			existingSHA = existing.SHA
		}
	}

	body := map[string]string{
		"message": "Add arc-detective e2e test workflow",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if existingSHA != "" {
		body["sha"] = existingSHA
	}

	jsonBody, _ := json.Marshal(body)
	req, err = http.NewRequest(http.MethodPut, url, bytes.NewReader(jsonBody))
	if err != nil {
		return "", err
	}
	setGitHubHeaders(req, token)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pushing workflow file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("PUT contents returned %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Content struct {
			SHA string `json:"sha"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Content.SHA, nil
}

// deleteWorkflowFile removes a file from the repo via the Contents API.
func deleteWorkflowFile(owner, repo, token, path, branch string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, owner, repo, path)

	// Get current SHA
	req, _ := http.NewRequest(http.MethodGet, url+"?ref="+branch, nil)
	setGitHubHeaders(req, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}

	var existing struct {
		SHA string `json:"sha"`
	}
	json.NewDecoder(resp.Body).Decode(&existing)

	body, _ := json.Marshal(map[string]string{
		"message": "Remove arc-detective e2e test workflow",
		"sha":     existing.SHA,
		"branch":  branch,
	})
	req, err = http.NewRequest(http.MethodDelete, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	setGitHubHeaders(req, token)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DELETE contents returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// triggerWorkflowDispatch triggers a workflow_dispatch event.
func triggerWorkflowDispatch(owner, repo, token, workflowFile, ref string, inputs map[string]string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/dispatches", githubAPIBase, owner, repo, workflowFile)

	body, _ := json.Marshal(map[string]interface{}{
		"ref":    ref,
		"inputs": inputs,
	})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	setGitHubHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("triggering workflow dispatch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST dispatches returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// waitForWorkflowRun polls until a workflow run appears for the given workflow file,
// returning the run ID. It looks for runs created after startedAfter.
func waitForWorkflowRun(owner, repo, token, workflowFile string, startedAfter time.Time, timeout time.Duration) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/runs?per_page=5", githubAPIBase, owner, repo, workflowFile)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		setGitHubHeaders(req, token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}

		var result struct {
			WorkflowRuns []struct {
				ID        int64     `json:"id"`
				CreatedAt time.Time `json:"created_at"`
				Status    string    `json:"status"`
			} `json:"workflow_runs"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		for _, run := range result.WorkflowRuns {
			if run.CreatedAt.After(startedAfter) {
				return run.ID, nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return 0, fmt.Errorf("timed out waiting for workflow run to appear")
}

// cancelWorkflowRun cancels a workflow run. Best-effort, ignores errors.
func cancelWorkflowRun(owner, repo, token string, runID int64) error {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/cancel", githubAPIBase, owner, repo, runID)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	setGitHubHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
