-- Per-workspace llm_billing_mode override (internal#691).
--
-- NULL = inherit the org-level default (organizations.llm_billing_mode on CP,
-- propagated to workspace-server via tenant_config as MOLECULE_LLM_BILLING_MODE).
-- A non-NULL value overrides the org default for this workspace only.
--
-- Resolver contract: workspaces.llm_billing_mode ?? org_default ?? 'platform_managed'.
-- Default-closed: any NULL, error, unknown enum, or JOIN miss resolves to
-- 'platform_managed' (the existing implicit default — see internal#691
-- spec sketch + Phase 1 design comment).
--
-- The check constraint mirrors the CP-side credits.LLMBillingMode* constants
-- (molecule-controlplane/internal/credits/llm_billing.go). Keep in sync if
-- a new mode is ever added; the resolver also enumerates them explicitly.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS llm_billing_mode TEXT
  CHECK (llm_billing_mode IS NULL OR llm_billing_mode IN ('platform_managed', 'byok', 'disabled'));
