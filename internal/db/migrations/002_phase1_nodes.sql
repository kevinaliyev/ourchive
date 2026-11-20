-- Phase 1: Enrollment tokens & node metadata columns

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    token_hash TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    display_name TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,
    used_at TEXT,
    created_at TEXT NOT NULL
);

ALTER TABLE nodes ADD COLUMN os TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN arch TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
