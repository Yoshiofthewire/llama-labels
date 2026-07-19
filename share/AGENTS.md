# Share

## Purpose

Persistent Ollama model cache bind-mounted from the host into the container. Survives container rebuilds and recreations without requiring a re-download of model weights.

## Ownership

`share/ollama/models/` — managed by the host system and the `pull-ollama-model.sh` script. Not owned by application code.

## Local Contracts

- `ollama/models/blobs/` — content-addressed model layer files (SHA256 filenames)
- `ollama/models/manifests/` — model manifest metadata
- Bind-mounted at container path `/kypost/ollama-models` via `OLLAMA_MODELS_HOST_DIR` in `docker-compose.yml`
- Files are never committed to git (covered by `.gitignore`)
- Do not manually add or remove blob files; use `pull-ollama-model.sh` or the Ollama CLI

## Work Guidance

- To add a new model: update `OLLAMA_MODEL` env var and re-run `pull-ollama-model.sh`
- To free space: use `ollama rm <model>` inside a running container, or delete blobs manually only after confirming no manifest references them

## Verification

No automated checks. Verify by confirming Ollama can load the model on container start.

## Child DOX Index

No child AGENTS.md files.
