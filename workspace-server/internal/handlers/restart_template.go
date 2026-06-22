package handlers

import (
	"log"
	"os"
	"path/filepath"
)

// restartTemplateInput is the subset of the /workspaces/:id/restart request
// body that affects which config source the provisioner uses. Extracted as
// a type so `resolveRestartTemplate` has a single pure-function signature
// for unit tests — no gin context, no DB, no filesystem writes.
type restartTemplateInput struct {
	// Template is an explicit template dir name from the request body.
	// Always honoured when resolvable — caller asked by name, that's
	// unambiguous consent to overwrite the config volume.
	Template string
	// ApplyTemplate opts the caller in to name-based auto-match AND the
	// runtime-default fallback. Without this flag a restart MUST NOT
	// overwrite the user's config volume — a user who edited their
	// model/provider/skills/prompts via the Canvas Config tab and hit
	// Save+Restart expects their edits to survive. The previous behaviour
	// (name-based auto-match unconditionally) silently reverted edits for
	// any workspace whose name matched a template dir (e.g. "Hermes Agent"
	// → hermes/), which is the regression this fix closes.
	ApplyTemplate bool
	// RebuildConfig (#239) is the recovery signal used when the workspace's
	// config volume was destroyed out-of-band. Tries org-templates as a
	// last-resort source so the workspace can self-heal without admin
	// intervention. Orthogonal to ApplyTemplate.
	RebuildConfig bool
}

// resolveRestartTemplate chooses the config source for a restart in the
// documented priority order:
//
//  1. Explicit `Template` from the request body (always honoured).
//  2. `ApplyTemplate=true` → name-based auto-match via findTemplateByName.
//  3. `RebuildConfig=true` → org-templates recovery fallback (#239).
//  4. `ApplyTemplate=true` + non-empty dbRuntime → runtime-default template
//     (e.g. `hermes-default/`) for runtime-change workflows.
//  5. Persisted `dbTemplate` (set by PATCH /workspaces/:id/template) when no
//     explicit body template was supplied. This makes `PATCH template=...`
//     followed by a plain `POST /restart` actually apply the newly stored
//     template instead of silently reusing the existing config volume.
//  6. Fall through → empty path + "existing-volume" label. Provisioner
//     reuses the workspace's existing config volume from the previous run.
//
// Returns (templatePath, configLabel). An empty templatePath is the signal
// to the provisioner that the existing volume is authoritative — the flow
// that preserves user edits.
//
// Pure function: no writes, no DB access, no network. Safe to unit-test
// with just a temp directory.
func resolveRestartTemplate(configsDir, wsName, dbRuntime, dbTemplate string, body restartTemplateInput) (templatePath, configLabel string) {
	template := body.Template

	// Tier 2: name-based auto-match, gated on ApplyTemplate.
	if template == "" && body.ApplyTemplate {
		template = findTemplateByName(configsDir, wsName)
	}

	// Tier 1 + 2 resolve via the same code path — validate + stat.
	if template != "" {
		candidatePath, resolveErr := resolveInsideRoot(configsDir, template)
		if resolveErr != nil {
			log.Printf("Restart: invalid template %q: %v — proceeding without it", template, resolveErr)
		} else if _, err := os.Stat(candidatePath); err == nil {
			return candidatePath, template
		} else {
			log.Printf("Restart: template %q dir not found — proceeding without it", template)
		}
	}

	// Tier 3: #239 rebuild_config — org-templates as last-resort recovery.
	if body.RebuildConfig {
		if p, label := resolveOrgTemplate(configsDir, wsName); p != "" {
			log.Printf("Restart: rebuild_config — using org-template %s (%s)", label, wsName)
			return p, label
		}
	}

	// Tier 4: runtime-default — apply_template=true + known runtime.
	// Use case: Canvas Config tab changed the runtime; we need the new
	// runtime's base files (entry point, Dockerfile, skill scaffolding)
	// because the existing volume was written by the old runtime.
	//
	// SECURITY (CWE-22 / F1502): dbRuntime comes from the workspaces DB
	// column — set by the PATCH Update handler which only validates length
	// and newlines, not path-traversal characters.  Without sanitisation an
	// attacker who holds a workspace token could set runtime to
	// "../../../etc" and, if a directory matching that path existed on the
	// host, load an arbitrary host directory as the workspace template.
	//
	// sanitizeRuntime applies an allowlist of known runtimes; any unknown
	// value (including traversal strings) is remapped to "claude-code".  The
	// attacker cannot choose an arbitrary host path — they can at most
	// trigger application of the claude-code-default template.
	if body.ApplyTemplate && dbRuntime != "" {
		safeRuntime := sanitizeRuntime(dbRuntime)
		runtimeTemplate := filepath.Join(configsDir, safeRuntime+"-default")
		if _, err := os.Stat(runtimeTemplate); err == nil {
			label := safeRuntime + "-default"
			log.Printf("Restart: applying template %s (runtime change)", label)
			return runtimeTemplate, label
		}
	}

	// Tier 5: persisted template from PATCH /workspaces/:id/template.
	// When the caller supplied no explicit body template, fall back to the
	// template stored in the DB. This makes `PATCH template=seo-agent`
	// followed by `POST /restart` with no body apply the newly installed
	// template instead of reusing the stale config volume.
	if body.Template == "" && dbTemplate != "" {
		if candidatePath, resolveErr := resolveInsideRoot(configsDir, dbTemplate); resolveErr != nil {
			log.Printf("Restart: invalid persisted template %q: %v — proceeding without it", dbTemplate, resolveErr)
		} else if _, err := os.Stat(candidatePath); err == nil {
			return candidatePath, dbTemplate
		} else {
			log.Printf("Restart: persisted template %q dir not found — proceeding without it", dbTemplate)
		}
	}

	// Tier 6: reuse existing volume. This is the default, and the path
	// the Canvas Save+Restart flow MUST hit to preserve user edits.
	return "", "existing-volume"
}
