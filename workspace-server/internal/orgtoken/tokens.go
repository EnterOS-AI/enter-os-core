// Package orgtoken — organization-scoped API tokens.
//
// These are full-admin bearer tokens for the tenant platform. One
// token authorizes every admin-gated endpoint on the tenant (all
// workspaces, all org settings, all bundles + templates, all
// secrets). Designed for beta integrations and CLI usage where
// session cookies aren't available.
//
// Mirrors internal/wsauth for plaintext/hash handling + UI display
// format so tooling that understands one format works for the other.
// Intentionally does NOT bind to a workspace — the whole point is
// org-wide scope.
//
// Forward path (post-beta): split into roles (admin, editor, reader)
// + per-workspace scoping. For now every token is full-admin and
// the only authorization is "does it match a live row".
package orgtoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

const (
	// 256 bits of entropy, base64url encoded. Same as wsauth so
	// prefix-based log correlation uses the same leading character
	// set.
	tokenPayloadBytes = 32
	// First 8 chars shown in UI for revoke/audit UX. Reveals nothing
	// crackable on its own (6 bits × 8 = 48 bits of prefix space —
	// good enough to disambiguate, nowhere near guessable).
	tokenPrefixLen = 8

	// listMax caps the number of rows List returns. Realistic org-
	// admin UIs show on the order of 10. 500 is enough headroom that
	// no legitimate flow hits it, low enough that a mint-storm can't
	// force a big allocation on every list render.
	listMax = 500

	// defaultMintCeiling is the per-org cap on live org API tokens.
	// Override with ORG_TOKEN_MINT_CEILING env var. A value <= 0
	// disables the ceiling (legacy / self-hosted behaviour).
	defaultMintCeiling = 100
)

// ErrInvalidToken is returned when a presented bearer doesn't match
// a live row. Callers map to HTTP 401 and must NOT distinguish
// "bad bytes" from "revoked" — that would be an enumeration signal
// on which tokens were ever minted.
var ErrInvalidToken = errors.New("invalid or revoked org api token")

// ErrMintCeilingExceeded is returned when an org already has the
// configured maximum number of live org API tokens. Callers map to
// HTTP 429/400.
var ErrMintCeilingExceeded = errors.New("org api token mint ceiling exceeded")

// ErrPastExpiry is returned when IssueWithExpiry is called with an
// expiry time that has already passed. Minting an immediately-invalid
// token is poor UX and almost always a caller bug.
var ErrPastExpiry = errors.New("expires_at is in the past")

// mintCeiling returns the per-org live-token ceiling from env, or
// the default. Cached at process start because it is not expected to
// change without a restart.
var mintCeiling = func() int {
	if v := os.Getenv("ORG_TOKEN_MINT_CEILING"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("orgtoken: ignoring non-numeric ORG_TOKEN_MINT_CEILING=%q", v)
	}
	return defaultMintCeiling
}()

// MintCeiling exposes the effective ceiling for tests.
func MintCeiling() int { return mintCeiling }

