package handlers

import (
	"testing"
)

// ── validateWorkspaceDir ───────────────────────────────────────────────────────

func TestValidateWorkspaceDir_RelativeRejected(t *testing.T) {
	cases := []string{
		"relative/path",
		"./myworkspace",
		"~/workspaces/dev",
	}
	for _, dir := range cases {
		t.Run(dir, func(t *testing.T) {
			if err := validateWorkspaceDir(dir); err == nil {
				t.Errorf("validateWorkspaceDir(%q): expected error (relative path), got nil", dir)
			}
		})
	}
}

func TestValidateWorkspaceDir_TraversalRejected(t *testing.T) {
	cases := []string{
		"/opt/molecule/../../../etc",
		"/workspaces/dev/../../root",
		"/opt/../opt/../etc",
	}
	for _, dir := range cases {
		t.Run(dir, func(t *testing.T) {
			if err := validateWorkspaceDir(dir); err == nil {
				t.Errorf("validateWorkspaceDir(%q): expected error (traversal), got nil", dir)
			}
		})
	}
}

func TestValidateWorkspaceDir_SystemPathsRejected(t *testing.T) {
	cases := []string{
		"/etc",
		"/etc/molecule",
		"/var",
		"/var/log",
		"/proc",
		"/proc/self",
		"/sys",
		"/sys/kernel",
		"/dev",
		"/dev/null",
		"/boot",
		"/sbin",
		"/bin",
		"/lib",
		"/usr",
		"/usr/local",
	}
	for _, dir := range cases {
		t.Run(dir, func(t *testing.T) {
			if err := validateWorkspaceDir(dir); err == nil {
				t.Errorf("validateWorkspaceDir(%q): expected error (system path), got nil", dir)
			}
		})
	}
}

func TestValidateWorkspaceDir_PrefixMatchesBlocked(t *testing.T) {
	// The blocklist checks prefix so /etc/foo must also be rejected.
	cases := []string{
		"/etc/molecule-config",
		"/var/log/workspace",
		"/usr/local/bin",
		"/usr/bin/molecule",
	}
	for _, dir := range cases {
		t.Run(dir, func(t *testing.T) {
			if err := validateWorkspaceDir(dir); err == nil {
				t.Errorf("validateWorkspaceDir(%q): expected error (prefix of blocked path), got nil", dir)
			}
		})
	}
}

// ── validateWorkspaceFields ────────────────────────────────────────────────────

func TestValidateWorkspaceFields_AllEmpty(t *testing.T) {
	// All empty → valid (creation uses defaults; empty is allowed)
	if err := validateWorkspaceFields("", "", "", ""); err != nil {
		t.Errorf("validateWorkspaceFields with all empty: expected nil, got %v", err)
	}
}

func TestValidateWorkspaceFields_ModelTooLong(t *testing.T) {
	longModel := make([]byte, 101)
	for i := range longModel {
		longModel[i] = 'x'
	}
	if err := validateWorkspaceFields("", "", string(longModel), ""); err == nil {
		t.Error("model > 100 chars: expected error, got nil")
	}
}

func TestValidateWorkspaceFields_RuntimeTooLong(t *testing.T) {
	longRuntime := make([]byte, 101)
	for i := range longRuntime {
		longRuntime[i] = 'x'
	}
	if err := validateWorkspaceFields("", "", "", string(longRuntime)); err == nil {
		t.Error("runtime > 100 chars: expected error, got nil")
	}
}

func TestValidateWorkspaceFields_CRLFInRole(t *testing.T) {
	if err := validateWorkspaceFields("", "Backend\r\nEngineer", "", ""); err == nil {
		t.Error("role with \\r\\n: expected error, got nil")
	}
}

func TestValidateWorkspaceFields_NewlineInModel(t *testing.T) {
	if err := validateWorkspaceFields("", "", "gpt-\n4o", ""); err == nil {
		t.Error("model with \\n: expected error, got nil")
	}
}

func TestValidateWorkspaceFields_NewlineInRuntime(t *testing.T) {
	if err := validateWorkspaceFields("", "", "", "lang\rgraph"); err == nil {
		t.Error("runtime with \\r: expected error, got nil")
	}
}

func TestValidateWorkspaceFields_YAMLSpecialChars(t *testing.T) {
	// yamlSpecialChars = "{}[]|>*&!"
	// These must be rejected in name and role.
	dangerous := []string{
		"Workspace{evil}",
		"Workspace[evil]",
		"Workspace]evil[",
		"Workspace|evil",
		"Workspace>evil",
		"Workspace*evil",
		"Workspace&evil",
		"Workspace!evil",
		"Name{}",
		"Role[]",
	}
	for _, v := range dangerous {
		t.Run(v, func(t *testing.T) {
			if err := validateWorkspaceFields(v, "", "", ""); err == nil {
				t.Errorf("name %q: expected error (YAML special char), got nil", v)
			}
		})
	}
}

func TestValidateWorkspaceFields_YAMLCharsAllowedInModelRuntime(t *testing.T) {
	// YAML special chars are only blocked in name/role, not model/runtime.
	if err := validateWorkspaceFields("", "", "model{}[]", "runtime*&!"); err != nil {
		t.Errorf("model/runtime with YAML chars: expected nil, got %v", err)
	}
}

func TestValidateWorkspaceFields_YAMLCharsAllowedInEmptyName(t *testing.T) {
	// Empty name is fine; YAML char restriction is only on non-empty values.
	if err := validateWorkspaceFields("", "Backend Engineer", "", ""); err != nil {
		t.Errorf("empty name with valid role: expected nil, got %v", err)
	}
}

// ─── validateWorkspaceID ───────────────────────────────────────────────────────

func TestValidateWorkspaceID_ValidUUIDv4(t *testing.T) {
	if err := validateWorkspaceID("550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Errorf("valid v4 UUID: expected nil, got %v", err)
	}
}

func TestValidateWorkspaceID_ValidUUIDv1(t *testing.T) {
	// UUIDv1 format is also accepted by uuid.Parse.
	if err := validateWorkspaceID("6ba7b810-9dad-11d1-80b4-00c04fd430c8"); err != nil {
		t.Errorf("valid v1 UUID: expected nil, got %v", err)
	}
}

func TestValidateWorkspaceID_EmptyString(t *testing.T) {
	if err := validateWorkspaceID(""); err == nil {
		t.Error("empty string: expected error, got nil")
	}
}

func TestValidateWorkspaceID_NotAUuid(t *testing.T) {
	if err := validateWorkspaceID("not-a-uuid"); err == nil {
		t.Error("not-a-uuid: expected error, got nil")
	}
}

func TestValidateWorkspaceID_WrongLength(t *testing.T) {
	if err := validateWorkspaceID("550e8400-e29b-41d4-a716"); err == nil {
		t.Error("short UUID: expected error, got nil")
	}
}

func TestValidateWorkspaceID_InvalidCharacters(t *testing.T) {
	// 'g' is not a valid hex character.
	if err := validateWorkspaceID("550e8400-e29b-41d4-a716-44665544000g"); err == nil {
		t.Error("invalid hex char: expected error, got nil")
	}
}
