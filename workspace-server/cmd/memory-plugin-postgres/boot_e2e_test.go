//go:build memory_plugin_e2e

// Package main's real-subprocess boot test (#293 fixup, RFC #2728).
//
// Build-tag gated so it only runs when an operator explicitly opts in:
//
//   MEMORY_PLUGIN_E2E_DB=postgres://test:test@localhost:5432/test?sslmode=disable \
//     go test -tags memory_plugin_e2e -v ./cmd/memory-plugin-postgres/
//
// Why a separate build tag:
//   - The default `go test ./...` run shouldn't require docker or a
//     live postgres
//   - CI gates that DO want to run this can set the env var + tag
//   - Operators verifying a custom plugin against the contract can
//     copy this file as the template (replace the binary build step
//     with their own)
//
// What this exercises that PR-11's swap test doesn't:
//   - Real `go build` of cmd/memory-plugin-postgres/
//   - Real binary boot via os/exec — catches mixed-key panics, missing
//     env vars, crash-on-startup issues that in-process tests skip
//   - Real postgres connection — catches wire-format bugs (e.g. the
//     pq.Array regression we hit during PR-3)
//   - Real HTTP round-trip with a TCP socket — catches encoding edge
//     cases sqlmock + httptest can't see
//
// What this does NOT cover:
//   - Schema migration drift (assumes the migrations dir is at the
//     conventional path; operator-customized layouts need their own
//     test)
//   - Plugin-internal recovery (kill backing store mid-request, etc.)

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	mclient "github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/client"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

const (
	bootProbeTimeout = 30 * time.Second
	bootProbeStep   = 500 * time.Millisecond
)

// requireE2EDB returns the test DSN. Skips the test (not fails) when
// the env var is unset — keeps `-tags memory_plugin_e2e` runs from
// crashing on dev machines without postgres.
func requireE2EDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MEMORY_PLUGIN_E2E_DB")
	if dsn == "" {
		t.Skip("MEMORY_PLUGIN_E2E_DB not set — skipping real-subprocess boot test")
	}
	return dsn
}

// buildBinary compiles cmd/memory-plugin-postgres/ to a temp dir.
// Returns the path of the built binary. Test cleanup deletes it.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "memory-plugin-postgres")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	// Find the cmd dir relative to this file.
	_, thisFile, _, _ := runtime.Caller(0)
	cmdDir := filepath.Dir(thisFile)
	build := exec.Command("go", "build", "-o", out, ".")
	build.Dir = cmdDir
	build.Env = os.Environ()
	if outErr, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, outErr)
	}
	return out
}

// startBinary launches the built binary with the supplied env. Returns
// the *exec.Cmd (test cleanup kills it) and the http URL it's listening
// on. Polls /v1/health until ready or times out.
func startBinary(t *testing.T, binary, dsn, listen string) (*exec.Cmd, string) {
	t.Helper()
	url := "http://" + listen
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"MEMORY_PLUGIN_DATABASE_URL="+dsn,
		"MEMORY_PLUGIN_LISTEN_ADDR="+listen,
		// Migrations dir lives next to the cmd source. The binary
		// reads it relative to cwd by default; we set the env var
		// override so the test doesn't depend on cwd.
		"MEMORY_PLUGIN_MIGRATIONS_DIR="+migrationsDirForTest(t),
	)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if t.Failed() {
			t.Logf("binary stdout:\n%s", stdout.String())
			t.Logf("binary stderr:\n%s", stderr.String())
		}
	})

	deadline := time.Now().Add(bootProbeTimeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/v1/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return cmd, url
			}
		}
		// Bail early if the binary already exited.
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("binary exited during boot: stderr:\n%s", stderr.String())
		}
		time.Sleep(bootProbeStep)
	}
	t.Fatalf("binary did not become ready within %v", bootProbeTimeout)
	return nil, ""
}

func migrationsDirForTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "migrations")
}

// TestE2E_BootAndHealth: build + start the real binary, hit /v1/health,
// confirm capabilities match what the built-in plugin declares. Catches
// "binary doesn't start" / "wrong env var name" / "panics on first
// request" classes that in-process tests miss.
func TestE2E_BootAndHealth(t *testing.T) {
	dsn := requireE2EDB(t)
	binary := buildBinary(t)
	_, url := startBinary(t, binary, dsn, "127.0.0.1:19100")
	cl := mclient.New(mclient.Config{BaseURL: url})

	hr, err := cl.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if hr.Status != "ok" {
		t.Errorf("status = %q", hr.Status)
	}
	wantCaps := map[string]bool{"fts": true, "embedding": true, "ttl": true, "pin": true, "propagation": true}
	gotCaps := map[string]bool{}
	for _, c := range hr.Capabilities {
		gotCaps[c] = true
	}
	for c := range wantCaps {
		if !gotCaps[c] {
			t.Errorf("capability %q missing — built-in plugin should declare all 5", c)
		}
	}
}

