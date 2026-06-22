package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/lib/pq"
)

var DB *sql.DB

func InitPostgres(databaseURL string) error {
	var err error
	DB, err = sql.Open("postgres", databaseURL)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	DB.SetMaxOpenConns(25)
	DB.SetMaxIdleConns(5)

	if err := DB.Ping(); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	log.Println("Connected to Postgres")
	return nil
}

// RunMigrations applies every forward migration file in migrationsDir on
// platform boot.
//
// Issue #211 — DO NOT glob `*.sql`. That matches both `.up.sql` and `.down.sql`,
// and sort.Strings orders "d" before "u", so every boot used to run the
// rollback BEFORE the forward migration for any pair, wiping data from any
// table the pair recreates (020_workspace_auth_tokens was the canary — every
// restart wiped live tokens, regressing AdminAuth to fail-open bypass for
// every subsequent request).
//
// The fix: only run files that are either `.up.sql` or plain `.sql` (legacy
// pre-pair migrations like 009_activity_logs.sql). Never touch `.down.sql`
// — those are intentional rollbacks, only to be run by operators manually
// via psql when a real rollback is required.
//
// NOTE: this runner still re-applies every migration on every boot. That
// works for idempotent `CREATE TABLE IF NOT EXISTS` + `ALTER TABLE ... IF NOT
// EXISTS` statements but means non-idempotent DDL will fail on restart.
// Migration authors must write idempotent SQL. A real schema_migrations
// tracking table would be better; tracked as follow-up.
func RunMigrations(migrationsDir string) error {
	// Create tracking table if it doesn't exist.
	if _, err := DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	allFiles, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	// Forward-only filter — skip *.down.sql explicitly.
	files := make([]string, 0, len(allFiles))
	for _, f := range allFiles {
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".down.sql") {
			continue
		}
		files = append(files, f)
	}
	sort.Strings(files)

	applied := 0
	skipped := 0
	for _, f := range files {
		base := filepath.Base(f)

		// Check if already applied.
		var exists bool
		if err := DB.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)", base).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", base, err)
		}
		if exists {
			skipped++
			continue
		}

		log.Printf("Applying migration: %s", base)
		content, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := DB.Exec(string(content)); err != nil {
			return fmt.Errorf("exec %s: %w", base, err)
		}

		// Record as applied.
		if _, err := DB.Exec("INSERT INTO schema_migrations (filename) VALUES ($1)", base); err != nil {
			return fmt.Errorf("record migration %s: %w", base, err)
		}
		applied++
	}
	log.Printf("Applied %d migrations (%d already applied)", applied, skipped)
	return nil
}
