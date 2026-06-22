package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func githubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newWebhookTestContext(t *testing.T, workspaceID string, body []byte) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/webhooks/github/"+workspaceID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	return w, c
}

func TestGitHubWebhook_MissingSignature_Unauthorized(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	t.Setenv("GITHUB_WEBHOOK_SECRET", "test-secret")
	body := []byte(`{"workspace_id":"ws-1","action":"created"}`)
	w, c := newWebhookTestContext(t, "ws-1", body)
	c.Request.Header.Set("X-GitHub-Event", "issue_comment")

	handler.GitHub(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGitHubWebhook_BadSignature_Unauthorized(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	t.Setenv("GITHUB_WEBHOOK_SECRET", "test-secret")
	body := []byte(`{"workspace_id":"ws-1","action":"created"}`)
	w, c := newWebhookTestContext(t, "ws-1", body)
	c.Request.Header.Set("X-GitHub-Event", "issue_comment")
	c.Request.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	handler.GitHub(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGitHubWebhook_UnsupportedAction_Accepted(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	body := []byte(`{
		"workspace_id":"ws-1",
		"action":"edited",
		"repository":{"full_name":"acme/repo"},
		"comment":{"body":"ignore this"}
	}`)
	w, c := newWebhookTestContext(t, "ws-1", body)
	c.Request.Header.Set("X-GitHub-Event", "issue_comment")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	// v1 behavior: unsupported actions are acknowledged but ignored.
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGitHubWebhook_ValidIssueComment_ForwardsAndLogsActivity(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	// Mock agent endpoint receives forwarded A2A payload.
	var gotForward bool
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForward = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	workspaceID := "ws-123"
	mr.Set(fmt.Sprintf("ws:%s:url", workspaceID), agentServer.URL)

	// Proxy logging summary may resolve workspace name.
	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Webhook Workspace"))
	// Proxy logging path performs an activity INSERT asynchronously.
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	body := []byte(`{
		"workspace_id":"ws-123",
		"action":"created",
		"repository":{"full_name":"acme/repo"},
		"issue":{"number":42},
		"comment":{"body":"@agent summarize this PR and risks"}
	}`)
	w, c := newWebhookTestContext(t, workspaceID, body)
	c.Request.Header.Set("X-GitHub-Event", "issue_comment")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	// Activity logging happens in a goroutine in the shared A2A proxy path.
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("expected status 200 or 202, got %d: %s", w.Code, w.Body.String())
	}
	if !gotForward {
		t.Fatal("expected webhook to forward a task to workspace A2A endpoint")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestGitHubWebhook_ValidPRReviewComment_Forwards(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	var gotForward bool
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForward = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	workspaceID := "ws-pr-1"
	mr.Set(fmt.Sprintf("ws:%s:url", workspaceID), agentServer.URL)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("PR Workspace"))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	body := []byte(`{
		"workspace_id":"ws-pr-1",
		"action":"created",
		"repository":{"full_name":"acme/repo"},
		"pull_request":{"number":7},
		"comment":{"body":"@agent list follow-up tasks"}
	}`)

	w, c := newWebhookTestContext(t, workspaceID, body)
	c.Request.Header.Set("X-GitHub-Event", "pull_request_review_comment")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("expected status 200 or 202, got %d: %s", w.Code, w.Body.String())
	}
	if !gotForward {
		t.Fatal("expected pull_request_review_comment to forward to workspace")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Event-driven cron trigger tests
// ---------------------------------------------------------------------------

func TestGitHubWebhook_IssuesOpened_TriggersCrons(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "Molecule-AI/molecule-core"},
		"sender": {"login": "alice"},
		"issue": {"number": 42, "title": "New feature request", "html_url": "https://github.com/Molecule-AI/molecule-core/issues/42"}
	}`)

	// Expect the UPDATE that sets next_run_at = now() on pick-up-work schedules.
	mock.ExpectExec("UPDATE workspace_schedules").
		WillReturnResult(sqlmock.NewResult(0, 3))

	w, c := newWebhookTestContext(t, "", body)
	c.Request.Header.Set("X-GitHub-Event", "issues")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify response includes trigger metadata.
	respBody := w.Body.String()
	if !strings.Contains(respBody, `"triggered"`) {
		t.Fatalf("expected 'triggered' in response, got: %s", respBody)
	}
	if !strings.Contains(respBody, `"schedules_affected"`) {
		t.Fatalf("expected 'schedules_affected' in response, got: %s", respBody)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestGitHubWebhook_IssuesClosed_Ignored(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	body := []byte(`{
		"action": "closed",
		"repository": {"full_name": "Molecule-AI/molecule-core"},
		"sender": {"login": "alice"},
		"issue": {"number": 42, "title": "Old issue", "html_url": "https://github.com/Molecule-AI/molecule-core/issues/42"}
	}`)

	w, c := newWebhookTestContext(t, "", body)
	c.Request.Header.Set("X-GitHub-Event", "issues")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGitHubWebhook_PRReviewSubmitted_TriggersCrons(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	body := []byte(`{
		"action": "submitted",
		"repository": {"full_name": "Molecule-AI/molecule-core"},
		"sender": {"login": "bob"},
		"review": {"state": "changes_requested", "html_url": "https://github.com/Molecule-AI/molecule-core/pull/7#pullrequestreview-1"},
		"pull_request": {"number": 7, "title": "Fix scheduler bug", "html_url": "https://github.com/Molecule-AI/molecule-core/pull/7"}
	}`)

	// Expect the UPDATE that sets next_run_at = now() on review schedules.
	mock.ExpectExec("UPDATE workspace_schedules").
		WillReturnResult(sqlmock.NewResult(0, 2))

	w, c := newWebhookTestContext(t, "", body)
	c.Request.Header.Set("X-GitHub-Event", "pull_request_review")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	respBody := w.Body.String()
	if !strings.Contains(respBody, `"triggered"`) {
		t.Fatalf("expected 'triggered' in response, got: %s", respBody)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestGitHubWebhook_PRReviewDismissed_Ignored(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWebhookHandler(broadcaster)

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	body := []byte(`{
		"action": "dismissed",
		"repository": {"full_name": "Molecule-AI/molecule-core"},
		"sender": {"login": "bob"},
		"review": {"state": "dismissed", "html_url": "https://github.com/Molecule-AI/molecule-core/pull/7#pullrequestreview-1"},
		"pull_request": {"number": 7, "title": "Fix scheduler bug", "html_url": "https://github.com/Molecule-AI/molecule-core/pull/7"}
	}`)

	w, c := newWebhookTestContext(t, "", body)
	c.Request.Header.Set("X-GitHub-Event", "pull_request_review")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d: %s", w.Code, w.Body.String())
	}
}
