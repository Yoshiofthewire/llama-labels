# Backend

## Purpose

Go module owning the email classification engine, HTTP API server, IMAP integration, Ollama integration, polling loop, configuration, state persistence, health tracking, logging, and PII redaction.

## Ownership

All code under `backend/`. Produces the `llama-lab` binary consumed by the container at runtime.

## Local Contracts

- Go 1.26.4; dependencies limited to `go-imap` and `yaml.v3`
- Entry point: `cmd/main.go` → `app.Run(os.Args)`
- All business logic lives under `internal/`; `cmd/` contains only the entry point
- Binary output: `llama-lab`, deployed to `/app/bin/llama-lab` in the container
- HTTP API listens on `WEB_PORT` (default 5866)
- Three runtime modes: `daemon` (poller only), `server` (API only), `all` (both)
- Secrets (IMAP password, Ollama auth) are encrypted at rest under `SECRET_DIR`
- State is persisted as JSON under `STATE_DIR`: checkpoint (last processed IMAP UID), processed set, decisions log
- Logs are structured JSON, written to stdout and a rotating file (16 MB max × 8 backups) under `LOG_DIR`

### Internal Package Layout

| Package | Responsibility |
|---------|---------------|
| `app/` | Mode flag parsing; bootstrap logger, config, poller, API server |
| `api/` | 21 HTTP endpoints; scrypt session auth; config/IMAP/Ollama/health/decisions/logs/tuning/mail-send/draft-save |
| `adapters/imap/` | IMAP UID-based email fetching; credential decrypt |
| `adapters/llama/` | Ollama `/api/generate` HTTP calls; 3s inter-request pacing; retry backoff |
| `processor/` | Timed polling loop (~90s default); orchestrates fetch → redact → classify → label → persist |
| `config/` | YAML config load/init; `Config` struct used across all packages |
| `state/` | Checkpoint, processed-set, and decisions JSON persistence |
| `health/` | Health status; sticky `aiCreditsExhausted` flag |
| `logging/` | Structured logger; rotating file writer |
| `redaction/` | Regex-based PII masking applied to sender, subject, and body before prompting Ollama |

### Classification Loop (daemon mode)

1. Poller fires on timer
2. Fetch unread emails from IMAP since last checkpoint
3. Apply redaction patterns to sender, subject, body
4. POST to Ollama `/api/generate` with tuning prompt + redacted email text
5. Fuzzy-match Ollama response against the label allowlist
6. Apply matched label as an IMAP keyword
7. Send browser push notifications for new emails when `notifications.mode` is `all`, or when mode is `keywords` and selected IMAP keywords match config
8. Persist decision (messageId, sender, subject, label, status, timestamp)
9. Advance checkpoint to next UID

### API Contract (consumed by frontend)

| Route | Auth | Notes |
|-------|------|-------|
| `POST /api/auth/login` | no | — |
| `GET /api/auth/me` | no | — |
| `POST /api/auth/logout` | yes | — |
| `POST /api/auth/password` | yes | — |
| `GET /api/setup` | no | Returns admin credential bootstrap status |
| `GET /api/health` | no | 503 when unhealthy |
| `POST /api/health/repair` | yes | Clears sticky failure state |
| `GET /api/status` | yes | Scan interval, checkpoint, rate limits, emails processed in the last hour, server time |
| `GET\|PUT /api/config` | yes | Full config; PUT broadcasts to running poller |
| `GET /api/labels` | yes | Allowed label list |
| `GET /api/decisions?limit=N` | yes | Audit trail |
| `GET /api/inbox?limit=N&mailbox=<name>` | yes | Live IMAP mailbox (read + unread) grouped by allowed keywords + Uncategorized |
| `GET\|POST\|PUT\|DELETE /api/inbox/folders` | yes | `GET` lists immediate child folders under an IMAP mailbox parent and marks which folders are deletable in the UI; omit `parent` to list top-level non-Archive mailbox links for the inbox nav. `POST` creates a single child folder under the requested parent (Inbox UI uses `parent=INBOX`). `PUT` renames a custom child folder by replacing only the leaf name. `DELETE` removes a custom child folder after moving its messages to the parent mailbox; built-in folders are rejected |
| `POST /api/inbox/actions` | yes | Bulk inbox actions: `delete`, `archive`, `spam`, `read`, `move` by `messageIds[]`, optional `mailbox`, and `targetMailbox` for `move`; actions execute in the selected mailbox, and `archive` moves to `Archive/<email sent year>` (fallback received year/current year) and creates folder if needed |
| `GET /api/logs?file=<name>.log&lines=<n>` | yes | Log tail |
| `GET /api/logs/list` | yes | Log file inventory |
| `GET\|POST /api/llama/auth` | yes | Ollama auth token management |
| `POST /api/llama/test` | yes | Classify a test email |
| `GET\|POST\|DELETE /api/imap/config` | yes | Encrypted IMAP credentials plus optional SMTP host/port override used by `/api/mail/send` |
| `POST /api/imap/test` | yes | Live IMAP connectivity check |
| `POST /api/mail/draft` | yes | Saves compose content to the IMAP Drafts folder |
| `POST /api/mail/send` | yes | Sends compose email via SMTP using configured credentials, logs send attempts/results, applies a send timeout, and appends successful sends to Sent mailbox (response can include warning when Sent append fails) |
| `GET\|PUT /api/tuning` | yes | TUNING.md read/write |
| `GET /api/notifications/vapid-public-key` | yes | VAPID public key for browser push subscription setup |
| `POST\|DELETE /api/notifications/subscriptions` | yes | Upsert or remove a browser push subscription for the signed-in user/device |
| `POST /api/notifications/test` | yes | Sends a test push notification to all stored subscriptions for the signed-in user and prunes stale endpoints |

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
| `LLAMA_AUTH_FILE` | `$CONFIG_DIR/llama-auth.json` | Ollama auth token storage |

### Key Data Files

| File | Purpose |
|------|---------|
| `$CONFIG_DIR/config.yaml` | Main application config |
| `$CONFIG_DIR/admin.env` | Scrypt-hashed admin credentials |
| `$CONFIG_DIR/TUNING.md` | Classification prompt template |
| `$CONFIG_DIR/notifications-vapid-private.pem` | Generated browser push private key for notification subscriptions |
| `$CONFIG_DIR/llama-auth.json` | Ollama auth token |
| `$SECRET_DIR/imap-config.json` | Encrypted IMAP credentials |
| `$SECRET_DIR/imap-config.key` | AES key for IMAP credentials |
| `$STATE_DIR/state.json` | Checkpoint + processed-set + persisted browser push subscriptions |
| `$STATE_DIR/decisions.json` | Decision audit log |

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
