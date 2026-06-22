package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// internal#212 — secret-safe scrubber applied to error_detail strings
// before they cross the canvas WebSocket. Defense in depth: the
// workspace runtime already runs `_sanitize_for_external` on its side
// (workspace/executor_helpers.py), but the broadcast layer is the last
// stop before the string reaches the user's browser, so we re-scrub
// here in case any caller path forgot.
//
// The scrubber is intentionally surgical — it MUST preserve the
// actionable parts (HTTP status codes, error codes like
// `oauth_org_not_allowed`, human-readable provider messages) and
// remove only what looks credential-ish. Over-redacting defeats the
// whole point of internal#212 (giving the user a reason they can act on).

// Capture (auth-key prefix) (value) so the prefix can be preserved in
// the output. The keyword anchor prevents false positives on regular
// text that happens to contain a long alphanumeric run.
var errorDetailSecretRE = regexp.MustCompile(`(?i)((?:bearer|token|api[_-]?key|sk-proj-|sk-)[ :=]*)[A-Za-z0-9_/.-]{20,}`)

// Stringly-typed JWT-shape: 3 dot-separated base64url segments, second
// and third at least 16 chars. Matches eyJ-prefixed tokens that the
// keyword-anchored rule above would miss when they appear bare.
var errorDetailJWTRE = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)

const errorDetailBroadcastCap = 4096

func sanitizeErrorDetailForBroadcast(s string) string {
	if s == "" {
		return s
	}
	// Cap first — a huge error body shouldn't tax every websocket
	// client's buffer. 4096 matches the workspace-side _MAX_STDERR
	// budget (it's actually larger here so the runtime's cap dominates).
	if len(s) > errorDetailBroadcastCap {
		s = s[:errorDetailBroadcastCap] + "…[truncated]"
	}
	s = errorDetailSecretRE.ReplaceAllString(s, "${1}[REDACTED]")
	s = errorDetailJWTRE.ReplaceAllString(s, "[REDACTED]")
	return s
}

type ActivityHandler struct {
	broadcaster events.EventEmitter
}

func NewActivityHandler(b events.EventEmitter) *ActivityHandler {
	return &ActivityHandler{broadcaster: b}
}

// extractAttachmentsFromRequestBody walks a JSON-RPC a2a inbound body to
// surface attachments (file/image/audio/video) as a flat `attachments[]`
// projection so callers don't have to drill into the request_body shape
// themselves.
//
// Two body shapes are walked in order:
//
//  1. a2a-sdk v1 message-part envelope (peer_agent inbound):
//
//     {"jsonrpc":"2.0","method":"message/send","params":{
//     "message":{"parts":[
//     {"kind":"text", "text":"hi"},
//     {"kind":"file", "file":{"uri":"workspace:foo.pdf","mime_type":"application/pdf","name":"foo.pdf"}},
//     {"kind":"image","file":{"uri":"workspace:bar.png","mime_type":"image/png","name":"bar.png"}},
//     ]}}}
//
//  2. canvas chat_upload_receive flat manifest (canvas_user upload):
//
//     {"uri":"platform-pending:<ws>/<file>",
//     "name":"pasted.png",
//     "size":12345,
//     "file_id":"<uuid>",
//     "mimeType":"image/png"}
//
//     The canvas upload pipe writes a single manifest directly at the
//     root of request_body (no JSON-RPC envelope) with camelCase
//     `mimeType`. We normalize to snake_case `mime_type` on the way out
//     so every downstream adaptor (channel / telegram / codex / hermes)
//     sees one wire shape regardless of which inbound shape produced it.
//
// Returns nil (omit-from-JSON) when the body has no attachments — the
// `?include=peer_info` envelope projects this as an array iff non-empty.
//
// Defensive on every step: any missing key / wrong-shape value falls
// through to the next arm or returns nil instead of panicking. The
// activity_logs row could carry literally any JSON in request_body
// (legacy formats, future formats); we only commit to the documented
// shapes and silently skip anything else.
func extractAttachmentsFromRequestBody(raw []byte) []map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	if atts := extractAttachmentsFromMessageParts(body); len(atts) > 0 {
		return atts
	}
	if att := extractAttachmentFromFlatUploadManifest(body); att != nil {
		return []map[string]interface{}{att}
	}
	return nil
}

