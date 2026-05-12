package handlers

// workspace_crud_helpers_test.go — tests for pure-logic helpers in workspace_crud.go.
//
// Covered helpers:
//   validateWorkspaceDir — bind-mount path safety (CWE-22 defence-in-depth)

import "testing"

// ─────────────────────────────────────────────────────────────────────────────
// validateWorkspaceDir
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateWorkspaceDir_AcceptsValidAbsolutePath(t *testing.T) {
	cases := []string{
		"/home/ubuntu/workspace",
		"/opt/myapp/data",
		"/tmp/molecule-workspace",
		"/Users/admin/workspace",
		"/workspace",
		"/mnt/volumes/data",
		"/srv/molecule",
		"/nix/store",
	}
	for _, dir := range cases {
		err := validateWorkspaceDir(dir)
		if err != nil {
			t.Errorf("validateWorkspaceDir(%q) returned error: %v; want nil", dir, err)
		}
	}
}

func TestValidateWorkspaceDir_RejectsRelativePath(t *testing.T) {
	cases := []string{
		"relative/path",
		"./local",
		"../sibling",
		"workspace",
		"",
	}
	for _, dir := range cases {
		err := validateWorkspaceDir(dir)
		if err == nil {
			t.Errorf("validateWorkspaceDir(%q) = nil; want error (relative path)", dir)
		}
	}
}

func TestValidateWorkspaceDir_RejectsTraversalSequence(t *testing.T) {
	cases := []string{
		"/etc/../../../etc/passwd",
		"/home/user/../../root",
		"/workspace/../../../sibling",
		"/foo/bar/..%2f..%2fetc",
		"/valid/../etc/passwd",
	}
	for _, dir := range cases {
		err := validateWorkspaceDir(dir)
		if err == nil {
			t.Errorf("validateWorkspaceDir(%q) = nil; want error (traversal)", dir)
		}
	}
}

func TestValidateWorkspaceDir_RejectsSystemPaths(t *testing.T) {
	// System paths must be rejected outright — a workspace binding /etc or
	// /proc would let the agent read host secrets or inspect kernel state.
	systemPaths := []string{
		"/etc",
		"/var",
		"/proc",
		"/sys",
		"/dev",
		"/boot",
		"/sbin",
		"/bin",
		"/usr",
	}
	for _, dir := range systemPaths {
		err := validateWorkspaceDir(dir)
		if err == nil {
			t.Errorf("validateWorkspaceDir(%q) = nil; want error (system path)", dir)
		}
	}
}

func TestValidateWorkspaceDir_RejectsDescendantsOfSystemPaths(t *testing.T) {
	// A descendant of a system path must also be rejected — /etc/shadow,
	// /proc/1/cmdline, /dev/null all fall in this category.
	descendants := []string{
		"/etc/passwd",
		"/etc/shadow",
		"/etc/ssh/sshd_config",
		"/var/log/syslog",
		"/proc/self/environ",
		"/sys/kernel/version",
		"/dev/null",
		"/boot/grub/grub.cfg",
		"/sbin/init",
		"/bin/bash",
		"/usr/bin/python3",
	}
	for _, dir := range descendants {
		err := validateWorkspaceDir(dir)
		if err == nil {
			t.Errorf("validateWorkspaceDir(%q) = nil; want error (descendant of system path)", dir)
		}
	}
}

func TestValidateWorkspaceDir_AcceptsPathsSimilarToSystemPaths(t *testing.T) {
	// Paths that LOOK like system paths but are NOT exact matches or
	// descendants should be accepted. These are valid workspace directories.
	valid := []string{
		"/etcworkspace",
		"/varworkspace",
		"/procworkspace",
		"/sysworkspace",
		"/devworkspace",
		"/bootworkspace",
		"/sbinworkspace",
		"/binworkspace",
		"/usrworkspace",
		"/etx",    // typo of /etc but a different path
		"/vartmp",  // /var/tmp is different from /var
		"/usrr",    // typo of /usr but a different path
		"/workspace/etc",
		"/workspace/var",
		"/home/user/etc",
		"/opt/etc",
	}
	for _, dir := range valid {
		err := validateWorkspaceDir(dir)
		if err != nil {
			t.Errorf("validateWorkspaceDir(%q) returned error: %v; want nil", dir, err)
		}
	}
}

func TestValidateWorkspaceDir_ErrorMessages(t *testing.T) {
	// Error messages must be descriptive enough for operators to self-diagnose.
	relErr := validateWorkspaceDir("relative")
	if relErr == nil {
		t.Fatal("relative path: want error, got nil")
	}
	if relErr.Error() == "" {
		t.Error("relative path error message is empty")
	}

	travErr := validateWorkspaceDir("/etc/../../../etc/passwd")
	if travErr == nil {
		t.Fatal("traversal: want error, got nil")
	}
	if travErr.Error() == "" {
		t.Error("traversal error message is empty")
	}

	sysErr := validateWorkspaceDir("/etc")
	if sysErr == nil {
		t.Fatal("system path: want error, got nil")
	}
	if sysErr.Error() == "" {
		t.Error("system path error message is empty")
	}
}
