# Llama Labels Push Relay (Cloudflare Worker)

This Worker centralizes native (FCM) push delivery for Llama Labels.

The published mobile app is compiled against **one** Firebase project, so only a
holder of that project's service account can deliver push to it. Instead of
shipping that credential to every self-hosted server, the **maintainer** runs
this one Worker. Self-hosted Llama Labels servers forward push requests to it,
each authenticated with its own API key. Self-hosters need **no Firebase account
and never recompile the app**.

```
self-hosted Go server  --(Bearer per-server key)-->  this Worker  --(service account)-->  FCM
```

## One-time setup (maintainer)

1. Install deps and log in:
   ```sh
   cd worker
   npm install
   npx wrangler login
   ```
2. Create your local config, then create the two KV namespaces and paste the
   returned ids into it. `wrangler.toml` is gitignored (it holds your live KV
   ids); `wrangler.toml.example` is the committed template:
   ```sh
   cp wrangler.toml.example wrangler.toml
   npx wrangler kv namespace create API_KEYS
   npx wrangler kv namespace create OAUTH_CACHE
   ```
3. Set secrets from your Firebase service-account JSON (the same file the Go
   backend used as `FCM_SERVICE_ACCOUNT_FILE`):
   ```sh
   npx wrangler secret put FCM_CLIENT_EMAIL   # "client_email"
   npx wrangler secret put FCM_PRIVATE_KEY    # "private_key" (full PEM, keep newlines)
   npx wrangler secret put FCM_PROJECT_ID     # "project_id"
   npx wrangler secret put ADMIN_SECRET       # a long random string you choose
   ```
4. Deploy:
   ```sh
   npx wrangler deploy
   ```

## Issuing a key to a self-hoster

```sh
curl -X POST https://<your-worker>.workers.dev/admin/keys \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"label":"alice-server"}'
# -> {"id":"...","label":"alice-server","key":"<RAW KEY, shown only once>"}
```

Give the self-hoster the raw `key` (out of band). They set on their server:

```
PUSH_RELAY_URL=https://<your-worker>.workers.dev
PUSH_RELAY_KEY=<the raw key>
```

List keys: `curl -H "Authorization: Bearer $ADMIN_SECRET" .../admin/keys`
Revoke:    `curl -X DELETE -H "Authorization: Bearer $ADMIN_SECRET" .../admin/keys/<id>`

## Endpoints

| Method | Path              | Auth                   | Purpose                          |
| ------ | ----------------- | ---------------------- | -------------------------------- |
| POST   | `/send`           | Bearer per-server key  | Deliver one push                 |
| POST   | `/admin/keys`     | Bearer `ADMIN_SECRET`  | Mint a key (returns raw key once)|
| GET    | `/admin/keys`     | Bearer `ADMIN_SECRET`  | List key metadata                |
| DELETE | `/admin/keys/{id}`| Bearer `ADMIN_SECRET`  | Revoke a key                     |

`POST /send` body:

```json
{ "token": "<FCM registration token>", "title": "…", "body": "…", "data": { "url": "/read" }, "platform": "android" }
```

Responses: `200 {"ok":true}` on delivery; `410 {"stale":true}` when the token is
no longer registered (the Go server then removes the device); `401` for a bad
key; `502` for other upstream FCM errors.

## Notes

- Only the SHA-256 hash of each API key is stored in KV; the raw key is shown
  once at creation.
- The Google OAuth access token is cached in the `OAUTH_CACHE` KV namespace and
  refreshed ~1 minute before expiry.
- Optional hardening: add a Cloudflare rate-limit rule on `/send`, or a per-key
  KV counter, to cap abuse. Not required for a basic deployment.
