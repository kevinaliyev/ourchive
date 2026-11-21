/**
 * passkeys.ts — Native WebAuthn / Passkey client helper following modern-web-guidance:
 * - Loads `webauthn-polyfills` for `getClientCapabilities()`, `parseRequestOptionsFromJSON()`,
 *   `parseCreationOptionsFromJSON()`, and `PublicKeyCredential.prototype.toJSON()`.
 * - Supports Conditional UI (`mediation: "conditional"`), explicit button sign-in,
 *   passkey registration, and WebAuthn Signal API synchronization using server-provided `rpId`.
 */
import "webauthn-polyfills";
import { User } from "./api";

export interface StoredPasskeyCredential {
  id: string;
  passkeyUserId: string;
  name: string;
  aaguid: string;
  credentialDeviceType: "singleDevice" | "multiDevice";
  credentialBackedUp: boolean;
  transports: string[];
  registeredAt: number;
  lastUsedAt?: number;
  counter: number;
}

export interface PasskeyCapabilities {
  supported: boolean;
  conditionalGet: boolean;
  platformAuthenticator: boolean;
}

export function isValidBase64Url(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    value.length % 4 !== 1 &&
    /^[A-Za-z0-9_-]+$/.test(value)
  );
}

export async function getPasskeyCapabilities(): Promise<PasskeyCapabilities> {
  if (typeof window === "undefined" || typeof window.PublicKeyCredential === "undefined") {
    return { supported: false, conditionalGet: false, platformAuthenticator: false };
  }
  try {
    const pkc = window.PublicKeyCredential as unknown as {
      getClientCapabilities?: () => Promise<Record<string, boolean>>;
    };
    if (typeof pkc.getClientCapabilities === "function") {
      const caps = await pkc.getClientCapabilities();
      return {
        supported: true,
        conditionalGet: caps.conditionalGet === true,
        platformAuthenticator: caps.passkeyPlatformAuthenticator === true || caps.userVerifyingPlatformAuthenticator === true,
      };
    }
  } catch {
    // Fall back gracefully
  }
  return { supported: true, conditionalGet: false, platformAuthenticator: false };
}

type SignalAPI = {
  signalUnknownCredential?: (opts: { rpId: string; credentialId: string }) => Promise<void>;
  signalAllAcceptedCredentials?: (opts: {
    rpId: string;
    userId: string;
    allAcceptedCredentialIds: string[];
  }) => Promise<void>;
  signalCurrentUserDetails?: (opts: {
    rpId: string;
    userId: string;
    name: string;
    displayName: string;
  }) => Promise<void>;
};

async function safeSignalUnknown(rpId: string | undefined, credentialId: string | undefined) {
  const pkc = window.PublicKeyCredential as unknown as SignalAPI | undefined;
  if (!pkc?.signalUnknownCredential || !rpId || !isValidBase64Url(credentialId)) return;
  try {
    await pkc.signalUnknownCredential({ rpId, credentialId });
  } catch (err) {
    console.warn("SignalUnknownCredential sync warning:", err);
  }
}

export async function syncAcceptedCredentials(
  rpId: string,
  userId: string,
  credentials: StoredPasskeyCredential[],
) {
  const pkc = window.PublicKeyCredential as unknown as SignalAPI | undefined;
  if (!pkc?.signalAllAcceptedCredentials) return;
  const ids = credentials.map((c) => c.id);
  if (!rpId || !isValidBase64Url(userId) || !ids.every(isValidBase64Url)) {
    console.warn("SignalAllAcceptedCredentials skipped: valid server rpId, userId, and credential IDs required.");
    return;
  }
  try {
    await pkc.signalAllAcceptedCredentials({
      rpId,
      userId,
      allAcceptedCredentialIds: ids,
    });
  } catch (err) {
    console.warn("SignalAllAcceptedCredentials sync warning:", err);
  }
}

export async function syncCurrentUserDetails(
  rpId: string,
  userId: string,
  name: string,
  displayName: string,
) {
  const pkc = window.PublicKeyCredential as unknown as SignalAPI | undefined;
  if (!pkc?.signalCurrentUserDetails) return;
  if (!rpId || !isValidBase64Url(userId)) {
    console.warn("SignalCurrentUserDetails skipped: valid server rpId and userId required.");
    return;
  }
  try {
    await pkc.signalCurrentUserDetails({
      rpId,
      userId,
      name,
      displayName,
    });
  } catch (err) {
    console.warn("SignalCurrentUserDetails sync warning:", err);
  }
}

async function fetchLoginOptions(): Promise<{ options: Record<string, unknown>; ceremony_token: string }> {
  const res = await fetch("/api/auth/passkeys/login/begin", {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "Passkey login unavailable");
  }
  return res.json();
}

