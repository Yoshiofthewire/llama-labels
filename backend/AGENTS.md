# Backend

## Purpose

Go module owning the email classification engine, HTTP API server, IMAP integration, Ollama integration, polling loop, configuration, state persistence, health tracking, logging, and PII redaction.

## Ownership

All code under `backend/`. Produces the `llama-lab` binary consumed by the container at runtime.

## Local Contracts

- Go 1.26.4; direct dependencies: `go-imap`, `yaml.v3`, `webpush-go`, `golang.org/x/crypto`
- Entry point: `cmd/main.go` → `app.Run(os.Args)`
- All business logic lives under `internal/`; `cmd/` contains only the entry point
- Binary output: `llama-lab`, deployed to `/app/bin/llama-lab` in the container
- HTTP API listens on `WEB_PORT` (default 5866)
- Three runtime modes: `daemon` (poller only), `server` (API only), `all` (both)
- In Docker `server` and `daemon` run as separate processes that share no memory; all cross-process coordination happens through disk. Any store mutated by both processes must re-read from disk before mutating and write atomically (see `state.Store` and `users.Store`)
- Multi-user with roles (`admin`, `user`): accounts in `$CONFIG_DIR/users.json` (`users.Store`); sessions map token → `{userID, expiresAt}`; role is looked up live per request so deactivation/role changes apply immediately. Legacy `admin.env` is imported into `users.json` on first start (`users.LoadOrMigrate`), and legacy global data files are copied into the first admin's per-user dirs (`app/migrate.go`)
- Per-user data: IMAP credentials/tuning/notification prefs under `$CONFIG_DIR/users/<userID>/`; mailbox state (checkpoint, processed set, decisions, push subscriptions, native devices, pairing `subscriberId`) under `$STATE_DIR/users/<userID>/`. Global: `config.yaml` (timezone, log level, scan interval, rate limits, redaction, labels, Remote LLM, VAPID keys), root `$STATE_DIR/state.json` (AI-credits flag only)
- Secrets (IMAP passwords) are encrypted at rest with the single master key `$SECRET_DIR/imap-config.key`
- Logs are structured JSON, written to stdout and a rotating file (16 MB max × 8 backups) under `LOG_DIR`

### Internal Package Layout

| Package | Responsibility |
|---------|---------------|
| `app/` | Mode flag parsing; bootstrap logger, config, users store, legacy migration, poller, API server |
| `api/` | HTTP endpoints; session auth with role enforcement (`withAuth`/`withAdmin`, `AuthContext` via request context); user management; per-user config/IMAP/tuning/notification scoping |
| `users/` | Multi-user account store (`users.json`): roles, scrypt password hashing, soft-delete lifecycle, legacy `admin.env` migration |
| `adapters/imap/` | IMAP UID-based email fetching; credential decrypt; one `APIClient` per credential file (per user) |
| `adapters/llama/` | Ollama `/api/generate` HTTP calls; 3s inter-request pacing; retry backoff; tuning text passed per classify call |
| `processor/` | Timed polling loop (~90s default); polls every active user's mailbox per tick with bounded concurrency (4); per-user rate budgets; fault isolation (only all-users-failing flips global health) |
| `config/` | YAML config load/init; global `Config` plus per-user `UserSettings` (notification prefs) |
| `state/` | Per-user checkpoint, processed-set, decisions, subscriptions, devices; instantiated per user directory |
| `fsutil/` | Shared atomic file write + UUIDv4 helpers |
| `health/` | Health status; sticky `aiCreditsExhausted` flag |
| `logging/` | Structured logger; rotating file writer |
| `redaction/` | Regex-based PII masking applied to sender, subject, and body before prompting Ollama (shared engine, rebuilt on pattern change) |

### Classification Loop (daemon mode)

1. Poller fires on timer; lists active users from `users.json` and fans out over those with a stored IMAP config (bounded concurrency 4, per-user panic recovery)
2. Per user: fetch unread emails from their IMAP mailbox since their checkpoint
3. Apply global redaction patterns to sender, subject, body
4. POST to Ollama `/api/generate` with the user's tuning prompt + redacted email text (one shared, serialized Llama client across all users)
5. Fuzzy-match Ollama response against the global label allowlist
6. Apply matched label as an IMAP keyword in the user's mailbox
7. Send browser and native push notifications using the user's notification-mode gate (`none`, `all`, `keywords`) and the shared VAPID keys
8. Persist decision to the user's `decisions.json`
9. Advance the user's checkpoint to next UID

One failing mailbox never blocks other users; global health flips unhealthy only when every polled mailbox fails in the same tick.

### API Contract (consumed by frontend)

