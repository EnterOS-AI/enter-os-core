-- Reverse of 20260520120000_drop_runtime_image_pins.up.sql.
--
-- Recreates the runtime_image_pins table verbatim from migration 047 so a
-- down-cycle leaves the schema bit-identical to the state before the drop.
-- The `workspaces.runtime_image_digest` column is unaffected by both the
-- up and the down (we never touched it on the up side).

CREATE TABLE IF NOT EXISTS runtime_image_pins (
    template_name   TEXT PRIMARY KEY,
    digest          TEXT NOT NULL CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      TEXT,
    notes           TEXT
);

