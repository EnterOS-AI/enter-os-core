package rescuestore

// Sqlmock-backed coverage for the rescue_bundles store (RFC internal#742
// Part 3). Exercises Persist (incl. section clamp) + GetLatest (happy
// path, no-rows→nil, org-scoping, query error) without a real DB.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescue"
	"github.com/DATA-DOG/go-sqlmock"
)

func newMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	dbh, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	return dbh, mock
}

func sampleBundle() rescue.Bundle {
	return rescue.Bundle{
		WorkspaceID: "ws-1",
		OrgID:       "org-9",
		InstanceID:  "i-abc",
		Reason:      "bootstrap_watcher",
		Sections: []rescue.Section{
			{Name: "config.yaml", Content: "model: gpt-4", Redacted: true},
			{Name: "docker-ps", Content: "(no agent container)", Redacted: false},
		},
	}
}

// TestPersist_InsertsRow asserts Persist issues one INSERT with the
// bundle fields and a JSON sections payload.
func TestPersist_InsertsRow(t *testing.T) {
	dbh, mock := newMock(t)
	b := sampleBundle()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO rescue_bundles`)).
		WithArgs("ws-1", "org-9", "i-abc", "bootstrap_watcher", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := NewPostgres(dbh).Persist(context.Background(), b); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestClampSections: a section over maxSectionBytes is truncated +
// marker-suffixed; a small section is untouched.
func TestClampSections(t *testing.T) {
	huge := strings.Repeat("x", maxSectionBytes+5000)
	in := []rescue.Section{
		{Name: "container.logs", Content: huge, Redacted: true},
		{Name: "small", Content: "ok", Redacted: true},
	}
	out := clampSections(in)

	if len(out[0].Content) > maxSectionBytes+len(truncationMarker) {
		t.Errorf("clamped content len = %d, want <= %d", len(out[0].Content), maxSectionBytes+len(truncationMarker))
	}
	if !strings.HasSuffix(out[0].Content, truncationMarker) {
		t.Error("clamped section missing truncation marker suffix")
	}
	if out[1].Content != "ok" {
		t.Errorf("small section was modified: %q", out[1].Content)
	}
}

// TestPersist_WritesClampedPayload: Persist marshals the clamped
// sections into the JSONB arg (the INSERT carries the truncation marker).
func TestPersist_WritesClampedPayload(t *testing.T) {
	dbh, mock := newMock(t)
	huge := strings.Repeat("x", maxSectionBytes+5000)
	b := rescue.Bundle{
		WorkspaceID: "ws-1",
		Sections:    []rescue.Section{{Name: "container.logs", Content: huge, Redacted: true}},
	}
	want, _ := json.Marshal(clampSections(b.Sections))

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO rescue_bundles`)).
		WithArgs("ws-1", "", "", "", string(want)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := NewPostgres(dbh).Persist(context.Background(), b); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestGetLatest_ReturnsBundle: a found row decodes back into the bundle.
func TestGetLatest_ReturnsBundle(t *testing.T) {
	dbh, mock := newMock(t)
	ts := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	secs, _ := json.Marshal([]rescue.Section{
		{Name: "config.yaml", Content: "redacted", Redacted: true},
	})

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instance_id, reason, captured_at, sections`)).
		WithArgs("ws-1", "org-9").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id", "reason", "captured_at", "sections"}).
			AddRow("i-abc", "bootstrap_watcher", ts, secs))

	got, err := NewPostgres(dbh).GetLatest(context.Background(), "ws-1", "org-9")
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want a bundle")
	}
	if !got.CapturedAt.Equal(ts) {
		t.Errorf("captured_at = %v, want %v", got.CapturedAt, ts)
	}
	if got.Bundle.InstanceID != "i-abc" || got.Bundle.Reason != "bootstrap_watcher" {
		t.Errorf("bundle meta wrong: %+v", got.Bundle)
	}
	if len(got.Bundle.Sections) != 1 || got.Bundle.Sections[0].Name != "config.yaml" {
		t.Errorf("sections decoded wrong: %+v", got.Bundle.Sections)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestGetLatest_NoRowsReturnsNil: no bundle → (nil, nil), so the handler
// can 404 without treating it as an error.
func TestGetLatest_NoRowsReturnsNil(t *testing.T) {
	dbh, mock := newMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instance_id, reason, captured_at, sections`)).
		WithArgs("ws-none", "org-9").
		WillReturnError(sql.ErrNoRows)

	got, err := NewPostgres(dbh).GetLatest(context.Background(), "ws-none", "org-9")
	if err != nil {
		t.Fatalf("GetLatest err = %v, want nil for no-rows", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil for no-rows", got)
	}
}

// TestGetLatest_OrgScopingArg: the org id is passed as the $2 filter arg
// with strict equality, so a row in a sibling org is excluded by the query
// itself. A mismatched org → no row → nil (same as no-rows).
func TestGetLatest_OrgScopingArg(t *testing.T) {
	dbh, mock := newMock(t)
	// Tenant org-B asks for ws-1 (owned by org-9). The strict predicate
	// filters it out → ErrNoRows → nil.
	mock.ExpectQuery(regexp.QuoteMeta(`AND org_id = $2`)).
		WithArgs("ws-1", "org-B").
		WillReturnError(sql.ErrNoRows)

	got, err := NewPostgres(dbh).GetLatest(context.Background(), "ws-1", "org-B")
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if got != nil {
		t.Fatal("sibling-org read returned a bundle; want nil")
	}
}

// TestGetLatest_EmptyOrgIDRejected: an empty orgID must fail closed with
// an error rather than disabling the org filter (#2020).
func TestGetLatest_EmptyOrgIDRejected(t *testing.T) {
	dbh, _ := newMock(t)
	_, err := NewPostgres(dbh).GetLatest(context.Background(), "ws-1", "")
	if err == nil {
		t.Fatal("GetLatest(empty orgID) should error")
	}
}

// TestGetLatest_QueryErrorPropagates: a real DB error (not ErrNoRows)
// surfaces as an error so the handler returns 503, not a false 404.
func TestGetLatest_QueryErrorPropagates(t *testing.T) {
	dbh, mock := newMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instance_id, reason, captured_at, sections`)).
		WithArgs("ws-1", "org-9").
		WillReturnError(errors.New("connection reset"))

	_, err := NewPostgres(dbh).GetLatest(context.Background(), "ws-1", "org-9")
	if err == nil {
		t.Fatal("want an error for a non-ErrNoRows DB failure")
	}
}

// TestNilDB: both methods return an error (never panic) when the db
// handle is nil — the degraded-boot guard the wiring relies on.
func TestNilDB(t *testing.T) {
	p := NewPostgres(nil)
	if err := p.Persist(context.Background(), sampleBundle()); err == nil {
		t.Error("Persist(nil db) should error")
	}
	if _, err := p.GetLatest(context.Background(), "ws-1", "org-9"); err == nil {
		t.Error("GetLatest(nil db) should error")
	}
}