// extractAttachmentsFromMessageParts handles the a2a-sdk v1 shape:
// body.params.message.parts[]. Walks file/image/audio parts; honors v1
// `kind` and v0 `type` discriminators; accepts nested `.file` sub-object
// or inlined uri/mime_type/name on the part itself.
func extractAttachmentsFromMessageParts(body map[string]interface{}) []map[string]interface{} {
	params, ok := body["params"].(map[string]interface{})
	if !ok {
		return nil
	}
	message, ok := params["message"].(map[string]interface{})
	if !ok {
		return nil
	}
	parts, ok := message["parts"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]map[string]interface{}, 0)
	for _, p := range parts {
		part, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		// a2a-sdk v1 uses "kind"; older v0 callers sent "type". Accept
		// both for the discriminator — same defensive read pattern as
		// the runtime-side extract_text helper.
		kind, _ := part["kind"].(string)
		if kind == "" {
			kind, _ = part["type"].(string)
		}
		if kind != "file" && kind != "image" && kind != "audio" && kind != "video" {
			continue
		}
		// The file sub-object holds uri/mime_type/name. The a2a-sdk v1
		// shape nests under "file"; some legacy payloads inlined the
		// fields onto the part itself. Support both.
		var fileObj map[string]interface{}
		if f, ok := part["file"].(map[string]interface{}); ok {
			fileObj = f
		} else {
			fileObj = part
		}
		uri, _ := fileObj["uri"].(string)
		mimeType, _ := fileObj["mime_type"].(string)
		name, _ := fileObj["name"].(string)
		// At minimum we need either a uri or a name to be useful.
		// Empty-part entries are skipped (they're a malformed inbound
		// — surface nothing rather than emit a no-info placeholder).
		if uri == "" && name == "" {
			continue
		}
		att := map[string]interface{}{"kind": kind}
		if uri != "" {
			att["uri"] = uri
		}
		if mimeType != "" {
			att["mime_type"] = mimeType
		}
		if name != "" {
			att["name"] = name
		}
		out = append(out, att)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractAttachmentFromFlatUploadManifest handles the canvas
// chat_upload_receive shape: a single upload manifest at the root of
// request_body with no JSON-RPC envelope. Canvas uses camelCase
// `mimeType`; we normalize to snake_case `mime_type` on emit so the
// wire shape matches the message-parts arm. Kind is derived from the
// mime prefix (image/* → "image", audio/* → "audio", video/* → "video",
// anything else → "file") because the canvas upload row doesn't carry
// an explicit discriminator. Returns nil if neither `uri` nor `file_id`
// is present at the root (i.e. not a flat upload manifest).
func extractAttachmentFromFlatUploadManifest(body map[string]interface{}) map[string]interface{} {
	uri, _ := body["uri"].(string)
	fileID, _ := body["file_id"].(string)
	if uri == "" && fileID == "" {
		return nil
	}
	mimeType, _ := body["mimeType"].(string)
	if mimeType == "" {
		// Defensive: future canvas versions might emit snake_case directly.
		mimeType, _ = body["mime_type"].(string)
	}
	name, _ := body["name"].(string)
	// Apply the same minimum-info rule as the message-parts arm: a
	// manifest with neither uri nor name is non-actionable; skip.
	if uri == "" && name == "" {
		return nil
	}
	att := map[string]interface{}{"kind": kindFromMimeType(mimeType)}
	if uri != "" {
		att["uri"] = uri
	}
	if mimeType != "" {
		att["mime_type"] = mimeType
	}
	if name != "" {
		att["name"] = name
	}
	return att
}

// kindFromMimeType derives the attachment `kind` discriminator from a
// MIME type. Used by the flat-upload-manifest arm where the source row
// has no explicit kind field.
func kindFromMimeType(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	default:
		return "file"
	}
}

// includeFlagSet returns true iff `flag` appears in the comma-separated
// `?include=` query value. Whitespace around entries is tolerated.
// Empty `include` returns false (existing back-compat shape).
//
// The comma-separable form lets future fields ("attachments_only",
// "tool_trace_expanded", etc.) slot in without further URL-param creep.
func includeFlagSet(includeQuery, flag string) bool {
	if includeQuery == "" || flag == "" {
		return false
	}
	for _, raw := range strings.Split(includeQuery, ",") {
		if strings.TrimSpace(raw) == flag {
			return true
		}
	}
	return false
}

