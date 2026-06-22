package handlers

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestStoredWorkspaceTemplate pins the RFC#2843 #33 restart-restore reader:
// the auto-restart cycle reads the persisted workspaces.template so the SaaS
// re-provision re-delivers config.yaml + prompts (TemplateIdentity is derived
// from payload.Template) instead of degrading to a 218-byte stub.
func TestStoredWorkspaceTemplate(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "ws-tmpl-1"

	t.Run("returns persisted template", func(t *testing.T) {
		mock.ExpectQuery(`SELECT COALESCE\(template, ''\) FROM workspaces WHERE id`).
			WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows([]string{"template"}).AddRow("seo-agent"))
		if got := storedWorkspaceTemplate(context.Background(), wsID); got != "seo-agent" {
			t.Fatalf("storedWorkspaceTemplate = %q, want seo-agent", got)
		}
	})

	t.Run("empty template → empty string (default/blank workspace)", func(t *testing.T) {
		mock.ExpectQuery(`SELECT COALESCE\(template, ''\) FROM workspaces WHERE id`).
			WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows([]string{"template"}).AddRow(""))
		if got := storedWorkspaceTemplate(context.Background(), wsID); got != "" {
			t.Fatalf("storedWorkspaceTemplate = %q, want empty", got)
		}
	})
}
