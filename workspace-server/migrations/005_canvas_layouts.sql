CREATE TABLE IF NOT EXISTS canvas_layouts (
    workspace_id  UUID REFERENCES workspaces(id) ON DELETE CASCADE,
    x             FLOAT NOT NULL DEFAULT 0,
    y             FLOAT NOT NULL DEFAULT 0,
    collapsed     BOOLEAN DEFAULT false,
    PRIMARY KEY (workspace_id)
);

CREATE TABLE IF NOT EXISTS canvas_viewport (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    x          FLOAT NOT NULL DEFAULT 0,
    y          FLOAT NOT NULL DEFAULT 0,
    zoom       FLOAT NOT NULL DEFAULT 1,
    saved_at   TIMESTAMPTZ DEFAULT now()
);
