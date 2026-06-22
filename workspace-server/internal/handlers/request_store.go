package handlers

// RequestStore is the SSOT for the unified "requests" primitive — the Tasks +
// Approvals inbox (RFC P1, docs/design/rfc-unified-requests-inbox.md). It
// generalizes UserTaskStore (agent→user worklist asks) and the inline
// approval_requests SQL in approvals.go into ONE store keyed by kind ∈
// {task, approval}, where requester and recipient may each be a user OR an
// agent.
//
// Every surface that mutates or reads `requests` — the REST handlers in
// requests.go AND (in a later phase) the MCP request tools — MUST route through
// this store rather than re-implement the SQL + status-enum validation +
// REQUEST_* broadcast inline. Two copies of one contract drift silently; this
// is the same consolidation rationale as UserTaskStore / AgentMessageWriter.
//
// The store owns persistence + validation + the event broadcast. HTTP-specific
// concerns (gin binding, status codes) stay in the handler. Construct per call
// via NewRequestStore over the live global db.DB so the test harness's db.DB
// swap is observed — mirroring UserTaskStore.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/google/uuid"
)

// ErrRequestNotFound is returned by Get/Respond/RequestInfo/Cancel/AddMessage
// when no row matches — the request does not exist, or (for the mutating
// actions) is already terminal. Callers translate to HTTP 404.
var ErrRequestNotFound = errors.New("request: not found")

// ErrInvalidRequestAction is returned when a respond action is outside the
// done/rejected/approved set, or is incompatible with the request's kind
// (approval→approved/rejected, task→done/rejected). Callers translate to 400.
var ErrInvalidRequestAction = errors.New("request: invalid action for this request kind")

// ErrInvalidRequestKind is returned when Create is given a kind outside
// task/approval. Callers translate to HTTP 400.
var ErrInvalidRequestKind = errors.New("request: kind must be 'task' or 'approval'")

// ErrInvalidRequestParty is returned when a requester_type / recipient_type /
// author_type is outside the user/agent enum. Callers translate to HTTP 400.
var ErrInvalidRequestParty = errors.New("request: type must be 'user' or 'agent'")

// ErrSelfResponse is returned by Respond when the responder is the same party
// as the requester (self-approval / self-rejection). Callers translate to HTTP 400.
var ErrSelfResponse = errors.New("request: responder cannot be the requester")

// CreateRequestInput is the set of fields Create needs. requester_* identifies
// who raised it (for a per-workspace POST that is the calling agent); recipient_*
// is who must act. Detail/OrgID/Priority are optional (zero values → NULL).
type CreateRequestInput struct {
	Kind          string
	RequesterType string
	RequesterID   string
	OrgID         string
	RecipientType string
	RecipientID   string
	Title         string
	Detail        string
	Priority      *int
}

// RequestRow is one request as returned by the list + get methods.
// WorkspaceName is non-empty only for ListPendingForOrg rows whose party is an
// agent (decorated via a LEFT JOIN on workspaces); the inbox/outgoing lists
// leave it empty.
type RequestRow struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	RequesterType string  `json:"requester_type"`
	RequesterID   string  `json:"requester_id"`
	OrgID         *string `json:"org_id"`
	RecipientType string  `json:"recipient_type"`
	RecipientID   string  `json:"recipient_id"`
	Title         string  `json:"title"`
	Detail        *string `json:"detail"`
	Status        string  `json:"status"`
	ResponderType *string `json:"responder_type"`
	ResponderID   *string `json:"responder_id"`
	Priority      *int    `json:"priority"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	RespondedAt   *string `json:"responded_at"`
	WorkspaceName *string `json:"workspace_name,omitempty"`
}

// RequestMessageRow is one row of a request's More-Info thread.
type RequestMessageRow struct {
	ID         string `json:"id"`
	RequestID  string `json:"request_id"`
	AuthorType string `json:"author_type"`
	AuthorID   string `json:"author_id"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
}

// RequestStore persists + broadcasts request mutations. Takes
// events.EventEmitter (not the concrete *Broadcaster) so tests can substitute
// a fake emitter, mirroring UserTaskStore.
type RequestStore struct {
	db          *sql.DB
	broadcaster events.EventEmitter
}

// NewRequestStore binds the store to a DB pool + the platform broadcaster.
func NewRequestStore(db *sql.DB, broadcaster events.EventEmitter) *RequestStore {
	return &RequestStore{db: db, broadcaster: broadcaster}
}

