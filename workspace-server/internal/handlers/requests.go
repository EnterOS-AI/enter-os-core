package handlers

import (
	"errors"
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
)

// RequestsHandler serves the unified "requests" inbox — the Tasks + Approvals
// primitive (RFC P1, docs/design/rfc-unified-requests-inbox.md). It generalizes
// UserTasksHandler (agent→user asks) and ApprovalsHandler (the gate) into one
// surface keyed by kind ∈ {task, approval}, where requester and recipient may
// each be a user OR an agent. Responding is asynchronous: the requester is
// never blocked; a REQUEST_RESPONDED event signals it to pick the answer up on
// its next tick.
type RequestsHandler struct {
	broadcaster *events.Broadcaster
}

// --- OpenAPI doc shapes (used by swaggo; the handlers emit gin.H inline) ---

// CreateRequestBody is the body of POST /workspaces/{id}/requests. requester is
// the calling workspace (agent); only the recipient + content are supplied.
type CreateRequestBody struct {
	Kind          string `json:"kind" binding:"required" enums:"task,approval"`
	RecipientType string `json:"recipient_type" binding:"required" enums:"user,agent"`
	RecipientID   string `json:"recipient_id"`
	Title         string `json:"title" binding:"required"`
	Detail        string `json:"detail"`
	Priority      *int   `json:"priority"`
}

// CreateRequestResponse is returned by POST /workspaces/{id}/requests.
type CreateRequestResponse struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

// RespondRequestBody is the body of POST /requests/{requestId}/respond. The
// responder identity is taken from the body for now (P1); the canvas path
// defaults responder_type to 'user'. action is validated against the kind.
type RespondRequestBody struct {
	Action        string `json:"action" binding:"required" enums:"done,rejected,approved"`
	ResponderType string `json:"responder_type" enums:"user,agent"`
	ResponderID   string `json:"responder_id"`
}

// AddRequestMessageBody is the body of POST /requests/{requestId}/messages —
// the More-Info thread. When the author is the recipient, the request flips to
// info_requested.
type AddRequestMessageBody struct {
	Body       string `json:"body" binding:"required"`
	AuthorType string `json:"author_type" binding:"required" enums:"user,agent"`
	AuthorID   string `json:"author_id"`
}

// RequestMutationResponse is the {status, request_id} echo returned by the
// respond / cancel / messages endpoints.
type RequestMutationResponse struct {
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
}

// RequestWithThread is the GET /requests/{requestId} shape — the request plus
// its More-Info thread.
type RequestWithThread struct {
	Request  RequestRow          `json:"request"`
	Messages []RequestMessageRow `json:"messages"`
}

func NewRequestsHandler(b *events.Broadcaster) *RequestsHandler {
	return &RequestsHandler{broadcaster: b}
}

// store builds a RequestStore over the live global db.DB per request — same
// rationale as UserTasksHandler.store(): the test harness swaps db.DB under us.
func (h *RequestsHandler) store() *RequestStore {
	return NewRequestStore(db.DB, h.broadcaster)
}

