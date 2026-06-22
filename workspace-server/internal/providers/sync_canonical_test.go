package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// sync_canonical_test.go — hermetic half of the canonical↔synced-copy drift
// gate (internal#718 P2-A).
//
// molecule-core's providers.yaml is a SYNCED COPY of the canonical SSOT in
// molecule-controlplane internal/providers/providers.yaml. The live cross-repo
// byte-compare lives in the sync-providers-yaml CI workflow (it fetches the
// canonical from CP and diffs). This test is the HERMETIC backstop: it pins the
// sha256 of the embedded synced copy to the value the canonical produced at sync
// time, so a HAND-EDIT of core's copy (or a partial sync) flips red locally and
// in `go test ./...` even when CI cannot reach controlplane.
//
// When the canonical legitimately changes, the sync procedure is:
//  1. Copy controlplane internal/providers/providers.yaml verbatim over this
//     copy.
//  2. `go generate ./...` to regenerate the artifact (verify-providers-gen).
//  3. Update canonicalProvidersYAMLSHA256 below to the new sha (the failure
//     message prints the observed sha to paste in).
// The deliberate constant bump is the human checkpoint that a registry change
// was consciously re-synced into core, not silently forked.

// canonicalProvidersYAMLSHA256 is the sha256 of the canonical providers.yaml as
// synced from molecule-controlplane. Bumped deliberately on each re-sync (see
// file doc). Cross-checked live by the sync-providers-yaml CI workflow.
const canonicalProvidersYAMLSHA256 = "99884faf2defa6f8213cc531304aa6c80b97537a92014d2627aa72fad4c4cdab"

func TestSyncedYAMLMatchesCanonicalSHA(t *testing.T) {
	sum := sha256.Sum256(embeddedYAML)
	got := hex.EncodeToString(sum[:])
	if got != canonicalProvidersYAMLSHA256 {
		t.Fatalf("embedded providers.yaml sha256 = %s, pinned canonical = %s\n"+
			"If you intentionally re-synced the canonical from molecule-controlplane, "+
			"update canonicalProvidersYAMLSHA256 to %s and regenerate (`go generate ./...`).\n"+
			"If you did NOT mean to edit core's copy, revert it — the canonical SSOT is "+
			"molecule-controlplane internal/providers/providers.yaml, not this synced copy.",
			got, canonicalProvidersYAMLSHA256, got)
	}
}