// requestColumns is the canonical SELECT projection for a single request row,
// shared by Get / ListInbox / ListOutgoing so the Scan order can't drift.
const requestColumns = `id, kind, requester_type, requester_id, org_id,
	recipient_type, recipient_id, title, detail, status,
	responder_type, responder_id, priority, created_at, updated_at, responded_at`

// scanRequest reads one row in requestColumns order into a RequestRow.
func scanRequest(rows *sql.Rows) (RequestRow, error) {
	var r RequestRow
	err := rows.Scan(
		&r.ID, &r.Kind, &r.RequesterType, &r.RequesterID, &r.OrgID,
		&r.RecipientType, &r.RecipientID, &r.Title, &r.Detail, &r.Status,
		&r.ResponderType, &r.ResponderID, &r.Priority, &r.CreatedAt, &r.UpdatedAt, &r.RespondedAt,
	)
	return r, err
}

func validParty(t string) bool { return t == "user" || t == "agent" }

// broadcastTarget picks the workspace id to anchor a REQUEST_* event on. Events
// are workspace-scoped (structure_events.workspace_id), so we anchor on the
// agent party when there is one — the requester (so its canvas/inbox is
// signalled on a response) or, lacking that, the recipient. A user-only request
// has no workspace anchor; we skip the broadcast rather than insert a bad row.
func broadcastTarget(requesterType, requesterID, recipientType, recipientID string) string {
	if requesterType == "agent" && requesterID != "" {
		return requesterID
	}
	if recipientType == "agent" && recipientID != "" {
		return recipientID
	}
	return ""
}

// requestNotifyEnqueue is the a2a-queue enqueue used to deliver
// request-outcome notifications to a REQUESTER agent as a real inbound turn.
// Package-level var (default: the real EnqueueA2A) so tests can intercept —
// mirrors RequestNudgeSweeper's enqueueFunc injection.
var requestNotifyEnqueue enqueueFunc = EnqueueA2A

// notifyRequesterAgent enqueues a message/send A2A turn to the requester
// agent. Used on terminal responses and recipient-authored More-Info
// messages (CTO 2026-06-11): a human clicking Done/Reject/Approve — or
// asking for clarification — must reach the agent that raised the request
// as an actual turn, not only as a structure event the agent never reads.
// Best-effort: an enqueue failure is logged, never surfaced — the durable
// truth is the requests row, and check_requests remains the pull path.
func (s *RequestStore) notifyRequesterAgent(ctx context.Context, req RequestRow, idemKey, text string) {
	body, err := json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": idemKey + "-" + uuid.New().String(),
				"parts":     []map[string]interface{}{{"kind": "text", "text": text}},
			},
		},
	})
	if err != nil {
		log.Printf("request: build requester notification for %s failed: %v", req.ID, err)
		return
	}
	if _, _, err := requestNotifyEnqueue(ctx, req.RequesterID, "", PriorityInfo, body, "message/send", idemKey, nil); err != nil {
		log.Printf("request: enqueue requester notification for %s -> %s failed: %v", req.ID, req.RequesterID, err)
	}
}

// Create inserts a new pending request and broadcasts REQUEST_CREATED (anchored
// on the recipient agent if any, so an agent recipient's inbox is signalled).
// Returns the new request id. Validates kind + party enums up front.
func (s *RequestStore) Create(ctx context.Context, in CreateRequestInput) (string, error) {
	if in.Kind != "task" && in.Kind != "approval" {
		return "", ErrInvalidRequestKind
	}
	if !validParty(in.RequesterType) || !validParty(in.RecipientType) {
		return "", ErrInvalidRequestParty
	}

	var detailArg interface{}
	if in.Detail != "" {
		detailArg = in.Detail
	}
	var orgArg interface{}
	if in.OrgID != "" {
		orgArg = in.OrgID
	}
	var priorityArg interface{}
	if in.Priority != nil {
		priorityArg = *in.Priority
	}

	var requestID string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO requests (
			kind, requester_type, requester_id, org_id,
			recipient_type, recipient_id, title, detail, priority
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`, in.Kind, in.RequesterType, in.RequesterID, orgArg,
		in.RecipientType, in.RecipientID, in.Title, detailArg, priorityArg).Scan(&requestID)
	if err != nil {
		return "", fmt.Errorf("request: create: %w", err)
	}

	// Anchor REQUEST_CREATED on the recipient agent (so its inbox is poked) if
	// the recipient is an agent; else on the requester. A user→user request has
	// no agent to signal — skip the broadcast.
	target := broadcastTarget(in.RecipientType, in.RecipientID, in.RequesterType, in.RequesterID)
	if target != "" {
		if err := s.broadcaster.RecordAndBroadcast(ctx, string(events.EventRequestCreated), target, map[string]interface{}{
			"request_id":     requestID,
			"kind":           in.Kind,
			"recipient_type": in.RecipientType,
			"recipient_id":   in.RecipientID,
			"title":          in.Title,
		}); err != nil {
			log.Printf("request: failed to broadcast created for %s: %v", target, err)
		}
	}

	return requestID, nil
}

// Get returns a single request by id, or ErrRequestNotFound.
func (s *RequestStore) Get(ctx context.Context, id string) (RequestRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE id = $1`, id)
	if err != nil {
		return RequestRow{}, fmt.Errorf("request: get: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return RequestRow{}, fmt.Errorf("request: get: %w", err)
		}
		return RequestRow{}, ErrRequestNotFound
	}
	r, err := scanRequest(rows)
	if err != nil {
		return RequestRow{}, fmt.Errorf("request: get scan: %w", err)
	}
	return r, nil
}

