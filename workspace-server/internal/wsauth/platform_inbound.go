// Package wsauth — platform→workspace inbound secret (per-workspace bearer
// the platform presents when calling INTO a workspace's HTTP server).
//
// Asymmetric to IssueToken in this same package, which mints the
// **outbound** bearer (workspace → platform). See the per-function
// comments and migration 044 for the full rationale on why the two
// roles use distinct secrets stored in different shapes.
//
//	IssueToken                   IssuePlatformInboundSecret
//	──────────                   ───────────────────────────
//	workspace_auth_tokens row    workspaces.platform_inbound_secret column
//	plaintext returned once,     plaintext stored AND returned (the platform
//	  hash stored                  must read it back on every forward call)
//	bcrypt-shape compare          string-equality compare on workspace side
package wsauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// platformInboundSecretBytes is the raw-random length before base64url
// encoding. Same 256-bit entropy floor as workspace_auth_tokens; the
// shape lets a future rotation script substitute one for the other
// without changing the bearer-presenter on either side.
const platformInboundSecretBytes = 32

// ErrNoInboundSecret is returned by ReadPlatformInboundSecret when the
// row exists but the column is NULL or empty. Callers MUST treat this
// as a structural failure (the row was created by a path that didn't
// mint a secret, or the migration ran but the row predates it) and
// surface the nil bearer to the user as a 500-class error rather than
// silently sending an unauthenticated request to the workspace.
var ErrNoInboundSecret = errors.New("wsauth: workspace has no platform_inbound_secret on file")

// inboundSecretCacheTTL is how long a cached secret survives in the
// process-local cache before the next read forces a fresh DB lookup.
// Picked large enough that the heartbeat hot path (60s/workspace,
// task #189 motivation) hits the cache for ~5 reads in a row before
// re-confirming, but short enough that an out-of-band rotation
// (operator running `UPDATE workspaces SET platform_inbound_secret=...`
// directly) propagates within minutes — not requiring a redeploy.
const inboundSecretCacheTTL = 5 * time.Minute

// inboundSecretCacheEntry is the per-workspace value stored in
// inboundSecretCache. Tracks the secret + when it was loaded so the
// reader can decide whether to trust it or refresh.
type inboundSecretCacheEntry struct {
	secret    string
	expiresAt time.Time
}

// inboundSecretCache caches per-workspace platform_inbound_secret values
// to absorb the heartbeat read storm. Heartbeats fire every 60s per
// workspace and were doing one DB SELECT each; for an N-workspace fleet
// that's N reads/minute purely to redeliver the same value. Cache hits
// short-circuit the DB call.
//
// Cache invariants:
//   - Read-through: cache miss → DB SELECT → populate → return.
//   - Write-through: every IssuePlatformInboundSecret call refreshes
//     the cache with the new value before returning, so the in-process
//     mint path never sees a stale read of the value it just wrote.
//   - TTL eviction: stale entries get re-validated against the DB after
//     inboundSecretCacheTTL so manual / out-of-band rotations propagate
//     bounded-quickly.
//   - Memory: bounded by the active workspace fleet. Deleted workspaces
//     leave dead entries until process restart — acceptable given the
//     small per-entry footprint (<200 bytes) and that workspace deletion
//     is operator-rare on the platform.
//
// Single-replica process safety: workspace-server runs as a single
// Railway service today, so the cache is process-local and consistent
// with itself. If the deployment ever fans out across replicas, an
// operator-rotation propagates per-replica TTL-bounded — there is no
// shared write log.
//
// Cleared by ResetInboundSecretCacheForTesting() in tests.
var inboundSecretCache sync.Map // key: workspaceID (string), value: *inboundSecretCacheEntry

// inboundSecretCacheNow is the time source used by the cache. Tests
// override it via SetInboundSecretCacheNowForTesting to drive TTL
// expiry deterministically without time.Sleep.
var inboundSecretCacheNow = time.Now

