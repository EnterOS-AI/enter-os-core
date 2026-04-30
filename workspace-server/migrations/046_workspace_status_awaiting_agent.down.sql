-- 046_workspace_status_awaiting_agent.down.sql
--
-- Reverse 046_workspace_status_awaiting_agent.up.sql.
--
-- Postgres does NOT support DROP VALUE on an enum. The standard rollback
-- recipe is rename → recreate → cast → drop, which is intrusive (locks
-- workspaces with ACCESS EXCLUSIVE the same way migration 043 did). We
-- punt: only run this manually, and only if you're prepared to nuke and
-- recreate the type. Application code WILL fail if the value disappears
-- and any row currently has it; pre-flight with:
--
--   UPDATE workspaces SET status = 'offline'
--     WHERE status = 'awaiting_agent';
--
-- before running the recipe below.

BEGIN;

SET LOCAL lock_timeout = '5s';

-- Convert any existing awaiting_agent / hibernating rows to a value the
-- new enum will accept. 'offline' is the safest fallback for awaiting_agent
-- (operator can re-register to bring them back online); 'hibernated' is
-- the natural terminal of an in-flight 'hibernating'.
UPDATE workspaces SET status = 'offline'    WHERE status = 'awaiting_agent';
UPDATE workspaces SET status = 'hibernated' WHERE status = 'hibernating';

ALTER TYPE workspace_status RENAME TO workspace_status_with_awaiting;

CREATE TYPE workspace_status AS ENUM (
    'provisioning',
    'online',
    'offline',
    'degraded',
    'failed',
    'removed',
    'paused',
    'hibernated'
);

ALTER TABLE workspaces
    ALTER COLUMN status DROP DEFAULT;

ALTER TABLE workspaces
    ALTER COLUMN status TYPE workspace_status
    USING status::text::workspace_status;

ALTER TABLE workspaces
    ALTER COLUMN status SET DEFAULT 'provisioning'::workspace_status;

DROP TYPE workspace_status_with_awaiting;

COMMIT;
