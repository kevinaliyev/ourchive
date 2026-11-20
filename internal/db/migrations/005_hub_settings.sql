-- Phase 4.6: Hub network policy, WebRTC P2P, and opt-in edge storage settings

CREATE TABLE IF NOT EXISTS hub_settings (
    key TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