async function verifyPasskeyLogin(
  token: string,
  rpId: string | undefined,
  credential: PublicKeyCredential,
): Promise<User> {
  const encoded = (credential as unknown as { toJSON: () => Record<string, unknown> }).toJSON();
  const res = await fetch(`/api/auth/passkeys/login/finish?token=${encodeURIComponent(token)}`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(encoded),
  });
  if (!res.ok) {
    if (res.status === 404) {
      await safeSignalUnknown(rpId, typeof encoded.id === "string" ? encoded.id : undefined);
    }
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || `Passkey sign-in failed (${res.status})`);
  }
  const data = await res.json();
  return data.user as User;
}

/**
 * Starts silent Conditional UI passkey autofill. Returns an abort function to cancel before explicit button triggers.
 */
export function startConditionalPasskeyLogin(
  onAuthenticated: (user: User) => void,
  onError?: (msg: string) => void,
): () => void {
  const controller = new AbortController();
  (async () => {
    const caps = await getPasskeyCapabilities();
    if (!caps.conditionalGet || controller.signal.aborted) return;

    try {
      const { options, ceremony_token } = await fetchLoginOptions();
      if (controller.signal.aborted) return;

      const pkc = window.PublicKeyCredential as unknown as {
        parseRequestOptionsFromJSON: (json: Record<string, unknown>) => PublicKeyCredentialRequestOptions;
      };
      const publicKey = pkc.parseRequestOptionsFromJSON(options);
      const cred = (await navigator.credentials.get({
        publicKey,
        signal: controller.signal,
        mediation: "conditional" as CredentialMediationRequirement,
      })) as PublicKeyCredential | null;
      if (!cred) return;

      const user = await verifyPasskeyLogin(ceremony_token, publicKey.rpId, cred);
      onAuthenticated(user);
    } catch (err) {
      const name = err instanceof Error ? err.name : "";
      if (name === "AbortError" || name === "NotAllowedError") return;
      if (onError && err instanceof Error) onError(err.message);
    }
  })();

  return () => controller.abort();
}

/**
 * Triggers an explicit "Sign in with passkey" prompt.
 */
export async function triggerButtonPasskeyLogin(): Promise<User | null> {
  const { options, ceremony_token } = await fetchLoginOptions();
  const pkc = window.PublicKeyCredential as unknown as {
    parseRequestOptionsFromJSON: (json: Record<string, unknown>) => PublicKeyCredentialRequestOptions;
  };
  const publicKey = pkc.parseRequestOptionsFromJSON(options);

  let cred: PublicKeyCredential | null;
  try {
    cred = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential | null;
  } catch (err) {
    const name = err instanceof Error ? err.name : "";
    if (name === "NotAllowedError" || name === "AbortError") {
      return null;
    }
    throw err;
  }
  if (!cred) return null;

  return verifyPasskeyLogin(ceremony_token, publicKey.rpId, cred);
}

/**
 * Registers a new passkey for the currently signed-in user.
 */
export async function registerNewPasskey(nickname = "", promotion = false): Promise<StoredPasskeyCredential | null> {
  const res = await fetch("/api/auth/passkeys/register/begin", {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ promotion }),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "Could not start passkey registration");
  }
  const { options, ceremony_token } = await res.json();
  const pkc = window.PublicKeyCredential as unknown as {
    parseCreationOptionsFromJSON: (json: Record<string, unknown>) => PublicKeyCredentialCreationOptions;
  };
  const publicKey = pkc.parseCreationOptionsFromJSON(options);

  let cred: PublicKeyCredential | null;
  try {
    cred = (await navigator.credentials.create({ publicKey })) as PublicKeyCredential | null;
  } catch (err) {
    const name = err instanceof Error ? err.name : "";
    if (name === "InvalidStateError") {
      throw new Error("A passkey for this account already exists on this authenticator.");
    }
    if (name === "SecurityError") {
      throw new Error("Passkeys require HTTPS (or localhost) with a matching domain.");
    }
    if (name === "NotAllowedError" || name === "AbortError") {
      return null;
    }
    throw err;
  }
  if (!cred) return null;

  const encoded = (cred as unknown as { toJSON: () => Record<string, unknown> }).toJSON();
  try {
    const verifyRes = await fetch(
      `/api/auth/passkeys/register/finish?token=${encodeURIComponent(ceremony_token)}&name=${encodeURIComponent(nickname)}`,
      {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(encoded),
      },
    );
    if (!verifyRes.ok) {
      await safeSignalUnknown(publicKey.rp?.id, typeof encoded.id === "string" ? encoded.id : undefined);
      const body = await verifyRes.json().catch(() => ({}));
      throw new Error(body.error || "Passkey registration verification failed");
    }
    const data = await verifyRes.json();
    return data.credential as StoredPasskeyCredential;
  } catch (err) {
    if (!(err instanceof Error && err.message.includes("verification failed"))) {
      await safeSignalUnknown(publicKey.rp?.id, typeof encoded.id === "string" ? encoded.id : undefined);
    }
    throw err;
  }
}