// Token is the admin-UI shape. Plaintext is NEVER part of this —
// the only place plaintext exists is the return value of Issue.
type Token struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	Name       string     `json:"name,omitempty"`
	OrgID      string     `json:"org_id,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// Issue mints a fresh token and persists sha256(plaintext) + prefix.
// Returns (plaintext, id, error). Plaintext is returned to the
// caller once and must be handed to the user verbatim — we cannot
// recover it from the database.
//
// name and orgID are both optional (nullable columns). createdBy
// records provenance for audit. orgID is the caller's org workspace
// ID and is used by requireCallerOwnsOrg to enforce org isolation
// on org-scoped routes (#1200 / F1094).
//
// reqCtx carries HTTP-derived metadata for the audit log. Callers
// without an HTTP request (background jobs, tests) may pass the zero
// value.
func Issue(ctx context.Context, db *sql.DB, name, createdBy, orgID string, reqCtx AuditLogRequestContext) (plaintext, id string, err error) {
	return IssueWithExpiry(ctx, db, name, createdBy, orgID, nil, reqCtx)
}

// IssueWithExpiry is like Issue but allows setting an optional expiry
// time. Tokens whose expiry has passed are rejected by Validate.
// expiresAt may be nil for unbounded tokens (legacy behaviour).
func IssueWithExpiry(ctx context.Context, db *sql.DB, name, createdBy, orgID string, expiresAt *time.Time, reqCtx AuditLogRequestContext) (plaintext, id string, err error) {
	// Reject immediately-invalid expiry times at the API boundary.
	if expiresAt != nil && !expiresAt.IsZero() && time.Now().After(*expiresAt) {
		return "", "", ErrPastExpiry
	}

	// Enforce per-org mint ceiling for anchored tokens. Unanchored
	// tokens (orgID == "") are not org-scoped, so the ceiling does
	// not apply to them; they remain limited by the global listMax
	// UI cap. The ceiling counts only currently-valid tokens:
	// non-revoked AND (no expiry or expiry in the future).
	if mintCeiling > 0 && orgID != "" {
		var count int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM org_api_tokens
			WHERE org_id = $1 AND revoked_at IS NULL
			  AND (expires_at IS NULL OR expires_at > now())
		`, nullIfEmpty(orgID)).Scan(&count); err != nil {
			return "", "", fmt.Errorf("orgtoken: count live tokens: %w", err)
		}
		if count >= mintCeiling {
			return "", "", ErrMintCeilingExceeded
		}
	}

	buf := make([]byte, tokenPayloadBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("orgtoken: generate: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	hash := sha256.Sum256([]byte(plaintext))
	prefix := plaintext[:tokenPrefixLen]

	err = db.QueryRowContext(ctx, `
		INSERT INTO org_api_tokens (token_hash, prefix, name, created_by, org_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, hash[:], prefix, nullIfEmpty(name), nullIfEmpty(createdBy), nullIfEmpty(orgID), nullTimeIfZero(expiresAt)).Scan(&id)
	if err != nil {
		return "", "", fmt.Errorf("orgtoken: persist: %w", err)
	}
	LogMint(ctx, db, id, createdBy, orgID, reqCtx, map[string]any{"name": name, "prefix": prefix})
	return plaintext, id, nil
}

// Validate looks up a presented bearer, returns ErrInvalidToken on
// any mismatch (bad bytes, revoked, deleted). On success, updates
// last_used_at best-effort (the hot path — failure to update doesn't
// fail the request) and returns the token id + display prefix + org_id
// for audit logging and org isolation.
//
// Returning the prefix alongside the id lets callers produce audit
// strings that match what users see in the UI (the plaintext prefix,
// not the UUID). Keeps the "who did what" trail visually
// correlatable to the revoke button in the token list.
//
// The org_id is the workspace UUID of the org that owns this token.
// May be empty for pre-migration tokens minted before #1212. Callers
// that need org isolation should use requireCallerOwnsOrg (which does
// a second lookup) rather than trusting an empty org_id here — this
// avoids a breaking change to the Validate interface while still
// populating the Gin context for callers that don't need it.
//
// reqCtx carries HTTP-derived metadata for the audit log. actor is an
// optional caller provenance label; when empty and the token is
// invalid, the actor is derived from the presented token prefix so
// audit queries can still correlate attempts.
//
// auditInvalid controls whether a failed validation (sql.ErrNoRows)
// writes a validate_fail audit row. Callers that use Validate as one
// probe in a multi-credential fallback chain — WorkspaceAuth,
// AdminAuth, discovery, A2A proxy — MUST pass false so routine
// workspace-token/admin-token misses are not logged as org-token
// lifecycle failures. Callers that genuinely expect an org token and
// want to audit rejected attempts can pass true.
func Validate(ctx context.Context, db *sql.DB, plaintext string, reqCtx AuditLogRequestContext, actor string, auditInvalid bool) (id, prefix, orgID string, err error) {
	if plaintext == "" {
		return "", "", "", ErrInvalidToken
	}
	hash := sha256.Sum256([]byte(plaintext))
	var orgIDNull sql.NullString
	var expiresAt sql.NullTime
	queryErr := db.QueryRowContext(ctx, `
		SELECT id, prefix, org_id, expires_at FROM org_api_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, hash[:]).Scan(&id, &prefix, &orgIDNull, &expiresAt)
	if queryErr != nil {
		// Collapse all failure shapes into ErrInvalidToken so the
		// caller can't accidentally leak "row exists but revoked" vs
		// "row never existed" via response shape.
		if errors.Is(queryErr, sql.ErrNoRows) {
			if auditInvalid {
				failActor := actor
				if failActor == "" {
					if len(plaintext) >= tokenPrefixLen {
						failActor = "org-token:" + plaintext[:tokenPrefixLen]
					} else {
						failActor = "org-token:<short>"
					}
				}
				LogValidateFail(ctx, db, failActor, "", reqCtx, map[string]any{"prefix": plaintext[:min(len(plaintext), tokenPrefixLen)]})
			}
		}
		return "", "", "", ErrInvalidToken
	}
	if expiresAt.Valid && time.Now().After(expiresAt.Time) {
		return "", "", "", ErrInvalidToken
	}
	if orgIDNull.Valid {
		orgID = orgIDNull.String
	}
	// Best-effort last_used_at bump. Failure here is acceptable — the
	// request is already authenticated; we don't want a transient DB
	// blip to flip a 200 into a 500.
	if _, err := db.ExecContext(ctx,
		`UPDATE org_api_tokens SET last_used_at = now() WHERE id = $1`, id); err != nil {
		log.Printf("orgtoken: last_used_at bump failed for %s: %v", id, err)
	}
	return id, prefix, orgID, nil
}

// List returns live (non-revoked) tokens newest-first. Safe to
// expose to the admin UI — no hash, no plaintext, only prefix.
//
// Capped at listMax rows. A UI page with more than that is a
// symptom of abuse or a bug — the hard cap prevents one runaway
// minting loop from O(N) pageloads in the admin UI.
func List(ctx context.Context, db *sql.DB) ([]Token, error) {
	// org_id is a UUID column — COALESCE must cast to text first,
	// otherwise Postgres rejects the empty-string literal with
	// "pq: invalid input syntax for type uuid: ''". sqlmock doesn't
	// exercise pq type coercion, so this bug only surfaces against
	// a real Postgres (prod).
	rows, err := db.QueryContext(ctx, `
		SELECT id, prefix, COALESCE(name,''), COALESCE(org_id::text,''),
		       COALESCE(created_by,''), created_at, last_used_at, expires_at
		FROM org_api_tokens
		WHERE revoked_at IS NULL
		ORDER BY created_at DESC
		LIMIT $1
	`, listMax)
	if err != nil {
		return nil, fmt.Errorf("orgtoken: list: %w", err)
	}
	defer rows.Close()

	out := []Token{}
	for rows.Next() {
		var t Token
		var lastUsed sql.NullTime
		var expiresAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.Prefix, &t.Name, &t.OrgID, &t.CreatedBy,
			&t.CreatedAt, &lastUsed, &expiresAt); err != nil {
			return nil, fmt.Errorf("orgtoken: scan: %w", err)
		}
		if lastUsed.Valid {
			v := lastUsed.Time
			t.LastUsedAt = &v
		}
		if expiresAt.Valid {
			v := expiresAt.Time
			t.ExpiresAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke flips revoked_at on the row with id. Idempotent — revoking
// an already-revoked token returns (false, nil). Returns (true, nil)
// when a row transitioned from live → revoked; (false, nil) when
// already revoked or absent. The caller maps (false, nil) to 404 so
// ops tooling can distinguish "already dealt with" from "silently
// worked".
//
// reqCtx carries HTTP-derived metadata for the audit log. actor is the
// caller provenance label (e.g. "admin-token", "session:<hash>",
// "org-token:<prefix>").
func Revoke(ctx context.Context, db *sql.DB, id string, reqCtx AuditLogRequestContext, actor string) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE org_api_tokens
		SET revoked_at = now()
		WHERE id = $1 AND revoked_at IS NULL
	`, id)
	if err != nil {
		return false, fmt.Errorf("orgtoken: revoke: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("orgtoken: revoke RowsAffected: %w", err)
	}
	if n > 0 {
		LogRevoke(ctx, db, id, actor, "", reqCtx, nil)
	}
	return n > 0, nil
}

// HasAnyLive returns true when at least one non-revoked token
// exists. Used by the middleware to decide whether to check the
// org-token tier at all — skipping a DB round-trip per request when
// nobody has minted any yet.
func HasAnyLive(ctx context.Context, db *sql.DB) (bool, error) {
	var ok bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM org_api_tokens WHERE revoked_at IS NULL)
	`).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("orgtoken: has-any-live: %w", err)
	}
	return ok, nil
}

// OrgIDByTokenID looks up the org workspace ID for a token.
// Used by requireCallerOwnsOrg to enforce org isolation on org-scoped
// routes (#1200 / F1094). Returns ("", nil) when the token has no org_id
// set (e.g. pre-migration tokens, ADMIN_TOKEN bootstrap tokens) — the
// caller treats this as "deny by default".
func OrgIDByTokenID(ctx context.Context, db *sql.DB, tokenID string) (string, error) {
	var orgID sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT org_id FROM org_api_tokens WHERE id = $1`, tokenID,
	).Scan(&orgID)
	if err != nil {
		return "", fmt.Errorf("orgtoken: org_id lookup: %w", err)
	}
	if !orgID.Valid || orgID.String == "" {
		return "", nil // unanchored token — deny by default
	}
	return orgID.String, nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullTimeIfZero(t *time.Time) interface{} {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}
