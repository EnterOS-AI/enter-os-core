ALTER TABLE workspaces
ADD COLUMN IF NOT EXISTS runtime TEXT DEFAULT 'claude-code';
