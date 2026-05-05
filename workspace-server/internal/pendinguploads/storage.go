// Package pendinguploads is the platform-side staging layer for chat file
// uploads bound for poll-mode workspaces (delivery_mode='poll', no public
// callback URL — typically external runtimes on a laptop / behind NAT).
//
// In push-mode the platform synchronously POSTs the multipart body to the
// workspace's /internal/chat/uploads/ingest endpoint and forgets about it.
// Poll-mode has no callback URL to forward to, so the platform parses the
// multipart on this side, persists each file as one pending_uploads row,
// and lets the workspace pull it on its next inbox poll cycle.
//
// The Storage interface keeps the bytes-vs-metadata split clean: today
// content is stored inline as bytea on the pending_uploads row, but the
// shape lets a future PR (RFC #2789, S3-backed shared storage) swap to
// object storage by adding a new Storage implementation without touching
// any of the handler-layer callers.
//
// Lifecycle:
//
//	Put         — handler creates a row with the file content; assigns file_id.
//	Get         — GET /workspaces/:id/pending-uploads/:fid/content reads bytes.
//	MarkFetched — stamps fetched_at on the row (Phase 3 observability).
//	Ack         — POST /workspaces/:id/pending-uploads/:fid/ack;
//	              terminal happy-path state. After ack, Get returns ErrNotFound.
//	              GC sweep deletes acked rows after a retention window.
//
// Hard TTL: every row has an expires_at default of created_at + 24h. After
// expiration the row is GC'd by Phase 3's sweep cron regardless of ack
// state. Get on an expired row returns ErrNotFound — the workspace's next
// poll will see the underlying activity_logs row was orphaned and the
// agent surfaces "file expired" to the user.
package pendinguploads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Per-file size cap. Mirrors workspace-side ingest_handler
// (workspace/internal_chat_uploads.py:198). Pinned at the DB level via
// the size_bytes CHECK constraint; this Go-side constant exists so the
// Put implementation can reject before round-tripping to Postgres.
const MaxFileBytes = 25 * 1024 * 1024

// ErrNotFound is returned by Get / MarkFetched / Ack when the row is
// absent. Callers turn this into HTTP 404. Treat acked + expired rows
// as not-found so the workspace can never re-fetch a file we've
// considered handed-off.
var ErrNotFound = errors.New("pendinguploads: row not found, expired, or already acked")

// ErrTooLarge is returned by Put when content exceeds MaxFileBytes.
// Callers turn this into HTTP 413. Pre-DB check so we don't push a
// 25 MB+1 byte payload through Postgres just to have the CHECK reject it.
var ErrTooLarge = errors.New("pendinguploads: content exceeds per-file cap")

// Record carries the full row including content. Returned by Get;
// the GET /content handler streams Record.Content as the response body.
type Record struct {
	FileID      uuid.UUID
	WorkspaceID uuid.UUID
	Content     []byte
	Filename    string
	Mimetype    string
	SizeBytes   int64
	CreatedAt   time.Time
	FetchedAt   *time.Time // nil before first MarkFetched
	AckedAt     *time.Time // nil before Ack (Get returns ErrNotFound after)
	ExpiresAt   time.Time
}

// SweepResult is the per-cycle accounting from Sweep. Both counts are
// non-negative; Total is just Acked + Expired for log/metrics
// convenience. Phase 3 metrics expose these as separate counters so
// dashboards can spot a stuck-ack pattern (high Expired, low Acked) vs.
// healthy churn (Acked dominates).
type SweepResult struct {
	Acked   int // rows deleted because acked_at + retention elapsed
	Expired int // rows deleted because expires_at < now AND never acked
}

// Total returns the sum of Acked + Expired — convenient for log lines.
func (r SweepResult) Total() int { return r.Acked + r.Expired }

// PutItem is one file in a PutBatch call. Same per-field rules as Put —
// empty content, missing filename, or content > MaxFileBytes is rejected
// up-front so a bad item in the batch doesn't poison the transaction.
type PutItem struct {
	Content  []byte
	Filename string
	Mimetype string
}

