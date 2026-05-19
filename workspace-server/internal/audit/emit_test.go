package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureLog redirects the std logger to a buffer for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevW := log.Writer()
	prevF := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevW)
		log.SetFlags(prevF)
	})
	fn()
	return buf.String()
}

// withTempAuditFile points MOLECULE_AUDIT_LOG_PATH at a fresh file for
// the duration of t.
func withTempAuditFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	t.Setenv("MOLECULE_AUDIT_LOG_PATH", p)
	return p
}

func TestEmit_WritesAuditPrefixedLineToStdout(t *testing.T) {
	withTempAuditFile(t)
	out := captureLog(t, func() {
		ctx := WithWorkspaceID(context.Background(), "ws-abc")
		ctx = WithUserID(ctx, "u-1")
		ctx = WithActorKind(ctx, ActorUser)
		Emit(ctx, "secret.set", map[string]any{"key": "ANTHROPIC_API_KEY"})
	})
	out = strings.TrimSpace(out)
	if !strings.HasPrefix(out, "audit: ") {
		t.Fatalf("expected 'audit: ' prefix, got %q", out)
	}
	jsonPart := strings.TrimPrefix(out, "audit: ")
	var got map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &got); err != nil {
		t.Fatalf("payload not JSON: %v (raw=%q)", err, jsonPart)
	}
	if got["event_type"] != "secret.set" {
		t.Errorf("event_type mismatch: %+v", got)
	}
	if got["workspace_id"] != "ws-abc" {
		t.Errorf("workspace_id mismatch: %+v", got)
	}
	if got["user_id"] != "u-1" {
		t.Errorf("user_id mismatch: %+v", got)
	}
	if got["actor_kind"] != "user" {
		t.Errorf("actor_kind mismatch: %+v", got)
	}
}

func TestEmit_AppendsToJSONLFile(t *testing.T) {
	path := withTempAuditFile(t)
	_ = captureLog(t, func() {
		Emit(context.Background(), "secret.set", map[string]any{"key": "X"})
		Emit(context.Background(), "secret.delete", map[string]any{"key": "Y"})
	})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("audit file unreadable: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d (raw=%q)", len(lines), b)
	}
	for i, ln := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(ln), &got); err != nil {
			t.Errorf("line %d not valid JSON: %v (%q)", i, err, ln)
		}
	}
}

func TestEmit_DefaultsActorToUserWhenUnset(t *testing.T) {
	withTempAuditFile(t)
	out := captureLog(t, func() {
		Emit(context.Background(), "secret.set", nil)
	})
	if !strings.Contains(out, `"actor_kind":"user"`) {
		t.Errorf("expected actor_kind=user default, got %q", out)
	}
}

func TestEmit_FieldsWorkspaceIDOverridesContext(t *testing.T) {
	withTempAuditFile(t)
	out := captureLog(t, func() {
		ctx := WithWorkspaceID(context.Background(), "ws-ctx")
		Emit(ctx, "secret.set", map[string]any{
			"workspace_id": "ws-override",
			"key":          "K",
		})
	})
	if !strings.Contains(out, `"workspace_id":"ws-override"`) {
		t.Errorf("fields workspace_id should win over ctx; got %q", out)
	}
	// Inner fields must NOT carry workspace_id (de-duplicated).
	if strings.Contains(out, `"fields":{"workspace_id"`) {
		t.Errorf("inner workspace_id should be deleted from fields; got %q", out)
	}
}

func TestEmit_NeverIncludesSecretValues_OnlyHash(t *testing.T) {
	// This is a contract test: the package documents that callers must
	// hash before emitting. We assert HashValuePrefix gives a stable
	// short hex and that the same value never round-trips through Emit.
	withTempAuditFile(t)
	secret := "sk-very-real-secret"
	prefix := HashValuePrefix(secret, 8)
	if len(prefix) != 8 {
		t.Fatalf("HashValuePrefix length=%d, want 8", len(prefix))
	}
	out := captureLog(t, func() {
		Emit(context.Background(), "secret.set", map[string]any{
			"key":        "TEST",
			"value_hash": prefix,
		})
	})
	if strings.Contains(out, secret) {
		t.Fatalf("audit line MUST NOT contain raw secret; got %q", out)
	}
	if !strings.Contains(out, prefix) {
		t.Errorf("expected value_hash %q in line; got %q", prefix, out)
	}
}

func TestEmit_FileAppendFailureDoesNotBlockStdout(t *testing.T) {
	// Point at an unwritable path; stdout transport must still fire.
	t.Setenv("MOLECULE_AUDIT_LOG_PATH", "/proc/this/is/not/writable/path.jsonl")
	out := captureLog(t, func() {
		Emit(context.Background(), "secret.set", map[string]any{"key": "K"})
	})
	if !strings.Contains(out, "audit: ") {
		t.Errorf("stdout audit line must fire even when file append fails; got %q", out)
	}
}

func TestEmit_Concurrent_NoInterleavedLines(t *testing.T) {
	path := withTempAuditFile(t)
	// Capture log to drop stdout noise; we're asserting file integrity.
	_ = captureLog(t, func() {
		const N = 50
		var wg sync.WaitGroup
		wg.Add(N)
		for i := 0; i < N; i++ {
			i := i
			go func() {
				defer wg.Done()
				Emit(context.Background(), "secret.set", map[string]any{"i": i})
			}()
		}
		wg.Wait()
	})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("audit file unreadable: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("expected 50 lines, got %d", len(lines))
	}
	for i, ln := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(ln), &got); err != nil {
			t.Errorf("line %d not valid JSON (interleave bug?): %v", i, err)
		}
	}
}

func TestHashValuePrefix_StableAndBounded(t *testing.T) {
	if HashValuePrefix("", 8) != "" {
		t.Errorf("empty input must return empty")
	}
	if got := HashValuePrefix("a", 8); len(got) != 8 {
		t.Errorf("len mismatch: %q", got)
	}
	// Clamp lower bound.
	if got := HashValuePrefix("a", 1); len(got) != 4 {
		t.Errorf("clamp-lo failed: %q", got)
	}
	// Clamp upper bound.
	if got := HashValuePrefix("a", 999); len(got) != 64 {
		t.Errorf("clamp-hi failed: %q", got)
	}
	// Stable across calls (same input → same prefix). Bind to vars so
	// staticcheck SA4000 does not flag the comparison as tautological;
	// the intent is to assert call-stability, which requires invoking
	// the function twice with the same input.
	a := HashValuePrefix("x", 8)
	b := HashValuePrefix("x", 8)
	if a != b {
		t.Errorf("hash not stable: a=%q b=%q", a, b)
	}
}
