# Security Model

Ourchive is built for private and semi-private groups pooling personal hardware. Its security model is centered on **owner sovereignty**: contributing storage to a Hub must never give another user—not even the Hub's Host Admin—control over your filesystem.

---

## Non-Negotiable Invariants

1. **Owner Sovereignty Over Local Nodes**
   - Only a node's owner can mount/unmount folders, trigger native OS folder pickers or "Reveal in File Manager", change node read-only/bandwidth settings, or grant write/delete permissions on their mounts.
   - **Host Admins** can manage Hub policy, users, groups, invites, broadcasts, and share moderation, but **cannot** write, delete, mount, or grant write/delete ACLs on another user's machine.
2. **Defense-in-Depth at the Agent**
   - Every mount is opened with Go's `os.OpenRoot`, enforcing kernel/OS-level path confinement against `../` and symlink traversal.
   - Running an Agent with `--read-only` rejects all mutating HTTP and P2P DataChannel operations locally, independent of Hub state.
   - `--allow-dir` restricts mountable paths to an explicit local whitelist.
3. **Subdomain Isolation for Public Shares**
   - Public share links and raw media embeds are served exclusively on a separate subdomain (`s.yourdomain.com`).
   - Session cookies are scoped strictly to the main app domain (`archive.yourdomain.com`), ensuring zero cookie overlap and preventing cross-origin session access from user-uploaded HTML/SVG/media files.

---

## Authentication

Ourchive supports three authentication methods that can be combined per account:

### 1. Invite-Only Accounts + Argon2id
- Open public signup is disabled. New accounts require a valid single-use or multi-use **Invite Key** issued by an authorized member.
- Every account records its inviter in a **lineage tree**, visible to Admins for community accountability.
- Passwords are hashed with **Argon2id** and protected by per-IP rate limiting on auth, invite, and enrollment endpoints.

### 2. Passkeys (WebAuthn / FIDO2)
- Implemented via `github.com/go-webauthn/webauthn` on the Hub and browser-native WebAuthn APIs (with `webauthn-polyfills`) in the Web UI.
- Supports discoverable sign-in with **Conditional UI** (`autocomplete="username webauthn"`), explicit Passkey login, multi-device Passkey registration, and credential renaming/removal.
- Implements the **WebAuthn Signal API** (`signalUnknownCredential`, `signalAllAcceptedCredentials`, `signalCurrentUserDetails`) so password managers stay synchronized when a credential is revoked or a username changes.

### 3. OpenID Connect (OIDC)
- Standard **OIDC Discovery + Authorization Code Flow with PKCE (`S256`)**.
- Supports multiple Admin-configured providers simultaneously (Google, Authentik, Keycloak, Pocket ID, or custom issuers).
- Automatically links existing accounts by verified email; new OIDC users are still gated by a valid Invite Key.

---

## Per-Mount Group ACLs

Access to contributor mounts is controlled through `mount_group_acls`:

- **Default Behavior**: A mount with zero ACL rows remains readable and shareable by authenticated Hub members, while write and delete stay restricted to the node owner.
- **Scoped Access**: Once a node owner attaches one or more group ACL rows to a mount, only the owner and members of permitted groups can access it according to the granted flags (`can_read`, `can_write`, `can_delete`, `can_share`).
- **Enforced Everywhere**: ACL checks are enforced across directory listings, file downloads, uploads, renames, deletes, `.zip` exports, global search, and WebRTC P2P DataChannel grants (`can_write` is signed/verified by the Hub during SDP negotiation).

---

## Tiered Audit Logging

Every sensitive action (auth events, invite creation/redemption, node enrollment, share creation/revocation, ACL changes, admin policy updates) is recorded in `audit_logs`:

- **Host Admin**: Full audit log visibility and filtering across the Hub.
- **Group Admin**: Scoped visibility limited to their own managed groups.
- **Regular Members**: No access to other users' audit trails.
