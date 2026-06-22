package handlers

import (
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
)

// RFC §5.7 / #30: a kind='platform' concierge with no explicit template must
// resolve to the platform-agent template (its identity), not the generic
// claude-code-default config.
func TestConciergeTemplateOrDefault(t *testing.T) {
	cases := []struct {
		name, kind, template, want string
	}{
		{"platform empty -> platform-agent", models.KindPlatform, "", "platform-agent"},
		{"platform blank -> platform-agent", models.KindPlatform, "  ", "platform-agent"},
		{"platform explicit kept", models.KindPlatform, "custom", "custom"},
		{"workspace empty stays empty", "workspace", "", ""},
		{"workspace seo-agent kept", "workspace", "seo-agent", "seo-agent"},
	}
	for _, c := range cases {
		if got := conciergeTemplateOrDefault(c.kind, c.template); got != c.want {
			t.Errorf("%s: conciergeTemplateOrDefault(%q,%q)=%q want %q", c.name, c.kind, c.template, got, c.want)
		}
	}
}
