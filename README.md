<img src="./llamalabel.png" alt="Llama Labels" />

# llama Mail

llama Mail is a self-hosted IMAP web client with automatic keyword labeling powered by a local Ollama model.

It polls unread mail, classifies messages, applies IMAP keywords, and includes a full browser UI for reading, configuration, notifications, logs, and compose (send + draft).

## Features

- Docker-first single-container runtime managed by supervisord
- Multi-user with roles: admins manage users and system settings; each user connects their own IMAP mailbox
- IMAP inbox reader with folder management and drag/drop move actions
- Automatic keyword labeling for unread mail (each active user's mailbox is polled independently)
- Built-in compose flow with SMTP send and IMAP draft save
- Browser push notifications (all mail or keyword-only), per user
- Config UI for IMAP, SMTP, model auth, tuning, logs, health, and decisions
- A dozen Theme presets 

## Architecture

The container runs these processes:

- API server: `llama-lab --mode server`
- Polling daemon: `llama-lab --mode daemon`
- Ollama service: `ollama serve`
- One-shot startup pull: `ollama pull <configured model>`

Classification flow:

1. Fetch unread messages from IMAP (`INBOX` by default).
2. Redact sensitive patterns.
3. Build prompt from sender, subject, body, and tuning context.
4. Call Ollama `/api/generate`.
5. Match output against allowed labels.
6. Apply IMAP keyword(s).
7. Persist checkpoint and decision history.

## Requirements

- Docker
- Docker Compose

Optional for local development (outside Docker):

- Go 1.26+
- Node.js 20+
- npm

## Quick Start

1. Clone the repository.
2. Optional: copy environment defaults.

```bash
cp .env.example .env
```

3. Build and start.

```bash
docker compose up --build -d
```

4. Open the UI:

- http://localhost:5866

5. Sign in with bootstrap credentials:

- Username: `admin`
- Password: `ChangeMeNow123!`

6. Change the password when prompted.
7. In Config, save IMAP and SMTP settings and run IMAP Test.
8. In Tuning, update labels/prompt and save.

## Session Behavior

- Login sessions expire after 24 hours of inactivity.
- Session expiry is sliding (each authenticated request extends TTL by 24h).
- Logout invalidates the server-side session and clears the cookie.
- Deactivating a user or changing their role takes effect on their very next request, not just at next login.

## Users and Roles

Accounts live in `/llama_lab/config/users.json` (roles: `admin`, `user`).

- Admins: manage users (create, change role, reset password, deactivate/reactivate) via the Manage Users page, view system logs, edit global settings (Application, Labels, Remote LLM), and trigger health repair.
- Users: connect their own IMAP/SMTP account, read and label their own mail, pair their own devices, set their own notification preferences, and tune their own prompt.
- Deactivation is a soft delete: the user can no longer sign in, but their data is retained on disk until removed manually.
- The last active admin cannot be deactivated or demoted.

Per-user data layout:

- `/llama_lab/config/users/<userID>/`: encrypted IMAP credentials, tuning prompt (`tuning.md`), notification preferences (`config.yaml`)
- `/llama_lab/state/users/<userID>/`: mailbox checkpoint + processed set (`state.json`), decision history (`decisions.json`), push subscriptions, paired devices

Upgrading from a single-admin install: on first start the legacy `admin.env` account is imported into `users.json`, and the legacy global mailbox state, IMAP credentials, tuning file, and notification preferences are copied into that admin's per-user directories. Legacy files are left in place but no longer read. There is no automated rollback; deleting `users.json` and the `users/` directories resets to a fresh multi-user state.

## Ports

- `5866`: web UI + backend API
- `11434`: Ollama API (not exposed by default in `docker-compose.yml`)

## Environment Variables

Common variables:

- `WEB_PORT` (default `5866`)
- `TZ` (default `America/New_York`)
- `SECRET_DIR` (default `/llama_lab/private`)
- `OLLAMA_BASE_URL` (default `http://127.0.0.1:11434`)
- `OLLAMA_MODEL` (Compose default `gemma4:e4b`)
- `TUNING_FILE` (default `/llama_lab/config/TUNING.md`)
- `OLLAMA_MODELS_HOST_DIR` (default `./share/ollama/models`)
- `IMAP_CONFIG_FILE` (default `/llama_lab/private/imap-config.json`)
- `IMAP_CONFIG_KEY_FILE` (default `/llama_lab/private/imap-config.key`)
- `TOTP_SECRET_KEY_FILE` (default `/llama_lab/private/totp-secret.key`)
- `SERVER_BASE_URL` (optional but recommended for mobile pairing; public URL embedded as `srv` in QR and used to build `reg`)
- `PAIRING_SECRET` (required for mobile pairing token signing/validation)
- `PUSH_RELAY_URL` (optional; base URL of the central push relay Worker that delivers Android native push to FCM)
- `PUSH_RELAY_KEY` (per-server API key issued by the relay operator; required together with `PUSH_RELAY_URL` to enable Android native push)
- `APNS_RELAY_URL` (optional; base URL of the central APNs relay Worker that delivers iOS native push)
- `APNS_RELAY_KEY` (per-server API key issued by the relay operator; required together with `APNS_RELAY_URL` to enable iOS native push)

Notes:

- `Dockerfile` sets a fallback model of `nemotron-3-nano:4b`.
- `docker-compose.yml` overrides model default to `gemma4:e4b` unless you set `OLLAMA_MODEL`.
- The image sets `OLLAMA_MODELS=/llama_lab/ollama-models`.

Create model cache directory once before first run:

```bash
mkdir -p share/ollama/models
```

## Mobile App Pairing (Native)

Mobile pairing is backend-native and does not require Novu.

- Set `PAIRING_SECRET` on the server.
- Optionally set `SERVER_BASE_URL` so QR payloads always point to the correct public backend URL.
- Keep all pairing secrets server-side only.

Desktop pairing behavior:

- Notifications page renders a QR link with `sub`, `hash`, `srv`, `reg`, and `pt`.
- Set `SERVER_BASE_URL` in `.env` so `srv` and `reg` always point to the deployment address the mobile app should use (no manual server URL entry required).
- `pt` is a signed pairing token valid for 90 seconds.
- UI shows a 4px countdown bar under the QR that shrinks over 90 seconds, transitions green to red, and is red during the last 15 seconds.
- Mobile app scans QR and registers its push token through `reg` (or `srv + /api/notifications/native/register` fallback).

Native registration behavior:

- `POST /api/notifications/native/register` validates pairing token and stores native device metadata/token in backend state.
- `GET /api/notifications/native/devices` lists paired native devices.
- `DELETE /api/notifications/native/devices` removes a paired native device by `deviceId`.
- `POST /api/notifications/native/unpair` revokes all paired native devices for the current signed-in user.

Firebase credential guidance:

- The backend never holds Firebase credentials and never reads `google-services.json`.
- Native push is delivered through a central **push relay** (Cloudflare Worker) that holds the single Firebase service account the published mobile app is built against. This is what lets anyone run their own server with the same app without a Firebase account or a recompile.
- `google-services.json` belongs in the mobile project (Android app module, typically `app/google-services.json`) and should never be committed.

## Push Relays (Cloudflare Workers)

Native push delivery lives in Cloudflare Workers, run by the project maintainer:
- **Android/FCM**: [`worker/`](worker/) — Firebase Cloud Messaging relay
- **iOS/APNs**: [`worker-apns/`](worker-apns/) — Apple Push Notification service relay

Self-hosters ask the relay operator for per-server API keys:
- Android: set `PUSH_RELAY_URL` and `PUSH_RELAY_KEY` (Firebase relay)
- iOS: set `APNS_RELAY_URL` and `APNS_RELAY_KEY` (APNs relay)

Self-hosters need no Firebase or Apple Developer account, and the app is never recompiled.

Maintainers/relay operators: deploy both Workers and mint per-server keys. See [`worker/README.md`](worker/README.md) and [`worker-apns/README.md`](worker-apns/README.md) for setup, secrets, and key management.

## Persistence

Named volumes:

- `llama_config` -> `/llama_lab/config`
- `llama_private` -> `/llama_lab/private`
- `llama_logs` -> `/llama_lab/logs`
- `llama_state` -> `/llama_lab/state`

Host bind mount:

- `${OLLAMA_MODELS_HOST_DIR:-./share/ollama/models}` -> `/llama_lab/ollama-models`

Important files:

- `/llama_lab/config/config.yaml` (global system config)
- `/llama_lab/config/users.json` (user accounts and roles)
- `/llama_lab/config/users/<userID>/` (per-user IMAP credentials, tuning, notification preferences)
- `/llama_lab/config/TUNING.md` (default tuning for new users)
- `/llama_lab/config/notifications-vapid-private.pem` (shared web-push signing key)
- `/llama_lab/private/imap-config.key` (master encryption key for stored IMAP credentials)
- `/llama_lab/private/totp-secret.key` (master encryption key for stored TOTP secrets)
- `/llama_lab/state/state.json` (global state: AI-credits flag)
- `/llama_lab/state/users/<userID>/` (per-user mailbox state, decisions, devices, subscriptions)
- `/llama_lab/config/admin.env` (legacy single-admin seed; imported once, then unused)

## API Highlights

Auth:

- `POST /api/auth/login`
- `GET /api/auth/me`
- `POST /api/auth/logout`
- `POST /api/auth/password`

User management (admin only):

- `GET|POST /api/users`
- `PUT /api/users/{id}` (change role)
- `POST /api/users/{id}/reset-password`
- `POST /api/users/{id}/deactivate`
- `POST /api/users/{id}/reactivate`

Runtime:

- `GET /api/status`
- `GET /api/health`
- `POST /api/health/repair` (admin only)

Config and data:

- `GET|PUT /api/config` (PUT of Remote LLM fields is admin only)
- `GET /api/labels`
- `GET /api/decisions` (caller's own decisions)
- `GET|PUT /api/tuning` (caller's own tuning prompt)

IMAP and inbox:

- `GET|POST|DELETE /api/imap/config`
- `POST /api/imap/test`
- `GET /api/inbox?limit=500&mailbox=<name>`
- `POST /api/inbox/actions`
- `GET|POST|PUT|DELETE /api/inbox/folders`

Mail:

- `POST /api/mail/send` (optional `attachments: [{name, mimeType, dataBase64}]`, 25 MB total)
- `POST /api/mail/draft` (same optional `attachments` shape)
- `GET /api/mail/attachments?mailbox=&messageId=` (list a message's attachment metadata)
- `GET /api/mail/attachment?mailbox=&messageId=&index=` (download one attachment)

Notifications (all scoped to the signed-in user):

- `GET|PUT /api/notifications/preferences`
- `GET /api/notifications/vapid-public-key`
- `POST|DELETE /api/notifications/subscriptions`
- `POST /api/notifications/test`
- `GET /api/notifications/pairing`
- `POST /api/notifications/native/register`
- `GET|DELETE /api/notifications/native/devices`
- `POST /api/notifications/native/unpair`

Logs (admin only):

- `GET /api/logs?file=<name>.log&lines=<n>`
- `GET /api/logs/list`

## Build and Dev Checks

Backend:

```bash
cd backend
go build -buildvcs=false ./...
go test ./...
```

Frontend:

```bash
cd frontend
npm install
npm run build
```

## Operations

Useful runtime checks:

```bash
docker compose ps
docker compose logs -f llama-lab
docker exec -it llama-lab ps aux
docker exec -it llama-lab ls -la /llama_lab/config /llama_lab/state
docker volume ls | grep llama
```

Persistence behavior:

- `docker compose up --build` keeps named volumes.
- `docker compose down -v` removes named volumes and stored app data.

## Troubleshooting

### Ollama or model issues

- Check logs with `docker compose logs -f llama-lab`.
- Confirm model pull completed for your configured `OLLAMA_MODEL`.
- Restart if needed: `docker compose restart`.

### IMAP connectivity issues

- Verify host, port, username, password, and mailbox in Config.
- Run IMAP Test in Config.
- Check `daemon.log` and `app.log` for auth/TLS/keyword failures.

### SMTP send issues

- Verify SMTP host and port in Config.
- Port 465 requires implicit TLS (supported).
- Use app passwords if your provider requires them.
- Check `app.log` for `mail send failed` details.

### Labels not being applied

- Confirm labels exist in allowlist/tuning.
- Confirm unread inbox has eligible messages.
- Check Decisions page and poller logs.

### PWA install on Firefox

- Firefox may not emit the same install prompt event as Chromium browsers.
- Service worker and manifest are still provided, but install UX can differ by browser.

## Project Structure

- `backend/`: Go API, poller, adapters, config, state, health
- `frontend/`: React + Vite UI
- `scripts/`: bootstrap and test helpers
- `Dockerfile`: single image build (backend, frontend, Ollama runtime)
- `docker-compose.yml`: local orchestration
- `supervisord.conf`: in-container process supervision