// Create handles POST /workspaces/:id/requests — the calling workspace (an
// agent) raises a task/approval addressed to a user or another agent.
//
//	@Summary	Raise a request (task or approval)
//	@Tags		requests
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string				true	"Requester workspace ID"
//	@Param		body	body		CreateRequestBody	true	"Request fields"
//	@Success	201		{object}	CreateRequestResponse
//	@Failure	400		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Router		/workspaces/{id}/requests [post]
//	@Security	BearerAuth && OrgSlugAuth
func (h *RequestsHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body CreateRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Decorate the request with its org anchor for the cross-org pending view.
	// The workspaces table has NO org_id column — an "org" is the parent_id-chain
	// root resolved by orgRootID (org_scope.go). Best-effort: a missing root
	// leaves org_id NULL (the org view simply won't surface it), never blocks
	// creation.
	orgID, err := orgRootID(ctx, db.DB, workspaceID)
	if err != nil {
		log.Printf("requests: failed to resolve org root for workspace=%s: %v", workspaceID, err)
		orgID = ""
	}

	requestID, err := h.store().Create(ctx, CreateRequestInput{
		Kind:          body.Kind,
		RequesterType: "agent",
		RequesterID:   workspaceID,
		OrgID:         orgID,
		RecipientType: body.RecipientType,
		RecipientID:   body.RecipientID,
		Title:         body.Title,
		Detail:        body.Detail,
		Priority:      body.Priority,
	})
	if err != nil {
		if errors.Is(err, ErrInvalidRequestKind) || errors.Is(err, ErrInvalidRequestParty) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		log.Printf("Create request error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"request_id": requestID, "status": "pending"})
}

// ListInbox handles GET /workspaces/:id/requests/inbox?status= — requests
// addressed TO this workspace (the agent's incoming).
//
//	@Summary	List a workspace's incoming requests (inbox)
//	@Tags		requests
//	@Produce	json
//	@Param		id		path	string	true	"Recipient workspace ID"
//	@Param		status	query	string	false	"Filter by status"
//	@Success	200	{array}		RequestRow
//	@Failure	500	{object}	ErrorResponse
//	@Router		/workspaces/{id}/requests/inbox [get]
//	@Security	BearerAuth && OrgSlugAuth
func (h *RequestsHandler) ListInbox(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	rows, err := h.store().ListInbox(ctx, "agent", workspaceID, c.Query("status"))
	if err != nil {
		log.Printf("List inbox error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// ListOutgoing handles GET /workspaces/:id/requests?status= — the requests this
// workspace RAISED (the async pickup of responses).
//
//	@Summary	List a workspace's outgoing requests
//	@Tags		requests
//	@Produce	json
//	@Param		id		path	string	true	"Requester workspace ID"
//	@Param		status	query	string	false	"Filter by status"
//	@Success	200	{array}		RequestRow
//	@Failure	500	{object}	ErrorResponse
//	@Router		/workspaces/{id}/requests [get]
//	@Security	BearerAuth && OrgSlugAuth
func (h *RequestsHandler) ListOutgoing(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	rows, err := h.store().ListOutgoing(ctx, "agent", workspaceID, c.Query("status"))
	if err != nil {
		log.Printf("List outgoing error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// Get handles GET /requests/:requestId — a single request plus its More-Info
// thread.
//
//	@Summary	Get a request with its message thread
//	@Tags		requests
//	@Produce	json
//	@Param		requestId	path	string	true	"Request ID"
//	@Success	200	{object}	RequestWithThread
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Router		/requests/{requestId} [get]
//	@Security	BearerAuth
func (h *RequestsHandler) Get(c *gin.Context) {
	requestID := c.Param("requestId")
	ctx := c.Request.Context()

	s := h.store()
	req, err := s.Get(ctx, requestID)
	if err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
			return
		}
		log.Printf("Get request error request=%s: %v", requestID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	// Workspace-token auth path: a workspace may read only a request it is a
	// party to (requester or recipient). Cross-workspace read is forbidden
	// (core#2542 full fix).
	if workspaceID := c.Param("id"); workspaceID != "" {
		isParty := (req.RequesterType == "agent" && req.RequesterID == workspaceID) ||
			(req.RecipientType == "agent" && req.RecipientID == workspaceID)
		if !isParty {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a participant"})
			return
		}
	}

	msgs, err := s.Messages(ctx, requestID)
	if err != nil {
		log.Printf("Get request messages error request=%s: %v", requestID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"request": req, "messages": msgs})
}

// Respond handles POST /requests/:requestId/respond — a terminal action
// (done/rejected/approved), validated against the request's kind.
//
// REAL participant-binding (RC 10416 follow-up): on the workspace-token auth
// path (/workspaces/:id/requests/...), the responder is BOUND to the
// authenticated workspace — the body fields are ignored. This prevents a
// workspace from impersonating a user or a different agent when responding.
// The canvas/admin path continues to take responder from the body (defaults
// to 'user' on the canvas).
//
//	@Summary	Respond to a request (done / rejected / approved)
//	@Tags		requests
//	@Accept		json
//	@Produce	json
//	@Param		requestId	path		string				true	"Request ID"
//	@Param		body		body		RespondRequestBody	true	"Response"
//	@Success	200			{object}	RequestMutationResponse
//	@Failure	400			{object}	ErrorResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/requests/{requestId}/respond [post]
//	@Security	BearerAuth
func (h *RequestsHandler) Respond(c *gin.Context) {
	requestID := c.Param("requestId")
	ctx := c.Request.Context()

	var body RespondRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	responderType := body.ResponderType
	responderID := body.ResponderID

	// Workspace-token auth path: bind responder to the authenticated workspace.
	if workspaceID := c.Param("id"); workspaceID != "" {
		responderType = "agent"
		responderID = workspaceID
	}

	// Workspace-token auth path: verify the caller is the request's recipient.
	// Cross-workspace respond is forbidden — an agent must not terminate a
	// request that was not addressed to it (core#2542 full fix).
	if workspaceID := c.Param("id"); workspaceID != "" {
		reqRow, err := h.store().Get(ctx, requestID)
		if err != nil {
			if errors.Is(err, ErrRequestNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "request not found or already resolved"})
				return
			}
			log.Printf("Respond authz error request=%s: %v", requestID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to respond"})
			return
		}
		if reqRow.RecipientType != "agent" || reqRow.RecipientID != workspaceID {
			c.JSON(http.StatusForbidden, gin.H{"error": "not the recipient"})
			return
		}
	}

	if _, err := h.store().Respond(ctx, requestID, body.Action, responderType, responderID); err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "request not found or already resolved"})
			return
		}
		if errors.Is(err, ErrInvalidRequestAction) || errors.Is(err, ErrInvalidRequestParty) || errors.Is(err, ErrSelfResponse) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		log.Printf("Respond request error request=%s: %v", requestID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": body.Action, "request_id": requestID})
}

// AddMessage handles POST /requests/:requestId/messages — append to the
// More-Info thread. When the author is the recipient, the request flips to
// info_requested.
//
// Workspace-token auth path (/workspaces/:id/requests/:requestId/messages):
// the caller must be a participant (requester or recipient), and the author
// identity is BOUND to the authenticated workspace — body author_type/author_id
// are ignored. This prevents a workspace-token holder from spoofing another
// party or flipping an unrelated request to info_requested (core#2542 /
// core#2606).
//
//	@Summary	Add a message to a request's More-Info thread
//	@Tags		requests
//	@Accept		json
//	@Produce	json
//	@Param		requestId	path		string					true	"Request ID"
//	@Param		body		body		AddRequestMessageBody	true	"Message"
//	@Success	201			{object}	RequestMutationResponse
//	@Failure	400			{object}	ErrorResponse
//	@Failure	403			{object}	ErrorResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/requests/{requestId}/messages [post]
//	@Security	BearerAuth
func (h *RequestsHandler) AddMessage(c *gin.Context) {
	requestID := c.Param("requestId")
	ctx := c.Request.Context()

	var body AddRequestMessageBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authorType := body.AuthorType
	authorID := body.AuthorID

	// Workspace-token auth path: bind author to the authenticated workspace and
	// verify the caller is a participant.
	if workspaceID := c.Param("id"); workspaceID != "" {
		authorType = "agent"
		authorID = workspaceID

		reqRow, err := h.store().Get(ctx, requestID)
		if err != nil {
			if errors.Is(err, ErrRequestNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
				return
			}
			log.Printf("AddMessage authz error request=%s: %v", requestID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add message"})
			return
		}
		isParty := (reqRow.RequesterType == "agent" && reqRow.RequesterID == workspaceID) ||
			(reqRow.RecipientType == "agent" && reqRow.RecipientID == workspaceID)
		if !isParty {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a participant"})
			return
		}
	}

	messageID, err := h.store().AddMessage(ctx, requestID, authorType, authorID, body.Body)
	if err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
			return
		}
		if errors.Is(err, ErrInvalidRequestParty) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		log.Printf("AddMessage request error request=%s: %v", requestID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add message"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"status": "created", "request_id": requestID, "message_id": messageID})
}

// Cancel handles POST /requests/:requestId/cancel — the requester withdraws.
//
//	@Summary	Cancel (withdraw) a request
//	@Tags		requests
//	@Produce	json
//	@Param		requestId	path		string	true	"Request ID"
//	@Success	200			{object}	RequestMutationResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/requests/{requestId}/cancel [post]
//	@Security	BearerAuth
func (h *RequestsHandler) Cancel(c *gin.Context) {
	requestID := c.Param("requestId")
	ctx := c.Request.Context()

	// Workspace-token auth path: verify the caller is the request's requester.
	// Cross-workspace cancel is forbidden — an agent must not withdraw a
	// request it did not raise (core#2542 full fix).
	if workspaceID := c.Param("id"); workspaceID != "" {
		reqRow, err := h.store().Get(ctx, requestID)
		if err != nil {
			if errors.Is(err, ErrRequestNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "request not found or already resolved"})
				return
			}
			log.Printf("Cancel authz error request=%s: %v", requestID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel"})
			return
		}
		if reqRow.RequesterType != "agent" || reqRow.RequesterID != workspaceID {
			c.JSON(http.StatusForbidden, gin.H{"error": "not the requester"})
			return
		}
	}

	if err := h.store().Cancel(ctx, requestID); err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "request not found or already resolved"})
			return
		}
		log.Printf("Cancel request error request=%s: %v", requestID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "cancelled", "request_id": requestID})
}

// ListPending handles GET /requests/pending?kind= — the cross-org pending view
// for the canvas Tasks/Approvals tabs. Cross-workspace, so AdminAuth-gated like
// /user-tasks/pending and /approvals/pending. ?kind=task|approval lets each tab
// query its own slice.
//
//	@Summary	List pending requests across the org (canvas tabs)
//	@Tags		requests
//	@Produce	json
//	@Param		kind	query	string	false	"Filter by kind"	Enums(task, approval)
//	@Param		org_id	query	string	false	"Filter by org"
//	@Success	200	{array}		RequestRow
//	@Failure	400	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Router		/requests/pending [get]
//	@Security	BearerAuth
func (h *RequestsHandler) ListPending(c *gin.Context) {
	ctx := c.Request.Context()

	kind := c.Query("kind")
	if kind != "" && kind != "task" && kind != "approval" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be 'task' or 'approval'"})
		return
	}

	rows, err := h.store().ListPendingForOrg(ctx, c.Query("org_id"), kind)
	if err != nil {
		log.Printf("ListPending requests error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	c.JSON(http.StatusOK, rows)
}
