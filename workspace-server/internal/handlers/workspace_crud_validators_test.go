package handlers

import (
	"testing"
)

// ── validateWorkspaceID ─────────────────────────────────────────────────────────

func TestValidateWorkspaceID_Valid(t *testing.T) {
	cases := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			if err := validateWorkspaceID(id); err != nil {
				t.Errorf("validateWorkspaceID(%q) returned error: %v", id, err)
			}
		})
	}
}

func TestValidateWorkspaceID_Invalid(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"not a UUID", "not-a-uuid"},
		{"traversal attack", "../../etc/passwd"},
		{"SQL injection", "'; DROP TABLE workspaces;--"},
		{"UUID too short", "550e8400-e29b-41d4-a716"},
		{"UUID with invalid hex chars", "550e8400-e29b-41d4-a716-44665544000g"},
		{"UUID all zeros", "00000000000000000000000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateWorkspaceID(tc.id); err == nil {
				t.Errorf("validateWorkspaceID(%q): expected error, got nil", tc.id)
			}
		})
	}
}

// ── validateWorkspaceDir ───────────────────────────────────────────────────────

func TestValidateWorkspaceDir_Valid(t *testing.T) {
	cases := []string{
		"/opt/molecule/workspaces/dev",
		"/home/user/.molecule/workspaces",
		"/var/data/workspace-abc-123",
		"/opt/services/molecule/tenant-workspaces",
	}
	for _, dir := range cases {
		t.Run(dir, func(t *testing.T) {
			if err := validateWorkspaceDir(dir); err != nil {
				t.Errorf("validateWorkspaceDir(%q) returned error: %v", dir, err)
			}
		})
	}
}

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

func TestValidateWorkspaceFields_Valid(t *testing.T) {
	if err := validateWorkspaceFields("My Workspace", "Backend Engineer", "gpt-4o", "langgraph"); err != nil {
		t.Errorf("validateWorkspaceFields with valid args: expected nil, got %v", err)
	}
}

func TestValidateWorkspaceFields_NameTooLong(t *testing.T) {
	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}
	if err := validateWorkspaceFields(string(longName), "", "", ""); err == nil {
		t.Error("name > 255 chars: expected error, got nil")
	}

	// Exactly 255 chars is OK
	validName := make([]byte, 255)
	for i := range validName {
		validName[i] = 'a'
	}
	if err := validateWorkspaceFields(string(validName), "", "", ""); err != nil {
		t.Errorf("name exactly 255 chars: expected nil, got %v", err)
	}
}

func TestValidateWorkspaceFields_RoleTooLong(t *testing.T) {
	longRole := make([]byte, 1001)
	for i := range longRole {
		longRole[i] = 'x'
	}
	if err := validateWorkspaceFields("", string(longRole), "", ""); err == nil {
		t.Error("role > 1000 chars: expected error, got nil")
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

func TestValidateWorkspaceFields_NewlineInName(t *testing.T) {
	if err := validateWorkspaceFields("My\nWorkspace", "", "", ""); err == nil {
		t.Error("name with \\n: expected error, got nil")
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
