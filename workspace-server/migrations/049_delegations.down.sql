DROP INDEX IF EXISTS idx_delegations_idempotency;
DROP INDEX IF EXISTS idx_delegations_callee_created;
DROP INDEX IF EXISTS idx_delegations_caller_created;
DROP INDEX IF EXISTS idx_delegations_inflight_heartbeat;
DROP TABLE IF EXISTS delegations;
