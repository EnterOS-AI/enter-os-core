-- core#2724: seed the ack-first directive as a default-on global
-- platform_instruction so it applies to EVERY workspace agent
-- without editing every template.
--
-- WHY a migration seed (vs a hardcoded string in instructions.go):
--   - Operators can disable / edit / re-scope via the standard
--     platform_instructions admin API (POST /platform/instructions,
--     PATCH /platform/instructions/:id). A hardcoded string is
--     immutable without a code change.
--   - Operators can see the directive's content in the table itself
--     (audit trail).
--   - The single source of truth for the directive lives in the
--     `platform_instructions` row; the resolution endpoint
--     (handlers/instructions.go:Resolve) concatenates it into the
--     agent's system prompt.
--
-- Surface coverage relative to PR #129 (workspace-runtime / MCP
-- preamble, CR2-approved on 3d0319e8):
--   - #129: runtime/MCP preamble (gated by mcp=True, reaches
--     agents via runtime-template roll).
--   - THIS seed: workspace-server platform_instructions, default-on
--     for ALL agents regardless of MCP flag, reaches agents via
--     GET /workspaces/:id/instructions/resolve.
--   The two are complementary — #129 catches the runtime-injected
--   preamble, this seed catches the platform-injected instruction.
--
-- Idempotency: INSERT ... WHERE NOT EXISTS (...), NOT ON CONFLICT.
-- The `platform_instructions` table (created in migration 040) has
-- only a UUID primary key plus the partial scope index — there is
-- no unique constraint on (scope, scope_target, title), and
-- PostgreSQL's ON CONFLICT (cols) DO NOTHING requires one.
-- Adding a unique constraint would be a schema change with
-- cross-cutting migration ordering risk; WHERE NOT EXISTS is
-- idempotent against the CURRENT schema and survives a re-apply.
-- The seed deliberately does NOT update an existing row's
-- content/priority/enabled if one already matches the title —
-- operator edits on production are preserved (same contract as
-- ON CONFLICT DO NOTHING would have given us).
INSERT INTO platform_instructions (scope, scope_target, title, content, priority, enabled)
SELECT 'global', NULL,
    'Acknowledge-first responsiveness',
    -- Direct copy of the runtime preamble added in PR #129
    -- (molecule-ai-workspace-runtime feat/ack-first-responsiveness,
    -- head 3d0319e8, CR2-approved). The two surfaces are
    -- complementary: runtime preamble for MCP-enabled runtimes,
    -- platform_instruction for every agent regardless of MCP flag
    -- and for the concierge (kind=platform) which the runtime
    -- preamble doesn't reach.
    '**Stay responsive — acknowledge first:** The moment you pick up a request that will take more than a few seconds, FIRST send a one-line acknowledgement + your plan with `send_message_to_user` (e.g. "On it — I''ll do X then Y, back shortly"), THEN start the work. For long tasks, drop a brief progress note when a phase finishes. Never go silent for minutes — a user with no acknowledgement assumes the agent is stuck.',
    100,  -- high priority so it''s near the top of the concatenated prompt
    true
WHERE NOT EXISTS (
    SELECT 1 FROM platform_instructions
    WHERE scope = 'global'
      AND scope_target IS NULL
      AND title = 'Acknowledge-first responsiveness'
);
