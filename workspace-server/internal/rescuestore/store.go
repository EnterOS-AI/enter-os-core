// Package rescuestore is the queryable persistence layer for rescue
// bundles (RFC internal#742 Part 3). It is the DB side of the read-path
// decision: because internal/audit (Part 2's ship transport) is
// Loki-only and tenants hold no obs read creds, the redacted bundle is
// ALSO written here on capture so GET /workspaces/:id/rescue can serve
// the latest one with a plain Postgres read.
//
// The package owns both the write (Persist, wired into
// rescue.PersistBundle at boot) and the read (GetLatest, used by the
// handler). It depends on internal/db and internal/rescue (for the
// Bundle/Section types); it is imported by handlers, never by the leaf
// internal/rescue or by registry — so no import cycle.
package rescuestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescue"
)

// maxSectionBytes bounds a single persisted section's content so a
// pathological capture (e.g. a multi-megabyte container log) can't bloat
// the row or the read response. Capture already tails to ~200 lines per
// section, so this is a backstop, not the primary limit. Truncated
// content is suffixed with a marker so a reader knows it was clipped.
const maxSectionBytes = 64 * 1024 // 64 KiB per section

// truncationMarker is appended to any section clipped at maxSectionBytes.
const truncationMarker = "\n…(rescue: section truncated at 64KiB)"

// StoredBundle is a persisted bundle plus its capture timestamp (the DB
// assigns captured_at on write). The handler maps this to the read
// response shape.
type StoredBundle struct {
	Bundle     rescue.Bundle
	CapturedAt time.Time
}

// Store is the read/write surface the handler and the capture wiring
// depend on. An interface so the handler test can fake it without a
// sqlmock; the production implementation is Postgres.
type Store interface {
	// Persist writes one bundle row (captured_at = now()).
	Persist(ctx context.Context, b rescue.Bundle) error
	// GetLatest returns the most recent bundle for workspaceID. When
	// orgID is non-empty the row must also match org_id (cross-org
	// defense-in-depth behind TenantGuard). Returns (nil, nil) — NOT an
	// error — when no bundle exists, so the handler can 404 cleanly.
	GetLatest(ctx context.Context, workspaceID, orgID string) (*StoredBundle, error)
}

// Postgres is the production Store backed by the rescue_bundles table.
type Postgres struct{ db *sql.DB }

// NewPostgres builds a Postgres-backed store over the given handle.
func NewPostgres(db *sql.DB) *Postgres { return &Postgres{db: db} }

// Persist writes the bundle as one row. Sections are stored as JSONB.
// Each section's content is clamped to maxSectionBytes before write.
func (p *Postgres) Persist(ctx context.Context, b rescue.Bundle) error {
	if p.db == nil {
		return fmt.Errorf("rescuestore: nil db")
	}
	clamped := clampSections(b.Sections)
	payload, err := json.Marshal(clamped)
	if err != nil {
		return fmt.Errorf("rescuestore: marshal sections: %w", err)
	}
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO rescue_bundles (workspace_id, org_id, instance_id, reason, sections)
		 VALUES ($1, $2, $3, $4, $5::jsonb)`,
		b.WorkspaceID, b.OrgID, b.InstanceID, b.Reason, string(payload),
	)
	if err != nil {
		return fmt.Errorf("rescuestore: insert: %w", err)
	}
	return nil
}

// GetLatest returns the newest bundle for workspaceID, org-scoped. The
// (workspace_id, captured_at DESC, id DESC) index serves this directly.
// sql.ErrNoRows maps to (nil, nil) so the handler 404s.
func (p *Postgres) GetLatest(ctx context.Context, workspaceID, orgID string) (*StoredBundle, error) {
	if p.db == nil {
		return nil, fmt.Errorf("rescuestore: nil db")
	}
	if orgID == "" {
		return nil, fmt.Errorf("rescuestore: org_id required")
	}

	var (
		instanceID  string
		reason      string
		capturedAt  time.Time
		sectionsRaw []byte
	)
	err := p.db.QueryRowContext(ctx,
		`SELECT instance_id, reason, captured_at, sections
		   FROM rescue_bundles
		  WHERE workspace_id = $1
		    AND org_id = $2
		  ORDER BY captured_at DESC, id DESC
		  LIMIT 1`,
		workspaceID, orgID,
	).Scan(&instanceID, &reason, &capturedAt, &sectionsRaw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rescuestore: query latest: %w", err)
	}

	var sections []rescue.Section
	if len(sectionsRaw) > 0 {
		if err := json.Unmarshal(sectionsRaw, &sections); err != nil {
			return nil, fmt.Errorf("rescuestore: unmarshal sections: %w", err)
		}
	}

	return &StoredBundle{
		Bundle: rescue.Bundle{
			WorkspaceID: workspaceID,
			OrgID:       orgID,
			InstanceID:  instanceID,
			Reason:      reason,
			Sections:    sections,
		},
		CapturedAt: capturedAt,
	}, nil
}

// clampSections returns a copy with each section's content clamped to
// maxSectionBytes. Clamps on a rune boundary so the marker doesn't split
// a multibyte sequence — the content is a forensic blob, never parsed.
func clampSections(in []rescue.Section) []rescue.Section {
	out := make([]rescue.Section, len(in))
	for i, s := range in {
		if len(s.Content) > maxSectionBytes {
			b := []byte(s.Content[:maxSectionBytes])
			// Back off to a valid utf-8 boundary (at most 3 bytes).
			for len(b) > 0 && b[len(b)-1]&0xC0 == 0x80 {
				b = b[:len(b)-1]
			}
			s.Content = string(b) + truncationMarker
		}
		out[i] = s
	}
	return out
}
