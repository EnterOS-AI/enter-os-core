-- Add response_body to a2a_queue so callers can poll GET /workspaces/:id/a2a/queue/:queue_id
-- and retrieve the actual agent reply once a queued A2A message/send item completes.
-- Previously only delegation queue items surfaced a result (via activity_logs stitch);
-- direct A2A queue items completed with no durable payload, leaving polling clients
-- unable to verify the response.

ALTER TABLE a2a_queue ADD COLUMN IF NOT EXISTS response_body JSONB;
