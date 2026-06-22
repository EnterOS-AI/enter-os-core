-- Reverse RFC internal#742 Part 3 rescue_bundles table.
-- Forensic-only table; dropping it loses post-mortem read history but
-- does not affect boot-failure semantics (capture still ships to Loki).
DROP TABLE IF EXISTS rescue_bundles;