// Storage is the platform-side persistence boundary for poll-mode chat
// uploads. The Postgres implementation backs all callers today; an S3-
// backed implementation can drop in once RFC #2789 lands by making
// content storage out-of-line and updating the Postgres-only metadata
// columns.
type Storage interface {
	// Put creates a row for one file targeting workspaceID and returns
	// the assigned file_id. content is bounded by MaxFileBytes;
	// filename / mimetype are stored verbatim — caller is responsible
	// for sanitization (matches workspace-side rule, see
	// internal_chat_uploads.py:sanitize_filename). Empty filename and
	// content > MaxFileBytes return errors before any DB write.
	Put(ctx context.Context, workspaceID uuid.UUID, content []byte, filename, mimetype string) (uuid.UUID, error)

	// PutBatch inserts N uploads atomically — either all rows commit or
	// none do. Returns assigned file_ids in input order on success;
	// returns an error and does NOT insert any row on failure.
	//
	// Use this from multi-file upload handlers so a per-row failure on
	// row K doesn't leave rows 1..K-1 orphaned in the table (a client
	// retry would then double-insert them on success). All-or-nothing
	// semantics match the multipart request the canvas sends — either
	// the whole batch succeeds or the user re-uploads.
	PutBatch(ctx context.Context, workspaceID uuid.UUID, items []PutItem) ([]uuid.UUID, error)

	// Get returns the full row including content. Returns ErrNotFound
	// when the row is absent, acked, or past expires_at. Caller should
	// not differentiate the three cases in the response — from the
	// workspace's perspective they all mean "not available, give up."
	Get(ctx context.Context, fileID uuid.UUID) (Record, error)

	// MarkFetched stamps fetched_at on the row. Idempotent — repeated
	// calls update fetched_at to the latest timestamp. Returns
	// ErrNotFound if the row is absent / acked / expired.
	MarkFetched(ctx context.Context, fileID uuid.UUID) error

	// Ack stamps acked_at on the row. Idempotent on the row state
	// (acked_at is only set the first time so workspace double-acks
	// don't move the timestamp). Returns ErrNotFound if the row is
	// absent or already expired; on already-acked, returns nil so
	// the workspace's at-least-once retry succeeds without an error.
	Ack(ctx context.Context, fileID uuid.UUID) error

	// Sweep deletes rows past their retention window:
	//   - acked rows older than ackRetention (give the workspace a
	//     window to re-fetch in case it processed but failed to write
	//     the file before crashing — at-least-once behavior).
	//   - unacked rows past expires_at (the platform's hard TTL — 24h
	//     by default; a workspace that hasn't fetched by then is
	//     considered dead from the upload's perspective).
	// Returns the per-category deletion counts for observability.
	// Errors are surfaced to the caller; a transient DB error must NOT
	// crash the sweeper loop (it just retries on the next tick).
	Sweep(ctx context.Context, ackRetention time.Duration) (SweepResult, error)
}

// PostgresStorage is the production Storage implementation backed by
// the pending_uploads table.
type PostgresStorage struct {
	db *sql.DB
}

// NewPostgres returns a Storage backed by db. db must be a connected
// pool; this constructor does no I/O.
func NewPostgres(db *sql.DB) *PostgresStorage {
	return &PostgresStorage{db: db}
}

// Compile-time check that PostgresStorage satisfies Storage.
var _ Storage = (*PostgresStorage)(nil)

