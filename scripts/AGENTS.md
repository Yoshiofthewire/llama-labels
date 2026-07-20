# Scripts

## Purpose

Container initialization, process orchestration, and Ollama model management.

## Ownership

All files under `scripts/`.

## Local Contracts

### Startup Sequence

`entrypoint.sh` (runs `bootstrap.sh` synchronously, then chowns, then execs) → `supervisord` → `api`, `daemon`, `ollama`, `ollama-model` (parallel, priority 10)

- `entrypoint.sh`: creates required directories, runs `bootstrap.sh` inline (as root, before the privilege-dropping `chown`), chowns `/kypost` to `kypost`, then execs `supervisord`. Running `bootstrap.sh` as a blocking step here — rather than as its own supervisord program — makes "credentials exist before any service starts" a hard guarantee instead of one relying on supervisord's priority-based ordering.
- `bootstrap.sh`: on a fresh install (neither `users.json` nor `admin.env` present) generates scrypt-hashed admin credentials into `admin.env`, which the backend imports into `users.json` on first start; skipped entirely when either file exists, so it is safe across restarts
- `start-ollama.sh`: launches Ollama daemon on port 11434
- `pull-ollama-model.sh`: pulls the model named by `OLLAMA_MODEL` (docker-compose default: `nemotron-3-nano:4b`); requires Ollama daemon to be running first

### Supervisord Programs (from `supervisord.conf` at repo root)

| Program | Type | Priority |
|---------|------|----------|
| api | daemon | 10 |
| daemon | daemon | 10 |
| ollama | daemon | 10 |
| ollama-model | one-shot | 10 |

## Work Guidance

- `bootstrap.sh` must keep running before the `chown -R kypost:kypost /kypost` step in `entrypoint.sh` — it writes as root, and that chown is what hands the resulting files (`admin.env` included) to `kypost`
- `pull-ollama-model.sh` is idempotent; safe to re-run

## Verification

- On a fresh install `bootstrap.sh` must produce a valid `admin.env` with a non-empty scrypt hash owned by `kypost`; on an install with `users.json` or `admin.env` present it must leave both untouched

## Child DOX Index

No child AGENTS.md files.
