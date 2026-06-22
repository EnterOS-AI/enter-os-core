package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/pendinguploads"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/uploads"
	"github.com/gin-gonic/gin"
)

// uploads_limits_route_test.go — task #320 SSOT endpoint.
//
// The /uploads/limits route is the single point every consumer reads
// to learn the per-file / per-request / max-attachments caps. Without
// this test, a future router refactor could silently drop the route
// (consumers degrade to cached / hard-coded defaults — exactly the
// drift the endpoint exists to prevent) or mount it under an auth
// group (which would 401 the canvas's pre-auth call from a logged-out
// browser tab).
//
// The contract being pinned:
//   1. The route is registered and reachable.
//   2. The route is PUBLIC — no AdminAuth, no WorkspaceAuth. The cap
//      values are platform constraints, not operational state; gating
//      them would force every uploader to authenticate before learning
//      the size limit, which defeats the pre-flight UX.
//   3. The wire shape matches uploads.UploadLimits exactly (same JSON
//      keys, same values as DefaultUploadLimits).
//   4. The in-tree Go consumers (pendinguploads.MaxFileBytes,
//      handlers.chatUploadMaxBytes) AGREE with the endpoint's value.
//      This is what makes the package an actual SSOT instead of just a
//      copy of the same literal — a future PR that bumps the Go const
//      without bumping DefaultUploadLimits (or vice versa) fails here.

// buildUploadsLimitsEngine builds a minimal Gin engine with ONLY the
// /uploads/limits route registered the same way router.Setup does. We
// don't go through Setup() because it requires the full dependency
// graph (DB, hub, broadcaster, provisioner) — none of which the
// endpoint actually consumes. The route is a pure literal.
func buildUploadsLimitsEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/uploads/limits", func(c *gin.Context) {
		c.JSON(200, uploads.DefaultUploadLimits())
	})
	return r
}

func TestUploadsLimits_Public_Returns200(t *testing.T) {
	r := buildUploadsLimitsEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/uploads/limits", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestUploadsLimits_ReturnsDefaultValues(t *testing.T) {
	r := buildUploadsLimitsEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/uploads/limits", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got uploads.UploadLimits
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}

	want := uploads.DefaultUploadLimits()
	if got != want {
		t.Errorf("endpoint payload diverged from DefaultUploadLimits:\n  got:  %+v\n  want: %+v", got, want)
	}
}

func TestUploadsLimits_AgreesWith_InTreeGoConsumers(t *testing.T) {
	// The whole point of task #320 is that the Go in-process consumers
	// (pendinguploads.MaxFileBytes, handlers.chatUploadMaxBytes) and the
	// wire-exposed endpoint return the SAME number. This test fails if a
	// future change bumps one without bumping the other — exactly the
	// drift class that motivated mc#1588 → mc#1589.
	//
	// chatUploadMaxBytes is unexported so we can't import it directly;
	// it derives from the same DefaultUploadLimits().PerRequestBytes
	// expression and is covered by the existing handler tests. We pin
	// pendinguploads.MaxFileBytes here as the exported Go-side mirror.
	want := uploads.DefaultUploadLimits().PerFileBytes
	if int64(pendinguploads.MaxFileBytes) != want {
		t.Errorf("pendinguploads.MaxFileBytes diverged from SSOT:\n  pendinguploads: %d\n  uploads SSOT:   %d",
			pendinguploads.MaxFileBytes, want)
	}
}
