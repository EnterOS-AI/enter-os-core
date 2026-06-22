package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests the workflow_run → DevOps A2A routing added for #101.

func TestBuildGitHubA2APayload_WorkflowRunFailure(t *testing.T) {
	raw := []byte(`{
		"workspace_id": "ws-devops",
		"action": "completed",
		"repository": {"full_name": "Molecule-AI/molecule-monorepo"},
		"sender": {"login": "test-user"},
		"workflow_run": {
			"id": 123456,
			"name": "CI",
			"event": "pull_request",
			"status": "completed",
			"conclusion": "failure",
			"head_branch": "fix/thing",
			"head_sha": "deadbeef1234567",
			"html_url": "https://github.com/Molecule-AI/molecule-monorepo/actions/runs/123456",
			"run_number": 42
		}
	}`)

	wsID, payload, err := buildGitHubA2APayload("workflow_run", "delivery-abc", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wsID != "ws-devops" {
		t.Errorf("workspace id: got %q want ws-devops", wsID)
	}

	body, _ := json.Marshal(payload)
	text := string(body)
	for _, needle := range []string{"failure", "CI", "run #42", "fix/thing", "deadbee", "Molecule-AI/molecule-monorepo"} {
		if !strings.Contains(text, needle) {
			t.Errorf("missing %q in payload: %s", needle, text)
		}
	}
}

func TestBuildGitHubA2APayload_WorkflowRunSuccessIgnored(t *testing.T) {
	raw := []byte(`{
		"workspace_id": "ws-devops",
		"action": "completed",
		"repository": {"full_name": "x/y"},
		"sender": {"login": "u"},
		"workflow_run": {"name": "CI", "status": "completed", "conclusion": "success", "head_sha": "abcdef1"}
	}`)
	_, _, err := buildGitHubA2APayload("workflow_run", "d1", raw)
	if err != errIgnoredGitHubAction {
		t.Errorf("success run should be ignored; got err=%v", err)
	}
}

func TestBuildGitHubA2APayload_WorkflowRunNonCompletedIgnored(t *testing.T) {
	raw := []byte(`{
		"workspace_id": "ws-devops",
		"action": "requested",
		"repository": {"full_name": "x/y"},
		"sender": {"login": "u"},
		"workflow_run": {"name": "CI", "status": "in_progress", "conclusion": "", "head_sha": "abc"}
	}`)
	_, _, err := buildGitHubA2APayload("workflow_run", "d2", raw)
	if err != errIgnoredGitHubAction {
		t.Errorf("non-completed action should be ignored; got err=%v", err)
	}
}

// Short-SHA truncation used to crash when head_sha was < 7 chars — the
// `min(7, len)` guard covers that edge case.
func TestBuildGitHubA2APayload_WorkflowRunShortSHA(t *testing.T) {
	raw := []byte(`{
		"workspace_id": "ws-devops",
		"action": "completed",
		"repository": {"full_name": "x/y"},
		"sender": {"login": "u"},
		"workflow_run": {"name": "CI", "status": "completed", "conclusion": "failure", "head_sha": "abc", "run_number": 1}
	}`)
	_, _, err := buildGitHubA2APayload("workflow_run", "d3", raw)
	if err != nil {
		t.Errorf("short-sha path: %v", err)
	}
}
