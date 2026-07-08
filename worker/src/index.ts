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
 *
 * The API-key admin/registration endpoints, rate limiting, usage analytics,
 * and small crypto/HTTP helpers are shared with the APNs relay (worker-apns/)
 * — see ../../push-relay-shared/push-relay-common.ts.
 */

import { FcmConfig, FcmMessage, sendFcmMessage } from "./fcm";
import {
  ApiKeyRecord,
  CommonEnv,
  DEFAULT_LIMIT_PER_MINUTE,
  KEY_PREFIX,
  RequestContext,
  bearer,
  checkMinuteLimit,
  fail,
  handleAdminCreate,
  handleAdminList,
  handleAdminRevoke,
  handleRegister,
  isExpired,
  json,
  recordUsageAnalytics,
  registrationEnabled,
  requireAdmin,
  resolveLimit,
  sha256Hex,
} from "../../push-relay-shared/push-relay-common";

export interface Env extends CommonEnv {
  OAUTH_CACHE: KVNamespace;
  FCM_CLIENT_EMAIL: string;
  FCM_PRIVATE_KEY: string;
  FCM_PROJECT_ID: string;
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

// ---- /send -----------------------------------------------------------------

async function handleSend(request: Request, rc: RequestContext<Env>): Promise<Response> {
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
      limit: resolveLimit(env.RATE_LIMIT_PER_MINUTE, DEFAULT_LIMIT_PER_MINUTE),
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

// ---- router ----------------------------------------------------------------

async function route(request: Request, path: string, rc: RequestContext<Env>): Promise<Response> {
  const { env } = rc;

  if (path === "/health" && request.method === "GET") {
    return json({
      ok: true,
      configured: isConfigured(fcmConfig(env)),
      rateLimits: { perMinute: resolveLimit(env.RATE_LIMIT_PER_MINUTE, DEFAULT_LIMIT_PER_MINUTE) },
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

    const rc: RequestContext<Env> = { env, ctx, requestId, log };

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
