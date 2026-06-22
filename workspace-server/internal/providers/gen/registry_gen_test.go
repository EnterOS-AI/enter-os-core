package gen

import (
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
)

// TestGeneratedProjectionMatchesManifest proves the checked-in artifact is a
// FAITHFUL projection of the live manifest — not just byte-stable, but
// semantically correct. The byte-level drift gate (cmd/gen-providers
// TestArtifactInSync) proves "regen produces this file"; this proves "this
// file's DATA equals the loader's data", so a consumer reading the artifact
// (P1+) sees exactly what the loader sees.
func TestGeneratedProjectionMatchesManifest(t *testing.T) {
	m, err := providers.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}

	if SchemaVersion != providers.SchemaVersion() {
		t.Errorf("generated SchemaVersion = %d, manifest = %d", SchemaVersion, providers.SchemaVersion())
	}

	if len(Providers) != len(m.Providers) {
		t.Fatalf("generated %d providers, manifest has %d", len(Providers), len(m.Providers))
	}
	for i, gp := range Providers {
		mp := m.Providers[i]
		if gp.Name != mp.Name {
			t.Errorf("provider[%d] name: gen=%q manifest=%q", i, gp.Name, mp.Name)
		}
		if gp.ModelPrefixMatch != mp.ModelPrefixMatch {
			t.Errorf("provider %q model_prefix_match: gen=%q manifest=%q", gp.Name, gp.ModelPrefixMatch, mp.ModelPrefixMatch)
		}
		if gp.AuthMode != mp.AuthMode {
			t.Errorf("provider %q auth_mode: gen=%q manifest=%q", gp.Name, gp.AuthMode, mp.AuthMode)
		}
		if gp.IsPlatform != mp.IsPlatform() {
			t.Errorf("provider %q IsPlatform: gen=%v manifest=%v", gp.Name, gp.IsPlatform, mp.IsPlatform())
		}
	}

	if len(Runtimes) != len(m.Runtimes) {
		t.Fatalf("generated %d runtimes, manifest has %d", len(Runtimes), len(m.Runtimes))
	}
	for rt, native := range m.Runtimes {
		genRefs, ok := Runtimes[rt]
		if !ok {
			t.Errorf("runtime %q missing from generated artifact", rt)
			continue
		}
		if len(genRefs) != len(native.Providers) {
			t.Errorf("runtime %q: gen has %d refs, manifest has %d", rt, len(genRefs), len(native.Providers))
			continue
		}
		for i, ref := range native.Providers {
			if genRefs[i].Name != ref.Name {
				t.Errorf("runtime %q ref[%d] name: gen=%q manifest=%q", rt, i, genRefs[i].Name, ref.Name)
			}
			if len(genRefs[i].Models) != len(ref.Models) {
				t.Errorf("runtime %q ref %q models count: gen=%d manifest=%d", rt, ref.Name, len(genRefs[i].Models), len(ref.Models))
			}
		}
	}
}

// TestExactlyOnePlatformProvider guards the closed-set invariant in the
// generated projection: the platform-managed provider is a single, core-only
// entry. A federation merge that introduced a second IsPlatform=true provider
// (a forged platform) would flip this red.
func TestExactlyOnePlatformProvider(t *testing.T) {
	count := 0
	for _, p := range Providers {
		if p.IsPlatform {
			count++
			if p.Name != "platform" {
				t.Errorf("IsPlatform provider has unexpected name %q (platform is core-only, name must be %q)", p.Name, "platform")
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 platform provider in the generated catalog, got %d", count)
	}
}
