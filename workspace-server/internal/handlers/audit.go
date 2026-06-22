package handlers

// AuditHandler implements GET /workspaces/:id/audit.
//
// EU AI Act Annex III compliance endpoint — queries the append-only HMAC-chained
// audit event log for a workspace and optionally verifies the HMAC chain inline.
//
// Route (behind WorkspaceAuth middleware):
//
//	GET /workspaces/:id/audit
//
// Query parameters:
//
//	agent_id   — filter by agent ID
//	session_id — filter by session/conversation ID
//	from       — ISO 8601 / RFC 3339 lower bound on timestamp (inclusive)
//	to         — ISO 8601 / RFC 3339 upper bound on timestamp (exclusive)
//	limit      — max rows returned (default 100, max 500)
//	offset     — pagination offset (default 0)
//
// Response:
//
//	{
//	    "events":      [...],     // slice of audit event rows
//	    "total":       N,         // total matching rows (ignoring limit/offset)
//	    "chain_valid": true|false|null
//	    // null when AUDIT_LEDGER_SALT is not configured on the platform side
//	}
//
// Chain verification
// ------------------
// When AUDIT_LEDGER_SALT is set, the handler re-derives the PBKDF2 key and
// verifies every HMAC in the result set (scoped to the queried agent_id, in
// chronological order).  Returns null when the salt is absent so operators
// know to use the Python CLI instead:
//
//	python -m molecule_audit.verify --agent-id <id>
//
// Environment variables:
//
//	AUDIT_LEDGER_SALT — secret salt for PBKDF2 key derivation (optional;
//	                    chain_valid is null when unset)

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/pbkdf2"
)

// pbkdf2 parameters — must match molecule_audit/ledger.py exactly.
var (
	auditPBKDF2Salt       = []byte("molecule-audit-ledger-v1")
	auditPBKDF2Iterations = 210_000
	auditPBKDF2KeyLen     = 32

	auditKeyOnce sync.Once
	auditHMACKey []byte // nil when AUDIT_LEDGER_SALT is unset
)

// getAuditHMACKey derives (and caches) the 32-byte HMAC key from AUDIT_LEDGER_SALT.
// Returns nil when the env var is not set.
func getAuditHMACKey() []byte {
	auditKeyOnce.Do(func() {
		if salt := os.Getenv("AUDIT_LEDGER_SALT"); salt != "" {
			auditHMACKey = pbkdf2.Key(
				[]byte(salt),
				auditPBKDF2Salt,
				auditPBKDF2Iterations,
				auditPBKDF2KeyLen,
				sha256.New,
			)
		}
	})
	return auditHMACKey
}

// AuditHandler queries the audit_events table.
type AuditHandler struct{}

// NewAuditHandler returns an AuditHandler (stateless — all deps via db package).
func NewAuditHandler() *AuditHandler {
	return &AuditHandler{}
}

// auditEventRow mirrors the audit_events DB columns for JSON serialisation.
type auditEventRow struct {
	ID                 string    `json:"id"`
	Timestamp          time.Time `json:"timestamp"`
	AgentID            string    `json:"agent_id"`
	SessionID          string    `json:"session_id"`
	Operation          string    `json:"operation"`
	InputHash          *string   `json:"input_hash"`
	OutputHash         *string   `json:"output_hash"`
	ModelUsed          *string   `json:"model_used"`
	HumanOversightFlag bool      `json:"human_oversight_flag"`
	RiskFlag           bool      `json:"risk_flag"`
	PrevHMAC           *string   `json:"prev_hmac"`
	HMAC               string    `json:"hmac"`
	WorkspaceID        string    `json:"workspace_id"`
}