func (p *PostgresStorage) Put(ctx context.Context, workspaceID uuid.UUID, content []byte, filename, mimetype string) (uuid.UUID, error) {
	if len(content) == 0 {
		return uuid.Nil, fmt.Errorf("pendinguploads: empty content")
	}
	if len(content) > MaxFileBytes {
		return uuid.Nil, ErrTooLarge
	}
	if filename == "" {
		return uuid.Nil, fmt.Errorf("pendinguploads: empty filename")
	}
	// Filename length cap is enforced both here (early reject) and at
	// the DB layer (CHECK constraint) so a buggy caller can't write a
	// 200-char filename that Phase 2's URI rewrite would then truncate.
	if len(filename) > 100 {
		return uuid.Nil, fmt.Errorf("pendinguploads: filename exceeds 100 chars")
	}

	var fileID uuid.UUID
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO pending_uploads (workspace_id, content, size_bytes, filename, mimetype)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING file_id
	`, workspaceID, content, int64(len(content)), filename, mimetype).Scan(&fileID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("pendinguploads: insert: %w", err)
	}
	return fileID, nil
}

// PutBatch inserts every item atomically inside a single Tx. On any
// per-item validation or per-row INSERT error the Tx is rolled back and
// the caller sees the error without any rows committed — no partial
// orphans for a multi-file upload that fails mid-batch.
//
// Validation runs BEFORE BEGIN so a bad input shape (empty content,
// over-cap size) doesn't even open a Tx. Once we're in the Tx, the only
// failures expected are DB-side (broken connection, statement timeout)
// — those abort cleanly via Rollback.
func (p *PostgresStorage) PutBatch(ctx context.Context, workspaceID uuid.UUID, items []PutItem) ([]uuid.UUID, error) {
	if len(items) == 0 {
		return nil, nil
	}
	for i, it := range items {
		if len(it.Content) == 0 {
			return nil, fmt.Errorf("pendinguploads: item %d: empty content", i)
		}
		if len(it.Content) > MaxFileBytes {
			return nil, ErrTooLarge
		}
		if it.Filename == "" {
			return nil, fmt.Errorf("pendinguploads: item %d: empty filename", i)
		}
		if len(it.Filename) > 100 {
			return nil, fmt.Errorf("pendinguploads: item %d: filename exceeds 100 chars", i)
		}
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pendinguploads: begin tx: %w", err)
	}
	// Defer-rollback is safe even after a successful Commit — the second
	// Rollback is a no-op (database/sql tracks tx state).
	defer func() {
		_ = tx.Rollback()
	}()

	out := make([]uuid.UUID, 0, len(items))
	for i, it := range items {
		var fid uuid.UUID
		err := tx.QueryRowContext(ctx, `
			INSERT INTO pending_uploads (workspace_id, content, size_bytes, filename, mimetype)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING file_id
		`, workspaceID, it.Content, int64(len(it.Content)), it.Filename, it.Mimetype).Scan(&fid)
		if err != nil {
			return nil, fmt.Errorf("pendinguploads: batch insert item %d: %w", i, err)
		}
		out = append(out, fid)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pendinguploads: commit batch: %w", err)
	}
	return out, nil
}

func (p *PostgresStorage) Get(ctx context.Context, fileID uuid.UUID) (Record, error) {
	// The expires_at + acked_at filter in the WHERE clause means a
	// caller sees ErrNotFound for absent / acked / expired without
	// needing per-case branching. Trade-off: we can't differentiate
	// in metrics, but the workspace's response is the same in all
	// three cases ("file gone, give up") so the granularity isn't
	// useful at this layer. Phase 3 dashboards aggregate row-state
	// counts directly off the table.
	var r Record
	err := p.db.QueryRowContext(ctx, `
		SELECT file_id, workspace_id, content, filename, mimetype,
		       size_bytes, created_at, fetched_at, acked_at, expires_at
		FROM pending_uploads
		WHERE file_id = $1
		  AND acked_at IS NULL
		  AND expires_at > now()
	`, fileID).Scan(
		&r.FileID, &r.WorkspaceID, &r.Content, &r.Filename, &r.Mimetype,
		&r.SizeBytes, &r.CreatedAt, &r.FetchedAt, &r.AckedAt, &r.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("pendinguploads: select: %w", err)
	}
	return r, nil
}

func (p *PostgresStorage) MarkFetched(ctx context.Context, fileID uuid.UUID) error {
	// UPDATE on the same gating predicate as Get — keeps the "absent
	// or acked or expired = ErrNotFound" contract symmetric. Without
	// the predicate a workspace could re-stamp fetched_at on an acked
	// row, which would mislead Phase 3's stuck-fetch dashboard.
	res, err := p.db.ExecContext(ctx, `
		UPDATE pending_uploads
		SET fetched_at = now()
		WHERE file_id = $1
		  AND acked_at IS NULL
		  AND expires_at > now()
	`, fileID)
	if err != nil {
		return fmt.Errorf("pendinguploads: mark_fetched: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pendinguploads: mark_fetched rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStorage) Ack(ctx context.Context, fileID uuid.UUID) error {
	// Set acked_at only if currently NULL — workspace at-least-once
	// retries don't move the timestamp, so dashboards see the first
	// successful ack as the "delivery time."  Two-clause WHERE: row
	// must exist and not be expired; acked-but-still-in-window is
	// returned as success (idempotent retry).
	res, err := p.db.ExecContext(ctx, `
		UPDATE pending_uploads
		SET acked_at = now()
		WHERE file_id = $1
		  AND acked_at IS NULL
		  AND expires_at > now()
	`, fileID)
	if err != nil {
		return fmt.Errorf("pendinguploads: ack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pendinguploads: ack rows: %w", err)
	}
	if n == 1 {
		return nil
	}
	// Zero-rows-affected: either the row doesn't exist / has expired,
	// OR it was already acked. Re-query to disambiguate so the
	// idempotent-retry case returns nil instead of ErrNotFound.
	var ackedAt sql.NullTime
	err = p.db.QueryRowContext(ctx, `
		SELECT acked_at FROM pending_uploads
		WHERE file_id = $1 AND expires_at > now()
	`, fileID).Scan(&ackedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("pendinguploads: ack disambiguate: %w", err)
	}
	if ackedAt.Valid {
		// Already acked — idempotent success.
		return nil
	}
	// Predicate matched a non-acked, non-expired row but RowsAffected
	// was 0. This means the row was concurrently modified between the
	// UPDATE and the SELECT (extremely rare; e.g. a Phase 3 sweep
	// raced with the ACK). Treat as success — the row is gone, but
	// the workspace's intent ("I'm done with this file") was honored.
	return nil
}

// Sweep deletes acked rows past their retention window plus any
// unacked rows whose hard TTL has elapsed. Single round-trip: a CTE
// captures the deletion in one DELETE … RETURNING and the outer
// SELECT sums by category. Cheaper and tighter than two round trips,
// and atomic w.r.t. concurrent writes (the WHERE predicate sees a
// consistent snapshot via Postgres MVCC).
//
// ackRetention=0 deletes all acked rows immediately; values <0 are
// clamped to 0 for safety. Caller defaults are documented at
// StartSweeper's DefaultAckRetention.
func (p *PostgresStorage) Sweep(ctx context.Context, ackRetention time.Duration) (SweepResult, error) {
	if ackRetention < 0 {
		ackRetention = 0
	}
	// make_interval expects integer seconds — Postgres accepts a
	// floating point but we deliberately round to the nearest second
	// so test fixtures pin a deterministic value across PG versions.
	retentionSecs := int64(ackRetention.Seconds())

	var acked, expired int
	err := p.db.QueryRowContext(ctx, `
		WITH deleted AS (
			DELETE FROM pending_uploads
			WHERE (acked_at IS NOT NULL AND acked_at < now() - make_interval(secs => $1))
			   OR (acked_at IS NULL     AND expires_at < now())
			RETURNING (acked_at IS NOT NULL) AS was_acked
		)
		SELECT
			COALESCE(SUM(CASE WHEN was_acked     THEN 1 ELSE 0 END), 0)::int AS acked,
			COALESCE(SUM(CASE WHEN NOT was_acked THEN 1 ELSE 0 END), 0)::int AS expired
		FROM deleted
	`, retentionSecs).Scan(&acked, &expired)
	if err != nil {
		return SweepResult{}, fmt.Errorf("pendinguploads: sweep: %w", err)
	}
	return SweepResult{Acked: acked, Expired: expired}, nil
}
