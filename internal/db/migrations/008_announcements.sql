-- Ourchive Sticky Announcements & Admin Broadcasts

CREATE TABLE IF NOT EXISTS announcements (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'broadcast', -- broadcast | notice | warning | error
    dismissible INTEGER NOT NULL DEFAULT 1,
    expires_at TEXT,
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_announcements_created ON announcements(created_at DESC);
