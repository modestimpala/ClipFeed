# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

**Do NOT run `go`, `npm`, or `python` on the host.** Always use Docker.

```bash
# Targeted rebuild + restart (preferred -- only rebuilds what changed)
docker compose up -d --build api       # Go API changes
docker compose up -d --build web       # frontend changes
docker compose up -d --build worker    # ingestion worker changes
docker compose up -d --build scout     # scout changes

# Run API tests inside Docker
make test-api-docker

# Stack management -- all commands respect COMPOSE_PROFILES / COMPOSE_FILE from .env
make up               # start services (profiles from .env)
make down             # stop all services
make build            # full rebuild, all images (slow -- avoid unless needed)
make logs-api
make logs-worker
make shell-db         # sqlite3 shell into /data/clipfeed.db

# Maintenance
make lifecycle        # expire old clips (run periodically / cron)
make score            # trigger score update pass
```

Configure which optional services start via `COMPOSE_PROFILES` in `.env`:

| `.env` setting | What starts |
|---|---|
| `COMPOSE_PROFILES=` | Base stack (no AI) |
| `COMPOSE_PROFILES=ai` | + scout (cloud LLM) |
| `COMPOSE_PROFILES=ai,ollama` | + scout + local Ollama |

For GPU, add `COMPOSE_FILE=docker-compose.yml:docker-compose.gpu.yml` to `.env`.

## Architecture

```
nginx :80
  ├── React PWA (web/)
  └── Go API :8080 (api/)
        ├── SQLite WAL (single connection, schema embedded)
        ├── MinIO :9000 (S3-compatible object storage)
        └── jobs table → Python Worker (ingestion/)
                           └── LLM :11434 ← Scout (scout/)
```

**Go API (`api/`)** -- all files are `package main`, split by domain. The central `App` struct in `main.go` holds `db`, `minio`, `cfg`, an in-memory `TopicGraph` (refreshed every few minutes), and an `LTRModel` (loaded from `l2r_model.json` beside the DB, refreshed every 5 min). Router is Chi. Schema is embedded via `//go:embed schema.sql` and applied idempotently at startup -- there is no migration runner.

**Feed algorithm** (`feed.go`, `ranking.go`) -- SQLite does the initial sort:
`content_score * (1 - exploration_rate) + random * exploration_rate`
Then Go post-processes: applies topic weight multipliers, embedding-based L2R rescoring (cosine similarity of float32 blobs), and deduplicates clips seen in the last 24 hours.

**Embeddings** are stored as raw `BLOB` (float32, little-endian) in `clip_embeddings` and `user_embeddings`. Helpers `blobToFloat32` / `float32ToBlob` in `ranking.go` handle conversion.

**Ingestion Worker (`ingestion/worker.py`)** -- polls the `jobs` SQLite table. Pipeline per job: yt-dlp download → ffmpeg scene-split (15–90s clips) → transcode → faster-whisper transcription → KeyBERT topic extraction → sentence-transformers embedding → MinIO upload → DB update. Retry with exponential backoff (base 30s). Max concurrent jobs controlled by `MAX_CONCURRENT_JOBS` env.

**Scout (`scout/worker.py`)** -- LLM-backed content discovery. Reads `scout_sources`, fetches candidate videos from platforms, scores them via LLM, stores in `scout_candidates`. Candidates approved via `POST /api/scout/candidates/{id}/approve` trigger ingestion.

**Frontend (`web/src/`)** -- feature-first layout:
- `app/` -- routing and composition only (`App.jsx` manages auth state and tab switching)
- `features/{auth,feed,ingest,jobs,saved,settings}/` -- all feature-specific logic and UI
- `shared/{api,ui,hooks}/` -- API client (`clipfeedApi.js`), reusable primitives, hooks

No router library -- tab state is plain `useState` in `App.jsx`. Auth is JWT stored in `localStorage`, passed as `Bearer` token.

## Key Conventions

- **No migration system**: the schema (`api/schema.sql`) uses `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` / `CREATE TRIGGER IF NOT EXISTS` throughout and is re-applied on every API startup.
- **SQLite single-connection**: `db.SetMaxOpenConns(1)` prevents write conflicts; all writes serialize naturally.
- **Clip lifecycle**: clips expire after `CLIP_TTL_DAYS` (default 30). Saving/favoriting sets `is_protected = 1` via trigger, exempting them from eviction.
- **Storage keys**: video files are stored in MinIO under `storage_key`; thumbnails under `thumbnail_key`. Stream URLs are presigned MinIO URLs returned by `/api/clips/{id}/stream`.
- **Cross-feature imports**: forbidden -- use `shared/` as the boundary between features.
- **API contracts**: preserve existing request/response shapes unless migration is explicitly in scope.