// List handles GET /workspaces/:id/activity?type=&source=&limit=&since_secs=&since_id=&include=
//
// The `include` query param is comma-separable; today the only flag is
// `peer_info`, which enriches a2a_receive rows with `peer_name`,
// `peer_role`, `agent_card_url`, and an `attachments[]` projection (see
// extractAttachmentsFromRequestBody). It's additive + opt-in — existing
// callers that don't pass `?include=peer_info` see the unchanged shape.
// Surface for the layered enrichment that lets Claude Code channel
// pushes carry full sender identity instead of bare UUIDs (sibling
// repos: molecule-ai-workspace-runtime + molecule-mcp-claude-channel).
//
// since_secs filters to activity_logs.created_at >= NOW() - INTERVAL '$N seconds'.
// Optional, additive — callers that don't pass it get today's behavior (the
// most-recent N events regardless of time). The harness runner
// (scripts/measure-coordinator-task-bounds-runner.sh) uses this to scope a
// trace to a specific test window; RFC #2251 §V1.0 step 6 also depends on it.
// Capped at 30 days (2_592_000s) — anything older has typically been paged
// out anyway, and a defensive ceiling keeps a paranoid client from triggering
// a full-table scan via since_secs=99999999999. Closes #2268.
//
// since_id is a CURSOR for poll-mode workspaces (#2339 PR 3). The agent
// passes the id of the last activity_logs row it has consumed; the server
// returns rows STRICTLY AFTER that cursor in chronological (ASC) order so
// the agent processes events in the order they were recorded. Telegram
// getUpdates / Slack RTM shape — same proven pattern.
//
// Cross-workspace safety: the cursor lookup is scoped by workspace_id, so a
// caller cannot peek at another workspace's activity by guessing its UUIDs.
//
// Cursor-not-found: returns 410 Gone. The client should reset its cursor
// (omit since_id) and re-fetch the recent backlog. This avoids the silent
// loss-window where a pruned cursor silently filters everything out.
//
// since_id + since_secs together: both filters apply (AND). Output is ASC
// when since_id is set (polling order), DESC otherwise (recent feed order).
func (h *ActivityHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	activityType := c.Query("type")
	source := c.Query("source")  // "canvas" = source_id IS NULL, "agent" = source_id IS NOT NULL
	peerID := c.Query("peer_id") // optional UUID — restrict to rows where this peer is sender OR target
	limitStr := c.DefaultQuery("limit", "100")
	sinceSecsStr := c.Query("since_secs")
	sinceID := c.Query("since_id")
	beforeTSStr := c.Query("before_ts") // optional RFC3339 — return rows strictly older than this timestamp
	include := c.Query("include")       // comma-separated; today's only flag is "peer_info"
	includePeerInfo := includeFlagSet(include, "peer_info")

	// Validate peer_id as a UUID at the trust boundary so a malformed
	// caller (the agent or a downstream MCP tool) can't smuggle SQL
	// fragments into the WHERE clause via the parameter, even though
	// args are bound. UUID-shape rejection is also the cleanest 400
	// signal for the wheel-side chat_history MCP tool — clearer than a
	// generic "no rows" empty list when the agent passed an obviously
	// wrong id.
	if peerID != "" {
		if _, err := uuid.Parse(peerID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "peer_id must be a UUID"})
			return
		}
	}

	// Parse before_ts as the wall-clock paging knob for the wheel-side
	// `chat_history` MCP tool. The agent passes the oldest `created_at`
	// from a previous response to walk backward through long histories.
	// Validated as RFC3339 at the trust boundary so a typoed value
	// surfaces as a clean 400 instead of being silently ignored.
	var beforeTS time.Time
	usingBeforeTS := false
	if beforeTSStr != "" {
		t, err := time.Parse(time.RFC3339, beforeTSStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "before_ts must be an RFC3339 timestamp (e.g. 2026-05-01T00:00:00Z)",
			})
			return
		}
		beforeTS = t
		usingBeforeTS = true
	}

	limit := 100
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
		if limit > 500 {
			limit = 500
		}
	}

	// Parse since_secs. Reject negative or non-integer values rather than
	// silently ignoring them — a typoed param shouldn't be lost as
	// most-recent-100, that's exactly the bug this fixes.
	var sinceSecs int
	if sinceSecsStr != "" {
		n, err := strconv.Atoi(sinceSecsStr)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since_secs must be a positive integer"})
			return
		}
		const maxSinceSecs = 30 * 24 * 60 * 60 // 30 days
		if n > maxSinceSecs {
			n = maxSinceSecs
		}
		sinceSecs = n
	}

	// Resolve since_id cursor (if set) BEFORE building the main query so we
	// can 410 cleanly when the cursor row is gone — and so the cursor's
	// created_at is bound as a regular timestamp parameter (not a subquery)
	// for clean sqlmock matching and to keep the planner predictable.
	//
	// The lookup is scoped by workspace_id: a caller cannot enumerate or
	// peek at another workspace's events by passing a UUID belonging to a
	// different workspace. Mismatched-workspace cursor → 410, same as
	// "row not found" — both indicate the cursor is no longer usable for
	// this caller, no information leak.
	var cursorTime time.Time
	var cursorSeq int64
	usingCursor := false
	if sinceID != "" {
		// Resolve BOTH ordering-key components of the cursor row. The feed is
		// ordered by (created_at, seq), so the strictly-after filter below must
		// compare the full tuple — comparing created_at alone silently drops a
		// row written in the SAME microsecond as the cursor row (the boundary
		// skip the since_id E2E intermittently tripped over).
		err := db.DB.QueryRowContext(c.Request.Context(),
			`SELECT created_at, seq FROM activity_logs WHERE id = $1 AND workspace_id = $2`,
			sinceID, workspaceID,
		).Scan(&cursorTime, &cursorSeq)
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusGone, gin.H{
				"error": "since_id cursor not found (row may have been pruned or belongs to a different workspace); omit since_id to reset",
			})
			return
		}
		if err != nil {
			log.Printf("Activity since_id cursor lookup error for ws=%s id=%s: %v", workspaceID, sinceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "cursor lookup failed"})
			return
		}
		usingCursor = true
	}

	// Build query with optional filters. When ?include=peer_info is set,
	// LEFT JOIN workspaces ON activity_logs.source_id = w.id so we can
	// surface w.name + w.role on the row. LEFT (not INNER) is required
	// for two reasons:
	//   1. Canvas rows have source_id IS NULL — those must still appear
	//      in the result set (with NULL peer_name/peer_role).
	//   2. A peer workspace may have been deleted since the row was
	//      written (no FK constraint on activity_logs.source_id) —
	//      LEFT JOIN preserves the activity row with NULL peer fields
	//      rather than silently dropping the row.
	//
	// agent_card_url is NOT pulled from the workspaces table; it's
	// computed server-side from externalPlatformURL + source_id at
	// projection time (mirrors molecule-ai-workspace-runtime
	// a2a_client._agent_card_url_for which constructs
	// {PLATFORM_URL}/registry/discover/{peer_id}).
	//
	// Column qualification (`activity_logs.<col>`) is added ONLY when
	// the JOIN is present — disambiguates `id` / `created_at` which
	// exist in both tables. When the JOIN is absent, unqualified
	// column references preserve the exact wire-shape existing callers
	// + existing test fixtures expect (back-compat).
	actCol := ""
	if includePeerInfo {
		actCol = "activity_logs."
	}
	selectClause := `SELECT ` + actCol + `id, ` + actCol + `workspace_id, ` + actCol + `activity_type, ` +
		actCol + `source_id, ` + actCol + `target_id, ` + actCol + `method, ` +
		actCol + `summary, ` + actCol + `request_body, ` + actCol + `response_body, ` +
		actCol + `tool_trace, ` + actCol + `duration_ms, ` + actCol + `status, ` +
		actCol + `error_detail, ` + actCol + `created_at`
	fromClause := ` FROM activity_logs`
	if includePeerInfo {
		selectClause += `, w.name AS peer_name, w.role AS peer_role`
		fromClause += ` LEFT JOIN workspaces w ON w.id = activity_logs.source_id`
	}
	query := selectClause + fromClause + ` WHERE ` + actCol + `workspace_id = $1`
	args := []interface{}{workspaceID}
	argIdx := 2

	// WHERE/ORDER column refs use the same `actCol` qualifier prefix
	// computed above — empty string when no JOIN (back-compat with
	// existing wire shape + sqlmock-regex test fixtures), or
	// `activity_logs.` when LEFT JOIN'd (disambiguates `id` /
	// `created_at` between the two tables).
	if activityType != "" {
		query += fmt.Sprintf(" AND "+actCol+"activity_type = $%d", argIdx)
		args = append(args, activityType)
		argIdx++
	}
	if source == "canvas" {
		query += " AND " + actCol + "source_id IS NULL"
	} else if source == "agent" {
		query += " AND " + actCol + "source_id IS NOT NULL"
	} else if source != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source must be 'canvas' or 'agent'"})
		return
	}
	if peerID != "" {
		// Restrict to rows where this peer is either the sender (source_id)
		// or the recipient (target_id) of an A2A turn. This is the
		// "conversation history with peer X" view the wheel-side
		// chat_history MCP tool surfaces — agent receives a peer_agent
		// push, wants to see the prior 20 turns with that workspace
		// without paging through every other peer's traffic.
		//
		// Bound as a single arg, matched twice — keeps argIdx accurate
		// and avoids duplicate parameter binding (some drivers reject the
		// same arg slot reused, ours is fine but the explicit form is
		// clearer to read and matches the rest of the builder.)
		query += fmt.Sprintf(" AND ("+actCol+"source_id = $%d OR "+actCol+"target_id = $%d)", argIdx, argIdx)
		args = append(args, peerID)
		argIdx++
	}
	if usingBeforeTS {
		// Strictly older — never replay a row with the exact same
		// timestamp, mirrors the `created_at > cursorTime` shape
		// `since_id` uses for forward paging.
		query += fmt.Sprintf(" AND "+actCol+"created_at < $%d", argIdx)
		args = append(args, beforeTS)
		argIdx++
	}
	if sinceSecs > 0 {
		// Use a parameterized interval so the value is bound, not
		// interpolated into the SQL string. `make_interval(secs => $N)`
		// avoids the lib/pq quirk where INTERVAL '$N seconds' won't
		// substitute a placeholder inside the literal.
		query += fmt.Sprintf(" AND "+actCol+"created_at >= NOW() - make_interval(secs => $%d)", argIdx)
		args = append(args, sinceSecs)
		argIdx++
	}
	if usingCursor {
		// Strictly after the cursor on the FULL ordering key (created_at, seq).
		// Tuple comparison: a row is "after" the cursor if its created_at is
		// later, OR it shares the cursor's created_at but has a higher seq.
		// This (a) never replays the cursor row itself and (b) — unlike a bare
		// `created_at > cursor` — never drops a row written in the same
		// microsecond as the cursor row. Expressed as the expanded boolean
		// rather than a row-value `(created_at, seq) > ($t, $s)` so it composes
		// with the actCol qualifier prefix and the existing placeholder/arg
		// builder cleanly.
		query += fmt.Sprintf(
			" AND ("+actCol+"created_at > $%d OR ("+actCol+"created_at = $%d AND "+actCol+"seq > $%d))",
			argIdx, argIdx, argIdx+1)
		args = append(args, cursorTime, cursorSeq)
		argIdx += 2
	}

	// Polling clients (since_id) need oldest-first within the new window so
	// they process events in recorded order. The recent-feed view (no
	// since_id) keeps DESC — that's the canvas/UI shape and changing it
	// would surprise existing callers.
	if usingCursor {
		// (created_at, seq) ASC — seq is the deterministic tiebreaker for rows
		// sharing a microsecond-collided created_at. Replays in recorded order.
		query += fmt.Sprintf(" ORDER BY "+actCol+"created_at ASC, "+actCol+"seq ASC LIMIT $%d", argIdx)
	} else {
		// (created_at, seq) DESC — same tiebreaker, newest-first for the
		// canvas/recent-feed shape.
		query += fmt.Sprintf(" ORDER BY "+actCol+"created_at DESC, "+actCol+"seq DESC LIMIT $%d", argIdx)
	}
	args = append(args, limit)

	rows, err := db.DB.QueryContext(c.Request.Context(), query, args...)

	if err != nil {
		log.Printf("Activity list error for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	// agent_card_url base computed once per request so we don't pay the
	// header-read cost per row. Only meaningful when includePeerInfo is
	// set; the empty string here is harmless when the flag is off.
	var platformBase string
	if includePeerInfo {
		platformBase = externalPlatformURL(c)
	}

	activities := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, wsID, actType, status string
		var sourceID, targetID, method, summary, errorDetail *string
		var reqBody, respBody, toolTrace []byte
		var durationMs *int
		var createdAt time.Time
		// LEFT JOIN'd peer columns — pointer-string so a NULL row
		// (canvas message OR deleted peer workspace) decodes as nil
		// rather than empty-string. Only scanned when includePeerInfo
		// is set (matched against the SELECT clause above).
		var peerName, peerRole *string

		var scanErr error
		if includePeerInfo {
			scanErr = rows.Scan(&id, &wsID, &actType, &sourceID, &targetID, &method,
				&summary, &reqBody, &respBody, &toolTrace, &durationMs, &status, &errorDetail, &createdAt,
				&peerName, &peerRole)
		} else {
			scanErr = rows.Scan(&id, &wsID, &actType, &sourceID, &targetID, &method,
				&summary, &reqBody, &respBody, &toolTrace, &durationMs, &status, &errorDetail, &createdAt)
		}
		if scanErr != nil {
			log.Printf("Activity scan error: %v", scanErr)
			continue
		}

		entry := map[string]interface{}{
			"id":            id,
			"workspace_id":  wsID,
			"activity_type": actType,
			"source_id":     sourceID,
			"target_id":     targetID,
			"method":        method,
			"summary":       summary,
			"duration_ms":   durationMs,
			"status":        status,
			"error_detail":  errorDetail,
			"created_at":    createdAt,
		}
		if reqBody != nil {
			entry["request_body"] = json.RawMessage(reqBody)
		}
		if respBody != nil {
			entry["response_body"] = json.RawMessage(respBody)
		}
		if toolTrace != nil {
			entry["tool_trace"] = json.RawMessage(toolTrace)
		}

		// peer_info enrichment (per ?include=peer_info). Only emit the
		// new fields when the flag is set — back-compat for callers
		// that don't request it.
		if includePeerInfo {
			// peer_name / peer_role: emit only when present (canvas
			// rows have source_id IS NULL → peer_name is NULL by JOIN;
			// also a peer workspace may have been deleted since the
			// row was written → same NULL outcome). Omit-when-absent
			// matches the Layer 3 adaptor's "spread when present"
			// pattern; canvas_user rows legitimately have no peer_*.
			if peerName != nil && *peerName != "" {
				entry["peer_name"] = *peerName
			}
			if peerRole != nil && *peerRole != "" {
				entry["peer_role"] = *peerRole
			}
			// agent_card_url: constructed server-side from
			// externalPlatformURL + source_id. Mirrors the runtime-
			// side helper a2a_client._agent_card_url_for which builds
			// {PLATFORM_URL}/registry/discover/{peer_id}. Only set
			// when source_id is present + non-empty.
			if sourceID != nil && *sourceID != "" && platformBase != "" {
				entry["agent_card_url"] = platformBase + "/registry/discover/" + *sourceID
			}
			// attachments: flatten file/image/audio parts from the
			// request_body. nil when none — only project when
			// non-empty so the omit-when-absent rule holds.
			if atts := extractAttachmentsFromRequestBody(reqBody); len(atts) > 0 {
				entry["attachments"] = atts
			}
		}

		activities = append(activities, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Activity list rows error for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}
	c.JSON(http.StatusOK, activities)
}

// SessionSearch handles GET /workspaces/:id/session-search?q=&limit=
// It searches the workspace's own activity logs and memories without adding a new storage layer.
func (h *ActivityHandler) SessionSearch(c *gin.Context) {
	workspaceID := c.Param("id")
	query, limit := parseSessionSearchParams(c)

	sqlQuery, args := buildSessionSearchQuery(workspaceID, query, limit)

	rows, err := db.DB.QueryContext(c.Request.Context(), sqlQuery, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session search failed"})
		return
	}
	defer rows.Close()

	items, scanErr := scanSessionSearchRows(rows)
	if scanErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}

	c.JSON(http.StatusOK, items)
}

