package handlers

// template_schedules.go — read a workspace template's `schedules:`
// block and seed workspace_schedules with source='template'. Mirrors
// the org/import flow (org_import.go) so a workspace created directly
// from a workspace template (e.g. via WorkspaceHandler.Create) lands
// with the same schedule grid the org/import path would have produced.
//
// Issue #24 contract (also enforced by org_import + schedules.go):
//   - INSERT new rows with source='template'
//   - On (workspace_id, name) collision, only refresh template-source
//     rows; runtime-added rows survive re-provisioning untouched
//   - Never DELETE (additive only)
//
// The actual INSERT statement is the canonical orgImportScheduleSQL
// defined in org.go — reused here verbatim so the four guarantees
// stay in one place.
//
// Hostile-template defenses (a tenant can upload a config.yaml via
// POST /templates/import or webhook-sync a repo they control):
//   - config.yaml is loaded through a 1 MiB LimitReader so a YAML
//     anchor-bomb / billion-laughs cannot pre-explode memory before
//     unmarshal returns.
//   - len(schedules), per-schedule cron length, and resolved prompt
//     body length are all bounded; over-sized entries are skipped
//     rather than committed.
//   - Per-row insert errors and ctx cancellation surface to the
//     caller via the returned counts so partial-seed states are
//     observable (workspace.go Create logs the (seeded, skipped)
//     pair when skipped > 0).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/scheduler"
)

// Bounds protecting the seeder against hostile or buggy templates.
// All chosen with generous headroom relative to legitimate use
// (acme-demo org template — the largest production schedule grid —
// runs ~10 entries per workspace, each prompt body well under 1 KiB).
const (
	maxTemplateConfigYAMLBytes int64 = 1 << 20  // 1 MiB — hard cap on config.yaml size
	maxTemplateSchedules             = 100      // 10x current largest grid
	maxScheduleCronExprLen           = 128      // cron-spec syntax is short by construction
	maxSchedulePromptBytes           = 16 << 10 // 16 KiB after prompt_file resolution
)

// templateConfigSchedules is the minimal shape parsed from a workspace
// template's config.yaml. Only the `schedules:` block is modelled;
// the rest of the file (providers, runtime_config, …) is opaque to
// this loader and continues to flow through the existing pass-through
// in workspace_provision.go.
type templateConfigSchedules struct {
	Schedules []OrgSchedule `yaml:"schedules"`
}

// parseTemplateSchedules reads `<templatePath>/config.yaml` and
// returns its `schedules:` block (nil + nil error when the file is
// absent or the block is empty).
//
// The file is read through a 1 MiB LimitReader so a billion-laughs
// or anchor-explosion YAML cannot pre-explode memory before
// Unmarshal returns. Returns an error only when a present
// config.yaml fails to read or parse — callers should treat that as
// a template-author bug rather than a runtime fault. The Create
// handler logs the error and continues so a broken schedules block
// can never block workspace provisioning.
func parseTemplateSchedules(templatePath string) ([]OrgSchedule, error) {
	if templatePath == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(templatePath, "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open template config.yaml: %w", err)
	}
	defer f.Close()

	// Read maxTemplateConfigYAMLBytes+1 — if we filled the buffer the
	// underlying file exceeded the cap and we refuse to unmarshal.
	data, err := io.ReadAll(io.LimitReader(f, maxTemplateConfigYAMLBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read template config.yaml: %w", err)
	}
	if int64(len(data)) > maxTemplateConfigYAMLBytes {
		return nil, fmt.Errorf("template config.yaml exceeds %d-byte cap", maxTemplateConfigYAMLBytes)
	}
	var cfg templateConfigSchedules
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse template config.yaml schedules: %w", err)
	}
	if len(cfg.Schedules) > maxTemplateSchedules {
		return nil, fmt.Errorf("template declares %d schedules; cap is %d", len(cfg.Schedules), maxTemplateSchedules)
	}
	return cfg.Schedules, nil
}

// seedTemplateSchedules INSERTs (or refreshes) each schedule into
// workspace_schedules with source='template'. Returns (seeded,
// skipped) counts so the caller can observe partial-seed states.
//
// Prompt body resolution mirrors org_import.go: inline `prompt:` wins,
// else `prompt_file:` is resolved relative to templatePath via
// resolvePromptRef. Per-schedule failures (bad cron, missing prompt
// file, DB error, oversize input) are logged with the schedule name
// quoted via %q (CRLF-safe) and skipped so one bad row never breaks
// the rest of the grid. A cancelled ctx breaks the loop early.
//
// Timezone defaults to "UTC" when unset. Env-var expansion in the
// timezone field is intentionally not performed — that mirrors the
// org/import behavior; template authors should pick a literal IANA
// zone (or rely on UTC + operator overrides per-tenant).
func seedTemplateSchedules(ctx context.Context, workspaceID, templatePath string, schedules []OrgSchedule) (seeded, skipped int) {
	for _, sched := range schedules {
		// Honour caller cancellation — protects against long seed
		// loops on a request whose client already gave up.
		if err := ctx.Err(); err != nil {
			log.Printf("Template schedule seed: ctx cancelled after %d/%d on %s: %v", seeded, len(schedules), workspaceID, err)
			skipped += len(schedules) - seeded - skipped
			return
		}
		if len(sched.CronExpr) > maxScheduleCronExprLen {
			log.Printf("Template schedule seed: cron_expr too long (%d > %d) for %q on %s — skipping", len(sched.CronExpr), maxScheduleCronExprLen, sched.Name, workspaceID)
			skipped++
			continue
		}
		tz := sched.Timezone
		if tz == "" {
			tz = "UTC"
		}
		enabled := true
		if sched.Enabled != nil {
			enabled = *sched.Enabled
		}
		prompt, promptErr := resolvePromptRef(sched.Prompt, sched.PromptFile, templatePath, "")
		if promptErr != nil {
			log.Printf("Template schedule seed: failed to resolve prompt for %q on %s: %v — skipping", sched.Name, workspaceID, promptErr)
			skipped++
			continue
		}
		if prompt == "" {
			log.Printf("Template schedule seed: schedule %q on %s has empty prompt — skipping", sched.Name, workspaceID)
			skipped++
			continue
		}
		if len(prompt) > maxSchedulePromptBytes {
			log.Printf("Template schedule seed: prompt too long (%d > %d bytes) for %q on %s — skipping", len(prompt), maxSchedulePromptBytes, sched.Name, workspaceID)
			skipped++
			continue
		}
		nextRun, nextRunErr := scheduler.ComputeNextRun(sched.CronExpr, tz, time.Now())
		if nextRunErr != nil {
			log.Printf("Template schedule seed: invalid cron for %q on %s: %v — skipping", sched.Name, workspaceID, nextRunErr)
			skipped++
			continue
		}
		if _, err := db.DB.ExecContext(ctx, orgImportScheduleSQL,
			workspaceID, sched.Name, sched.CronExpr, tz, prompt, enabled, nextRun); err != nil {
			log.Printf("Template schedule seed: failed to upsert %q on %s: %v", sched.Name, workspaceID, err)
			skipped++
			continue
		}
		seeded++
		log.Printf("Template schedule seed: %q (%s, %d chars) upserted on %s (source=template)", sched.Name, sched.CronExpr, len(prompt), workspaceID)
	}
	return
}
