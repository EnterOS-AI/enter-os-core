-- Drop child first (FK), then parent. Idempotent (IF EXISTS) so a re-run of the
-- down migration is a no-op rather than a crash.
DROP TABLE IF EXISTS request_messages;
DROP TABLE IF EXISTS requests;
