// Package pgplugin is the storage layer for the built-in postgres
// memory plugin. It implements the operations the HTTP handlers (in
// this same package) need: namespace CRUD, memory CRUD, and search.
//
// This package is owned by the plugin, NOT by workspace-server's
// memory layer. workspace-server talks to the plugin via the HTTP
// contract (PR-1, PR-2); this package is what's behind that wire.
package pgplugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

// ErrNotFound is the typed sentinel for "namespace or memory not
// found." Handlers map this to HTTP 404.
var ErrNotFound = errors.New("not found")

// Store is the postgres-backed implementation of the plugin's data
// layer. Safe for concurrent use.
type Store struct {
	db *sql.DB
}

// NewStore wraps the given DB handle. The DB must already be
// connected and have run the plugin's migrations.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// --- Namespace operations ---

// UpsertNamespace creates or updates a namespace. Idempotent.
func (s *Store) UpsertNamespace(ctx context.Context, name string, body contract.NamespaceUpsert) (*contract.Namespace, error) {
	metadata, err := marshalMetadata(body.Metadata)
	if err != nil {
		return nil, err
	}
	const query = `
		INSERT INTO memory_namespaces (name, kind, expires_at, metadata)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE
		SET kind = EXCLUDED.kind,
		    expires_at = EXCLUDED.expires_at,
		    metadata = EXCLUDED.metadata
		RETURNING name, kind, expires_at, metadata, created_at
	`
	row := s.db.QueryRowContext(ctx, query, name, string(body.Kind), nullTime(body.ExpiresAt), metadata)
	return scanNamespace(row)
}

// PatchNamespace mutates an existing namespace. Each field is
// optional; only non-nil fields are written.
func (s *Store) PatchNamespace(ctx context.Context, name string, body contract.NamespacePatch) (*contract.Namespace, error) {
	// COALESCE pattern: NULL means "don't update" — but the caller's
	// nil pointer to ExpiresAt is distinct from "set to NULL". To
	// honor both, we use a sentinel via Validate().
	//
	// Validate() guarantees at least one field is set, so this update
	// always writes something.
	parts := []string{}
	args := []interface{}{name}
	idx := 2
	if body.ExpiresAt != nil {
		parts = append(parts, fmt.Sprintf("expires_at = $%d", idx))
		args = append(args, *body.ExpiresAt)
		idx++
	}
	if body.Metadata != nil {
		metadata, err := marshalMetadata(body.Metadata)
		if err != nil {
			return nil, err
		}
		parts = append(parts, fmt.Sprintf("metadata = $%d", idx))
		args = append(args, metadata)
		idx++
	}
	query := fmt.Sprintf(`
		UPDATE memory_namespaces SET %s
		WHERE name = $1
		RETURNING name, kind, expires_at, metadata, created_at
	`, strings.Join(parts, ", "))
	row := s.db.QueryRowContext(ctx, query, args...)
	ns, err := scanNamespace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ns, err
}