// Messages returns a request's More-Info thread, oldest first.
func (s *RequestStore) Messages(ctx context.Context, requestID string) ([]RequestMessageRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, request_id, author_type, author_id, body, created_at
		FROM request_messages WHERE request_id = $1
		ORDER BY created_at ASC LIMIT 200
	`, requestID)
	if err != nil {
		return nil, fmt.Errorf("request: messages: %w", err)
	}
	defer rows.Close()

	msgs := make([]RequestMessageRow, 0)
	for rows.Next() {
		var m RequestMessageRow
		if rows.Scan(&m.ID, &m.RequestID, &m.AuthorType, &m.AuthorID, &m.Body, &m.CreatedAt) != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		log.Printf("request: messages rows.Err request=%s: %v", requestID, err)
	}
	return msgs, nil
}

// ListInbox returns the requests addressed TO a recipient (recipient_type +
// recipient_id), newest first, optionally filtered by status. status "" = all.
func (s *RequestStore) ListInbox(ctx context.Context, recipientType, recipientID, status string) ([]RequestRow, error) {
	q := `SELECT ` + requestColumns + ` FROM requests
		WHERE recipient_type = $1 AND recipient_id = $2`
	args := []interface{}{recipientType, recipientID}
	if status != "" {
		q += ` AND status = $3`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 50`
	return s.queryRequests(ctx, "list inbox", q, args)
}

