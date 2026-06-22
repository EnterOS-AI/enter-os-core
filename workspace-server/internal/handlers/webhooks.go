package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
)

type WebhookHandler struct {
	workspaces *WorkspaceHandler
}

func NewWebhookHandler(broadcaster *events.Broadcaster) *WebhookHandler {
	return &WebhookHandler{
		workspaces: NewWorkspaceHandler(broadcaster, nil, "", ""),
	}
}

func NewWebhookHandlerWithWorkspace(workspaces *WorkspaceHandler) *WebhookHandler {
	return &WebhookHandler{
		workspaces: workspaces,
	}
}

// shortSHA returns the first n characters of a commit SHA, or the
// full value if it's shorter than n. Safe for empty strings.
func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// GitHub handles POST /webhooks/github/:id
// It verifies X-Hub-Signature-256, maps supported events to A2A message/send,
// then forwards through the same proxy flow used by /workspaces/:id/a2a.
func (h *WebhookHandler) GitHub(c *gin.Context) {
	workspaceID := c.Param("id")

	secret := strings.TrimSpace(os.Getenv("GITHUB_WEBHOOK_SECRET"))
	if secret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "github webhook secret is not configured"})
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, maxProxyRequestBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	signature := c.GetHeader("X-Hub-Signature-256")
	if !verifyGitHubSignature(secret, rawBody, signature) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
		return
	}

	eventType := c.GetHeader("X-GitHub-Event")

	// Event-driven cron triggers: certain GitHub events fire matching
	// schedules immediately instead of forwarding to a specific workspace.
	if triggered, triggerErr := h.handleCronTriggerEvent(c, eventType, rawBody); triggered {
		if triggerErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": triggerErr.Error()})
		}
		return
	}

	deliveryID := c.GetHeader("X-GitHub-Delivery")
	payloadWorkspaceID, a2aPayload, buildErr := buildGitHubA2APayload(eventType, deliveryID, rawBody)
	if buildErr != nil {
		if buildErr == errUnsupportedGitHubEvent || buildErr == errIgnoredGitHubAction {
			c.JSON(http.StatusAccepted, gin.H{"status": "ignored", "reason": "unsupported event type"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": buildErr.Error()})
		return
	}
	if workspaceID == "" {
		workspaceID = payloadWorkspaceID
	}
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing workspace id"})
		return
	}

	forwardBody, err := json.Marshal(a2aPayload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal a2a payload"})
		return
	}

	status, respBody, proxyErr := h.workspaces.proxyA2ARequest(
		c.Request.Context(),
		workspaceID,
		forwardBody,
		"webhook:github",
		true,
		false,
	)
	if proxyErr != nil {
		c.JSON(proxyErr.Status, proxyErr.Response)
		return
	}

	c.Data(status, "application/json", respBody)
}

var errUnsupportedGitHubEvent = fmt.Errorf("unsupported github event")
var errIgnoredGitHubAction = fmt.Errorf("ignored github action")

func verifyGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}

	gotHex := strings.TrimPrefix(header, prefix)
	got, err := hex.DecodeString(gotHex)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(got, expected)
}

type githubRepository struct {
	FullName string `json:"full_name"`
}

type githubSender struct {
	Login string `json:"login"`
}

type githubComment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

type githubIssue struct {
	Number int `json:"number"`
}

type githubPullRequest struct {
	Number int `json:"number"`
}

type githubIssueCommentEvent struct {
	WorkspaceID string           `json:"workspace_id"`
	Action      string           `json:"action"`
	Repository  githubRepository `json:"repository"`
	Sender      githubSender     `json:"sender"`
	Issue       githubIssue      `json:"issue"`
	Comment     githubComment    `json:"comment"`
}

type githubPRReviewCommentEvent struct {
	WorkspaceID string            `json:"workspace_id"`
	Action      string            `json:"action"`
	Repository  githubRepository  `json:"repository"`
	Sender      githubSender      `json:"sender"`
	PullRequest githubPullRequest `json:"pull_request"`
	Comment     githubComment     `json:"comment"`
}

