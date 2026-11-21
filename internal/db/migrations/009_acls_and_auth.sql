-- Per-mount group ACLs, WebAuthn/Passkeys, and OIDC identity links.

CREATE TABLE IF NOT EXISTS mount_group_acls (
    id          TEXT PRIMARY KEY,
    node_id     TEXT NOT NULL,
    mount_id    TEXT NOT NULL,
    group_id    TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    can_read    INTEGER NOT NULL DEFAULT 1,
    can_write   INTEGER NOT NULL DEFAULT 0,
    can_delete  INTEGER NOT NULL DEFAULT 0,
    can_share   INTEGER NOT NULL DEFAULT 1,
    can_compute INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    UNIQUE(node_id, mount_id, group_id)
);

CREATE INDEX IF NOT EXISTS idx_mount_group_acls_lookup ON mount_group_acls(node_id, mount_id);

CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id                      TEXT PRIMARY KEY, -- Unpadded Base64URL credential ID
    user_id                 TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                    TEXT NOT NULL DEFAULT 'Passkey',
    credential_json         TEXT NOT NULL,    -- Serialized go-webauthn Credential JSON
    aaguid                  TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    device_type             TEXT NOT NULL DEFAULT 'multiDevice',
    backed_up               INTEGER NOT NULL DEFAULT 0,
    transports_json         TEXT NOT NULL DEFAULT '[]',
    sign_count              INTEGER NOT NULL DEFAULT 0,
    registered_at           INTEGER NOT NULL,
    last_used_at            INTEGER
);

CREATE INDEX IF NOT EXISTS idx_webauthn_user ON webauthn_credentials(user_id);

CREATE TABLE IF NOT EXISTS oidc_links (
    id          TEXT PRIMARY KEY,
    provider    TEXT NOT NULL,
    subject     TEXT NOT NULL,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    UNIQUE(provider, subject)
);

CREATE INDEX IF NOT EXISTS idx_oidc_user ON oidc_links(user_id);
