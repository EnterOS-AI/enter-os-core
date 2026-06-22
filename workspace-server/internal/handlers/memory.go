package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// MemoryEntry is what GET returns. The Version field enables optimistic-
// concurrency on subsequent writes — callers echo it back as
// if_match_version to detect concurrent modification.
type MemoryEntry struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Version   int64           `json:"version"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type MemoryHandler struct{}

func NewMemoryHandler() *MemoryHandler { return &MemoryHandler{} }

// List handles GET /workspaces/:id/memory
func (h *MemoryHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")

	rows, err := db.DB.QueryContext(c.Request.Context(), `
		SELECT key, value, version, expires_at, updated_at
		FROM workspace_memory
		WHERE workspace_id = $1 AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY key
	`, workspaceID)
	if err != nil {
		log.Printf("Memory list error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	entries := make([]MemoryEntry, 0)
	for rows.Next() {
		var entry MemoryEntry
		var value []byte
		if err := rows.Scan(&entry.Key, &value, &entry.Version, &entry.ExpiresAt, &entry.UpdatedAt); err != nil {
			log.Printf("Memory list scan error: %v", err)
			continue
		}
		entry.Value = json.RawMessage(value)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Memory list iteration error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}

	c.JSON(http.StatusOK, entries)
}

// Get handles GET /workspaces/:id/memory/:key
func (h *MemoryHandler) Get(c *gin.Context) {
	workspaceID := c.Param("id")
	key := c.Param("key")

	var entry MemoryEntry
	var value []byte
	err := db.DB.QueryRowContext(c.Request.Context(), `
		SELECT key, value, version, expires_at, updated_at
		FROM workspace_memory
		WHERE workspace_id = $1 AND key = $2 AND (expires_at IS NULL OR expires_at > NOW())
	`, workspaceID, key).Scan(&entry.Key, &value, &entry.Version, &entry.ExpiresAt, &entry.UpdatedAt)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
		return
	}
	if err != nil {
		log.Printf("Memory get error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	entry.Value = json.RawMessage(value)
	c.JSON(http.StatusOK, entry)
}

// Set handles POST /workspaces/:id/memory with optimistic-locking support.
//
// Back-compat (no if_match_version): behaves exactly as before — last-
// write-wins upsert. Every existing agent tool keeps working unmodified.
//
// Optimistic-locking (if_match_version set): the write is conditional on
// the current row version. On conflict (concurrent writer incremented
// version since the caller read), returns 409 with the latest version so
// the caller can re-read + retry. This closes the silent-overwrite hole
// for orchestrators running concurrent delegation-ledger / task-queue
// state in memory.
//
// Expected call pattern for conflict-free reads:
//
//  1. GET /memory/:key → {value, version: V}
//  2. modify value
//  3. POST /memory with {key, value, if_match_version: V}
//  4. on 200 → done; on 409 → goto 1.
func (h *MemoryHandler) Set(c *gin.Context) {
	workspaceID := c.Param("id")

	var body struct {
		Key        string          `json:"key"`
		Value      json.RawMessage `json:"value"`
		TTLSeconds *int            `json:"ttl_seconds"`
		// IfMatchVersion, when non-nil, gates the write on the row's
		// current version matching this value. Mismatch → 409 + latest
		// version in the response so the caller can retry cleanly.
		IfMatchVersion *int64 `json:"if_match_version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if body.Key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}

	var expiresAt *time.Time
	if body.TTLSeconds != nil {
		t := time.Now().Add(time.Duration(*body.TTLSeconds) * time.Second)
		expiresAt = &t
	}

	// Path A — no version guard: unchanged last-write-wins upsert.
	if body.IfMatchVersion == nil {
		var newVersion int64
		err := db.DB.QueryRowContext(c.Request.Context(), `
			INSERT INTO workspace_memory(id, workspace_id, key, value, expires_at, updated_at, version)
			VALUES(gen_random_uuid(), $1, $2, $3::jsonb, $4, NOW(), 1)
			ON CONFLICT(workspace_id, key) DO UPDATE
			SET value = EXCLUDED.value,
			    expires_at = EXCLUDED.expires_at,
			    updated_at = NOW(),
			    version = workspace_memory.version + 1
			RETURNING version
		`, workspaceID, body.Key, string(body.Value), expiresAt).Scan(&newVersion)
		if err != nil {
			log.Printf("Memory set error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set memory"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "key": body.Key, "version": newVersion})
		return
	}

	// Path B — optimistic-locking guard.
	//
	// Strategy:
	//   1. Try to UPDATE the existing row with version check. RETURNING
	//      the new version tells us whether the guard matched.
	//   2. If the UPDATE affected zero rows, the row either doesn't exist
	//      (treat if_match_version=0 as "must not exist yet", otherwise
	//      409) or the version didn't match (409).
	//
	// We don't collapse into a single ON CONFLICT because we need the
	// "caller expected version N, current is M" response shape to be
	// accurate — ON CONFLICT DO NOTHING would hide whether it was a
	// version-mismatch or something else.
	expected := *body.IfMatchVersion
	var newVersion int64
	updateErr := db.DB.QueryRowContext(c.Request.Context(), `
		UPDATE workspace_memory
		SET value = $3::jsonb,
		    expires_at = $4,
		    updated_at = NOW(),
		    version = version + 1
		WHERE workspace_id = $1 AND key = $2 AND version = $5
		RETURNING version
	`, workspaceID, body.Key, string(body.Value), expiresAt, expected).Scan(&newVersion)

	if updateErr == sql.ErrNoRows {
		// Either the row doesn't exist yet, or version mismatch. Look
		// up the actual state so the 409 body carries useful context.
		var currentVersion sql.NullInt64
		probeErr := db.DB.QueryRowContext(c.Request.Context(), `
			SELECT version FROM workspace_memory
			WHERE workspace_id = $1 AND key = $2
		`, workspaceID, body.Key).Scan(&currentVersion)

		if probeErr == sql.ErrNoRows {
			// Row absent. Caller with expected=0 means "create only" —
			// honour it. Any other expected is a 409 (tried to update a
			// non-existent key with version assertion).
			if expected == 0 {
				var createdVersion int64
				err := db.DB.QueryRowContext(c.Request.Context(), `
					INSERT INTO workspace_memory(id, workspace_id, key, value, expires_at, updated_at, version)
					VALUES(gen_random_uuid(), $1, $2, $3::jsonb, $4, NOW(), 1)
					RETURNING version
				`, workspaceID, body.Key, string(body.Value), expiresAt).Scan(&createdVersion)
				if err != nil {
					log.Printf("Memory set error (create-only path): %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set memory"})
					return
				}
				c.JSON(http.StatusOK, gin.H{"status": "ok", "key": body.Key, "version": createdVersion})
				return
			}
			c.JSON(http.StatusConflict, gin.H{
				"error":            "if_match_version mismatch: key does not exist",
				"expected_version": expected,
				"current_version":  nil,
			})
			return
		}
		if probeErr != nil {
			log.Printf("Memory set probe error: %v", probeErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to probe current version"})
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":            "if_match_version mismatch",
			"expected_version": expected,
			"current_version":  currentVersion.Int64,
		})
		return
	}
	if updateErr != nil {
		log.Printf("Memory set conditional update error: %v", updateErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set memory"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "key": body.Key, "version": newVersion})
}

// Delete handles DELETE /workspaces/:id/memory/:key
func (h *MemoryHandler) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	key := c.Param("key")

	_, err := db.DB.ExecContext(c.Request.Context(), `
		DELETE FROM workspace_memory WHERE workspace_id = $1 AND key = $2
	`, workspaceID, key)
	if err != nil {
		log.Printf("Memory delete error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}