// githubWorkflowRun captures the subset of GitHub's `workflow_run` event we
// route to workspaces (#101). Full schema is ~50 fields; we only need the
// handful that tell DevOps "which CI job failed, where, and how to get there."
type githubWorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`         // workflow name, e.g. "CI"
	Event      string `json:"event"`        // push / pull_request / etc.
	Status     string `json:"status"`       // queued / in_progress / completed
	Conclusion string `json:"conclusion"`   // success / failure / cancelled / timed_out
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	HTMLURL    string `json:"html_url"`
	RunNumber  int    `json:"run_number"`
}

type githubWorkflowRunEvent struct {
	WorkspaceID string            `json:"workspace_id"`
	Action      string            `json:"action"` // requested / in_progress / completed
	Repository  githubRepository  `json:"repository"`
	Sender      githubSender      `json:"sender"`
	WorkflowRun githubWorkflowRun `json:"workflow_run"`
}

func buildGitHubA2APayload(eventType, deliveryID string, rawBody []byte) (string, map[string]interface{}, error) {
	switch eventType {
	case "issue_comment":
		var payload githubIssueCommentEvent
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			return "", nil, fmt.Errorf("invalid issue_comment payload: %w", err)
		}
		if payload.Action != "created" {
			return payload.WorkspaceID, nil, errIgnoredGitHubAction
		}
		text := fmt.Sprintf(
			"GitHub issue_comment event (%s) in %s issue #%d by %s:\n%s",
			payload.Action,
			payload.Repository.FullName,
			payload.Issue.Number,
			payload.Sender.Login,
			strings.TrimSpace(payload.Comment.Body),
		)
		return payload.WorkspaceID, newGitHubMessagePayload(text, map[string]interface{}{
			"source":       "github",
			"event":        eventType,
			"action":       payload.Action,
			"delivery_id":  deliveryID,
			"repository":   payload.Repository.FullName,
			"sender":       payload.Sender.Login,
			"issue_number": payload.Issue.Number,
			"comment_url":  payload.Comment.HTMLURL,
		}), nil
	case "pull_request_review_comment":
		var payload githubPRReviewCommentEvent
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			return "", nil, fmt.Errorf("invalid pull_request_review_comment payload: %w", err)
		}
		if payload.Action != "created" {
			return payload.WorkspaceID, nil, errIgnoredGitHubAction
		}
		text := fmt.Sprintf(
			"GitHub pull_request_review_comment event (%s) in %s PR #%d by %s:\n%s",
			payload.Action,
			payload.Repository.FullName,
			payload.PullRequest.Number,
			payload.Sender.Login,
			strings.TrimSpace(payload.Comment.Body),
		)
		return payload.WorkspaceID, newGitHubMessagePayload(text, map[string]interface{}{
			"source":           "github",
			"event":            eventType,
			"action":           payload.Action,
			"delivery_id":      deliveryID,
			"repository":       payload.Repository.FullName,
			"sender":           payload.Sender.Login,
			"pull_request_num": payload.PullRequest.Number,
			"comment_url":      payload.Comment.HTMLURL,
		}), nil
	case "workflow_run":
		// #101 — CI-break notifications for DevOps Engineer. Only surface
		// *completed* runs with a non-success conclusion; queued / in_progress
		// are noise. A success completion is dropped too (explicit filter
		// rather than `errIgnoredGitHubAction` so the behaviour is visible
		// in the switch).
		var payload githubWorkflowRunEvent
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			return "", nil, fmt.Errorf("invalid workflow_run payload: %w", err)
		}
		if payload.Action != "completed" {
			return payload.WorkspaceID, nil, errIgnoredGitHubAction
		}
		if payload.WorkflowRun.Conclusion == "success" || payload.WorkflowRun.Conclusion == "skipped" || payload.WorkflowRun.Conclusion == "neutral" {
			return payload.WorkspaceID, nil, errIgnoredGitHubAction
		}
		text := fmt.Sprintf(
			"GitHub CI break — workflow '%s' run #%d %s on %s@%s\nTriggered by: %s (%s)\nRepo: %s\nRun URL: %s",
			payload.WorkflowRun.Name,
			payload.WorkflowRun.RunNumber,
			payload.WorkflowRun.Conclusion,
			payload.WorkflowRun.HeadBranch,
			shortSHA(payload.WorkflowRun.HeadSHA),
			payload.Sender.Login,
			payload.WorkflowRun.Event,
			payload.Repository.FullName,
			payload.WorkflowRun.HTMLURL,
		)
		return payload.WorkspaceID, newGitHubMessagePayload(text, map[string]interface{}{
			"source":         "github",
			"event":          eventType,
			"action":         payload.Action,
			"delivery_id":    deliveryID,
			"repository":     payload.Repository.FullName,
			"sender":         payload.Sender.Login,
			"workflow_name":  payload.WorkflowRun.Name,
			"run_id":         payload.WorkflowRun.ID,
			"run_number":     payload.WorkflowRun.RunNumber,
			"conclusion":     payload.WorkflowRun.Conclusion,
			"head_branch":    payload.WorkflowRun.HeadBranch,
			"head_sha":       payload.WorkflowRun.HeadSHA,
			"run_url":        payload.WorkflowRun.HTMLURL,
			"trigger_event":  payload.WorkflowRun.Event,
		}), nil
	default:
		return "", nil, errUnsupportedGitHubEvent
	}
}

func newGitHubMessagePayload(text string, metadata map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role": "user",
				"parts": []map[string]string{
					{"text": text},
				},
			},
			"metadata": metadata,
		},
	}
}

// ---------------------------------------------------------------------------
// Event-driven cron triggers
//
// Some GitHub events don't target a specific workspace — instead they should
// wake up all engineer work crons immediately so the team reacts to new issues
// or PR reviews without waiting for the next 30-minute timer tick.
//
// Supported events:
//   - issues (action=opened)        → fires schedules with "pick-up-work" in name
//   - pull_request_review (action=submitted) → fires schedules with "PR review"
//                                               or "security review" in name
//
// Mechanism: UPDATE next_run_at = NOW() on matching enabled schedules. The
// scheduler's 30-second poll loop picks them up on the next tick.
// ---------------------------------------------------------------------------

// githubIssuesEvent is the minimal subset of the GitHub "issues" webhook payload.
type githubIssuesEvent struct {
	Action     string           `json:"action"`
	Repository githubRepository `json:"repository"`
	Sender     githubSender     `json:"sender"`
	Issue      struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
}

// githubPullRequestReviewEvent is the minimal subset of the GitHub
// "pull_request_review" webhook payload.
type githubPullRequestReviewEvent struct {
	Action     string           `json:"action"`
	Repository githubRepository `json:"repository"`
	Sender     githubSender     `json:"sender"`
	Review     struct {
		State   string `json:"state"` // approved, changes_requested, commented
		HTMLURL string `json:"html_url"`
	} `json:"review"`
	PullRequest struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
}

// handleCronTriggerEvent checks if the GitHub event is one that should trigger
// schedules immediately. Returns (true, nil) if it handled the event and wrote
// the HTTP response, (true, err) if it handled but errored, or (false, nil) if
// the event is not a cron-trigger type and should fall through to A2A forwarding.
func (h *WebhookHandler) handleCronTriggerEvent(c *gin.Context, eventType string, rawBody []byte) (bool, error) {
	ctx := c.Request.Context()

	switch eventType {
	case "issues":
		var payload githubIssuesEvent
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid issues payload"})
			return true, nil
		}
		if payload.Action != "opened" {
			c.JSON(http.StatusAccepted, gin.H{"status": "ignored", "reason": "only issues action=opened triggers crons"})
			return true, nil
		}

		// Fire all enabled schedules whose name contains "pick-up-work" (case-insensitive).
		result, err := db.DB.ExecContext(ctx, `
			UPDATE workspace_schedules
			SET next_run_at = now(), updated_at = now()
			WHERE enabled = true
			  AND next_run_at IS NOT NULL
			  AND LOWER(name) LIKE '%pick-up-work%'
		`)
		if err != nil {
			log.Printf("Webhook: cron trigger (issues/opened) DB error: %v", err)
			return true, fmt.Errorf("failed to trigger schedules: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			log.Printf("Webhook: issues/opened RowsAffected error: %v", err)
		} else {
			log.Printf("Webhook: issues/opened in %s #%d by %s — triggered %d pick-up-work schedule(s)",
				payload.Repository.FullName, payload.Issue.Number, payload.Sender.Login, affected)
		}

		c.JSON(http.StatusOK, gin.H{
			"status":             "triggered",
			"event":              "issues",
			"action":             "opened",
			"schedules_affected": affected,
		})
		return true, nil

	case "pull_request_review":
		var payload githubPullRequestReviewEvent
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pull_request_review payload"})
			return true, nil
		}
		if payload.Action != "submitted" {
			c.JSON(http.StatusAccepted, gin.H{"status": "ignored", "reason": "only pull_request_review action=submitted triggers crons"})
			return true, nil
		}

		// Fire all enabled schedules whose name contains "PR review" or "security review" (case-insensitive).
		result, err := db.DB.ExecContext(ctx, `
			UPDATE workspace_schedules
			SET next_run_at = now(), updated_at = now()
			WHERE enabled = true
			  AND next_run_at IS NOT NULL
			  AND (LOWER(name) LIKE '%pr review%' OR LOWER(name) LIKE '%security review%')
		`)
		if err != nil {
			log.Printf("Webhook: cron trigger (pull_request_review/submitted) DB error: %v", err)
			return true, fmt.Errorf("failed to trigger schedules: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			log.Printf("Webhook: pull_request_review/submitted RowsAffected error: %v", err)
		} else {
			log.Printf("Webhook: pull_request_review/submitted in %s PR #%d by %s (state=%s) — triggered %d review schedule(s)",
				payload.Repository.FullName, payload.PullRequest.Number, payload.Sender.Login, payload.Review.State, affected)
		}

		c.JSON(http.StatusOK, gin.H{
			"status":             "triggered",
			"event":              "pull_request_review",
			"action":             "submitted",
			"schedules_affected": affected,
		})
		return true, nil

	default:
		return false, nil
	}
}
