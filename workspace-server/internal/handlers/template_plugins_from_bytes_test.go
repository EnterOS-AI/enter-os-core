package handlers

import "testing"

// TestParseTemplatePluginsFromBytes_SaaS pins the RFC#2843 #32 fix: declared
// plugins are parsed from raw (Gitea-fetched) config.yaml bytes — the SaaS
// source — not a local templatePath. Behavioral coverage is the
// template-delivery-e2e gate; this guards the byte-parser itself.
func TestParseTemplatePluginsFromBytes_SaaS(t *testing.T) {
	got, err := parseTemplatePluginsFromBytes([]byte("plugins:\n  - gitea://o/r/agent-skills/seo-all#main\n"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 || got[0] != "gitea://o/r/agent-skills/seo-all#main" {
		t.Fatalf("want one seo-all source, got %v", got)
	}
	if empty, err := parseTemplatePluginsFromBytes(nil); err != nil || empty != nil {
		t.Fatalf("nil bytes => (nil,nil); got (%v,%v)", empty, err)
	}
}
