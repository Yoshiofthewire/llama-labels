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
 *   GET    /health          (public)                       -> liveness + config
 *   POST   /register        (public)                       -> self-issue a key
 *   POST   /send            (Bearer <per-server API key>)  -> deliver one push
 *   POST   /admin/keys      (Bearer ADMIN_SECRET)          -> mint an API key
 *   GET    /admin/keys      (Bearer ADMIN_SECRET)          -> list key metadata
 *   DELETE /admin/keys/{id} (Bearer ADMIN_SECRET)          -> revoke a key
 *
 * Per-key controls:
 *   - Expiry:        keys may carry an expiresAt; expired keys are rejected.
 *   - Rate limiting: per-minute cap via the native rate-limiting binding (no KV
 *                    writes). Hour/day rolling caps are deferred to the paid tier.
 *   - Usage:         per-send data points in Analytics Engine (off the KV path).
 *
 * Observability: every request gets a UUID request id, echoed in the
 * X-Request-Id response header and in error bodies, plus one structured JSON
 * access log line (visible via `wrangler tail` / Workers Logs / Logpush).
 */

import { FcmConfig, FcmMessage, sendFcmMessage } from "./fcm";

/** Cloudflare native rate-limiting binding (configured in wrangler.toml). */
interface RateLimitBinding {
  limit(options: { key: string }): Promise<{ success: boolean }>;
}

/** Cloudflare Workers Analytics Engine binding (subset we use). */
interface AnalyticsEngineDatasetLike {
  writeDataPoint(event: { indexes?: string[]; blobs?: (string | null)[]; doubles?: number[] }): void;
}

export interface Env {
  API_KEYS: KVNamespace;
  OAUTH_CACHE: KVNamespace;
  /** Minute-tier rate limiter (native binding, no KV writes). */
  PUSH_RATE_LIMITER: RateLimitBinding;
  /** Per-key usage counters, offloaded off the KV write path. */
  USAGE_ANALYTICS?: AnalyticsEngineDatasetLike;
  FCM_CLIENT_EMAIL: string;
  FCM_PRIVATE_KEY: string;
  FCM_PROJECT_ID: string;
  ADMIN_SECRET: string;
  /**
   * Minute limit is ENFORCED by the PUSH_RATE_LIMITER binding (simple.limit in
   * wrangler.toml). This var is display-only (/health + 429 body) and should be
   * kept equal to that binding's limit.
   *
   * TODO(paid-tier): hour/day rolling limits were removed to keep an accepted
   * send at zero KV writes on the free tier. Restore them with Durable Objects
   * (exact atomic counters, no KV write pressure) once on Workers Paid.
   */
  RATE_LIMIT_PER_MINUTE?: string; // display only; default 10
  /** Public self-registration (`POST /register`). "false" closes it; default open. */
  REGISTRATION_ENABLED?: string;
}

interface ApiKeyRecord {
  id: string;
  label: string;
  enabled: boolean;
  createdAt: string;
  /** ISO timestamp after which the key is rejected; null/absent = never expires. */
  expiresAt?: string | null;
  /** How the key was issued: "admin" (via ADMIN_SECRET) or "self" (via /register). */
  source?: "admin" | "self";
  /** Client IP captured at self-registration, for auditing abuse. */
  registeredIp?: string | null;
}

const KEY_PREFIX = "key:"; // key:<sha256(key)>      -> ApiKeyRecord (durable)
const KEY_INDEX_PREFIX = "keyid:"; // keyid:<id>     -> <sha256(key)> for revoke-by-id
const IP_INDEX_PREFIX = "ipkey:"; // ipkey:<ip>      -> keyId (one active key per IP)

const DEFAULT_LIMIT_PER_MINUTE = 10;

// ---- small helpers ---------------------------------------------------------