Auth values: `no` (public), `yes` (any signed-in user), `admin` (admin role required; non-admin gets 403). All `yes` routes that touch mailbox/notification/tuning data operate on the calling user's own resources.

| Route | Auth | Notes |
|-------|------|-------|
| `POST /api/auth/login` | no | Validates against `users.json`; inactive users rejected |
| `GET /api/auth/me` | no | Returns `userId`, `username`, `role`, `mustChangePassword`, `subscriberId` when authenticated |
| `POST /api/auth/logout` | yes | — |
| `POST /api/auth/password` | yes | Changes the calling user's own password |
| `GET\|POST /api/users` | admin | List / create users |
| `PUT /api/users/{id}` | admin | Change role; demoting the last active admin is rejected |
| `POST /api/users/{id}/reset-password` | admin | Sets a temp password with forced change on next login |
| `POST /api/users/{id}/deactivate` | admin | Soft delete; last active admin protected; live sessions die on next request |
| `POST /api/users/{id}/reactivate` | admin | — |
| `GET /api/setup` | no | Returns admin credential bootstrap status |
| `GET /api/health` | no | 503 when unhealthy |
| `POST /api/health/repair` | admin | Clears sticky failure state (container restart) |
| `GET /api/status` | yes | Scan interval, rate limits, caller's checkpoint and emails processed in the last hour, server time |
| `GET\|PUT /api/config` | yes | Global config; PUT rejects Remote LLM (`llama.*`) changes from non-admins with 403; PUT broadcasts to running poller |
| `GET /api/labels` | yes | Allowed label list + labels discovered in the caller's mailbox |
| `GET /api/decisions?limit=N` | yes | Caller's own audit trail |
| `GET /api/inbox?limit=N&mailbox=<name>` | yes | Live IMAP mailbox (read + unread) grouped by allowed keywords + Uncategorized |
| `GET\|POST\|PUT\|DELETE /api/inbox/folders` | yes | `GET` lists immediate child folders under an IMAP mailbox parent and marks which folders are deletable in the UI; omit `parent` to list top-level non-Archive mailbox links for the inbox nav. `POST` creates a single child folder under the requested parent (Inbox UI uses `parent=INBOX`). `PUT` renames a custom child folder by replacing only the leaf name. `DELETE` removes a custom child folder after moving its messages to the parent mailbox; built-in folders are rejected |
| `POST /api/inbox/actions` | yes | Bulk inbox actions: `delete`, `archive`, `spam`, `read`, `move` by `messageIds[]`, optional `mailbox`, and `targetMailbox` for `move`; actions execute in the selected mailbox, and `archive` moves to `Archive/<email sent year>` (fallback received year/current year) and creates folder if needed |
| `GET /api/logs?file=<name>.log&lines=<n>` | admin | Log tail |
| `GET /api/logs/list` | admin | Log file inventory |
| `POST /api/llama/test` | yes | Classify a test email |
| `GET\|POST\|DELETE /api/imap/config` | yes | Caller's encrypted IMAP credentials plus optional SMTP host/port override used by `/api/mail/send` |
| `POST /api/imap/test` | yes | Live IMAP connectivity check (falls back to caller's stored config) |
| `POST /api/mail/draft` | yes | Saves compose content to the caller's IMAP Drafts folder |
| `POST /api/mail/send` | yes | Sends compose email via SMTP using the caller's credentials, logs send attempts/results, applies a send timeout, and appends successful sends to Sent mailbox (response can include warning when Sent append fails) |
| `GET\|PUT /api/tuning` | yes | Caller's own tuning prompt (`users/<id>/tuning.md`); GET falls back to the install default `TUNING.md`; PUT needs no llama restart (tuning is passed per classify call) |
| `GET\|PUT /api/notifications/preferences` | yes | Caller's delivery mode + keywords (moved out of global config) |
| `GET /api/notifications/vapid-public-key` | yes | Shared VAPID public key for browser push subscription setup |
| `POST\|DELETE /api/notifications/subscriptions` | yes | Upsert or remove a browser push subscription in the caller's store |
| `POST /api/notifications/test` | yes | Sends a test push notification to the caller's subscriptions/devices and prunes stale endpoints |
| `GET /api/notifications/pairing` | yes | Returns native pairing info for the desktop QR code: caller's `subscriberId`, `serverBaseUrl`, `registerEndpoint`, `subscriberHash` (HMAC), `pairingToken`, `pairingExpiresAt`, `pairingTtlSeconds`, `configured` |
| `POST /api/notifications/native/register` | no | Native mobile registration. Accepts `subscriberId`, `pairingToken`, `deviceToken`, optional `subscriberHash` + device metadata; validates pairing token, resolves `subscriberId` → owning user (in-memory index over `$STATE_DIR/users/*/state.json`, lazily rescanned), stores the device in that user's state |
| `GET\|DELETE /api/notifications/native/devices` | yes | Lists or removes the caller's native devices by `deviceId` |
| `POST /api/notifications/native/unpair` | yes | Removes all of the caller's paired native devices |

### Environment Variables

| Variable | Default | Purpose |
|----------|---------|--------|
| `CONFIG_DIR` | `/llama_lab/config` | Config and admin files |
| `STATE_DIR` | `/llama_lab/state` | State JSON files |
| `LOG_DIR` | `/llama_lab/logs` | Log file directory |
| `SECRET_DIR` | `/llama_lab/private` | Encrypted secrets (IMAP key) |
| `WEB_PORT` | `5866` | HTTP API listen port |
| `OLLAMA_BASE_URL` | `http://127.0.0.1:11434` | Ollama service endpoint |
| `OLLAMA_MODEL` | `nemotron-3-nano:4b` | Classification model name |
| `TUNING_FILE` | `$CONFIG_DIR/TUNING.md` | Classification prompt template |
| `IMAP_CONFIG_FILE` | `$SECRET_DIR/imap-config.json` | Encrypted IMAP credentials |
| `IMAP_CONFIG_KEY_FILE` | `$SECRET_DIR/imap-config.key` | AES key for IMAP credentials |
| `SERVER_BASE_URL` | empty | Public backend URL embedded in mobile pairing QR (`srv`) and used to build register endpoint (`reg`) |
| `PAIRING_SECRET` | empty | HMAC secret used to sign and validate short-lived mobile pairing tokens |
| `PUSH_RELAY_URL` | empty | Base URL of the central push relay (Cloudflare Worker) that delivers native push to FCM. When set with `PUSH_RELAY_KEY`, enables native push |
| `PUSH_RELAY_KEY` | empty | Per-server API key issued by the relay operator; sent as `Authorization: Bearer` to the relay |

### Key Data Files

| File | Purpose |
|------|---------|
| `$CONFIG_DIR/config.yaml` | Global system config (admin-editable) |
| `$CONFIG_DIR/users.json` | User accounts, roles, scrypt password hashes (version-marked) |
| `$CONFIG_DIR/users/<userID>/imap-config.json` | User's encrypted IMAP credentials (master key encrypted) |
| `$CONFIG_DIR/users/<userID>/tuning.md` | User's classification prompt |
| `$CONFIG_DIR/users/<userID>/config.yaml` | User's notification delivery preferences |
| `$CONFIG_DIR/TUNING.md` | Default prompt template for users without their own tuning |
| `$CONFIG_DIR/notifications-vapid-private.pem` | Shared browser push private key |
| `$CONFIG_DIR/admin.env` | Legacy single-admin seed; imported once into `users.json`, then unused |
| `$SECRET_DIR/imap-config.key` | Master AES key for all stored IMAP credentials |
| `$SECRET_DIR/imap-config.json` | Legacy global IMAP credentials; migrated to the first admin, then unused |
| `$STATE_DIR/state.json` | Global state: sticky AI-credits flag |
| `$STATE_DIR/users/<userID>/state.json` | User's checkpoint + processed-set + push subscriptions + pairing `subscriberId` + native devices |
| `$STATE_DIR/users/<userID>/decisions.json` | User's decision audit log |

### Log Files

| File | Written by | Content |
|------|------------|--------|
| `app.log` | Go backend Logger | Structured API/app events |
| `api.log` / `api.err.log` | supervisord | stdout/stderr of the `api` process |
| `daemon.log` / `daemon.err.log` | supervisord | stdout/stderr of the `daemon` process |
| `llama.log` / `llama.err.log` | supervisord | Ollama runtime output |
| `llama-server.log` | llama adapter | Classify/warmup trace lines |
| `bootstrap.log` / `bootstrap.err.log` | supervisord | Bootstrap script output |
| `supervisord.log` | supervisord | Process manager events |

## Work Guidance

- Build: `cd backend && go build -buildvcs=false ./...`
- Test: `cd backend && go test ./...`
- Keep adapter packages free of direct state mutation; they communicate via interfaces and channels defined in `processor/`
- PII redaction must be applied before any text is sent to Ollama
- Do not add dependencies outside the go.mod without explicit approval

## Verification

- `go build -buildvcs=false ./...` must succeed with zero errors
- `go vet ./...` must pass

## Child DOX Index

- `internal/adapters/` — external protocol clients (IMAP + Ollama); see [internal/adapters/AGENTS.md](internal/adapters/AGENTS.md)
