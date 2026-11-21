-- Machine-wide and per-folder visibility modes + per-user access segregation rules.

ALTER TABLE nodes ADD COLUMN visibility TEXT NOT NULL DEFAULT 'open';
ALTER TABLE mounts ADD COLUMN visibility TEXT NOT NULL DEFAULT 'inherit';

CREATE TABLE IF NOT EXISTS share_user_rules (
    id         TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    mount_id   TEXT NOT NULL DEFAULT '', -- '' = machine-wide default; non-empty = per-folder override
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access     TEXT NOT NULL,            -- 'none' | 'view' | 'download' | 'all'
    created_at TEXT NOT NULL,
    UNIQUE(node_id, mount_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_share_user_rules_lookup ON share_user_rules(node_id, mount_id, user_id);
