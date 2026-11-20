-- Phase 4.7: Dual-consent opt-in parity sync (contributor allows + user subscribes)

ALTER TABLE mounts ADD COLUMN allow_sync INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS sync_subscriptions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    source_mount_id TEXT NOT NULL REFERENCES mounts(id) ON DELETE CASCADE,
    target_node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    target_path TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    UNIQUE (user_id, source_mount_id, target_node_id)
);