// parseSessionSearchParams extracts the `q` and `limit` query params for SessionSearch,
// applying the default limit (50) and cap (200).
func parseSessionSearchParams(c *gin.Context) (string, int) {
	query := strings.TrimSpace(c.DefaultQuery("q", ""))
	limit := 50
	if n, err := strconv.Atoi(c.DefaultQuery("limit", "50")); err == nil && n > 0 {
		limit = n
		if limit > 200 {
			limit = 200
		}
	}
	return query, limit
}

// buildSessionSearchQuery composes the session-search SQL over
// activity_logs, returning the SQL string and positional args ready
// for QueryContext.
//
// Phase A3 (#1792): the agent_memories UNION branch was removed when
// the v1 table was dropped. Memory items no longer appear in session
// search; the canvas MemoryInspectorPanel queries /v2/memories
// directly for memory-tab content, so the UNION here only served
// callers that wanted activity + memory blended results — that
// surface is unused in production (verified via traffic audit
// 2026-05-24).
func buildSessionSearchQuery(workspaceID, query string, limit int) (string, []interface{}) {
	sqlQuery := `
		WITH session_items AS (
			SELECT
				'activity' AS kind,
				id,
				workspace_id,
				activity_type AS label,
				COALESCE(summary, '') AS content,
				COALESCE(method, '') AS method,
				COALESCE(status, '') AS status,
				request_body,
				response_body,
				created_at,
				seq
			FROM activity_logs
			WHERE workspace_id = $1
		)
		SELECT kind, id, workspace_id, label, content, method, status, request_body, response_body, created_at
		FROM session_items
	`

	args := []interface{}{workspaceID}
	if query != "" {
		sqlQuery += `
		WHERE (
			content ILIKE $2 OR
			label ILIKE $2 OR
			method ILIKE $2 OR
			status ILIKE $2 OR
			COALESCE(request_body::text, '') ILIKE $2 OR
			COALESCE(response_body::text, '') ILIKE $2
		)`
		args = append(args, "%"+query+"%")
	}

	// Deterministic order: created_at alone is not unique (same-microsecond
	// rows), so tie-break on the monotonic seq — same fix as the since_id feed
	// (§ No flakes: no unstable sorts, even on an unused surface). `seq` is
	// projected through the session_items CTE above so this outer ORDER BY can
	// reference it — the outer SELECT can only sort on the CTE's output columns,
	// not on activity_logs directly.
	sqlQuery += ` ORDER BY created_at DESC, seq DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	return sqlQuery, args
}

// scanSessionSearchRows materialises rows from the SessionSearch query into the
// JSON-shaped maps the endpoint returns. Per-row scan errors are logged and
// skipped (matches prior behavior); a rows.Err() failure is surfaced.
func scanSessionSearchRows(rows interface {
	Next() bool
	Scan(dest ...interface{}) error
	Err() error
}) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			kind, id, wsID, label, content, method, status string
			reqBody, respBody                              []byte
			createdAt                                      time.Time
		)
		if err := rows.Scan(&kind, &id, &wsID, &label, &content, &method, &status, &reqBody, &respBody, &createdAt); err != nil {
			log.Printf("Session search scan error: %v", err)
			continue
		}

		item := map[string]interface{}{
			"kind":         kind,
			"id":           id,
			"workspace_id": wsID,
			"label":        label,
			"content":      content,
			"method":       method,
			"status":       status,
			"created_at":   createdAt,
		}
		if reqBody != nil {
			item["request_body"] = json.RawMessage(reqBody)
		}
		if respBody != nil {
			item["response_body"] = json.RawMessage(respBody)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Session search rows error: %v", err)
		return nil, err
	}
	return items, nil
}

// NotifyAttachment is one file the agent wants to attach to its push.
// URIs come from /workspaces/:id/chat/uploads (canonical "workspace:"
// scheme) — the runtime's tool_send_message_to_user uploads any
// caller-specified file path through that endpoint first to get a
// shape the canvas can resolve via the existing Download path.
type NotifyAttachment struct {
	URI      string `json:"uri" binding:"required"`
	Name     string `json:"name" binding:"required"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// Notify handles POST /workspaces/:id/notify — agents push messages to the canvas chat.
// This enables agents to send interim updates ("I'll check on it") and follow-up results
// without waiting for the user to poll. Messages are broadcast via WebSocket only.
//
// Attachments: optional list of file references. Each renders as a
// download chip in the canvas via the existing extractFilesFromTask
// path. The runtime tool uploads file bytes to /chat/uploads first
// and passes the returned URIs here, so this handler only stores
// metadata — never raw bytes.
func (h *ActivityHandler) Notify(c *gin.Context) {
	workspaceID := c.Param("id")
	var body struct {
		Message     string             `json:"message" binding:"required"`
		Attachments []NotifyAttachment `json:"attachments,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return
	}

	// Per-element attachment validation: gin's go-playground/validator
	// does NOT iterate slice elements without `dive`, so the inner
	// `binding:"required"` tags on NotifyAttachment.URI/Name don't
	// actually run. Without this loop, attachments: [{"uri":"","name":""}]
	// would slip through, broadcast empty-URI chips that render
	// blank/broken in the canvas, and persist them in activity_logs
	// for every page reload to re-render. Validate explicitly.
	for i, a := range body.Attachments {
		if a.URI == "" || a.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("attachment[%d]: uri and name are required", i),
			})
			return
		}
	}

	// Single source of truth for chat-bearing agent → user messages —
	// see agent_message_writer.go for the contract. Pre-RFC-#2945, the
	// broadcast + INSERT pair was inlined here and again in
	// mcp_tools.go's send_message_to_user, and the duplication is what
	// produced the acme-demo data-loss regression. Both paths now
	// route through the same writer; future channels (Slack, Discord,
	// Lark) hook in here too.
	attachments := make([]AgentMessageAttachment, 0, len(body.Attachments))
	for _, a := range body.Attachments {
		attachments = append(attachments, AgentMessageAttachment(a))
	}
	writer := NewAgentMessageWriter(db.DB, h.broadcaster)
	if err := writer.Send(c.Request.Context(), workspaceID, body.Message, attachments); err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		if errors.Is(err, ErrTalkToUserDisabled) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "talk_to_user_disabled",
				"hint":  "This workspace is not allowed to send messages directly to the user. Forward your update to a parent workspace using delegate_task — they may be able to reach the user.",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// Report handles POST /workspaces/:id/activity — agents self-report activity logs.
func (h *ActivityHandler) Report(c *gin.Context) {
	workspaceID := c.Param("id")
	var body struct {
		ActivityType string      `json:"activity_type" binding:"required"`
		Method       string      `json:"method"`
		Summary      string      `json:"summary"`
		TargetID     string      `json:"target_id"`
		SourceID     string      `json:"source_id"`
		Status       string      `json:"status"`
		ErrorDetail  string      `json:"error_detail"`
		DurationMs   *int        `json:"duration_ms"`
		RequestBody  interface{} `json:"request_body"`
		ResponseBody interface{} `json:"response_body"`
		Metadata     interface{} `json:"metadata"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Validate activity type. memory_write was added per #125 so the
	// commit_memory tool can surface in the Canvas Agent Comms tab —
	// previously its writes were invisible outside the agent_memories
	// table.
	switch body.ActivityType {
	case "a2a_send", "a2a_receive", "task_update", "agent_log", "skill_promotion", "memory_write", "error":
		// valid
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid activity_type, must be one of: a2a_send, a2a_receive, task_update, agent_log, skill_promotion, memory_write, error"})
		return
	}

	status := body.Status
	if status == "" {
		status = "ok"
	}

	// Resolve request/response body — prefer explicit fields, fall back to metadata
	reqBody := body.RequestBody
	if reqBody == nil {
		reqBody = body.Metadata
	}
	// C2 (from #169) — source_id spoof defense. WorkspaceAuth middleware
	// already proves the caller owns :id, but that check doesn't cover the
	// body field. Without this guard, workspace A authenticated for its own
	// /activity endpoint could still set source_id=<workspace B's UUID> in
	// the payload and attribute the log to B. Reject any body where
	// source_id is non-empty AND differs from the authenticated workspace.
	// Empty source_id falls through to the default-to-self branch below.
	sourceID := body.SourceID
	if sourceID != "" && sourceID != workspaceID {
		// #234: sanitize attacker-controlled values before logging.
		// body.SourceID comes from a JSON request, and json.Unmarshal
		// decodes \n escapes into literal newlines — an authenticated
		// workspace could otherwise inject fake log lines. Use %q which
		// emits a Go-quoted string with all control characters escaped.
		log.Printf("security: source_id spoof attempt — authed_workspace=%s body_source_id=%q remote=%q",
			workspaceID, sourceID, c.ClientIP())
		c.JSON(http.StatusForbidden, gin.H{"error": "source_id must match authenticated workspace"})
		return
	}
	if sourceID == "" {
		sourceID = workspaceID
	}

	LogActivity(c.Request.Context(), h.broadcaster, ActivityParams{
		WorkspaceID:  workspaceID,
		ActivityType: body.ActivityType,
		SourceID:     &sourceID,
		TargetID:     nilIfEmpty(body.TargetID),
		Method:       nilIfEmpty(body.Method),
		Summary:      nilIfEmpty(body.Summary),
		RequestBody:  reqBody,
		ResponseBody: body.ResponseBody,
		DurationMs:   body.DurationMs,
		Status:       status,
		ErrorDetail:  nilIfEmpty(body.ErrorDetail),
	})

	c.JSON(http.StatusOK, gin.H{"status": "logged"})
}

// LogActivity inserts an activity log and optionally broadcasts via WebSocket.
// Takes events.EventEmitter (#1814) so callers passing a stub broadcaster
// in tests no longer need to construct the full *events.Broadcaster.
//
// Errors are logged and swallowed — this is the fire-and-forget contract
// most callers expect. For atomic-with-sibling-writes use LogActivityTx
// and propagate the error.
func LogActivity(ctx context.Context, broadcaster events.EventEmitter, params ActivityParams) {
	hook, err := logActivityExec(ctx, db.DB, broadcaster, params)
	if err != nil {
		log.Printf("LogActivity insert error: %v", err)
		return
	}
	hook()
}

// LogActivityWithResult is the error-returning variant of LogActivity.
// It is identical to LogActivity EXCEPT it returns the INSERT error
// instead of swallowing it, so callers that need to gate downstream
// side effects (e.g. WebSocket broadcasts) on persist-success can
// observe the outcome.
//
// The returned commitHook is the post-commit ACTIVITY_LOGGED
// broadcast function. It is safe to call only when err is nil — on
// err, the hook is nil and the caller MUST NOT invoke it (the
// post-commit broadcast would be a phantom for a row that doesn't
// exist).
//
// Use case (core#2697, CR2 #11302): persistUserMessageAtIngest
// previously used LogActivity() and then unconditionally broadcast
// a USER_MESSAGE event. LogActivity's "best-effort" contract
// swallowed INSERT errors, so the USER_MESSAGE broadcast could
// fire even when the activity_logs row was missing — a phantom
// (every other device would render the bubble but the row would
// not be in chat-history on reload). LogActivityWithResult lets
// the caller gate the broadcast on actual persist-success.
func LogActivityWithResult(ctx context.Context, broadcaster events.EventEmitter, params ActivityParams) (commitHook func(), err error) {
	return logActivityExec(ctx, db.DB, broadcaster, params)
}

// LogActivityTx inserts the activity row inside the caller-provided tx
// and returns a commitHook that fires the post-commit ACTIVITY_LOGGED
// broadcast. Caller MUST invoke commitHook AFTER tx.Commit() — firing
// it before commit can leak a WebSocket event for a row that ends up
// rolled back, which the canvas's optimistic UI then shows then loses.
//
// Returns an error if the INSERT fails — caller should Rollback. Caller
// is also responsible for tx.BeginTx + tx.Commit/Rollback. Used by
// chat_files uploadPollMode so PutBatchTx + N activity rows commit
// atomically; if any activity row fails, the pending_uploads rows roll
// back too and the client retries the entire multipart upload cleanly.
func LogActivityTx(ctx context.Context, tx *sql.Tx, broadcaster events.EventEmitter, params ActivityParams) (commitHook func(), err error) {
	if tx == nil {
		return nil, errors.New("LogActivityTx: tx is nil")
	}
	return logActivityExec(ctx, tx, broadcaster, params)
}

// activityExecutor is the SQL surface LogActivity[Tx] needs. *sql.Tx
// and *sql.DB both satisfy it, so the same insert path serves the
// fire-and-forget caller (db.DB) and the Tx-aware caller (*sql.Tx).
type activityExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func logActivityExec(ctx context.Context, exec activityExecutor, broadcaster events.EventEmitter, params ActivityParams) (commitHook func(), err error) {
	reqJSON, reqErr := json.Marshal(params.RequestBody)
	if reqErr != nil {
		log.Printf("LogActivity: failed to marshal request_body for %s: %v", params.WorkspaceID, reqErr)
		reqJSON = []byte("null")
	}
	respJSON, respErr := json.Marshal(params.ResponseBody)
	if respErr != nil {
		log.Printf("LogActivity: failed to marshal response_body for %s: %v", params.WorkspaceID, respErr)
		respJSON = []byte("null")
	}

	var reqStr, respStr, traceStr *string
	if params.RequestBody != nil {
		s := string(reqJSON)
		reqStr = &s
	}
	if params.ResponseBody != nil {
		s := string(respJSON)
		respStr = &s
	}
	if len(params.ToolTrace) > 0 {
		s := string(params.ToolTrace)
		traceStr = &s
	}

	// Agent-Liveness RFC, Layer 3 (A2): write-through the activity timestamp
	// onto the workspaces row IN THE SAME STATEMENT as the activity_logs
	// INSERT, via a CTE. Every activity_logs write IS, by definition, the
	// workspace doing something, so this is the single canonical point to
	// stamp last_activity_at — the stall-watchdog sweeper
	// (handlers/stall_watchdog.go) reads it to tell a busy-but-silently-hung
	// workspace from one that's actively producing activity (the case the
	// Redis TTL liveness monitor and the status='failed' watchdog both miss,
	// the one that let JRS sit dead for 2.5h).
	//
	// Folded into ONE statement (CTE) rather than a second ExecContext so it
	// adds NO extra round-trip on this latency-critical path, commits
	// atomically with the activity row in the Tx case, and — being one Exec
	// whose text still contains `INSERT INTO activity_logs` — preserves the
	// existing sqlmock expectations across the codebase. $1 (workspace_id) is
	// reused by the UPDATE; arg list is otherwise unchanged.
	//
	// #2560 (chat UX: persist in-flight exchange): the INSERT now also
	// carries message_id ($13) and uses ON CONFLICT (workspace_id,
	// message_id) DO UPDATE to attach the agent's response_body onto the
	// SAME row that the ingest path (#2560) wrote at receipt — so a
	// mid-turn leave/refresh shows the user message in chat-history
	// (request_body from the ingest row), and the eventual completion
	// stamps response_body + status onto the same row (no duplicate
	// bubble). The conflict target only fires when message_id IS NOT NULL
	// (idx_activity_logs_msg_id is partial); rows without message_id keep
	// the original always-INSERT behavior.
	if _, err := exec.ExecContext(ctx, `
		WITH ins AS (
			INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary, request_body, response_body, tool_trace, duration_ms, status, error_detail, message_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb, $10, $11, $12, $13)
			ON CONFLICT (workspace_id, message_id) WHERE message_id IS NOT NULL
			DO UPDATE SET
				response_body = COALESCE(EXCLUDED.response_body, activity_logs.response_body),
				status        = EXCLUDED.status,
				duration_ms   = COALESCE(EXCLUDED.duration_ms, activity_logs.duration_ms),
				error_detail  = EXCLUDED.error_detail,
				tool_trace    = CASE WHEN EXCLUDED.tool_trace IS NOT NULL THEN EXCLUDED.tool_trace ELSE activity_logs.tool_trace END
		)
		UPDATE workspaces SET last_activity_at = now() WHERE id = $1
	`, params.WorkspaceID, params.ActivityType, params.SourceID, params.TargetID,
		params.Method, params.Summary, reqStr, respStr, traceStr,
		params.DurationMs, params.Status, params.ErrorDetail,
		nilIfEmpty(params.MessageId)); err != nil {
		return nil, err
	}

	// Build the broadcast payload up-front so the post-commit hook is a
	// pure in-memory call — no JSON marshaling between commit and emit
	// where a panic would leak the row without an event.
	var payload map[string]interface{}
	if broadcaster != nil {
		payload = map[string]interface{}{
			"activity_type": params.ActivityType,
			"method":        params.Method,
			"summary":       params.Summary,
			"status":        params.Status,
			"source_id":     params.SourceID,
			"target_id":     params.TargetID,
			"duration_ms":   params.DurationMs,
		}
		if len(params.ToolTrace) > 0 {
			payload["tool_trace"] = json.RawMessage(params.ToolTrace)
		}
		// Include request/response bodies in the live broadcast so the
		// canvas's Agent Comms panel can render the actual task text
		// and reply text immediately, instead of falling back to the
		// "Delegating to <peer>" boilerplate. Without this, the live
		// bubble was useless until a refresh re-fetched the activity
		// row from /workspaces/:id/activity (which DOES return these
		// columns from the DB). The workspace's report_activity helper
		// caps each side at sensible sizes (4096 chars for error_detail,
		// 256 for summary; request/response are bounded by the
		// runtime's own caps — typical delegate_task payload is a few
		// hundred chars to a few KB). json.RawMessage avoids a
		// re-marshal round-trip; reqJSON/respJSON were already encoded
		// for the DB insert above.
		if reqStr != nil {
			payload["request_body"] = json.RawMessage(reqJSON)
		}
		if respStr != nil {
			payload["response_body"] = json.RawMessage(respJSON)
		}
		// internal#212 — surface the secret-safe failure reason on the
		// live broadcast so the canvas chat-tab error banner can show
		// the user *why* (provider HTTP status, error code, the
		// provider's own human message) instead of the opaque
		// "Agent error (Exception) — see workspace logs for details."
		// hardcoded fallback. Omitted when nil so the canvas's "has
		// actionable reason" guard doesn't trip on empty-string keys.
		if params.ErrorDetail != nil && *params.ErrorDetail != "" {
			payload["error_detail"] = sanitizeErrorDetailForBroadcast(*params.ErrorDetail)
		}
		if params.MessageId != "" {
			payload["message_id"] = params.MessageId
		}
	}

	return func() {
		if broadcaster != nil {
			broadcaster.BroadcastOnly(params.WorkspaceID, string(events.EventActivityLogged), payload)
		}
	}, nil
}

type ActivityParams struct {
	WorkspaceID  string
	ActivityType string // a2a_send, a2a_receive, task_update, agent_log, skill_promotion, error
	SourceID     *string
	TargetID     *string
	Method       *string
	Summary      *string
	RequestBody  interface{}
	ResponseBody interface{}
	ToolTrace    json.RawMessage // tools/commands the agent actually invoked
	DurationMs   *int
	Status       string // ok, error, timeout
	// MessageId, when non-empty, is persisted to activity_logs.message_id
	// and is the conflict key for the partial unique index
	// (idx_activity_logs_msg_id). The ingest path (#2560) sets this so a
	// mid-turn leave/refresh shows the user message in chat-history
	// hydration; the completion path (logA2ASuccess / logA2AReceiveQueued)
	// also sets it and uses ON CONFLICT (workspace_id, message_id) DO
	// UPDATE to stamp response_body onto the same row instead of inserting
	// a duplicate. Empty for non-per-message activity (task_update, etc.).
	MessageId string
	ErrorDetail  *string
}