// ListOutgoing returns the requests a requester RAISED (requester_type +
// requester_id), newest first — the async pickup of responses.
func (s *RequestStore) ListOutgoing(ctx context.Context, requesterType, requesterID, status string) ([]RequestRow, error) {
	q := `SELECT ` + requestColumns + ` FROM requests
		WHERE requester_type = $1 AND requester_id = $2`
	args := []interface{}{requesterType, requesterID}
	if status != "" {
		q += ` AND status = $3`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 50`
	return s.queryRequests(ctx, "list outgoing", q, args)
}

// ListPendingForOrg powers the cross-org "all incoming" canvas view. Returns
// pending + info_requested requests in an org, newest first, decorated with the
// requesting/responding agent's workspace name via a LEFT JOIN (NULL for a user
// party). kind "" = both tabs; "task"/"approval" filters one.
func (s *RequestStore) ListPendingForOrg(ctx context.Context, orgID, kind string) ([]RequestRow, error) {
	// LEFT JOIN workspaces on the requester id when it is an agent, to surface a
	// human-readable name in the org view. recipient name is left to the caller.
	q := `SELECT ` +
		`r.id, r.kind, r.requester_type, r.requester_id, r.org_id,
		r.recipient_type, r.recipient_id, r.title, r.detail, r.status,
		r.responder_type, r.responder_id, r.priority, r.created_at, r.updated_at, r.responded_at,
		w.name
		FROM requests r
		LEFT JOIN workspaces w
			ON r.requester_type = 'agent' AND w.id = r.requester_id::uuid
		WHERE r.status IN ('pending', 'info_requested')`
	args := []interface{}{}
	if orgID != "" {
		args = append(args, orgID)
		q += fmt.Sprintf(" AND r.org_id = $%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND r.kind = $%d", len(args))
	}
	q += ` ORDER BY r.created_at DESC LIMIT 50`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("request: list pending org: %w", err)
	}
	defer rows.Close()

	out := make([]RequestRow, 0)
	for rows.Next() {
		var r RequestRow
		if rows.Scan(
			&r.ID, &r.Kind, &r.RequesterType, &r.RequesterID, &r.OrgID,
			&r.RecipientType, &r.RecipientID, &r.Title, &r.Detail, &r.Status,
			&r.ResponderType, &r.ResponderID, &r.Priority, &r.CreatedAt, &r.UpdatedAt, &r.RespondedAt,
			&r.WorkspaceName,
		) != nil {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("request: list pending org rows.Err org=%s: %v", orgID, err)
	}
	return out, nil
}

// queryRequests runs a SELECT in requestColumns order and scans into RequestRows.
func (s *RequestStore) queryRequests(ctx context.Context, op, q string, args []interface{}) ([]RequestRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("request: %s: %w", op, err)
	}
	defer rows.Close()

	out := make([]RequestRow, 0)
	for rows.Next() {
		r, scanErr := scanRequest(rows)
		if scanErr != nil {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("request: %s rows.Err: %v", op, err)
	}
	return out, nil
}

// actionToStatus maps a terminal respond action to the status it sets, gated by
// the request's kind: approval accepts approved/rejected, task accepts
// done/rejected. Returns ErrInvalidRequestAction on any other combination.
func actionToStatus(kind, action string) (string, error) {
	switch kind {
	case "approval":
		if action == "approved" || action == "rejected" {
			return action, nil
		}
	case "task":
		if action == "done" || action == "rejected" {
			return action, nil
		}
	}
	return "", ErrInvalidRequestAction
}

// Respond applies a terminal action (done/rejected/approved, validated against
// the request's kind), stamps responder + responded_at, and broadcasts
// REQUEST_RESPONDED so the requester/canvas picks it up asynchronously. The
// requester is NOT blocked — it reads this via ListOutgoing on its next tick.
// responderType defaults to "user" (the canvas path); responderID defaults to
// "human" when empty. Only acts on a non-terminal (pending/info_requested) row.
func (s *RequestStore) Respond(ctx context.Context, id, action, responderType, responderID string) (RequestRow, error) {
	if responderType == "" {
		responderType = "user"
	}
	if !validParty(responderType) {
		return RequestRow{}, ErrInvalidRequestParty
	}
	if responderID == "" {
		responderID = "human"
	}

	// Look the request up first so we can validate action↔kind compatibility and
	// know who to signal (the requester).
	req, err := s.Get(ctx, id)
	if err != nil {
		return RequestRow{}, err
	}

	// SECURITY: prevent self-approval / self-rejection. The requester must not
	// be the same party as the responder — that would let an agent approve its
	// own tasks or a user self-approve their own requests (RC 10416).
	if responderType == req.RequesterType && responderID == req.RequesterID {
		return RequestRow{}, ErrSelfResponse
	}

	status, err := actionToStatus(req.Kind, action)
	if err != nil {
		return RequestRow{}, err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE requests
		SET status = $1, responder_type = $2, responder_id = $3,
			responded_at = now(), updated_at = now()
		WHERE id = $4 AND status IN ('pending', 'info_requested')
	`, status, responderType, responderID, id)
	if err != nil {
		return RequestRow{}, fmt.Errorf("request: respond: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return RequestRow{}, fmt.Errorf("request: respond RowsAffected: %w", err)
	}
	if n == 0 {
		return RequestRow{}, ErrRequestNotFound
	}

	// Signal the requester (the agent that raised it) so its inbox/canvas picks
	// up the resolution asynchronously.
	target := broadcastTarget(req.RequesterType, req.RequesterID, req.RecipientType, req.RecipientID)
	if target != "" {
		if err := s.broadcaster.RecordAndBroadcast(ctx, string(events.EventRequestResponded), target, map[string]interface{}{
			"request_id":     id,
			"status":         status,
			"responder_type": responderType,
			"responder_id":   responderID,
			// title + kind let the canvas render a live decision line in My
			// Chat ("You approved 'X'") the moment a user responds, instead
			// of the decision being invisible until a reload (core#2636).
			"title": req.Title,
			"kind":  req.Kind,
		}); err != nil {
			log.Printf("request: failed to broadcast responded for %s: %v", target, err)
		}
	}

	// Deliver the outcome to the requester AGENT as a real inbound turn
	// (core#2606 follow-up, CTO 2026-06-11). The REQUEST_RESPONDED event
	// above only feeds the canvas/event stream; an agent waiting on an
	// approval otherwise learns the decision only if something prompts it
	// to call check_requests. Skip self-notification (agent responded to
	// its own... impossible per the self-response guard, but cheap belt).
	if req.RequesterType == "agent" && req.RequesterID != "" &&
		(responderType != "agent" || responderID != req.RequesterID) {
		by := "the user"
		if responderType == "agent" {
			by = "agent " + responderID
		}
		s.notifyRequesterAgent(ctx, req,
			"request-responded:"+req.ID,
			fmt.Sprintf("Your %s request %q (id %s) was %s by %s. Use get_request for the thread or check_requests for all your outcomes. "+
				"If this outcome changes what you owe the user, acknowledge it briefly with send_message_to_user — this notification is a background turn and does NOT appear in their chat.",
				req.Kind, req.Title, req.ID, status, by))
	}

	req.Status = status
	req.ResponderType = &responderType
	req.ResponderID = &responderID
	return req, nil
}

// RequestInfo flips a request to 'info_requested' (the "More Info" transition)
// without a terminal action. Used when the recipient asks the requester for
// clarification. Only acts on a non-terminal row.
func (s *RequestStore) RequestInfo(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE requests SET status = 'info_requested', updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'info_requested')
	`, id)
	if err != nil {
		return fmt.Errorf("request: request-info: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("request: request-info RowsAffected: %w", err)
	}
	if n == 0 {
		return ErrRequestNotFound
	}
	return nil
}

// AddMessage appends a row to the More-Info thread and broadcasts
// REQUEST_MESSAGE. When the author is the request's RECIPIENT (i.e. the party
// being asked is asking back for clarification), it also flips the request to
// 'info_requested' so the requester knows it's their turn. Returns the new
// message id.
func (s *RequestStore) AddMessage(ctx context.Context, id, authorType, authorID, body string) (string, error) {
	if !validParty(authorType) {
		return "", ErrInvalidRequestParty
	}

	req, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}

	var messageID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO request_messages (request_id, author_type, author_id, body)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, id, authorType, authorID, body).Scan(&messageID)
	if err != nil {
		return "", fmt.Errorf("request: add message: %w", err)
	}

	// A message from anyone OTHER than the requester is a "please clarify" back
	// TO the requester — flip to info_requested so the requester is prompted.
	// We key off "not the requester" rather than "is the recipient" on purpose:
	// an agent→user request is stored with an EMPTY recipient_id (the generic
	// "the user"), but the canvas posts the reply with a concrete author_id
	// (the session user_id, or the "admin" placeholder). A strict
	// authorID==RecipientID match would never hold for that common case, so the
	// flip (and the requester notification below) would silently never fire.
	// Only flip a non-terminal request; a closed request keeps its terminal
	// status even if a late note is appended.
	authoredByRequester := authorType == req.RequesterType && authorID == req.RequesterID
	if !authoredByRequester {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE requests SET status = 'info_requested', updated_at = now()
			WHERE id = $1 AND status IN ('pending', 'info_requested')
		`, id); err != nil {
			log.Printf("request: failed to flip info_requested on message for %s: %v", id, err)
		}
	}

	// Signal both ends via the requester anchor (the canvas thread listens there).
	target := broadcastTarget(req.RequesterType, req.RequesterID, req.RecipientType, req.RecipientID)
	if target != "" {
		if err := s.broadcaster.RecordAndBroadcast(ctx, string(events.EventRequestMessage), target, map[string]interface{}{
			"request_id":  id,
			"message_id":  messageID,
			"author_type": authorType,
			"author_id":   authorID,
		}); err != nil {
			log.Printf("request: failed to broadcast message for %s: %v", id, err)
		}
	}

	// More-Info from the other party must reach a requester AGENT as a real
	// turn (same rationale as the Respond notification — CTO 2026-06-11).
	// Same "not the requester" gate as the flip above: the user's reply on an
	// agent→user request carries a concrete author_id while recipient_id is
	// empty, so we must NOT require authorID==RecipientID here. We only skip
	// when the requester authored the message (it would be notifying itself).
	// Keyed per message so a multi-round clarification thread delivers each
	// ask; the requester replies with add_request_message.
	if !authoredByRequester && req.RequesterType == "agent" && req.RequesterID != "" {
		s.notifyRequesterAgent(ctx, req,
			"request-message:"+messageID,
			fmt.Sprintf("More info requested on your %s request %q (id %s): %s\nReply with add_request_message.",
				req.Kind, req.Title, req.ID, body))
	}

	return messageID, nil
}

// Cancel withdraws a request (the requester changed its mind). Sets status
// 'cancelled' + updated_at. Only acts on a non-terminal row; returns
// ErrRequestNotFound when missing or already terminal.
func (s *RequestStore) Cancel(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE requests SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'info_requested')
	`, id)
	if err != nil {
		return fmt.Errorf("request: cancel: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("request: cancel RowsAffected: %w", err)
	}
	if n == 0 {
		return ErrRequestNotFound
	}
	return nil
}
