package orgtoken

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestLogMint_WritesAuditRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO org_token_audit_logs`).
		WithArgs("tok-1", "mint", "user_01", "org-1", "127.0.0.1", "test-agent", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	LogMint(context.Background(), db, "tok-1", "user_01", "org-1",
		AuditLogRequestContext{IPAddress: "127.0.0.1", UserAgent: "test-agent"},
		map[string]any{"name": "ci"})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLogRevoke_WritesAuditRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO org_token_audit_logs`).
		WithArgs("tok-1", "revoke", "admin-token", nil, nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	LogRevoke(context.Background(), db, "tok-1", "admin-token", "", AuditLogRequestContext{}, nil)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLogValidateFail_WritesAuditRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO org_token_audit_logs`).
		WithArgs(nil, "validate_fail", "org-token:abcd1234", nil, nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	LogValidateFail(context.Background(), db, "org-token:abcd1234", "", AuditLogRequestContext{},
		map[string]any{"prefix": "abcd1234"})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLogAuditEvent_SwallowsError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO org_token_audit_logs`).
		WillReturnError(sql.ErrConnDone)

	// Must not panic or return error — audit failures are best-effort.
	LogAuditEvent(context.Background(), db, nil, AuditActionMint, "actor", "", AuditLogRequestContext{}, nil)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestListAuditEvents_ReturnsRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := "2026-06-18T00:00:00Z"
	mock.ExpectQuery(`SELECT id, token_id, action, actor, org_id, ip_address, user_agent, metadata, created_at FROM org_token_audit_logs`).
		WithArgs("tok-1", 10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_id", "action", "actor", "org_id", "ip_address", "user_agent", "metadata", "created_at"}).
			AddRow("evt-1", "tok-1", "mint", "user_01", "org-1", "127.0.0.1", "ua", []byte(`{"name":"ci"}`), now))

	events, err := ListAuditEvents(context.Background(), db, "tok-1", 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Action != AuditActionMint {
		t.Errorf("action = %q, want mint", events[0].Action)
	}
	if events[0].TokenID == nil || *events[0].TokenID != "tok-1" {
		t.Errorf("token_id = %v, want tok-1", events[0].TokenID)
	}
}

func TestListAuditEvents_DefaultLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT id, token_id, action, actor, org_id, ip_address, user_agent, metadata, created_at FROM org_token_audit_logs`).
		WithArgs("tok-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_id", "action", "actor", "org_id", "ip_address", "user_agent", "metadata", "created_at"}))

	_, err = ListAuditEvents(context.Background(), db, "tok-1", 0)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
}