// Query handles GET /workspaces/:id/audit.
func (h *AuditHandler) Query(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Parse query parameters ------------------------------------------------
	agentID := c.Query("agent_id")
	sessionID := c.Query("session_id")
	fromStr := c.Query("from")
	toStr := c.Query("to")

	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	// Build parameterized WHERE clause --------------------------------------
	where := "WHERE workspace_id = $1"
	args := []interface{}{workspaceID}
	idx := 2

	if agentID != "" {
		where += fmt.Sprintf(" AND agent_id = $%d", idx)
		args = append(args, agentID)
		idx++
	}
	if sessionID != "" {
		where += fmt.Sprintf(" AND session_id = $%d", idx)
		args = append(args, sessionID)
		idx++
	}
	if fromStr != "" {
		t, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "from must be RFC 3339 (e.g. 2026-04-17T00:00:00Z)"})
			return
		}
		where += fmt.Sprintf(" AND timestamp >= $%d", idx)
		args = append(args, t)
		idx++
	}
	if toStr != "" {
		t, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "to must be RFC 3339 (e.g. 2026-04-17T23:59:59Z)"})
			return
		}
		where += fmt.Sprintf(" AND timestamp < $%d", idx)
		args = append(args, t)
		idx++
	}

	// Count total matching rows (for pagination) ----------------------------
	countQuery := "SELECT COUNT(*) FROM audit_events " + where
	var total int
	if err := db.DB.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		log.Printf("audit: count query failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	// Fetch rows ------------------------------------------------------------
	selectQuery := `SELECT id, timestamp, agent_id, session_id, operation,
		input_hash, output_hash, model_used,
		human_oversight_flag, risk_flag, prev_hmac, hmac, workspace_id
		FROM audit_events ` + where +
		fmt.Sprintf(" ORDER BY timestamp ASC, id ASC LIMIT $%d OFFSET $%d", idx, idx+1)

	rows, err := db.DB.QueryContext(ctx, selectQuery, append(args, limit, offset)...)
	if err != nil {
		log.Printf("audit: query failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	events, err := scanAuditRows(rows)
	if err != nil {
		log.Printf("audit: scan failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "scan failed"})
		return
	}
	if err := rows.Err(); err != nil {
		log.Printf("audit: rows error for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "scan failed"})
		return
	}

	// Chain verification (inline when AUDIT_LEDGER_SALT is set) ------------
	// Paginated views cannot verify chain integrity — earlier events are absent
	// from the result set so any verdict would be misleading. Return null to
	// signal "not computed" rather than false (which would imply tampering).
	var chainValid *bool
	if offset == 0 {
		chainValid = verifyAuditChain(events)
	}

	c.JSON(http.StatusOK, gin.H{
		"events":      events,
		"total":       total,
		"chain_valid": chainValid,
	})
}

// scanAuditRows reads all rows from a *sql.Rows into a slice.
func scanAuditRows(rows *sql.Rows) ([]auditEventRow, error) {
	var result []auditEventRow
	for rows.Next() {
		var ev auditEventRow
		if err := rows.Scan(
			&ev.ID,
			&ev.Timestamp,
			&ev.AgentID,
			&ev.SessionID,
			&ev.Operation,
			&ev.InputHash,
			&ev.OutputHash,
			&ev.ModelUsed,
			&ev.HumanOversightFlag,
			&ev.RiskFlag,
			&ev.PrevHMAC,
			&ev.HMAC,
			&ev.WorkspaceID,
		); err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// verifyAuditChain verifies the HMAC chain across the supplied events.
//
// Returns nil when AUDIT_LEDGER_SALT is not configured (chain_valid: null in
// the response — use the Python CLI to verify in that case).
// Returns a pointer to true/false otherwise.
func verifyAuditChain(events []auditEventRow) *bool {
	key := getAuditHMACKey()
	if key == nil {
		return nil // AUDIT_LEDGER_SALT not set — cannot verify
	}

	// Group events by agent_id and verify each agent's chain independently.
	type chainState struct {
		prevHMAC *string
	}
	chains := map[string]*chainState{}

	for i := range events {
		ev := &events[i]
		state, ok := chains[ev.AgentID]
		if !ok {
			state = &chainState{}
			chains[ev.AgentID] = state
		}

		// Recompute the expected HMAC.
		expected := computeAuditHMAC(key, ev)
		if !hmac.Equal([]byte(ev.HMAC), []byte(expected)) {
			// Truncate for logging only after confirming the slice is safe.
			storedPrefix := ev.HMAC
			computedPrefix := expected
			if len(storedPrefix) > 12 {
				storedPrefix = storedPrefix[:12]
			}
			if len(computedPrefix) > 12 {
				computedPrefix = computedPrefix[:12]
			}
			log.Printf(
				"audit: HMAC mismatch at event %s (agent=%s): stored=%q computed=%q",
				ev.ID, ev.AgentID, storedPrefix, computedPrefix,
			)
			f := false
			return &f
		}

		// Check chain linkage (constant-time to prevent HMAC oracle timing attacks).
		prevMatches := (state.prevHMAC == nil && ev.PrevHMAC == nil) ||
			(state.prevHMAC != nil && ev.PrevHMAC != nil && hmac.Equal([]byte(*state.prevHMAC), []byte(*ev.PrevHMAC)))
		if !prevMatches {
			log.Printf(
				"audit: chain break at event %s (agent=%s)",
				ev.ID, ev.AgentID,
			)
			f := false
			return &f
		}

		h := ev.HMAC
		state.prevHMAC = &h
	}

	t := true
	return &t
}

// computeAuditHMAC replicates Python's _compute_event_hmac() for a single row.
//
// Canonical JSON rules (must match ledger.py exactly):
//   - All fields except "hmac", serialised as a JSON object
//   - Keys sorted alphabetically (encoding/json.Marshal on map does this)
//   - Compact separators (no spaces)
//   - Timestamp as RFC-3339 seconds-precision with Z suffix
//   - Null values as JSON null (Go *string nil → null)
func computeAuditHMAC(key []byte, ev *auditEventRow) string {
	// Build the canonical map — keys must sort alphabetically to match Python.
	canonical := map[string]interface{}{
		"agent_id":             ev.AgentID,
		"human_oversight_flag": ev.HumanOversightFlag,
		"id":                   ev.ID,
		"input_hash":           nilOrString(ev.InputHash),
		"model_used":           nilOrString(ev.ModelUsed),
		"operation":            ev.Operation,
		"output_hash":          nilOrString(ev.OutputHash),
		"prev_hmac":            nilOrString(ev.PrevHMAC),
		"risk_flag":            ev.RiskFlag,
		"session_id":           ev.SessionID,
		"timestamp":            ev.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}

	payload, marshalErr := json.Marshal(canonical) // compact, sorted keys
	if marshalErr != nil {
		log.Printf("auditChainHash: json.Marshal canonical failed: %v", marshalErr)
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// nilOrString converts a *string to interface{} where nil → nil (JSON null).
func nilOrString(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}
