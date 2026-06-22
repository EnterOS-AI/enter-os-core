-- Seed identity (name + role + agent_card description/skills) for the
-- 6 production-team workspaces. Pairs with the PR #1427 server-side
-- reconcile (internal#492): #1427 added the platform-side backfill that
-- pulls workspaces.name and workspaces.role into the stored agent_card
-- on /registry/register; this migration populates the trusted DB row
-- those reads consume.
--
-- Without this seed, the reconcile has nothing to substitute and the
-- card stays at name=UUID / description="" / role=null for the prod
-- team agents — the exact gap internal#492 is filed against.
--
-- Identity stays platform-controlled — the agent runtime cannot
-- self-write these fields. The 6 workspace UUIDs are the CTO-locked
-- production-team topology (see project_production_agent_team_topology):
--
--   PM             8a71d4d4... — Claude Code on Opus, read-only,
--                                A2A-delegate-only coordinator
--   Reviewer       27e66b5a... — codex on openai-subscription,
--                                5-axis non-author review
--   Researcher     5773bd5f... — codex on openai-subscription,
--                                root-cause investigation
--   Dev-A          4ca4c06c... — Claude Code on Kimi K2.6
--                                (api.kimi.com/coding base + ANTHROPIC_API_KEY)
--   Dev-B          31eb65ed... — Claude Code on MiniMax
--                                (api.minimax.io/anthropic base + sk-cp-* key)
--   CEO-Assistant  30ba7f0b-b303-4a20-aefe-3a4a675b8aa4 — Claude Code,
--                                orchestrator-side operations + canvas relay
--
-- Match strategy: 5 of 6 production UUIDs were provided to me by the CTO
-- as 8-char prefixes only (the full UUIDs live in the prod tenant DB).
-- We match those 5 with `id::text LIKE '<prefix>-%'` so this migration
-- is unambiguous when reviewed without DB access — the CTO will confirm
-- on review that each prefix resolves to a single row. CEO-Assistant
-- (30ba7f0b-b303-4a20-aefe-3a4a675b8aa4) is known in full from
-- chat_files_test.go and is matched exactly.
--
-- Idempotent: each UPDATE only touches the three identity fields. Re-
-- running rewrites the same values. UUIDs not present in a given tenant
-- DB match zero rows and are silently skipped — the migration never
-- INSERTs rows it doesn't own.
--
-- All names obey validateWorkspaceFields (workspace_crud.go:526):
-- <=255 chars, no newline/CR, no YAML-special chars `{}[]|>*&!`.
-- All roles obey the same contract <=1000 chars. Per-skill description
-- <=120 chars matches the discovery card surface shown on the canvas
-- Agent Card view and the mobile peer chip.

BEGIN;

-- PM — read-only A2A coordinator
UPDATE workspaces
SET name = 'Production Manager',
    role = 'product manager',
    agent_card = COALESCE(agent_card, '{}'::jsonb) || jsonb_build_object(
        'name', 'Production Manager',
        'description', 'Read-only A2A coordinator that plans work and delegates to Dev/Reviewer/Researcher peers; never writes code itself.',
        'role', 'product manager',
        'skills', jsonb_build_array(
            jsonb_build_object('id','planning','name','planning','description','Decompose CTO directives into peer-delegable units','tags',jsonb_build_array('planning'),'examples',jsonb_build_array()),
            jsonb_build_object('id','delegation','name','delegation','description','Route work to Dev-A / Dev-B / Reviewer / Researcher via A2A','tags',jsonb_build_array('delegation'),'examples',jsonb_build_array()),
            jsonb_build_object('id','coordination','name','coordination','description','Track peer activity and surface blockers back to the CTO','tags',jsonb_build_array('coordination'),'examples',jsonb_build_array()),
            jsonb_build_object('id','read-only','name','read-only','description','Never edits code or merges; proposes only','tags',jsonb_build_array('read-only','safety'),'examples',jsonb_build_array())
        ),
        'updated_at', now()::text
    ),
    updated_at = now()
WHERE id::text LIKE '8a71d4d4-%';

-- Reviewer — codex/openai, 5-axis non-author review
UPDATE workspaces
SET name = 'Code Reviewer',
    role = 'code reviewer',
    agent_card = COALESCE(agent_card, '{}'::jsonb) || jsonb_build_object(
        'name', 'Code Reviewer',
        'description', 'Non-author 5-axis review on codex/openai-subscription; runs the merge gate, never approves PRs it authored.',
        'role', 'code reviewer',
        'skills', jsonb_build_array(
            jsonb_build_object('id','code-review','name','code-review','description','Five-axis PR review against the merge gate','tags',jsonb_build_array('review'),'examples',jsonb_build_array()),
            jsonb_build_object('id','security-axis','name','security-axis','description','Trust-boundary, secret-handling, injection surface checks','tags',jsonb_build_array('security'),'examples',jsonb_build_array()),
            jsonb_build_object('id','correctness-axis','name','correctness-axis','description','Logic, error-handling, race and boundary case checks','tags',jsonb_build_array('correctness'),'examples',jsonb_build_array()),
            jsonb_build_object('id','non-author-approve','name','non-author-approve','description','Approves only PRs the reviewer did not author','tags',jsonb_build_array('two-eyes'),'examples',jsonb_build_array())
        ),
        'updated_at', now()::text
    ),
    updated_at = now()
WHERE id::text LIKE '27e66b5a-%';

-- Researcher — codex/openai, root-cause investigation
UPDATE workspaces
SET name = 'Root-Cause Researcher',
    role = 'researcher',
    agent_card = COALESCE(agent_card, '{}'::jsonb) || jsonb_build_object(
        'name', 'Root-Cause Researcher',
        'description', 'Diagnostic investigation on codex/openai-subscription; obs-first, source-as-corroboration, no drive-by fixes.',
        'role', 'researcher',
        'skills', jsonb_build_array(
            jsonb_build_object('id','root-cause','name','root-cause','description','Diagnose the underlying cause, never patch symptoms','tags',jsonb_build_array('investigation'),'examples',jsonb_build_array()),
            jsonb_build_object('id','obs-first','name','obs-first','description','Grafana/Loki query before source-guessing','tags',jsonb_build_array('observability'),'examples',jsonb_build_array()),
            jsonb_build_object('id','log-correlation','name','log-correlation','description','Cross-service step= / Delegation uuid tracing','tags',jsonb_build_array('observability'),'examples',jsonb_build_array()),
            jsonb_build_object('id','source-archaeology','name','source-archaeology','description','Git blame and prior-art recall across repos','tags',jsonb_build_array('git'),'examples',jsonb_build_array())
        ),
        'updated_at', now()::text
    ),
    updated_at = now()
WHERE id::text LIKE '5773bd5f-%';

-- Dev-A — Claude Code on Kimi K2.6
UPDATE workspaces
SET name = 'Dev Engineer A (Kimi)',
    role = 'dev engineer',
    agent_card = COALESCE(agent_card, '{}'::jsonb) || jsonb_build_object(
        'name', 'Dev Engineer A (Kimi)',
        'description', 'Claude Code routed to Kimi K2.6 via api.kimi.com/coding; implements PRs against the dev-tree protected branches.',
        'role', 'dev engineer',
        'skills', jsonb_build_array(
            jsonb_build_object('id','implementation','name','implementation','description','Write code to merge gate (tests, lint, types)','tags',jsonb_build_array('coding'),'examples',jsonb_build_array()),
            jsonb_build_object('id','test-driven','name','test-driven','description','Failing test first, then minimal fix','tags',jsonb_build_array('tdd'),'examples',jsonb_build_array()),
            jsonb_build_object('id','bug-fixing','name','bug-fixing','description','Root-caused bug fixes with regression test','tags',jsonb_build_array('debugging'),'examples',jsonb_build_array()),
            jsonb_build_object('id','refactoring','name','refactoring','description','In-scope, behavior-preserving refactors only','tags',jsonb_build_array('refactor'),'examples',jsonb_build_array())
        ),
        'updated_at', now()::text
    ),
    updated_at = now()
WHERE id::text LIKE '4ca4c06c-%';

-- Dev-B — Claude Code on MiniMax
UPDATE workspaces
SET name = 'Dev Engineer B (MiniMax)',
    role = 'dev engineer',
    agent_card = COALESCE(agent_card, '{}'::jsonb) || jsonb_build_object(
        'name', 'Dev Engineer B (MiniMax)',
        'description', 'Claude Code routed to MiniMax via api.minimax.io/anthropic; parallel dev capacity to Dev-A on the same gate.',
        'role', 'dev engineer',
        'skills', jsonb_build_array(
            jsonb_build_object('id','implementation','name','implementation','description','Write code to merge gate (tests, lint, types)','tags',jsonb_build_array('coding'),'examples',jsonb_build_array()),
            jsonb_build_object('id','test-driven','name','test-driven','description','Failing test first, then minimal fix','tags',jsonb_build_array('tdd'),'examples',jsonb_build_array()),
            jsonb_build_object('id','bug-fixing','name','bug-fixing','description','Root-caused bug fixes with regression test','tags',jsonb_build_array('debugging'),'examples',jsonb_build_array()),
            jsonb_build_object('id','refactoring','name','refactoring','description','In-scope, behavior-preserving refactors only','tags',jsonb_build_array('refactor'),'examples',jsonb_build_array())
        ),
        'updated_at', now()::text
    ),
    updated_at = now()
WHERE id::text LIKE '31eb65ed-%';

-- CEO-Assistant — Claude Code, orchestrator + canvas relay
-- Full UUID known from chat_files_test.go:286 — match exactly.
UPDATE workspaces
SET name = 'CEO Assistant',
    role = 'operator orchestrator',
    agent_card = COALESCE(agent_card, '{}'::jsonb) || jsonb_build_object(
        'name', 'CEO Assistant',
        'description', 'Orchestrator-side Claude Code that runs the triage loop, relays canvas and Telegram, dispatches non-author reviewers.',
        'role', 'operator orchestrator',
        'skills', jsonb_build_array(
            jsonb_build_object('id','triage-loop','name','triage-loop','description','Run the CI/PR triage loop; fix-what-you-find','tags',jsonb_build_array('orchestration'),'examples',jsonb_build_array()),
            jsonb_build_object('id','review-routing','name','review-routing','description','Dispatch non-author reviewers via delegate_task','tags',jsonb_build_array('routing'),'examples',jsonb_build_array()),
            jsonb_build_object('id','canvas-relay','name','canvas-relay','description','Relay CTO canvas/Telegram messages to peers','tags',jsonb_build_array('relay'),'examples',jsonb_build_array()),
            jsonb_build_object('id','ops','name','ops','description','Direct hands-on ops on operator host and Neon','tags',jsonb_build_array('ops','direct-action'),'examples',jsonb_build_array())
        ),
        'updated_at', now()::text
    ),
    updated_at = now()
WHERE id = '30ba7f0b-b303-4a20-aefe-3a4a675b8aa4'::uuid;

COMMIT;
