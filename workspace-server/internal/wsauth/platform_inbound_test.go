package wsauth

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// IssuePlatformInboundSecret persists the plaintext (not a hash) so the
// platform can read it back on every forward call. This is the primary
// shape difference vs. IssueToken; pin it explicitly.
func TestIssuePlatformInboundSecret_PersistsPlaintext(t *testing.T) {
	db, mock := setupMock(t)

	// Capture the plaintext written by the UPDATE so we can verify the
	// returned value matches what was stored. AnyArg captures, then we
	// pull the captured value via a separate ExpectExec... wait, sqlmock
	// doesn't return the captured args. Use a regex-style match on the
	// SQL and trust that the function persisted SOMETHING — then assert
	// the returned plaintext shape (length + alphabet) matches the
	// generator. The end-to-end "platform reads the same value back" is
	// covered by ReadPlatformInboundSecret tests.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), "ws-abc").
		WillReturnResult(sqlmock.NewResult(1, 1))

	plaintext, err := IssuePlatformInboundSecret(context.Background(), db, "ws-abc")
	if err != nil {
		t.Fatalf("IssuePlatformInboundSecret: %v", err)
	}
	if len(plaintext) < 40 {
		t.Errorf("plaintext too short for 256-bit entropy: len=%d", len(plaintext))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(plaintext) {
		t.Errorf("plaintext contains non-urlsafe chars: %q", plaintext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Re-issue rotates: the same workspace gets a different secret on the
// second call. Without rotation, a leaked secret would be permanent.
func TestIssuePlatformInboundSecret_RotatesOnReissue(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret`).WillReturnResult(sqlmock.NewResult(1, 1))

	a, _ := IssuePlatformInboundSecret(context.Background(), db, "ws-1")
	b, _ := IssuePlatformInboundSecret(context.Background(), db, "ws-1")
	if a == b {
		t.Errorf("expected fresh secret on re-issue, got %q twice", a)
	}
}

func TestIssuePlatformInboundSecret_RejectsEmptyWorkspaceID(t *testing.T) {
	db, _ := setupMock(t)
	_, err := IssuePlatformInboundSecret(context.Background(), db, "")
	if err == nil {
		t.Error("expected error for empty workspaceID, got nil")
	}
}

func TestReadPlatformInboundSecret_HappyPath(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-abc").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("the-plaintext"))

	got, err := ReadPlatformInboundSecret(context.Background(), db, "ws-abc")
	if err != nil {
		t.Fatalf("ReadPlatformInboundSecret: %v", err)
	}
	if got != "the-plaintext" {
		t.Errorf("got %q, want %q", got, "the-plaintext")
	}
}

// NULL column → ErrNoInboundSecret, not empty string. This is load-bearing:
// callers that ignored err and used the empty string would send an
// unauthenticated request to the workspace.
func TestReadPlatformInboundSecret_NullReturnsErrNoInbound(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-abc").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	_, err := ReadPlatformInboundSecret(context.Background(), db, "ws-abc")
	if !errors.Is(err, ErrNoInboundSecret) {
		t.Errorf("expected ErrNoInboundSecret on NULL, got %v", err)
	}
}

// Empty string is treated identically to NULL.
func TestReadPlatformInboundSecret_EmptyReturnsErrNoInbound(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-abc").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(""))

	_, err := ReadPlatformInboundSecret(context.Background(), db, "ws-abc")
	if !errors.Is(err, ErrNoInboundSecret) {
		t.Errorf("expected ErrNoInboundSecret on empty, got %v", err)
	}
}

// Missing workspace row → ErrNoInboundSecret (collapsed from sql.ErrNoRows).
func TestReadPlatformInboundSecret_MissingWorkspace(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}))

	_, err := ReadPlatformInboundSecret(context.Background(), db, "ws-missing")
	if !errors.Is(err, ErrNoInboundSecret) {
		t.Errorf("expected ErrNoInboundSecret on missing row, got %v", err)
	}
}

func TestReadPlatformInboundSecret_RejectsEmptyWorkspaceID(t *testing.T) {
	db, _ := setupMock(t)
	_, err := ReadPlatformInboundSecret(context.Background(), db, "")
	if err == nil {
		t.Error("expected error for empty workspaceID, got nil")
	}
}

// ------------------------------------------------------------
// Cache (#189) — heartbeat-storm absorption
// ------------------------------------------------------------

// A second read inside the TTL window MUST hit the cache and NOT
// re-issue a SELECT to the DB. This is the entire point of #189:
// the heartbeat fires every 60s/workspace and was doing one DB read
// each time to redeliver an unchanged value.
func TestReadPlatformInboundSecret_CacheHitWithinTTL(t *testing.T) {
	db, mock := setupMock(t)
	// Exactly ONE expected SELECT — the second read must be served
	// from cache. If the cache doesn't fire, a second SELECT will
	// arrive without a matching expectation and ExpectationsWereMet
	// will pass while the call panics — so we ALSO assert via the
	// returned value.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-cached").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("plaintext-1"))

	first, err := ReadPlatformInboundSecret(context.Background(), db, "ws-cached")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := ReadPlatformInboundSecret(context.Background(), db, "ws-cached")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if first != second {
		t.Errorf("cache returned different value: %q vs %q", first, second)
	}
	if second != "plaintext-1" {
		t.Errorf("cache returned %q, want %q", second, "plaintext-1")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (cache likely failed to short-circuit DB): %v", err)
	}
}

// After TTL expires the next read MUST hit the DB again so an
// out-of-band rotation propagates within minutes.
func TestReadPlatformInboundSecret_CacheRefreshesAfterTTL(t *testing.T) {
	db, mock := setupMock(t)
	// Two SELECTs expected. The first populates the cache; the second
	// fires after we advance the clock past the TTL. They return
	// DIFFERENT values to simulate an operator rotating the secret
	// directly via SQL.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("v1"))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("v2-rotated"))

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	restore := SetInboundSecretCacheNowForTesting(func() time.Time { return now })
	defer restore()

	first, err := ReadPlatformInboundSecret(context.Background(), db, "ws-rotated")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first != "v1" {
		t.Errorf("first read = %q, want v1", first)
	}

	// Advance past the TTL.
	now = now.Add(inboundSecretCacheTTL).Add(time.Second)

	second, err := ReadPlatformInboundSecret(context.Background(), db, "ws-rotated")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if second != "v2-rotated" {
		t.Errorf("post-TTL read = %q, want v2-rotated (rotation didn't propagate)", second)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Issue MUST update the cache (write-through) so a subsequent read
// from the same process sees the just-minted value without a DB
// round-trip. This pins the lazy-heal path in
// readOrLazyHealInboundSecret, which mints then immediately wants the
// fresh value.
func TestIssuePlatformInboundSecret_WriteThroughCachesValue(t *testing.T) {
	db, mock := setupMock(t)
	// ONE Exec for the mint. NO SELECT expected — the read should hit
	// cache because Issue populated it.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), "ws-write-through").
		WillReturnResult(sqlmock.NewResult(1, 1))

	minted, err := IssuePlatformInboundSecret(context.Background(), db, "ws-write-through")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := ReadPlatformInboundSecret(context.Background(), db, "ws-write-through")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != minted {
		t.Errorf("read after Issue = %q, want minted %q", got, minted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (read should not have hit DB): %v", err)
	}
}

// ErrNoInboundSecret (NULL/empty column) must NOT be cached — the
// row may legitimately appear later (race between Heartbeat and the
// initial INSERT in provisionWorkspaceCP, or a manual operator
// backfill). Caching absence would defeat the lazy-heal recovery
// contract.
func TestReadPlatformInboundSecret_DoesNotCacheAbsence(t *testing.T) {
	db, mock := setupMock(t)
	// First read returns NULL → ErrNoInboundSecret, NO cache.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))
	// Second read returns the freshly-backfilled value — must hit DB
	// because absence wasn't cached.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("backfilled"))

	_, err := ReadPlatformInboundSecret(context.Background(), db, "ws-null-then-set")
	if !errors.Is(err, ErrNoInboundSecret) {
		t.Fatalf("expected ErrNoInboundSecret on first read, got %v", err)
	}
	got, err := ReadPlatformInboundSecret(context.Background(), db, "ws-null-then-set")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got != "backfilled" {
		t.Errorf("second read = %q, want backfilled (absence was cached)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ResetInboundSecretCacheForTesting must clear ALL entries, not just
// the one matching a specific key. The setupMock helper uses this on
// every test to keep entries from leaking across runs.
func TestResetInboundSecretCacheForTesting_ClearsAllEntries(t *testing.T) {
	db, mock := setupMock(t)
	// Populate cache for two workspaces.
	for _, id := range []string{"ws-a", "ws-b"} {
		mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
			WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("v-" + id))
		if _, err := ReadPlatformInboundSecret(context.Background(), db, id); err != nil {
			t.Fatalf("populate %s: %v", id, err)
		}
	}
	ResetInboundSecretCacheForTesting()
	// After reset BOTH must miss the cache and trigger a fresh SELECT.
	for _, id := range []string{"ws-a", "ws-b"} {
		mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
			WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("v-" + id))
		if _, err := ReadPlatformInboundSecret(context.Background(), db, id); err != nil {
			t.Fatalf("post-reset %s: %v", id, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
