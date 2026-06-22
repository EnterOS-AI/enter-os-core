package handlers

// UserTaskStore is the SSOT for the "user tasks" primitive — the structured
// asks an agent raises for the human user (e.g. "Review the draft", "Provide
// the API key"). Every surface that mutates or reads user_tasks — the REST
// handlers in user_tasks.go AND the MCP tools in mcp_tools.go — MUST route
// through this store rather than re-implement the SQL + status-enum
// validation + USER_TASK_* broadcast inline.
//
// Why: pre-consolidation the REST handler and the MCP bridge each hand-wrote
// the SAME INSERT / COALESCE-UPDATE / DELETE SQL, the SAME pending/done/
// dismissed enum check, and the SAME EventUserTaskRequested broadcast. Two
// copies of one contract drift silently (the AgentMessageWriter consolidation
// in agent_message_writer.go exists for exactly this reason — the acme-demo
// data-loss incident was the symptom of one half lagging the other). This
// store gives both call sites a single well-tested implementation.
//
// The store owns persistence + validation + the event broadcast. HTTP-specific
// concerns (gin binding, status codes) and MCP-specific concerns (arg parsing,
// string replies) stay in their respective handlers.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
)

// ErrUserTaskNotFound is returned by Update/Delete/Resolve when no row matches
// the (id, workspace_id) scope — the task does not exist, is owned by another
// workspace, or (for Resolve) is already resolved. Callers translate to HTTP
// 404 / a JSON-RPC error.
var ErrUserTaskNotFound = errors.New("user_task: not found")

// ErrInvalidUserTaskStatus is returned when a caller supplies a status outside
// the pending/done/dismissed enum. Callers translate to HTTP 400.
var ErrInvalidUserTaskStatus = errors.New("user_task: status must be 'pending', 'done' or 'dismissed'")

// UserTaskRow is one row of a workspace's own user-task list (List).
type UserTaskRow struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Detail     *string `json:"detail"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	ResolvedAt *string `json:"resolved_at"`
	ResolvedBy *string `json:"resolved_by"`
}

// UserTaskStore persists + broadcasts user-task mutations. Construct per call
// site via NewUserTaskStore (mirroring AgentMessageWriter's usage in
// activity.go / mcp_tools.go) so the REST handlers — which read the global
// db.DB that tests swap under them — and the MCP bridge share one code path.
//
// Takes events.EventEmitter (not the concrete *Broadcaster) so tests can
// substitute a fake emitter.
type UserTaskStore struct {
	db          *sql.DB
	broadcaster events.EventEmitter
}

// NewUserTaskStore binds the store to a DB pool + the platform broadcaster.
func NewUserTaskStore(db *sql.DB, broadcaster events.EventEmitter) *UserTaskStore {
	return &UserTaskStore{db: db, broadcaster: broadcaster}
}

// Create inserts a new pending user task and broadcasts USER_TASK_REQUESTED.
// detail is optional — pass "" to leave it NULL. Returns the new task id.
func (s *UserTaskStore) Create(ctx context.Context, workspaceID, title, detail string) (string, error) {
	var detailArg interface{}
	if detail != "" {
		detailArg = detail
	}

	var taskID string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO user_tasks (workspace_id, title, detail)
		VALUES ($1, $2, $3)
		RETURNING id
	`, workspaceID, title, detailArg).Scan(&taskID)
	if err != nil {
		return "", fmt.Errorf("user_task: create: %w", err)
	}

	if err := s.broadcaster.RecordAndBroadcast(ctx, string(events.EventUserTaskRequested), workspaceID, map[string]interface{}{
		"user_task_id": taskID,
		"title":        title,
	}); err != nil {
		log.Printf("user_task: failed to broadcast requested for %s: %v", workspaceID, err)
	}

	return taskID, nil
}

