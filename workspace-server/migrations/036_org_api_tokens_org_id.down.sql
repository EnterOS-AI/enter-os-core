-- Revert: drop org_id column from org_api_tokens.
-- Note: this does NOT restore the broken requireCallerOwnsOrg behaviour
-- (which used created_by as org anchor). That function was fixed alongside
-- this migration to read org_id; it will error on org-token callers if this
-- migration is reverted without also reverting the handler.
DROP INDEX IF EXISTS org_api_tokens_org_id_idx;
ALTER TABLE org_api_tokens DROP COLUMN IF EXISTS org_id;