package handlers

import (
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
)

// Proper-SSOT (task #65): required_env is DERIVED from the resolved provider's
// serving classification (IsPlatform), not hand-authored — platform injects
// creds server-side (none required), BYOK requires its auth_env.
func TestRequiredEnvForRegistryProvider(t *testing.T) {
	if got := requiredEnvForRegistryProvider(providers.Provider{Name: providers.PlatformProviderName}); got != nil {
		t.Errorf("platform provider requiredEnv = %v; want nil (creds injected server-side)", got)
	}
	byok := providers.Provider{Name: "google", AuthEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}
	got := requiredEnvForRegistryProvider(byok)
	if len(got) != 2 || got[0] != "GEMINI_API_KEY" {
		t.Errorf("byok requiredEnv = %v; want its auth_env", got)
	}
}
