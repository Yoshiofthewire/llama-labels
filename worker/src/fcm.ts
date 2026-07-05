/**
 * FCM HTTP v1 delivery for the Cloudflare Worker relay.
 *
 * Ports the Go backend's SDK-free FCM sender (manual RS256 JWT -> OAuth token
 * -> HTTP v1 send) to WebCrypto. The service account credentials live in Worker
 * secrets; the short-lived Google access token is cached in KV.
 */

const FCM_OAUTH_SCOPE = "https://www.googleapis.com/auth/firebase.messaging";
const GOOGLE_TOKEN_URL = "https://oauth2.googleapis.com/token";
const OAUTH_CACHE_KEY = "google_access_token";

export interface FcmConfig {
  clientEmail: string;
  privateKeyPem: string;
  projectId: string;
}

export interface FcmMessage {
  token: string;
  title: string;
  body: string;
  data?: Record<string, string>;
  platform?: string;
}

export type FcmResult =
  | { ok: true }
  | { ok: false; stale: true; status: number; detail: string }
  | { ok: false; stale: false; status: number; detail: string };

function base64UrlEncode(bytes: ArrayBuffer | Uint8Array): string {
  const arr = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let binary = "";
  for (let i = 0; i < arr.length; i++) {
    binary += String.fromCharCode(arr[i]);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function base64UrlEncodeString(input: string): string {
  return base64UrlEncode(new TextEncoder().encode(input));
}

/**
 * Import a PKCS8 PEM RSA private key for RS256 signing.
 */
async function importPrivateKey(pem: string): Promise<CryptoKey> {
  const normalized = pem.replace(/\\n/g, "\n").trim();
  const body = normalized
    .replace(/-----BEGIN [^-]+-----/, "")
    .replace(/-----END [^-]+-----/, "")
    .replace(/\s+/g, "");
  const der = Uint8Array.from(atob(body), (c) => c.charCodeAt(0));
  return crypto.subtle.importKey(
    "pkcs8",
    der,
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["sign"],
  );
}

async function signServiceAccountAssertion(config: FcmConfig, nowSeconds: number): Promise<string> {
  const header = base64UrlEncodeString(JSON.stringify({ alg: "RS256", typ: "JWT" }));
  const claims = base64UrlEncodeString(
    JSON.stringify({
      iss: config.clientEmail,
      scope: FCM_OAUTH_SCOPE,
      aud: GOOGLE_TOKEN_URL,
      iat: nowSeconds,
      exp: nowSeconds + 3600,
    }),
  );
  const signingInput = `${header}.${claims}`;
  const key = await importPrivateKey(config.privateKeyPem);
  const signature = await crypto.subtle.sign(
    "RSASSA-PKCS1-v1_5",
    key,
    new TextEncoder().encode(signingInput),
  );
  return `${signingInput}.${base64UrlEncode(signature)}`;
}

/**
 * Return a valid Google OAuth access token, using the KV cache when possible.
 */
async function getAccessToken(config: FcmConfig, cache: KVNamespace): Promise<string> {
  const cached = await cache.get(OAUTH_CACHE_KEY);
  if (cached) {
    return cached;
  }

  const nowSeconds = Math.floor(Date.now() / 1000);
  const assertion = await signServiceAccountAssertion(config, nowSeconds);

  const form = new URLSearchParams();
  form.set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer");
  form.set("assertion", assertion);

  const resp = await fetch(GOOGLE_TOKEN_URL, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: form.toString(),
  });
  const text = await resp.text();
  if (!resp.ok) {
    throw new Error(`fcm oauth failed: status=${resp.status} response=${text}`);
  }
  const parsed = JSON.parse(text) as { access_token?: string; expires_in?: number };
  const token = (parsed.access_token ?? "").trim();
  if (!token) {
    throw new Error("fcm oauth token missing");
  }
  const expiresIn = parsed.expires_in && parsed.expires_in > 0 ? parsed.expires_in : 3600;
  // Refresh a minute early, and respect KV's 60s minimum TTL.
  const ttl = Math.max(60, expiresIn - 60);
  await cache.put(OAUTH_CACHE_KEY, token, { expirationTtl: ttl });
  return token;
}

/**
 * Detect FCM's "token no longer registered" signal. Mirrors the Go backend's
 * isFCMStaleResponse logic.
 */
function isStaleResponse(status: number, response: string): boolean {
  const lower = response.toLowerCase();
  if (
    lower.includes("unregistered") ||
    lower.includes("notregistered") ||
    lower.includes("registration-token-not-registered")
  ) {
    return true;
  }
  if (status === 404 && lower.includes("requested entity was not found")) {
    return true;
  }
  return false;
}

/**
 * Send a single push via FCM HTTP v1. Reproduces the exact envelope the Go
 * backend used: message.token, message.notification.{title,body},
 * message.data, message.android.priority = "HIGH".
 */
export async function sendFcmMessage(
  config: FcmConfig,
  cache: KVNamespace,
  message: FcmMessage,
): Promise<FcmResult> {
  const accessToken = await getAccessToken(config, cache);

  const payload = {
    message: {
      token: message.token,
      notification: {
        title: message.title,
        body: message.body,
      },
      data: message.data ?? {},
      android: {
        priority: "HIGH",
      },
    },
  };

  const sendURL = `https://fcm.googleapis.com/v1/projects/${config.projectId}/messages:send`;
  const resp = await fetch(sendURL, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${accessToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });

  if (resp.ok) {
    return { ok: true };
  }

  const detail = (await resp.text()).trim();
  if (isStaleResponse(resp.status, detail)) {
    return { ok: false, stale: true, status: resp.status, detail };
  }
  return { ok: false, stale: false, status: resp.status, detail };
}
