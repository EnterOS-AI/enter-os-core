-- migration: 20260523000000_schedule_consecutive_sdk_errors.up.sql
-- Fixes #1696: Add consecutive_sdk_errors counter to track SDK errors (HTTP 200
-- responses where the Claude Code runtime returned a non-ok result_kind).
-- When this counter reaches 3, the scheduler sets last_status='rate_limited'
-- and auto-disables the schedule.
--
-- The core issue: the claude-code-sdk adapter returns HTTP 200 even when the
-- inner LLM call throws (e.g. Max-plan rate-limit). All 3 observed runs logged
-- "completed (HTTP 200)" yet surfaced agent errors in the workspace chat.
-- This counter lets us detect that pattern and escalate appropriately.

ALTER TABLE workspace_schedules
    ADD COLUMN IF NOT EXISTS consecutive_sdk_errors INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN workspace_schedules.consecutive_sdk_errors IS
    'Count of consecutive scheduler fires where ProxyA2ARequest returned HTTP 200
     but the response body contained a non-ok result_kind (e.g. rate_limited,
     sdk_error, quota_exhausted). Reset to 0 on any non-SDK-error status.
     After 3 consecutive SDK errors the schedule is auto-disabled with
     status rate_limited. Fixes #1696.';