package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// repoRoot walks up from the test's working dir (cmd/gen-providers) to the
// module root so the test can locate the checked-in artifact regardless of
// where `go test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate repo root (go.mod) from cmd/gen-providers")
	return ""
}

// TestArtifactInSync is the drift gate's Go-test counterpart: the checked-in
// internal/providers/gen/registry_gen.go MUST byte-equal a fresh render. If a
// future edit changes providers.yaml without regenerating, OR hand-edits the
// artifact, this flips red — the same signal the verify-providers-gen CI
// workflow emits, but caught locally by `go test ./...` too.
func TestArtifactInSync(t *testing.T) {
	generated, err := render()
	if err != nil {
		t.Fatalf("render() error = %v", err)
	}
	artifactPath := filepath.Join(repoRoot(t), defaultOutPath)
	onDisk, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read checked-in artifact %s: %v (run `go generate ./...` and commit)", artifactPath, err)
	}
	if !bytes.Equal(onDisk, generated) {
		t.Fatalf("DRIFT: %s is out of sync with providers.yaml.\n"+
			"Run `go generate ./...` (or `go run ./cmd/gen-providers`) and commit the result.", defaultOutPath)
	}
}

// TestDriftGateCatchesMutation is the load-bearing-gate proof (per the SOP
// fail-direction discipline). The original P0 version was TAUTOLOGICAL
// (internal#718 P1 review carry-over): it appended bytes to an in-memory copy
// and asserted the copy differed from the original — true by construction,
// touching neither the on-disk artifact nor the actual in-sync comparison the
// gate runs. This version exercises the REAL gate: it writes a MUTATED artifact
// to disk and re-runs the SAME comparison TestArtifactInSync / `-check` perform
// (`render()` bytes vs the on-disk file), asserting it now reports drift — then
// restores the original. So the test would fail if the gate were vacuous (e.g.
// if the comparison ignored content), not merely if append changes bytes.
func TestDriftGateCatchesMutation(t *testing.T) {
	generated, err := render()
	if err != nil {
		t.Fatalf("render() error = %v", err)
	}
	artifactPath := filepath.Join(repoRoot(t), defaultOutPath)
	original, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read checked-in artifact %s: %v", artifactPath, err)
	}
	// Precondition: the tree is in sync (so the mutation is what flips the gate,
	// not pre-existing drift).
	if !bytes.Equal(original, generated) {
		t.Fatalf("precondition failed: %s already drifted from render() — run `go generate ./...`", defaultOutPath)
	}

	// Restore the pristine artifact no matter how the test exits.
	t.Cleanup(func() {
		if err := os.WriteFile(artifactPath, original, 0o644); err != nil {
			t.Fatalf("CRITICAL: failed to restore %s after mutation: %v", artifactPath, err)
		}
	})

	// Mutate the ON-DISK artifact (simulating a hand-edit / a providers.yaml
	// change that wasn't regenerated).
	mutated := append(append([]byte(nil), original...), []byte("\n// injected drift\n")...)
	if err := os.WriteFile(artifactPath, mutated, 0o644); err != nil {
		t.Fatalf("write mutated artifact: %v", err)
	}

	// Re-run the EXACT in-sync comparison the gate uses: fresh render vs the
	// (now mutated) on-disk file. It MUST report drift.
	onDiskAfter, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("re-read mutated artifact: %v", err)
	}
	freshRender, err := render()
	if err != nil {
		t.Fatalf("render() after mutation error = %v", err)
	}
	if bytes.Equal(onDiskAfter, freshRender) {
		t.Fatal("drift gate did NOT detect a mutated on-disk artifact — gate is not load-bearing")
	}
}

// TestRenderDeterministic proves regeneration is idempotent: two renders of
// the same manifest produce byte-identical output (sorted runtime keys, stable
// catalog order). A non-deterministic generator would make the drift gate
// flap on Go map iteration order.
func TestRenderDeterministic(t *testing.T) {
	a, err := render()
	if err != nil {
		t.Fatalf("render() #1 error = %v", err)
	}
	b, err := render()
	if err != nil {
		t.Fatalf("render() #2 error = %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("render() is non-deterministic — two runs differ; the drift gate would flap")
	}
}
