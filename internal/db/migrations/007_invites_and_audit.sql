-- Ourchive Invite-Only Registration, Lineage Tracking & Configurable Audit Logs

ALTER TABLE users ADD COLUMN username TEXT;
ALTER TABLE users ADD COLUMN invited_by TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN invite_key_id TEXT;
ALTER TABLE users ADD COLUMN can_invite INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN invite_quota INTEGER NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username
    ON users(username)
    WHERE username IS NOT NULL AND username != '';

ALTER TABLE groups ADD COLUMN audit_downloads INTEGER NOT NULL DEFAULT 0;
ALTER TABLE groups ADD COLUMN audit_previews INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS invite_keys (
    id TEXT PRIMARY KEY,
    code_prefix TEXT NOT NULL,
    code_hash TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL DEFAULT 'member',
    group_id TEXT REFERENCES groups(id) ON DELETE SET NULL,
    max_uses INTEGER NOT NULL DEFAULT 1,
    use_count INTEGER NOT NULL DEFAULT 0,
    note TEXT NOT NULL DEFAULT '',
    expires_at TEXT,
    revoked_at TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_invite_keys_creator ON invite_keys(created_by, created_at DESC);

CREATE TABLE IF NOT EXISTS audit_logs (
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    category TEXT NOT NULL, -- core | download | preview
    action TEXT NOT NULL,
    actor_id TEXT,
    actor_label TEXT NOT NULL DEFAULT '',
    group_id TEXT,
    target_type TEXT NOT NULL DEFAULT '',
    target_id TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    ip_address TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_category ON audit_logs(category, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor ON audit_logs(actor_id, created_at DESC);
