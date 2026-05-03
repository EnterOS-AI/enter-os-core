package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// resolveRuntimeImage returns the digest-pinned image ref for a runtime when
// an operator has promoted one via the runtime_image_pins table (#2272 layer 1),
// otherwise "" so the caller falls back to the legacy `:latest` lookup in
// provisioner.RuntimeImages.
//
// Policy: availability over pinning. Any DB hiccup (sql.ErrNoRows is the
// steady-state when nothing is pinned, but transient network blips, table
// missing post-rollback, etc.) returns "" and the provision continues on
// the moving tag — better one workspace on a slightly-newer image than a
// provision-blocked tenant.
//
// WORKSPACE_IMAGE_LOCAL_OVERRIDE=1 short-circuits the lookup entirely so a
// developer rebuilding template images locally gets their fresh build via
// `:latest` even when a remote digest is pinned for the same runtime.
func resolveRuntimeImage(ctx context.Context, runtime string) string {
	if runtime == "" {
		return ""
	}
	base, ok := provisioner.RuntimeImages[runtime]
	if !ok {
		// Unknown runtime — let provisioner.Start fall through to its own
		// DefaultImage. Querying the pin table for a runtime that doesn't
		// exist would only produce noise and a guaranteed ErrNoRows.
		return ""
	}
	if os.Getenv("WORKSPACE_IMAGE_LOCAL_OVERRIDE") != "" {
		return ""
	}
	if db.DB == nil {
		return ""
	}

	var digest string
	err := db.DB.QueryRowContext(ctx,
		`SELECT digest FROM runtime_image_pins WHERE template_name = $1`, runtime,
	).Scan(&digest)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("resolveRuntimeImage: pin lookup for %q failed (%v) — falling back to :latest", runtime, err)
		}
		return ""
	}

	// Strip the moving tag suffix (`:latest`, `:staging`) before appending
	// the immutable digest. Docker treats `name:tag@sha256:...` as valid
	// but the tag is ignored; dropping it keeps logs and admin diffs honest
	// about what's actually being pulled.
	pinned := base
	if idx := strings.LastIndex(pinned, ":"); idx > strings.LastIndex(pinned, "/") {
		pinned = pinned[:idx]
	}
	return pinned + "@" + digest
}