// TestE2E_FullCommitSearchForgetRoundTrip: the full agent flow against
// real postgres + real HTTP. Catches wire-format regressions (the
// pq.Array bug we hit during PR-3 development) and contract-level
// drift between Go bindings and the spec.
func TestE2E_FullCommitSearchForgetRoundTrip(t *testing.T) {
	dsn := requireE2EDB(t)
	binary := buildBinary(t)
	_, url := startBinary(t, binary, dsn, "127.0.0.1:19101")
	cl := mclient.New(mclient.Config{BaseURL: url})

	ctx := context.Background()
	ns := fmt.Sprintf("workspace:e2e-%d", time.Now().UnixNano())

	// 1. Upsert namespace.
	if _, err := cl.UpsertNamespace(ctx, ns, contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace}); err != nil {
		t.Fatalf("UpsertNamespace: %v", err)
	}
	t.Cleanup(func() { _ = cl.DeleteNamespace(context.Background(), ns) })

	// 2. Commit a memory.
	resp, err := cl.CommitMemory(ctx, ns, contract.MemoryWrite{
		Content: "user prefers tabs over spaces",
		Kind:    contract.MemoryKindFact,
		Source:  contract.MemorySourceAgent,
	})
	if err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("plugin returned empty memory id")
	}

	// 3. Search and find the memory we just wrote.
	sresp, err := cl.Search(ctx, contract.SearchRequest{Namespaces: []string{ns}, Query: "tabs"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(sresp.Memories) == 0 {
		t.Errorf("Search returned 0 memories, want at least 1")
	}
	found := false
	for _, m := range sresp.Memories {
		if m.ID == resp.ID && m.Content == "user prefers tabs over spaces" {
			found = true
			break
		}
	}
	if !found {
		got, _ := json.Marshal(sresp.Memories)
		t.Errorf("committed memory not found in search results: %s", got)
	}

	// 4. Forget the memory.
	if err := cl.ForgetMemory(ctx, resp.ID, contract.ForgetRequest{RequestedByNamespace: ns}); err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}

	// 5. Search again — gone.
	sresp, err = cl.Search(ctx, contract.SearchRequest{Namespaces: []string{ns}, Query: "tabs"})
	if err != nil {
		t.Fatalf("Search after forget: %v", err)
	}
	for _, m := range sresp.Memories {
		if m.ID == resp.ID {
			t.Errorf("forgotten memory still in search results")
		}
	}
}

// TestE2E_IdempotencyKey covers the C1 fix end-to-end: same id passed
// twice should upsert (one row, updated content), not duplicate.
func TestE2E_IdempotencyKey(t *testing.T) {
	dsn := requireE2EDB(t)
	binary := buildBinary(t)
	_, url := startBinary(t, binary, dsn, "127.0.0.1:19102")
	cl := mclient.New(mclient.Config{BaseURL: url})

	ctx := context.Background()
	ns := fmt.Sprintf("workspace:e2e-idem-%d", time.Now().UnixNano())
	if _, err := cl.UpsertNamespace(ctx, ns, contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace}); err != nil {
		t.Fatalf("UpsertNamespace: %v", err)
	}
	t.Cleanup(func() { _ = cl.DeleteNamespace(context.Background(), ns) })

	fixedID := "11111111-2222-3333-4444-555555555555"
	for i, content := range []string{"first version", "second version (updated)"} {
		if _, err := cl.CommitMemory(ctx, ns, contract.MemoryWrite{
			ID:      fixedID,
			Content: content,
			Kind:    contract.MemoryKindFact,
			Source:  contract.MemorySourceAgent,
		}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	sresp, err := cl.Search(ctx, contract.SearchRequest{Namespaces: []string{ns}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	matches := 0
	for _, m := range sresp.Memories {
		if m.ID == fixedID {
			matches++
			if m.Content != "second version (updated)" {
				t.Errorf("upsert did not update content: got %q", m.Content)
			}
		}
	}
	if matches != 1 {
		t.Errorf("upsert produced %d rows for id=%s, want 1", matches, fixedID)
	}
}
