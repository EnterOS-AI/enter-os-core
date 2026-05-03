-- Reverse of 044_runtime_image_pins.up.sql.
--
-- Drop the index before the column it references to satisfy Postgres
-- ordering. The pins table can drop independently of the column.

DROP INDEX IF EXISTS idx_workspaces_runtime_image_digest;
ALTER TABLE workspaces DROP COLUMN IF EXISTS runtime_image_digest;
DROP TABLE IF EXISTS runtime_image_pins;
