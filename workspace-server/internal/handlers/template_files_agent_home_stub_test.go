package handlers

// template_files_agent_home_stub_test.go — pins the Phase-1 stub
// contract for the /agent-home root added by internal#425 RFC.
//
// Today (pre-Phase-2b), every Files API verb against `?root=/agent-home`
// must return HTTP 501 with the canonical pending-message body. The
// stub MUST NOT:
//   1. Hit the DB (the workspace might not even exist yet from the
//      canvas's POV — the root selector is testable without one).
//   2. Touch the EIC tunnel / Docker / template-dir paths — those
//      would 500/404/[] depending on the env and confuse the canvas.
//   3. Accept writes/deletes that the future docker-exec backend
//      would reject — fail closed.
//
// When Phase 2b lands, this file gets replaced by a real
// docker-exec dispatch test; the stub-message constant in
// templates.go disappears.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestAgentHomeAllowedRoot pins that /agent-home is in the allowedRoots
// set. Without this, a future refactor that drops the key would
// silently degrade the canvas root selector to a 400 instead of the
// stub 501.
func TestAgentHomeAllowedRoot(t *testing.T) {
	if !allowedRoots["/agent-home"] {
		t.Fatal("/agent-home must be in allowedRoots — RFC #425 contract")
	}
}

// TestAgentHomeStub_AllVerbs_Return501 pins the canonical stub
// response across all four verbs. Each must:
//
//   - status 501
//   - body contains the canonical "/agent-home not implemented" prefix
//   - NOT contain "workspace not found" (proves we short-circuit before
//     the DB lookup)
//
// Driven as a table to keep symmetry — adding a fifth verb in the
// future means adding one row here.
func TestAgentHomeStub_AllVerbs_Return501(t *testing.T) {
	cases := []struct {
		name   string
		method string
		invoke func(c *gin.Context)
	}{
		{
			name:   "ListFiles",
			method: "GET",
			invoke: func(c *gin.Context) { (&TemplatesHandler{}).ListFiles(c) },
		},
		{
			name:   "ReadFile",
			method: "GET",
			invoke: func(c *gin.Context) { (&TemplatesHandler{}).ReadFile(c) },
		},
		{
			name:   "WriteFile",
			method: "PUT",
			invoke: func(c *gin.Context) { (&TemplatesHandler{}).WriteFile(c) },
		},
		{
			name:   "DeleteFile",
			method: "DELETE",
			invoke: func(c *gin.Context) { (&TemplatesHandler{}).DeleteFile(c) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{
				{Key: "id", Value: "ws-stub"},
				// Path param without leading slash so DeleteFile's
				// filepath.IsAbs guard doesn't 400 before the root
				// dispatch runs. The List/Read/Write paths strip the
				// leading slash themselves and accept either form.
				{Key: "path", Value: "notes.md"},
			}
			// WriteFile binds JSON; provide a minimal valid body so the
			// short-circuit isn't masked by the bind-error path.
			var body string
			if tc.method == "PUT" {
				body = `{"content":"x"}`
			}
			c.Request = httptest.NewRequest(
				tc.method,
				"/workspaces/ws-stub/files/notes.md?root=/agent-home",
				strings.NewReader(body),
			)
			if body != "" {
				c.Request.Header.Set("Content-Type", "application/json")
			}

			tc.invoke(c)

			if w.Code != http.StatusNotImplemented {
				t.Fatalf("expected 501, got %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "/agent-home not implemented") {
				t.Errorf("body should contain canonical stub message; got %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "workspace not found") {
				t.Errorf("stub leaked through to DB lookup; body=%s", w.Body.String())
			}
		})
	}
}
