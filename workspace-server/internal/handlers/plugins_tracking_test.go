package handlers

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestValidateTrackedRef: pin the exact set of accepted track values
// the install endpoint stores. Drift detector reads this column; any
// value that slips through here without structural validation would
// silently fail at drift-check time.
func TestValidateTrackedRef(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		// Defaults
		{"", "none", false},
		{"   ", "none", false},
		{"none", "none", false},

		// Tag shape
		{"tag:v1.0.0", "tag:v1.0.0", false},
		{"tag:v0.4.0-gitea.1", "tag:v0.4.0-gitea.1", false},
		{"tag:latest", "tag:latest", false},

		// SHA shape
		{"sha:abc123", "sha:abc123", false},
		{"sha:0123456789abcdef0123456789abcdef01234567", "sha:0123456789abcdef0123456789abcdef01234567", false},

		// Reject malformed
		{"tag:", "", true},      // empty after prefix
		{"sha:", "", true},      // empty after prefix
		{"latest", "", true},    // bare 'latest' is ambiguous (tag? branch?)
		{"main", "", true},      // bare branch name not allowed
		{"v1.0.0", "", true},    // missing tag: prefix
		{"random", "", true},    // not in allowlist
		{"tag", "", true},       // prefix without separator
	}
	for _, tc := range cases {
		got, err := validateTrackedRef(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("validateTrackedRef(%q) = (%q, nil); want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("validateTrackedRef(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("validateTrackedRef(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestRecordWorkspacePluginInstall_PrivilegedPluginEntitlement mirrors the
// recordDeclaredPlugin gate in the INSTALL path (workspace_plugins). The
// privileged org-management MCP plugin must only be installable on the
// kind='platform' concierge; any other workspace must be refused before the
// row is written.
func TestRecordWorkspacePluginInstall_PrivilegedPluginEntitlement(t *testing.T) {
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	const installInsert = `INSERT INTO workspace_plugins`

	t.Run("platform concierge MAY install the privileged management MCP", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(installInsert).
			WithArgs("ws-concierge", conciergePlatformMCPName, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := recordWorkspacePluginInstall(context.Background(), "ws-concierge", conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp", "none", "abc123"); err != nil {
			t.Fatalf("platform concierge install of the management MCP must succeed: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("non-platform workspace is REFUSED — no INSERT (privilege-escalation guard)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-user").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))
		// NO ExpectExec: the gate MUST refuse before any INSERT fires.
		err := recordWorkspacePluginInstall(context.Background(), "ws-user", conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp", "none", "abc123")
		if err == nil {
			t.Fatal("a non-platform workspace MUST NOT be able to install the privileged management MCP plugin")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an INSERT fired — that is the privilege escalation this gate must stop): %v", err)
		}
	})

	t.Run("an ordinary plugin skips the kind precheck entirely (no extra query)", func(t *testing.T) {
		mock := setupTestDB(t)
		// No kind precheck for non-privileged names — straight to the upsert.
		mock.ExpectExec(installInsert).
			WithArgs("ws-user", "browser-automation", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := recordWorkspacePluginInstall(context.Background(), "ws-user", "browser-automation", "gitea://molecule-ai/plugin-browser-automation", "none", "abc123"); err != nil {
			t.Fatalf("ordinary plugin install must succeed: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}