interface RequestContext {
  env: Env;
  ctx: ExecutionContext;
  requestId: string;
  log: (fields: Record<string, unknown>) => void;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** Error response carrying the request id so callers can correlate with logs. */
function fail(
  rc: RequestContext,
  status: number,
  message: string,
  extra?: Record<string, unknown>,
): Response {
  return json({ error: message, requestId: rc.requestId, ...(extra ?? {}) }, status);
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

function isConfigured(config: FcmConfig): boolean {
  return Boolean(config.clientEmail && config.privateKeyPem && config.projectId);
}

function isExpired(record: ApiKeyRecord, now: number): boolean {
  if (!record.expiresAt) {
    return false;
  }
  const at = Date.parse(record.expiresAt);
  return Number.isFinite(at) && at <= now;
}

// ---- per-key rate limits ---------------------------------------------------

/** Resolve a limit var: unset -> default; "0"/negative/invalid -> disabled. */
function resolveLimit(raw: string | undefined, fallback: number): number {
  if (raw === undefined || raw.trim() === "") {
    return fallback;
  }
  const parsed = Number.parseInt(raw.trim(), 10);
  return Number.isFinite(parsed) ? Math.max(0, parsed) : fallback;
}

/** Effective limits (0 = disabled), used by /health and the 429 body. */
function effectiveLimits(env: Env): { perMinute: number } {
  return { perMinute: resolveLimit(env.RATE_LIMIT_PER_MINUTE, DEFAULT_LIMIT_PER_MINUTE) };
}

// TODO(paid-tier): only the minute tier is enforced right now. The hour/day
// rolling limits were removed so an accepted send performs zero KV writes (they
// required a KV read-modify-write per send, which capped the free tier at
// ~1,000 pushes/day). Restore rolling hour/day caps with Durable Objects (exact,
// atomic, no KV write pressure) once this runs on Workers Paid — see
// push-relay-free-tier-ceiling notes.

/**
 * Minute-tier check via the native rate-limiting binding (no KV writes). Returns
 * true when allowed. Fails open if the binding is missing (e.g. local dev
 * without support) or errors, so delivery is never blocked by the limiter itself.
 */
async function checkMinuteLimit(env: Env, rc: RequestContext, hash: string): Promise<boolean> {
  const limiter = env.PUSH_RATE_LIMITER;
  if (!limiter || typeof limiter.limit !== "function") {
    return true;
  }
  try {
    const { success } = await limiter.limit({ key: hash });
    return success;
  } catch (err) {
    rc.log({ level: "error", event: "ratelimit.binding_error", error: String((err as Error).message ?? err) });
    return true;
  }
}

/**
 * Record one accepted send to Analytics Engine (off the KV write path). Query
 * lifetime totals per key later via the WAE SQL API. Best-effort: never throws.
 */
function recordUsageAnalytics(env: Env, record: ApiKeyRecord): void {
  const wae = env.USAGE_ANALYTICS;
  if (!wae) {
    return;
  }
  try {
    wae.writeDataPoint({
      indexes: [record.id],
      blobs: [record.id, record.label, record.source ?? "admin"],
      doubles: [1],
    });
  } catch {
    // analytics is best-effort; a send must never fail on it.
  }
}

// ---- /send -----------------------------------------------------------------

async function handleSend(request: Request, rc: RequestContext): Promise<Response> {
  const { env } = rc;
  const presented = bearer(request);
  if (!presented) {
    return fail(rc, 401, "missing api key");
  }
  const hash = await sha256Hex(presented);
  const record = await env.API_KEYS.get<ApiKeyRecord>(KEY_PREFIX + hash, "json");
  if (!record || !record.enabled) {
    rc.log({ level: "warn", event: "send.denied", reason: "invalid_key" });
    return fail(rc, 401, "invalid api key");
  }
  if (isExpired(record, Date.now())) {
    rc.log({ level: "warn", event: "send.denied", reason: "expired", keyId: record.id });
    return fail(rc, 401, "api key expired", { expiresAt: record.expiresAt });
  }

  // Validate the request before it counts against the rolling quota, so
  // malformed (400) or misconfigured (500) calls don't consume a self-hoster's
  // message budget.
  let payload: Partial<FcmMessage>;
  try {
    payload = (await request.json()) as Partial<FcmMessage>;
  } catch {
    return fail(rc, 400, "invalid json body");
  }
  const token = (payload.token ?? "").trim();
  if (!token) {
    return fail(rc, 400, "missing token");
  }

  const config = fcmConfig(env);
  if (!isConfigured(config)) {
    rc.log({ level: "error", event: "send.misconfigured", keyId: record.id });
    return fail(rc, 500, "relay not configured");
  }

  // Minute tier first (native binding, no KV). Then the hour/day tiers in KV.
  if (!(await checkMinuteLimit(env, rc, hash))) {
    rc.log({ level: "warn", event: "send.denied", reason: "rate_limited", keyId: record.id, window: "minute" });
    const response = fail(rc, 429, "rate limit exceeded", {
      window: "minute",
      limit: effectiveLimits(env).perMinute,
      retryAfterSeconds: 60,
    });
    response.headers.set("Retry-After", "60");
    return response;
  }

  // Count the accepted send in Analytics Engine (off the KV write path). No KV
  // write happens on the send path — see the hour/day TODO above.
  recordUsageAnalytics(env, record);

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
    rc.log({ level: "error", event: "send.error", keyId: record.id, error: String((err as Error).message ?? err) });
    return fail(rc, 502, `relay send failed: ${(err as Error).message}`);
  }

  if (result.ok) {
    rc.log({ level: "info", event: "send.ok", keyId: record.id });
    return json({ ok: true });
  }
  if (result.stale) {
    rc.log({ level: "info", event: "send.stale", keyId: record.id, fcmStatus: result.status });
    return json({ stale: true, requestId: rc.requestId }, 410);
  }
  rc.log({ level: "error", event: "send.fcm_failed", keyId: record.id, fcmStatus: result.status });
  return fail(rc, 502, `fcm send failed: status=${result.status} response=${result.detail}`);
}

// ---- /admin/keys -----------------------------------------------------------

type ExpiryResult = { ok: true; expiresAt: string | null } | { ok: false; error: string };

/**
 * Resolve an optional expiry from the admin create body. An explicit ISO
 * `expiresAt` wins; otherwise `ttlDays` (a positive number) is added to now.
 * `expiresAt: null` means the key never expires.
 */
function resolveExpiry(body: { expiresAt?: unknown; ttlDays?: unknown }): ExpiryResult {
  if (typeof body.expiresAt === "string" && body.expiresAt.trim()) {
    const at = Date.parse(body.expiresAt.trim());
    if (!Number.isFinite(at)) {
      return { ok: false, error: "invalid expiresAt (expected an ISO 8601 timestamp)" };
    }
    return { ok: true, expiresAt: new Date(at).toISOString() };
  }
  if (body.ttlDays !== undefined && body.ttlDays !== null) {
    const days = Number(body.ttlDays);
    if (!Number.isFinite(days) || days <= 0) {
      return { ok: false, error: "invalid ttlDays (expected a positive number)" };
    }
    return { ok: true, expiresAt: new Date(Date.now() + days * 86_400_000).toISOString() };
  }
  return { ok: true, expiresAt: null };
}

/**
 * Mint an API key, persist only its hash, and return the record plus the raw
 * key (which the caller returns to the client exactly once). Shared by the
 * admin endpoint and public self-registration.
 */
async function mintKey(
  env: Env,
  rc: RequestContext,
  opts: { label: string; expiresAt: string | null; source: "admin" | "self"; registeredIp?: string | null },
): Promise<{ record: ApiKeyRecord; key: string }> {
  const key = randomToken();
  const id = crypto.randomUUID();
  const record: ApiKeyRecord = {
    id,
    label: opts.label,
    enabled: true,
    createdAt: new Date().toISOString(),
    expiresAt: opts.expiresAt,
    source: opts.source,
    registeredIp: opts.registeredIp ?? null,
  };
  const hash = await sha256Hex(key);
  await env.API_KEYS.put(KEY_PREFIX + hash, JSON.stringify(record));
  await env.API_KEYS.put(KEY_INDEX_PREFIX + id, hash);
  rc.log({ level: "info", event: "key.created", keyId: id, label: opts.label, source: opts.source, expiresAt: opts.expiresAt });
  return { record, key };
}

async function handleAdminCreate(request: Request, rc: RequestContext): Promise<Response> {
  const { env } = rc;
  let body: { label?: string; ttlDays?: unknown; expiresAt?: unknown };
  try {
    body = (await request.json()) as typeof body;
  } catch {
    body = {};
  }
  const label = (body.label ?? "").trim() || "unnamed";

  const expiry = resolveExpiry(body);
  if (!expiry.ok) {
    return fail(rc, 400, expiry.error);
  }

  const { record, key } = await mintKey(env, rc, { label, expiresAt: expiry.expiresAt, source: "admin" });
  // The raw key is returned exactly once; only its hash is stored.
  return json({ id: record.id, label: record.label, key, expiresAt: record.expiresAt }, 201);
}

// ---- /register (public self-service) ---------------------------------------

/** Whether public self-registration is open (default: yes). */
function registrationEnabled(env: Env): boolean {
  return (env.REGISTRATION_ENABLED ?? "").trim().toLowerCase() !== "false";
}

/**
 * Public, unauthenticated self-registration: a self-hosted server obtains its
 * own per-server key with no maintainer involvement.
 *
 * One active key per IP: registering from an IP that already holds a key
 * invalidates the previous one (so a server that loses its key file can simply
 * re-register). Abuse is further bounded by the per-key rolling limits and the
 * REGISTRATION_ENABLED kill-switch; add a Cloudflare rate-limiting rule on this
 * route to cap how often a single IP can churn keys.
 */
async function handleRegister(request: Request, rc: RequestContext): Promise<Response> {
  const { env } = rc;
  if (!registrationEnabled(env)) {
    return fail(rc, 403, "self-registration is disabled");
  }
  let body: { label?: string };
  try {
    body = (await request.json()) as { label?: string };
  } catch {
    body = {};
  }
  const label = (body.label ?? "").trim() || "self-registered";
  const registeredIp = request.headers.get("CF-Connecting-IP");

  // Enforce one active key per IP: invalidate any prior key for this IP first.
  if (registeredIp) {
    const priorId = await env.API_KEYS.get(IP_INDEX_PREFIX + registeredIp);
    if (priorId && (await revokeKeyById(env, priorId))) {
      rc.log({ level: "info", event: "key.superseded", keyId: priorId, ip: registeredIp });
    }
  }

  // Self-registered keys don't expire by default — they back long-lived servers.
  const { record, key } = await mintKey(env, rc, { label, expiresAt: null, source: "self", registeredIp });
  if (registeredIp) {
    await env.API_KEYS.put(IP_INDEX_PREFIX + registeredIp, record.id);
  }
  return json({ id: record.id, label: record.label, key, expiresAt: record.expiresAt }, 201);
}

async function handleAdminList(rc: RequestContext): Promise<Response> {
  const { env } = rc;
  const now = Date.now();
  const listed = await env.API_KEYS.list({ prefix: KEY_PREFIX });
  const keys = [];
  for (const entry of listed.keys) {
    const record = await env.API_KEYS.get<ApiKeyRecord>(entry.name, "json");
    if (!record) {
      continue;
    }
    keys.push({
      id: record.id,
      label: record.label,
      enabled: record.enabled,
      createdAt: record.createdAt,
      expiresAt: record.expiresAt ?? null,
      expired: isExpired(record, now),
      source: record.source ?? "admin",
      registeredIp: record.registeredIp ?? null,
      // Usage (send counts + last-seen) lives in Analytics Engine, not KV.
    });
  }
  return json({ keys });
}

/**
 * Delete a key and all its indexes by id. Also clears the per-IP index, but only
 * if it still points at this key (so superseding a key doesn't drop a newer
 * registration's mapping). Returns false if the id was unknown.
 */
async function revokeKeyById(env: Env, id: string): Promise<boolean> {
  const hash = await env.API_KEYS.get(KEY_INDEX_PREFIX + id);
  if (!hash) {
    return false;
  }
  const record = await env.API_KEYS.get<ApiKeyRecord>(KEY_PREFIX + hash, "json");
  await env.API_KEYS.delete(KEY_PREFIX + hash);
  await env.API_KEYS.delete(KEY_INDEX_PREFIX + id);
  const ip = record?.registeredIp;
  if (ip) {
    const current = await env.API_KEYS.get(IP_INDEX_PREFIX + ip);
    if (current === id) {
      await env.API_KEYS.delete(IP_INDEX_PREFIX + ip);
    }
  }
  return true;
}

async function handleAdminRevoke(id: string, rc: RequestContext): Promise<Response> {
  const revoked = await revokeKeyById(rc.env, id);
  if (!revoked) {
    return fail(rc, 404, "key not found");
  }
  rc.log({ level: "info", event: "key.revoked", keyId: id });
  return json({ ok: true });
}

// ---- router ----------------------------------------------------------------

async function route(request: Request, path: string, rc: RequestContext): Promise<Response> {
  const { env } = rc;

  if (path === "/health" && request.method === "GET") {
    return json({
      ok: true,
      configured: isConfigured(fcmConfig(env)),
      rateLimits: effectiveLimits(env),
      registrationEnabled: registrationEnabled(env),
    });
  }

  if (path === "/send" && request.method === "POST") {
    return handleSend(request, rc);
  }

  if (path === "/register" && request.method === "POST") {
    return handleRegister(request, rc);
  }

  if (path === "/admin/keys") {
    if (!requireAdmin(request, env)) {
      return fail(rc, 401, "unauthorized");
    }
    if (request.method === "POST") {
      return handleAdminCreate(request, rc);
    }
    if (request.method === "GET") {
      return handleAdminList(rc);
    }
    return fail(rc, 405, "method not allowed");
  }

  const revokeMatch = /^\/admin\/keys\/([^/]+)$/.exec(path);
  if (revokeMatch && request.method === "DELETE") {
    if (!requireAdmin(request, env)) {
      return fail(rc, 401, "unauthorized");
    }
    return handleAdminRevoke(decodeURIComponent(revokeMatch[1]), rc);
  }

  return fail(rc, 404, "not found");
}

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const requestId = crypto.randomUUID();
    const start = Date.now();
    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, "") || "/";
    const log = (fields: Record<string, unknown>) =>
      console.log(JSON.stringify({ ts: new Date().toISOString(), requestId, ...fields }));

    const rc: RequestContext = { env, ctx, requestId, log };

    let response: Response;
    try {
      response = await route(request, path, rc);
    } catch (err) {
      log({ level: "error", event: "unhandled", method: request.method, path, error: String((err as Error)?.message ?? err) });
      response = json({ error: "internal error", requestId }, 500);
    }

    response.headers.set("X-Request-Id", requestId);
    log({
      level: response.status >= 500 ? "error" : "info",
      event: "request",
      method: request.method,
      path,
      status: response.status,
      durationMs: Date.now() - start,
    });
    return response;
  },
};
