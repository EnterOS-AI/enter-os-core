ALTER TABLE workspaces
    DROP COLUMN IF EXISTS budget_limit,
    DROP COLUMN IF EXISTS monthly_spend;
