package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Issue #1733: every legacy SQL-path test in this file was removed when
// the v1 fallback was deleted from AdminMemoriesHandler. The v2-plugin
// coverage (the only path now) lives in admin_memories_cutover_test.go:
//
//   - TestExport_RoutesThroughPluginWhenCutoverActive
//   - TestExport_DeduplicatesByMemoryID
//   - TestExport_SkipsWorkspaceWhenResolverFails
//   - TestExport_SkipsWorkspaceWhenPluginSearchFails
//   - TestExport_WorkspacesQueryFails
//   - TestExport_EmptyReadable
//   - TestExport_RedactsSecretsInPluginPath
//   - TestExport_BatchesPluginCallsByRoot
//   - TestExport_IncludesEveryMembersPrivateNamespace
//   - TestImport_RoutesThroughPluginWhenCutoverActive
//   - TestImport_SkipsUnknownWorkspace
//   - TestImport_PluginUpsertNamespaceError
//   - TestImport_PluginCommitError
//   - TestImport_RedactsBeforePluginSeesContent
//   - TestImport_SkipsUnknownScope
//   - TestImport_SkipsWhenResolverErrors
//   - TestExport_503WhenPluginNotWired   (new in A1)
//   - TestImport_503WhenPluginNotWired   (new in A1)
//
// Only the JSON-envelope rejection test stays here because it runs
// before the plugin gate.

// TestAdminMemories_Import_InvalidJSON verifies that a malformed
// payload is rejected with HTTP 400 before any plugin or DB call is
// attempted. This guards the request-decode path independent of the
// memory backend choice.
func TestAdminMemories_Import_InvalidJSON(t *testing.T) {
	_ = setupTestDB(t)
	h := NewAdminMemoriesHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/memories/import", bytes.NewReader([]byte("not json")))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Import(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
