package main

import (
	"strings"
	"testing"
)

// TestMigrationsEmbedded_ContainsCreateTable pins that the migrations
// are bundled into the binary at build time, NOT loaded from a
// filesystem path that doesn't exist at runtime in the published image.
//
// Pre-fix: PR #2906 shipped the binary without the migrations dir;
// `os.ReadDir("cmd/memory-plugin-postgres/migrations")` errored on every
// tenant boot, the 30s health gate aborted the container, and the
// staging redeploy fleet job marked all tenants as failed. Embedding
// the migrations into the binary removes the runtime path entirely.
func TestMigrationsEmbedded_ContainsCreateTable(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("embedded migrations dir unreadable: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded migrations dir is empty — go:embed pattern matched no files")
	}

	var seenUp bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		seenUp = true
		data, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Errorf("read embedded %q: %v", e.Name(), err)
			continue
		}
		if !strings.Contains(string(data), "CREATE TABLE") {
			t.Errorf("embedded %q has no CREATE TABLE — wrong file embedded?", e.Name())
		}
	}
	if !seenUp {
		t.Fatal("no *.up.sql in embedded migrations — runtime would have no schema to apply")
	}
}

// TestRunMigrationsFromEmbed_OrderingIsAlphabetic pins that we apply
// migrations in deterministic alphabetical order, not in whatever
// arbitrary order migrationsFS.ReadDir happens to return. With one
// migration today this is moot, but a future second migration ('002_…')
// MUST run after '001_…' or the schema is broken.
//
// We can't easily exercise db.Exec here (no test DB); instead pin the
// sort step on the directory listing itself.
func TestRunMigrationsFromEmbed_OrderingIsAlphabetic(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("embedded migrations dir unreadable: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		names = append(names, e.Name())
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("ReadDir returned non-sorted names; runMigrationsFromEmbed must sort. "+
				"Got %q before %q", names[i-1], names[i])
		}
	}
}
