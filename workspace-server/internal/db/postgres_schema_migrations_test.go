package db

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Tests for schema_migrations tracking — verifies migrations only run once.

func TestRunMigrations_FirstBoot_AppliesAndRecords(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	DB = mockDB

	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "001_init.up.sql"), []byte("CREATE TABLE foo();"), 0o644)

	// Expect: CREATE tracking table
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS schema_migrations")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Expect: check if 001_init.up.sql already applied → returns false
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)")).
		WithArgs("001_init.up.sql").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// Expect: apply migration
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE foo();")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Expect: record as applied
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO schema_migrations (filename) VALUES ($1)")).
		WithArgs("001_init.up.sql").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := RunMigrations(tmp); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestRunMigrations_SecondBoot_SkipsApplied(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	DB = mockDB

	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "001_init.up.sql"), []byte("CREATE TABLE foo();"), 0o644)
	os.WriteFile(filepath.Join(tmp, "002_next.up.sql"), []byte("CREATE TABLE bar();"), 0o644)

	// Tracking table create is always attempted
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS schema_migrations")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// 001 already applied → skip
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)")).
		WithArgs("001_init.up.sql").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// 002 also already applied → skip
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)")).
		WithArgs("002_next.up.sql").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// No ExecExec for the migration bodies — they shouldn't run

	if err := RunMigrations(tmp); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestRunMigrations_MixedState_AppliesOnlyNew(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	DB = mockDB

	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "001_old.up.sql"), []byte("SELECT 1;"), 0o644)
	os.WriteFile(filepath.Join(tmp, "002_new.up.sql"), []byte("SELECT 2;"), 0o644)

	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS schema_migrations")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// 001 already applied
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)")).
		WithArgs("001_old.up.sql").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// 002 not yet applied
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)")).
		WithArgs("002_new.up.sql").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// Apply 002
	mock.ExpectExec(regexp.QuoteMeta("SELECT 2;")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Record 002
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO schema_migrations (filename) VALUES ($1)")).
		WithArgs("002_new.up.sql").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := RunMigrations(tmp); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestRunMigrations_SkipsDownSqlFilesEvenInTracking(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	DB = mockDB

	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "001_init.up.sql"), []byte("CREATE TABLE foo();"), 0o644)
	os.WriteFile(filepath.Join(tmp, "001_init.down.sql"), []byte("DROP TABLE foo;"), 0o644)

	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS schema_migrations")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Only .up.sql should be checked — not .down.sql
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)")).
		WithArgs("001_init.up.sql").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE foo();")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO schema_migrations (filename) VALUES ($1)")).
		WithArgs("001_init.up.sql").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := RunMigrations(tmp); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
