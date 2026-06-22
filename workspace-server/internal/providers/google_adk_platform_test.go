package providers

import "testing"

// Proper-SSOT (task #65): google-adk keyless Gemini resolves to the closed
// platform provider -> IsPlatform=true; BYOK AI Studio -> google. The
// platform: select ids are registered so workspace-create accepts them
// (was 422 UNREGISTERED_MODEL_FOR_RUNTIME).
func TestGoogleADK_PlatformGeminiResolvesToPlatform(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"platform:gemini-2.5-pro", "platform:gemini-2.5-flash"} {
		p, err := m.DeriveProvider("google-adk", id, nil)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if p.Name != PlatformProviderName || !p.IsPlatform() {
			t.Errorf("%s -> %q IsPlatform=%v; want platform", id, p.Name, p.IsPlatform())
		}
	}
	p, err := m.DeriveProvider("google-adk", "gemini-2.5-pro", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.IsPlatform() || p.Name != "google" {
		t.Errorf("gemini-2.5-pro -> %q IsPlatform=%v; want google byok", p.Name, p.IsPlatform())
	}
	models, _ := m.ModelsForRuntime("google-adk")
	want := map[string]bool{"platform:gemini-2.5-pro": false, "platform:gemini-2.5-flash": false}
	for _, id := range models {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, ok := range want {
		if !ok {
			t.Errorf("%s not registered for google-adk — create would 422", id)
		}
	}
}
