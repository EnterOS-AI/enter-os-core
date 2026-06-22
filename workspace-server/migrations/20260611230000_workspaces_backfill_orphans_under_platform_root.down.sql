-- Irreversible data repair: the pre-backfill NULL parents are not recorded,
-- so down is a no-op by design (re-orphaning workspaces would re-break A2A).
SELECT 1;
