package templatecache

import "testing"

func TestSafeTemplateName(t *testing.T) {
	for _, name := range []string{"seo-agent", "claude_code", "T4"} {
		if !safeTemplateName(name) {
			t.Fatalf("%q should be safe", name)
		}
	}
	for _, name := range []string{"", "../seo", "seo/agent", "seo.agent"} {
		if safeTemplateName(name) {
			t.Fatalf("%q should be rejected", name)
		}
	}
}
