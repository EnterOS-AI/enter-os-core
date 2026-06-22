-- Reverse of 20260518000000_seed_production_team_agent_cards.up.sql.
--
-- Clears the identity fields back to the gap state that the up
-- migration was designed to fix. After this down migration, the PR
-- #1427 reconcile has nothing to substitute again: name reverts to the
-- workspace UUID (the runtime's fallback), role to NULL, agent_card
-- description/skills to empty. This is the pre-#1427 + pre-this-seed
-- behaviour.
--
-- Match strategy mirrors the up migration (id::text LIKE prefix for 5,
-- exact UUID for CEO-Assistant) so any down-roll touches the exact
-- same rows.

BEGIN;

-- PM
UPDATE workspaces
SET name = id::text,
    role = NULL,
    agent_card = (agent_card - 'description' - 'skills' - 'role') ||
        jsonb_build_object('name', id::text),
    updated_at = now()
WHERE id::text LIKE '8a71d4d4-%';

-- Reviewer
UPDATE workspaces
SET name = id::text,
    role = NULL,
    agent_card = (agent_card - 'description' - 'skills' - 'role') ||
        jsonb_build_object('name', id::text),
    updated_at = now()
WHERE id::text LIKE '27e66b5a-%';

-- Researcher
UPDATE workspaces
SET name = id::text,
    role = NULL,
    agent_card = (agent_card - 'description' - 'skills' - 'role') ||
        jsonb_build_object('name', id::text),
    updated_at = now()
WHERE id::text LIKE '5773bd5f-%';

-- Dev-A
UPDATE workspaces
SET name = id::text,
    role = NULL,
    agent_card = (agent_card - 'description' - 'skills' - 'role') ||
        jsonb_build_object('name', id::text),
    updated_at = now()
WHERE id::text LIKE '4ca4c06c-%';

-- Dev-B
UPDATE workspaces
SET name = id::text,
    role = NULL,
    agent_card = (agent_card - 'description' - 'skills' - 'role') ||
        jsonb_build_object('name', id::text),
    updated_at = now()
WHERE id::text LIKE '31eb65ed-%';

-- CEO-Assistant
UPDATE workspaces
SET name = id::text,
    role = NULL,
    agent_card = (agent_card - 'description' - 'skills' - 'role') ||
        jsonb_build_object('name', id::text),
    updated_at = now()
WHERE id = '30ba7f0b-b303-4a20-aefe-3a4a675b8aa4'::uuid;

COMMIT;
