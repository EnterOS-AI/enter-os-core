// memory-plugin-postgres is the built-in implementation of the memory
// plugin contract (RFC #2728). Operators run it next to workspace-
// server; workspace-server points MEMORY_PLUGIN_URL at it.
//
// Owns its own postgres tables (see migrations/). When an operator
// swaps in a different plugin, this binary's tables become orphaned
// — not auto-dropped. Document this in the plugin docs (PR-10).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/pgplugin"
)

const (
	envDatabaseURL = "MEMORY_PLUGIN_DATABASE_URL"
	envListenAddr  = "MEMORY_PLUGIN_LISTEN_ADDR"
	envSkipMigrate = "MEMORY_PLUGIN_SKIP_MIGRATE"

	defaultListenAddr = ":9100"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("memory-plugin-postgres: %v", err)
	}
}

// run is the boot path. Extracted from main() so tests can drive it
// with synthesized env. Returns nil on graceful shutdown, an error on
// failure to bring up.
func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	db, err := openDB(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if !cfg.SkipMigrate {
		if err := runMigrations(db); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	store := pgplugin.NewStore(db)
	handler := pgplugin.NewHandler(store, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return db.PingContext(ctx)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Listen separately so we can log the bound port (handy when
	// :0 is used in tests).
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	log.Printf("memory-plugin-postgres listening on %s", ln.Addr())

	// Run server in a goroutine; main waits on signal.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		log.Println("shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

type config struct {
	DatabaseURL string
	ListenAddr  string
	SkipMigrate bool
}

func loadConfig() (*config, error) {
	dbURL := strings.TrimSpace(os.Getenv(envDatabaseURL))
	if dbURL == "" {
		return nil, fmt.Errorf("%s is required", envDatabaseURL)
	}
	addr := strings.TrimSpace(os.Getenv(envListenAddr))
	if addr == "" {
		addr = defaultListenAddr
	}
	return &config{
		DatabaseURL: dbURL,
		ListenAddr:  addr,
		SkipMigrate: os.Getenv(envSkipMigrate) == "1",
	}, nil
}

func openDB(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

// runMigrations applies the schema migrations bundled at
// cmd/memory-plugin-postgres/migrations/. Idempotent on repeat boot.
//
// Implementation note: rather than embedding the full migrate engine,
// we read the migration files at boot from a known relative path. The
// down migrations are deliberately NOT applied here — that's a manual
// operator action. This keeps the binary tiny and avoids dragging in
// golang-migrate's drivers.
func runMigrations(db *sql.DB) error {
	// Find the migrations directory. In `go run` mode it's relative
	// to the cmd dir; in the prebuilt binary case it's expected next
	// to the binary OR via env var override.
	dir := os.Getenv("MEMORY_PLUGIN_MIGRATIONS_DIR")
	if dir == "" {
		// Best-effort: try the cwd-relative path that works for `go test`.
		dir = "cmd/memory-plugin-postgres/migrations"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		path := dir + "/" + e.Name()
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %q: %w", path, err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply %q: %w", path, err)
		}
		log.Printf("applied migration %s", e.Name())
	}
	return nil
}
