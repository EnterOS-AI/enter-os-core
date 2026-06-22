package provisioner

// Tests for the per-platform-instance container label that prevents
// co-resident platforms (two stacks / two CI runs on one Docker daemon)
// from cross-reaping each other's live workspaces in the orphan
// sweeper's wiped-DB pass.
//
// Root bug: managedLabels() used to stamp only LabelManaged=true, which
// every Molecule platform's provisioner stamps. Platform A's wiped-DB
// sweep ("labeled container with no row in A's DB → reap") then reaped
// platform B's healthy containers (B has the row, A doesn't). The fix
// adds LabelInstance, derived from DATABASE_URL, so the sweep is scoped
// to the database that actually owns the container.

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
)

// withFreshInstanceID resets the process-lifetime memoisation of
// PlatformInstanceID so a test can drive it from a chosen DATABASE_URL.
// The production code memoises via sync.Once (one DSN per process); tests
// need to re-derive, so they swap in a fresh Once + clear the cache.
func withFreshInstanceID(t *testing.T, databaseURL string) {
	t.Helper()
	t.Setenv("DATABASE_URL", databaseURL)
	platformInstanceOnce = sync.Once{}
	platformInstanceID = ""
	t.Cleanup(func() {
		platformInstanceOnce = sync.Once{}
		platformInstanceID = ""
	})
}

// TestPlatformInstanceID_DerivedFromDatabaseURL pins the chosen identity
// source: a SHA-256 of DATABASE_URL, first 16 hex chars. Stable for a
// given DSN (so the same platform's restart re-derives the same id) and
// never leaks the raw connection string into a Docker label.
func TestPlatformInstanceID_DerivedFromDatabaseURL(t *testing.T) {
	const dsn = "postgres://u:p@db-a:5432/molecule_a?sslmode=disable"
	withFreshInstanceID(t, dsn)

	sum := sha256.Sum256([]byte(dsn))
	want := hex.EncodeToString(sum[:])[:16]

	got := PlatformInstanceID()
	if got != want {
		t.Fatalf("PlatformInstanceID() = %q, want %q (sha256(DATABASE_URL)[:16])", got, want)
	}
	if len(got) != 16 {
		t.Errorf("instance id length = %d, want 16", len(got))
	}
}

// TestPlatformInstanceID_StableAcrossCalls — the value must be identical
// across calls in one process (the provisioner stamps it and the sweeper
// filters on it; they MUST agree). Memoisation guarantees this.
func TestPlatformInstanceID_StableAcrossCalls(t *testing.T) {
	withFreshInstanceID(t, "postgres://u:p@db-a:5432/molecule_a")
	first := PlatformInstanceID()
	for i := 0; i < 5; i++ {
		if got := PlatformInstanceID(); got != first {
			t.Fatalf("PlatformInstanceID not stable: call %d = %q, want %q", i, got, first)
		}
	}
}

// TestPlatformInstanceID_DistinctPerDatabase — THE load-bearing property
// for the cross-reap fix. Two co-resident platforms point at different
// databases, so their instance ids (hence their LabelInstance stamps)
// MUST differ. If they ever collided, platform A's wiped-DB sweep could
// still reap platform B's containers.
func TestPlatformInstanceID_DistinctPerDatabase(t *testing.T) {
	withFreshInstanceID(t, "postgres://u:p@db-a:5432/molecule_a")
	idA := PlatformInstanceID()

	withFreshInstanceID(t, "postgres://u:p@db-b:5432/molecule_b")
	idB := PlatformInstanceID()

	if idA == idB {
		t.Fatalf("two distinct DATABASE_URLs produced the same instance id %q — "+
			"co-resident platforms would cross-reap", idA)
	}
}

// TestPlatformInstanceID_UnsetFallsBackToDefault — no DATABASE_URL (test
// harness / misconfig) yields the literal "default". Behaviour for a
// lone platform is unchanged from the pre-namespacing world.
func TestPlatformInstanceID_UnsetFallsBackToDefault(t *testing.T) {
	withFreshInstanceID(t, "")
	if got := PlatformInstanceID(); got != "default" {
		t.Fatalf("PlatformInstanceID() with no DATABASE_URL = %q, want %q", got, "default")
	}
}

// TestManagedLabels_StampsInstanceLabel — every provisioned container +
// volume must carry BOTH labels. The orphan sweeper's instance-scoped
// Docker filter (ListManagedContainerIDPrefixes) keys off LabelInstance;
// if the provisioner stopped stamping it, the sweep would silently match
// zero containers (filter never satisfied) and wiped-DB orphans would
// leak — but at least it would never cross-reap. The bigger regression
// guard is that the value EQUALS PlatformInstanceID(): stamping a
// different value than the sweeper filters on would make the workspace's
// OWN containers invisible to its OWN wiped-DB reap.
func TestManagedLabels_StampsInstanceLabel(t *testing.T) {
	withFreshInstanceID(t, "postgres://u:p@db-a:5432/molecule_a")

	labels := managedLabels()
	if labels[LabelManaged] != "true" {
		t.Errorf("%s = %q, want \"true\"", LabelManaged, labels[LabelManaged])
	}
	if labels[LabelInstance] != PlatformInstanceID() {
		t.Errorf("%s = %q, want PlatformInstanceID() %q",
			LabelInstance, labels[LabelInstance], PlatformInstanceID())
	}
}
