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
| `api/` | 18 HTTP endpoints; scrypt session auth; config/IMAP/Ollama/health/decisions/logs/tuning |
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
7. Persist decision (messageId, sender, subject, label, status, timestamp)
8. Advance checkpoint to next UID

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
| `GET /api/status` | yes | Scan interval, checkpoint, rate limits, server time |
| `GET\|PUT /api/config` | yes | Full config; PUT broadcasts to running poller |
| `GET /api/labels` | yes | Allowed label list |
| `GET /api/decisions?limit=N` | yes | Audit trail |
| `GET /api/inbox?limit=N` | yes | Live unread IMAP inbox grouped by allowed keywords + Uncategorized |
| `POST /api/inbox/actions` | yes | Bulk inbox actions: `delete`, `archive`, `spam`, `read` by `messageIds[]` |
| `GET /api/logs?file=<name>.log&lines=<n>` | yes | Log tail |
| `GET /api/logs/list` | yes | Log file inventory |
| `GET\|POST /api/llama/auth` | yes | Ollama auth token management |
| `POST /api/llama/test` | yes | Classify a test email |
| `GET\|POST\|DELETE /api/imap/config` | yes | Encrypted IMAP credentials |
| `POST /api/imap/test` | yes | Live IMAP connectivity check |
| `GET\|PUT /api/tuning` | yes | TUNING.md read/write |

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
| `$CONFIG_DIR/llama-auth.json` | Ollama auth token |
| `$SECRET_DIR/imap-config.json` | Encrypted IMAP credentials |
| `$SECRET_DIR/imap-config.key` | AES key for IMAP credentials |
| `$STATE_DIR/state.json` | Checkpoint + processed-set |
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