// DeleteNamespace removes a namespace and (via FK CASCADE) all its
// memories. Returns ErrNotFound when the namespace doesn't exist.
func (s *Store) DeleteNamespace(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memory_namespaces WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("delete namespace: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Memory operations ---

// CommitMemory inserts a new memory record. The namespace must
// already exist (auto-created by handler if not).
func (s *Store) CommitMemory(ctx context.Context, namespace string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error) {
	propagation, err := marshalMetadata(body.Propagation)
	if err != nil {
		return nil, err
	}
	embedding := nullVectorString(body.Embedding)

	// Two paths so that the upsert branch only fires when the caller
	// supplied an idempotency key. Production agent commits leave id
	// empty and rely on gen_random_uuid() — splitting the SQL avoids
	// adding a NULL guard inside the conflict target.
	if body.ID != "" {
		const upsertQuery = `
			INSERT INTO memory_records
				(id, namespace, content, kind, source, expires_at, propagation, pin, embedding)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::vector)
			ON CONFLICT (id) DO UPDATE SET
				namespace = EXCLUDED.namespace,
				content = EXCLUDED.content,
				kind = EXCLUDED.kind,
				source = EXCLUDED.source,
				expires_at = EXCLUDED.expires_at,
				propagation = EXCLUDED.propagation,
				pin = EXCLUDED.pin,
				embedding = EXCLUDED.embedding
			RETURNING id, namespace
		`
		row := s.db.QueryRowContext(ctx, upsertQuery,
			body.ID,
			namespace,
			body.Content,
			string(body.Kind),
			string(body.Source),
			nullTime(body.ExpiresAt),
			propagation,
			body.Pin,
			embedding,
		)
		var resp contract.MemoryWriteResponse
		if err := row.Scan(&resp.ID, &resp.Namespace); err != nil {
			return nil, fmt.Errorf("commit memory (upsert): %w", err)
		}
		return &resp, nil
	}

	const query = `
		INSERT INTO memory_records
			(namespace, content, kind, source, expires_at, propagation, pin, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::vector)
		RETURNING id, namespace
	`
	row := s.db.QueryRowContext(ctx, query,
		namespace,
		body.Content,
		string(body.Kind),
		string(body.Source),
		nullTime(body.ExpiresAt),
		propagation,
		body.Pin,
		embedding,
	)
	var resp contract.MemoryWriteResponse
	if err := row.Scan(&resp.ID, &resp.Namespace); err != nil {
		return nil, fmt.Errorf("commit memory: %w", err)
	}
	return &resp, nil
}

// ForgetMemory deletes a memory by id, but only if it lives in a
// namespace the caller has access to. The handler enforces this; the
// store just executes the DELETE.
func (s *Store) ForgetMemory(ctx context.Context, id string, requestedByNamespace string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_records WHERE id = $1 AND namespace = $2`,
		id, requestedByNamespace)
	if err != nil {
		return fmt.Errorf("forget memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Search runs a multi-namespace search across one or more of FTS,
// semantic (pgvector cosine), or substring fallback. The choice of
// path is gated on what the request supplies:
//
//   - body.Embedding present → semantic search
//   - body.Query present (>=2 chars) → FTS
//   - body.Query present (<2 chars) → ILIKE substring
//   - neither → recent-first listing
func (s *Store) Search(ctx context.Context, body contract.SearchRequest) (*contract.SearchResponse, error) {
	limit := body.Limit
	if limit <= 0 {
		limit = 20
	}

	args := []interface{}{}
	args = append(args, anyArrayFromStrings(body.Namespaces))
	idx := 2

	where := []string{`namespace = ANY($1)`}
	// TTL filter: never return expired memories. NULL expires_at = "no TTL".
	where = append(where, `(expires_at IS NULL OR expires_at > now())`)

	if len(body.Kinds) > 0 {
		where = append(where, fmt.Sprintf(`kind = ANY($%d)`, idx))
		args = append(args, anyArrayFromKinds(body.Kinds))
		idx++
	}

	var orderBy, scoreSelect string
	switch {
	case len(body.Embedding) > 0:
		// Semantic — cosine distance, score = 1 - distance.
		scoreSelect = fmt.Sprintf(`, 1 - (embedding <=> $%d::vector) AS score`, idx)
		orderBy = fmt.Sprintf(`ORDER BY embedding <=> $%d::vector ASC`, idx)
		where = append(where, `embedding IS NOT NULL`)
		args = append(args, vectorString(body.Embedding))
		idx++
	case len(body.Query) >= 2:
		// FTS via tsvector + ts_rank.
		scoreSelect = fmt.Sprintf(`, ts_rank(content_tsv, plainto_tsquery('english', $%d)) AS score`, idx)
		where = append(where, fmt.Sprintf(`content_tsv @@ plainto_tsquery('english', $%d)`, idx))
		orderBy = fmt.Sprintf(`ORDER BY ts_rank(content_tsv, plainto_tsquery('english', $%d)) DESC`, idx)
		args = append(args, body.Query)
		idx++
	case body.Query != "":
		// 1-char query — ILIKE substring. Score is a sentinel (NULL).
		scoreSelect = `, NULL::float AS score`
		where = append(where, fmt.Sprintf(`content ILIKE '%%' || $%d || '%%'`, idx))
		orderBy = `ORDER BY pin DESC, created_at DESC`
		args = append(args, body.Query)
		idx++
	default:
		// No query — recent-first.
		scoreSelect = `, NULL::float AS score`
		orderBy = `ORDER BY pin DESC, created_at DESC`
	}

	args = append(args, limit)
	limitPos := idx

	query := fmt.Sprintf(`
		SELECT id, namespace, content, kind, source, expires_at, propagation, pin, created_at%s
		FROM memory_records
		WHERE %s
		%s
		LIMIT $%d
	`, scoreSelect, strings.Join(where, " AND "), orderBy, limitPos)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	out := contract.SearchResponse{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out.Memories = append(out.Memories, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return &out, nil
}

// --- Helpers ---

func scanNamespace(row interface{ Scan(dest ...interface{}) error }) (*contract.Namespace, error) {
	var ns contract.Namespace
	var kindStr string
	var expires sql.NullTime
	var metadata []byte
	if err := row.Scan(&ns.Name, &kindStr, &expires, &metadata, &ns.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan namespace: %w", err)
	}
	ns.Kind = contract.NamespaceKind(kindStr)
	if expires.Valid {
		t := expires.Time
		ns.ExpiresAt = &t
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &ns.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	return &ns, nil
}

func scanMemory(row interface{ Scan(dest ...interface{}) error }) (*contract.Memory, error) {
	var m contract.Memory
	var kindStr, sourceStr string
	var expires sql.NullTime
	var propagation []byte
	var score sql.NullFloat64
	if err := row.Scan(
		&m.ID, &m.Namespace, &m.Content, &kindStr, &sourceStr,
		&expires, &propagation, &m.Pin, &m.CreatedAt, &score,
	); err != nil {
		return nil, fmt.Errorf("scan memory: %w", err)
	}
	m.Kind = contract.MemoryKind(kindStr)
	m.Source = contract.MemorySource(sourceStr)
	if expires.Valid {
		t := expires.Time
		m.ExpiresAt = &t
	}
	if len(propagation) > 0 {
		if err := json.Unmarshal(propagation, &m.Propagation); err != nil {
			return nil, fmt.Errorf("unmarshal propagation: %w", err)
		}
	}
	if score.Valid {
		v := score.Float64
		m.Score = &v
	}
	return &m, nil
}

func marshalMetadata(m map[string]interface{}) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	return b, nil
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// vectorString formats a []float32 as the postgres vector literal
// "[1.5,2.5,...]". The caller casts it to ::vector in SQL.
func vectorString(v []float32) string {
	if len(v) == 0 {
		return ""
	}
	b := strings.Builder{}
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%g", x))
	}
	b.WriteByte(']')
	return b.String()
}

// nullVectorString returns nil for empty embedding (so postgres
// stores NULL) and a vector literal otherwise.
func nullVectorString(v []float32) interface{} {
	if len(v) == 0 {
		return nil
	}
	return vectorString(v)
}

// anyArrayFromStrings wraps the slice in pq.Array so lib/pq's
// driver-level encoder turns it into a postgres TEXT[] literal.
// Same shape on both production and sqlmock test paths.
func anyArrayFromStrings(in []string) interface{} {
	return pq.Array(in)
}

func anyArrayFromKinds(in []contract.MemoryKind) interface{} {
	out := make([]string, len(in))
	for i, k := range in {
		out[i] = string(k)
	}
	return pq.Array(out)
}