// List returns the asks a workspace itself raised, any status, newest first.
func (s *UserTaskStore) List(ctx context.Context, workspaceID string) ([]UserTaskRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, detail, status, created_at, resolved_at, resolved_by
		FROM user_tasks WHERE workspace_id = $1
		ORDER BY created_at DESC LIMIT 50
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("user_task: list: %w", err)
	}
	defer rows.Close()

	tasks := make([]UserTaskRow, 0)
	for rows.Next() {
		var t UserTaskRow
		if rows.Scan(&t.ID, &t.Title, &t.Detail, &t.Status, &t.CreatedAt, &t.ResolvedAt, &t.ResolvedBy) != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		log.Printf("user_task: list rows.Err workspace=%s: %v", workspaceID, err)
	}
	return tasks, nil
}

// Update applies a partial edit (title / detail / status — nil leaves a column
// untouched via COALESCE), scoped by workspace_id so an agent only touches its
// own tasks. Returns ErrInvalidUserTaskStatus on a bad status and
// ErrUserTaskNotFound when no row matches.
func (s *UserTaskStore) Update(ctx context.Context, workspaceID, taskID string, title, detail, status *string) error {
	if err := validateUserTaskStatusPtr(status); err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE user_tasks SET
			title  = COALESCE($1, title),
			detail = COALESCE($2, detail),
			status = COALESCE($3, status)
		WHERE id = $4 AND workspace_id = $5
	`, title, detail, status, taskID, workspaceID)
	if err != nil {
		return fmt.Errorf("user_task: update: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("user_task: update RowsAffected: %w", err)
	}
	if n == 0 {
		return ErrUserTaskNotFound
	}
	return nil
}

// Delete removes a workspace's own task, scoped by workspace_id. Returns
// ErrUserTaskNotFound when no row matches.
func (s *UserTaskStore) Delete(ctx context.Context, workspaceID, taskID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM user_tasks WHERE id = $1 AND workspace_id = $2
	`, taskID, workspaceID)
	if err != nil {
		return fmt.Errorf("user_task: delete: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("user_task: delete RowsAffected: %w", err)
	}
	if n == 0 {
		return ErrUserTaskNotFound
	}
	return nil
}

// Resolve marks a pending task done or dismissed (the user worklist action)
// and broadcasts USER_TASK_RESOLVED. status MUST be "done" or "dismissed"
// (the resolve enum is narrower than Update's). resolvedBy defaults to "human"
// when empty. Returns ErrInvalidUserTaskStatus on a bad status and
// ErrUserTaskNotFound when the task is missing or already resolved.
func (s *UserTaskStore) Resolve(ctx context.Context, workspaceID, taskID, status, resolvedBy string) (string, error) {
	if status != "done" && status != "dismissed" {
		return "", ErrInvalidUserTaskStatus
	}
	if resolvedBy == "" {
		resolvedBy = "human"
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE user_tasks
		SET status = $1, resolved_at = now(), resolved_by = $2
		WHERE id = $3 AND workspace_id = $4 AND status = 'pending'
	`, status, resolvedBy, taskID, workspaceID)
	if err != nil {
		return "", fmt.Errorf("user_task: resolve: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("user_task: resolve RowsAffected: %w", err)
	}
	if n == 0 {
		return "", ErrUserTaskNotFound
	}

	if err := s.broadcaster.RecordAndBroadcast(ctx, string(events.EventUserTaskResolved), workspaceID, map[string]interface{}{
		"user_task_id": taskID,
		"status":       status,
		"resolved_by":  resolvedBy,
	}); err != nil {
		log.Printf("user_task: failed to broadcast resolved for %s: %v", workspaceID, err)
	}

	return resolvedBy, nil
}

// validateUserTaskStatusPtr enforces the pending/done/dismissed enum for a
// nil-able status (Update semantics: nil = "don't change", so skip the check).
func validateUserTaskStatusPtr(status *string) error {
	if status == nil {
		return nil
	}
	switch *status {
	case "pending", "done", "dismissed":
		return nil
	default:
		return ErrInvalidUserTaskStatus
	}
}
