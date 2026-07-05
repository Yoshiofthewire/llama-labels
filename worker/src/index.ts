/**
 * Llama Labels push relay — a Cloudflare Worker that centralizes FCM delivery.
 *
 * The published mobile app is bound at build time to a single Firebase project.
 * This Worker holds that project's service account (as secrets) and delivers
 * push notifications on behalf of many self-hosted Llama Labels servers, each
 * authenticated with its own API key. Self-hosters therefore never need a
 * Firebase account and never recompile the app.
 *
 * Endpoints:
 *   POST   /send            (Bearer <per-server API key>)  -> deliver one push
 *   POST   /admin/keys      (Bearer ADMIN_SECRET)          -> mint an API key
 *   GET    /admin/keys      (Bearer ADMIN_SECRET)          -> list key metadata
 *   DELETE /admin/keys/{id} (Bearer ADMIN_SECRET)          -> revoke a key
 */

import { FcmConfig, FcmMessage, sendFcmMessage } from "./fcm";

export interface Env {
  API_KEYS: KVNamespace;
  OAUTH_CACHE: KVNamespace;
  FCM_CLIENT_EMAIL: string;
  FCM_PRIVATE_KEY: string;
  FCM_PROJECT_ID: string;
  ADMIN_SECRET: string;
}

interface ApiKeyRecord {
  id: string;
  label: string;
  enabled: boolean;
  createdAt: string;
}

const KEY_PREFIX = "key:";
const KEY_INDEX_PREFIX = "keyid:"; // keyid:<id> -> <sha256(key)> for revoke-by-id

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function bearer(request: Request): string {
  const header = request.headers.get("Authorization") ?? "";
  const match = /^Bearer\s+(.+)$/i.exec(header.trim());
  return match ? match[1].trim() : "";
}

/**
 * Constant-time-ish comparison for the admin secret.
 */
function timingSafeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) {
    return false;
  }
  let mismatch = 0;
  for (let i = 0; i < a.length; i++) {
    mismatch |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return mismatch === 0;
}

async function sha256Hex(input: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(input));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function randomToken(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function requireAdmin(request: Request, env: Env): boolean {
  const secret = (env.ADMIN_SECRET ?? "").trim();
  if (!secret) {
    return false;
  }
  return timingSafeEqual(bearer(request), secret);
}

function fcmConfig(env: Env): FcmConfig {
  return {
    clientEmail: (env.FCM_CLIENT_EMAIL ?? "").trim(),
    privateKeyPem: env.FCM_PRIVATE_KEY ?? "",
    projectId: (env.FCM_PROJECT_ID ?? "").trim(),
  };
}

// ---- /send -----------------------------------------------------------------

async function handleSend(request: Request, env: Env): Promise<Response> {
  const presented = bearer(request);
  if (!presented) {
    return json({ error: "missing api key" }, 401);
  }
  const record = await env.API_KEYS.get<ApiKeyRecord>(KEY_PREFIX + (await sha256Hex(presented)), "json");
  if (!record || !record.enabled) {
    return json({ error: "invalid api key" }, 401);
  }

  let payload: Partial<FcmMessage>;
  try {
    payload = (await request.json()) as Partial<FcmMessage>;
  } catch {
    return json({ error: "invalid json body" }, 400);
  }
  const token = (payload.token ?? "").trim();
  if (!token) {
    return json({ error: "missing token" }, 400);
  }

  const config = fcmConfig(env);
  if (!config.clientEmail || !config.privateKeyPem || !config.projectId) {
    return json({ error: "relay not configured" }, 500);
  }

  const message: FcmMessage = {
    token,
    title: payload.title ?? "",
    body: payload.body ?? "",
    data: payload.data ?? {},
    platform: payload.platform,
  };

  let result;
  try {
    result = await sendFcmMessage(config, env.OAUTH_CACHE, message);
  } catch (err) {
    return json({ error: `relay send failed: ${(err as Error).message}` }, 502);
  }

  if (result.ok) {
    return json({ ok: true });
  }
  if (result.stale) {
    return json({ stale: true }, 410);
  }
  return json({ error: `fcm send failed: status=${result.status} response=${result.detail}` }, 502);
}

// ---- /admin/keys -----------------------------------------------------------

async function handleAdminCreate(request: Request, env: Env): Promise<Response> {
  let body: { label?: string };
  try {
    body = (await request.json()) as { label?: string };
  } catch {
    body = {};
  }
  const label = (body.label ?? "").trim() || "unnamed";
  const key = randomToken();
  const id = crypto.randomUUID();
  const record: ApiKeyRecord = {
    id,
    label,
    enabled: true,
    createdAt: new Date().toISOString(),
  };
  const hash = await sha256Hex(key);
  await env.API_KEYS.put(KEY_PREFIX + hash, JSON.stringify(record));
  await env.API_KEYS.put(KEY_INDEX_PREFIX + id, hash);

  // The raw key is returned exactly once; only its hash is stored.
  return json({ id, label, key }, 201);
}

async function handleAdminList(env: Env): Promise<Response> {
  const listed = await env.API_KEYS.list({ prefix: KEY_PREFIX });
  const records: ApiKeyRecord[] = [];
  for (const entry of listed.keys) {
    const record = await env.API_KEYS.get<ApiKeyRecord>(entry.name, "json");
    if (record) {
      records.push(record);
    }
  }
  return json({ keys: records });
}

async function handleAdminRevoke(id: string, env: Env): Promise<Response> {
  const hash = await env.API_KEYS.get(KEY_INDEX_PREFIX + id);
  if (!hash) {
    return json({ error: "key not found" }, 404);
  }
  await env.API_KEYS.delete(KEY_PREFIX + hash);
  await env.API_KEYS.delete(KEY_INDEX_PREFIX + id);
  return json({ ok: true });
}

// ---- router ----------------------------------------------------------------

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, "") || "/";

    if (path === "/send" && request.method === "POST") {
      return handleSend(request, env);
    }

    if (path === "/admin/keys") {
      if (!requireAdmin(request, env)) {
        return json({ error: "unauthorized" }, 401);
      }
      if (request.method === "POST") {
        return handleAdminCreate(request, env);
      }
      if (request.method === "GET") {
        return handleAdminList(env);
      }
      return json({ error: "method not allowed" }, 405);
    }

    const revokeMatch = /^\/admin\/keys\/([^/]+)$/.exec(path);
    if (revokeMatch && request.method === "DELETE") {
      if (!requireAdmin(request, env)) {
        return json({ error: "unauthorized" }, 401);
      }
      return handleAdminRevoke(decodeURIComponent(revokeMatch[1]), env);
    }

    return json({ error: "not found" }, 404);
  },
};
