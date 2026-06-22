-- Reverse internal#691 per-workspace billing mode column.
-- The column is nullable + check-constrained; dropping it is non-destructive
-- to org-level behavior (workspaces fall back to the org default again).
ALTER TABLE workspaces DROP COLUMN IF EXISTS llm_billing_mode;
