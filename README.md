<img src="./llamalabel.png" alt="Llama Labels" />

# llama Mail

llama Mail is a self-hosted IMAP web client with automatic keyword labeling powered by a local Ollama model.

It polls unread mail, classifies messages, applies IMAP keywords, and includes a full browser UI for reading, configuration, notifications, logs, and compose (send + draft).

## Features

- Docker-first single-container runtime managed by supervisord
- IMAP inbox reader with folder management and drag/drop move actions
- Automatic keyword labeling for unread mail
- Built-in compose flow with SMTP send and IMAP draft save
- Browser push notifications (all mail or keyword-only)
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
- `SERVER_BASE_URL` (optional but recommended for mobile pairing; public URL embedded as `srv` in QR and used to build `relay`)
- `NOVU_SECRET_KEY` (required for mobile push pairing and Novu event trigger)
- `NOVU_WORKFLOW_ID` (required Novu workflow identifier, for example `new-email-push-notification`)
- `NOVU_APPLICATION_IDENTIFIER` (required public Novu app id encoded in desktop pairing QR)
- `NOVU_API_BASE` (default `https://api.novu.co`; set EU endpoint only if your Novu project is in EU region)

Notes:

- `Dockerfile` sets a fallback model of `nemotron-3-nano:4b`.
- `docker-compose.yml` overrides model default to `gemma4:e4b` unless you set `OLLAMA_MODEL`.
- The image sets `OLLAMA_MODELS=/llama_lab/ollama-models`.

Create model cache directory once before first run:

```bash
mkdir -p share/ollama/models
```

## Mobile App Pairing (Novu)

If you use the mobile app pairing flow, each deployment/client must provide its own Novu credentials.

- Do not reuse or share Novu keys across organizations/environments.
- This repo does not ship default Novu secrets.
- The backend keeps `NOVU_SECRET_KEY` server-side and never sends it to clients.

Required Novu setup per client/deployment:

1. Create your own Novu project.
2. Create a workflow and set `NOVU_WORKFLOW_ID` to that workflow's identifier.
3. Copy your Novu application identifier into `NOVU_APPLICATION_IDENTIFIER`.
4. Set `NOVU_SECRET_KEY` from your Novu environment.
5. Connect and activate an `fcm` push integration in that same Novu environment (Integration Store -> Push -> Firebase Cloud Messaging).
6. Ensure your mobile app is configured for that same Novu project and its own Firebase/FCM project.

Desktop pairing behavior:

- Notifications page renders a QR link with `app`, `sub`, `hash`, `api`, `srv`, `relay`, and `pt`.
- Set `SERVER_BASE_URL` in `.env` so `srv` and `relay` always point to the deployment address the mobile app should use (no manual server URL entry required).
- `pt` is a signed pairing token valid for 90 seconds.
- UI shows a 4px countdown bar under the QR that shrinks over 90 seconds, transitions green to red, and is red during the last 15 seconds.
- Mobile app scans QR and syncs its device token through `relay` (or `srv + /api/notifications/novu/relay/fcm` fallback).
- Relay endpoint (`POST /api/notifications/novu/relay/fcm`) validates `pt` and registers token to Novu using server-side credentials to avoid mobile-side 401s.

## Persistence

Named volumes:

- `llama_config` -> `/llama_lab/config`
- `llama_private` -> `/llama_lab/private`
- `llama_logs` -> `/llama_lab/logs`
- `llama_state` -> `/llama_lab/state`

Host bind mount:

- `${OLLAMA_MODELS_HOST_DIR:-./share/ollama/models}` -> `/llama_lab/ollama-models`

Important files:

- `/llama_lab/config/config.yaml`
- `/llama_lab/config/admin.env`
- `/llama_lab/config/TUNING.md`
- `/llama_lab/config/notifications-vapid-private.pem`
- `/llama_lab/private/imap-config.json`
- `/llama_lab/private/imap-config.key`
- `/llama_lab/state/state.json`
- `/llama_lab/state/decisions.json`

## API Highlights

Auth:

- `POST /api/auth/login`
- `GET /api/auth/me`
- `POST /api/auth/logout`
- `POST /api/auth/password`

Runtime:

- `GET /api/status`
- `GET /api/health`
- `POST /api/health/repair`

Config and data:

- `GET|PUT /api/config`
- `GET /api/labels`
- `GET /api/decisions`
- `GET|PUT /api/tuning`

IMAP and inbox:

- `GET|POST|DELETE /api/imap/config`
- `POST /api/imap/test`
- `GET /api/inbox?limit=500&mailbox=<name>`
- `POST /api/inbox/actions`
- `GET|POST|PUT|DELETE /api/inbox/folders`

Mail:

- `POST /api/mail/send`
- `POST /api/mail/draft`

Notifications:

- `GET /api/notifications/vapid-public-key`
- `POST|DELETE /api/notifications/subscriptions`
- `POST /api/notifications/test`

Logs:

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