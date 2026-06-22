package handlers

import (
	"strings"
	"testing"
)

func TestMissingRequiredEnv_NoConfig(t *testing.T) {
	// Zero configFiles → nothing to validate → no missing.
	if got := missingRequiredEnv(nil, map[string]string{}); got != nil {
		t.Errorf("nil configFiles: got %v, want nil", got)
	}
	if got := missingRequiredEnv(map[string][]byte{}, map[string]string{}); got != nil {
		t.Errorf("empty configFiles: got %v, want nil", got)
	}
}

func TestMissingRequiredEnv_NoConfigYaml(t *testing.T) {
	// A map without config.yaml → no schema → no missing.
	files := map[string][]byte{
		"other.txt": []byte("irrelevant"),
	}
	if got := missingRequiredEnv(files, map[string]string{}); got != nil {
		t.Errorf("no config.yaml: got %v, want nil", got)
	}
}

func TestMissingRequiredEnv_NoRequiredEnvInYaml(t *testing.T) {
	// config.yaml without runtime_config.required_env → no missing.
	// Mirrors the default config emitted by ensureDefaultConfig (see the
	// #1028 comment in workspace_provision.go about why required_env is
	// intentionally omitted for auto-generated configs).
	yml := `
name: example
runtime: claude-code
runtime_config:
  timeout: 0
`
	files := map[string][]byte{"config.yaml": []byte(yml)}
	if got := missingRequiredEnv(files, map[string]string{}); got != nil {
		t.Errorf("no required_env in YAML: got %v, want nil", got)
	}
}

func TestMissingRequiredEnv_AllSatisfied(t *testing.T) {
	yml := `
runtime: claude-code
runtime_config:
  required_env:
    - CLAUDE_CODE_OAUTH_TOKEN
    - ANTHROPIC_API_KEY
`
	files := map[string][]byte{"config.yaml": []byte(yml)}
	env := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-...",
		"ANTHROPIC_API_KEY":       "sk-ant-api-...",
	}
	if got := missingRequiredEnv(files, env); got != nil {
		t.Errorf("all set: got %v, want nil", got)
	}
}

func TestMissingRequiredEnv_OneMissing(t *testing.T) {
	// Reproduces the reported issue: Claude Code Agent config declares
	// CLAUDE_CODE_OAUTH_TOKEN required but the tenant only has
	// ANTHROPIC_API_KEY set globally.
	yml := `
runtime: claude-code
runtime_config:
  required_env:
    - CLAUDE_CODE_OAUTH_TOKEN
    - ANTHROPIC_API_KEY
`
	files := map[string][]byte{"config.yaml": []byte(yml)}
	env := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-...",
	}
	got := missingRequiredEnv(files, env)
	if len(got) != 1 || got[0] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("expected [CLAUDE_CODE_OAUTH_TOKEN], got %v", got)
	}
}

func TestMissingRequiredEnv_EmptyStringCountsAsMissing(t *testing.T) {
	// A secret row with empty value is effectively unset; the in-container
	// preflight treats empty string as missing, so the server must match.
	yml := `
runtime_config:
  required_env: [FOO]
`
	files := map[string][]byte{"config.yaml": []byte(yml)}
	env := map[string]string{"FOO": ""}
	got := missingRequiredEnv(files, env)
	if len(got) != 1 || got[0] != "FOO" {
		t.Errorf("expected [FOO], got %v", got)
	}
}

func TestMissingRequiredEnv_MalformedYamlReturnsNil(t *testing.T) {
	// Malformed YAML should not panic and should not block provisioning —
	// the in-container preflight is the source of truth for config.yaml
	// shape, and we don't want to double-fail on parse quirks.
	files := map[string][]byte{"config.yaml": []byte("{ not: valid: yaml: [[")}
	if got := missingRequiredEnv(files, map[string]string{}); got != nil {
		t.Errorf("malformed YAML: got %v, want nil", got)
	}
}

func TestFormatMissingEnvError_Single(t *testing.T) {
	msg := formatMissingEnvError([]string{"CLAUDE_CODE_OAUTH_TOKEN"})
	if !strings.Contains(msg, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("message should name the var: %q", msg)
	}
	if !strings.Contains(msg, "retry") {
		t.Errorf("message should tell user how to fix it: %q", msg)
	}
}

func TestFormatMissingEnvError_Multiple(t *testing.T) {
	msg := formatMissingEnvError([]string{"FOO", "BAR"})
	if !strings.Contains(msg, "FOO, BAR") {
		t.Errorf("multi-var message should join with ', ' — got %q", msg)
	}
}
