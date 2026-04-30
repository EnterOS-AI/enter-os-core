-- 045_workspaces_delivery_mode.up.sql
--
-- Per-workspace declaration of how A2A traffic is delivered TO the workspace.
--
--   push (default, today's behavior)
--     Platform synchronously POSTs to workspaces.url and surfaces the response
--     to the caller. Requires a publicly-routable URL (SSRF gate at
--     a2a_proxy.go:455). Used by all hosted runtimes (claude-code, hermes,
--     etc.) where the platform's provisioner sets the URL at boot.
--
--   poll
--     Platform records the inbound A2A as an a2a_receive activity row and
--     returns 200 to the caller without dispatching. The agent client (e.g.
--     molecule-mcp-claude-channel) consumes the inbox via
--     GET /workspaces/:id/activity?since_id=… and replies via
--     POST /workspaces/:peer/a2a. NO URL required — works through every NAT,
--     firewall, and dev-laptop without a tunnel.
--
-- Why a column and not a derived signal:
--
--   * Mutual exclusivity matches Telegram's getUpdates / setWebhook
--     semantics — operationally cleaner than "both half-work because URL
--     is empty". Telegram explicitly rejects double-delivery; we now do
--     the same.
--   * The platform short-circuits BEFORE the SSRF check, so a poll-mode
--     workspace with a stale or missing URL never trips the silent-404
--     failure mode that motivated #2339.
--   * Push-mode is the safe default: every existing workspace continues
--     to work exactly as before with no migration of behavior.
--
-- Backwards compatibility:
--
--   * NOT NULL with DEFAULT 'push' — the ALTER backfills existing rows.
--   * Push-mode workspaces are unchanged: SSRF check still gates dispatch,
--     activity logging unchanged.
--   * Poll-mode opt-in only via POST /workspaces (delivery_mode='poll')
--     or POST /registry/register with delivery_mode='poll'. Cannot be
--     toggled after the fact via heartbeat — flipping mode mid-life is
--     ambiguous (in-flight pushes vs queued polls), so an explicit
--     PATCH /workspaces/:id/delivery_mode endpoint will be added later
--     if the use case appears.
--
-- Reverse plan: the .down.sql drops the column. Any short-circuit code
-- that reads delivery_mode would then hit a "column does not exist"
-- error — readers fall back to push mode (behaviour pre-2339), which is
-- the safe degradation. Acceptable for a forward-only schema; the down
-- exists for migration tooling parity, not as a recommended runtime path.

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS delivery_mode TEXT NOT NULL DEFAULT 'push'
        CHECK (delivery_mode IN ('push', 'poll'));

COMMENT ON COLUMN workspaces.delivery_mode IS
    'How inbound A2A is delivered: push (synchronous to workspaces.url) or poll (logged to activity_logs, agent reads via GET /activity?since_id=). See migration 045 + RFC #2339.';