// IssuePlatformInboundSecret generates a fresh per-workspace shared
// secret, persists the plaintext into workspaces.platform_inbound_secret,
// and returns the plaintext so the provisioner can write it into
// /configs/.platform_inbound_secret on the workspace's volume.
//
// The plaintext is INTENTIONALLY retained on the platform side: every
// platform→workspace forward call reads it back to put in the
// Authorization header. Hashing would force a re-mint on every call
// (defeating the purpose of the shared secret) or a separate plaintext
// store (defeating the simplicity). Encryption-at-rest is delegated to
// the underlying Postgres volume — application-layer encryption via
// SECRETS_ENCRYPTION_KEY is a defense-in-depth follow-up.
func IssuePlatformInboundSecret(ctx context.Context, db *sql.DB, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", fmt.Errorf("wsauth: workspaceID required")
	}
	buf := make([]byte, platformInboundSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("wsauth: generate platform_inbound_secret: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(buf)

	_, err := db.ExecContext(ctx, `
		UPDATE workspaces SET platform_inbound_secret = $1 WHERE id = $2
	`, plaintext, workspaceID)
	if err != nil {
		return "", fmt.Errorf("wsauth: persist platform_inbound_secret: %w", err)
	}
	// Write-through cache update so an immediate ReadPlatformInboundSecret
	// from the same process (e.g. registry handler returning the freshly
	// minted secret to the workspace in the heartbeat response) doesn't
	// see a stale or empty value via a parallel cache hit. Same expiry
	// rules as a regular read population.
	inboundSecretCache.Store(workspaceID, &inboundSecretCacheEntry{
		secret:    plaintext,
		expiresAt: inboundSecretCacheNow().Add(inboundSecretCacheTTL),
	})
	return plaintext, nil
}

// ReadPlatformInboundSecret returns the plaintext secret for a workspace
// or ErrNoInboundSecret if the column is NULL/empty. Used by platform-
// side handlers that forward HTTPS calls into the workspace.
//
// Callers MUST handle ErrNoInboundSecret explicitly; never default to
// an empty bearer. An empty Authorization header would let any caller
// through if the workspace's auth is fail-open (it is not today, but
// defense-in-depth keeps it that way).
func ReadPlatformInboundSecret(ctx context.Context, db *sql.DB, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", fmt.Errorf("wsauth: workspaceID required")
	}
	// Cache fast path. Heartbeats fire every 60s per workspace and were
	// the dominant caller before #189. The TTL keeps cached entries
	// fresh enough that operator-side rotations propagate within
	// minutes; see inboundSecretCacheTTL.
	if v, ok := inboundSecretCache.Load(workspaceID); ok {
		if entry, ok := v.(*inboundSecretCacheEntry); ok {
			if inboundSecretCacheNow().Before(entry.expiresAt) {
				return entry.secret, nil
			}
		}
	}
	var secret sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT platform_inbound_secret FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&secret)
	if err == sql.ErrNoRows {
		// Don't cache absence — the row may appear momentarily after
		// provision_workspace's INSERT lands, and the lazy-heal path
		// is the recovery contract for the column-NULL case (see
		// readOrLazyHealInboundSecret in workspace_provision_shared.go).
		return "", ErrNoInboundSecret
	}
	if err != nil {
		return "", fmt.Errorf("wsauth: read platform_inbound_secret: %w", err)
	}
	if !secret.Valid || secret.String == "" {
		return "", ErrNoInboundSecret
	}
	// Read-through cache population on success.
	inboundSecretCache.Store(workspaceID, &inboundSecretCacheEntry{
		secret:    secret.String,
		expiresAt: inboundSecretCacheNow().Add(inboundSecretCacheTTL),
	})
	return secret.String, nil
}

// ResetInboundSecretCacheForTesting clears the process-local cache.
// Tests that exercise rotation or DB-side mutation of the secret column
// MUST call this between scenarios to keep an earlier entry from
// shadowing a fresh DB read.
//
// Exported (`...ForTesting` suffix) so cross-package tests in the
// handlers/ tree can call it directly without circular imports.
func ResetInboundSecretCacheForTesting() {
	inboundSecretCache.Range(func(k, _ any) bool {
		inboundSecretCache.Delete(k)
		return true
	})
}

// SetInboundSecretCacheNowForTesting overrides the package-level time
// source for cache TTL calculations. Tests use this to advance past
// the TTL deterministically rather than waiting on the wall clock.
// Returns a restore function that the caller MUST defer to avoid
// leaking the override into other tests.
func SetInboundSecretCacheNowForTesting(now func() time.Time) func() {
	prev := inboundSecretCacheNow
	inboundSecretCacheNow = now
	return func() { inboundSecretCacheNow = prev }
}
