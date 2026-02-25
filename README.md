# ClipFeed

Self-hosted short-form video platform with a transparent, user-controllable algorithm.

## Architecture

```
nginx :80
  ├── React PWA  (web/)
  └── Go API :8080  (api/)
        ├── SQLite WAL  (single connection, schema embedded)
        ├── MinIO :9000  (S3-compatible object storage)
        └── jobs table → Python Worker  (ingestion/)
                           └── LLM :11434 ← Scout *  (scout/)
                                              * ai profile only
```

## Stack

| Component | Tech | Purpose |
|-----------|------|---------|
| API | Go + Chi | REST API, auth, feed algorithm, search |
| Frontend | React + Vite PWA | Mobile-first swipe feed |
| Worker | Python | Video download, scene-split, transcode, transcribe |
| Score Updater | Python | Periodic content score recalculation |
| Scout | Python (ai profile) | LLM-backed content discovery and scoring |
| LLM | Ollama or hosted API (ai profile) | Local or hosted inference (summaries, scoring, scouting) |
| Database | SQLite (WAL) | Single-connection WAL: users, clips, interactions, jobs, topic graph, embeddings |
| Storage | MinIO | S3-compatible object storage for video and thumbnail files |
| Search | SQLite FTS5 | Full-text search across clip titles, transcripts, channels |
| Proxy | nginx | Reverse proxy, SPA routing, streaming optimization |

## Quick Start

```bash
# Clone and configure
cp .env.example .env
# Edit .env — set secrets and choose profiles (see below)

# Launch
make up

# Watch logs
make logs-worker
```

**Choosing what to start — set `COMPOSE_PROFILES` in `.env`:**

| `.env` setting | Services started |
|---|---|
| `COMPOSE_PROFILES=` | Base stack (no AI) |
| `COMPOSE_PROFILES=ai` | + Scout (cloud LLM) |
| `COMPOSE_PROFILES=ai,ollama` | + Scout + local Ollama |

**GPU acceleration** (requires NVIDIA Container Toolkit):
```
COMPOSE_FILE=docker-compose.yml:docker-compose.gpu.yml
```

**HTTPS (Automatic Let's Encrypt via Caddy):**
```bash
SERVER_NAME=clipfeed.yourdomain.com docker compose -f docker-compose.yml -f docker-compose.caddy.yml up -d
```

The app will be available at `http://localhost`.

## Content Pipeline

1. **Scouting** *(ai profile)*: Scout worker discovers and evaluates candidate videos via LLM scoring.
2. **Ingestion:** User submits a URL (or Scout auto-approves above threshold). Supports YouTube, Vimeo, TikTok, Instagram, etc.
3. **Download:** Worker fetches via yt-dlp (with optional platform cookies for authenticated access).
4. **Segmentation:** ffmpeg detects scene changes and splits into 15–90s clips.
5. **Transcoding:** Each clip is transcoded to mobile-optimized mp4.
6. **Transcription:** faster-whisper transcribes audio for search and topic extraction.
7. **Embeddings & Topics:** sentence-transformers generates embeddings; KeyBERT extracts topics and maps them into the topic graph.
8. **Storage:** Clips and thumbnails upload to MinIO; metadata writes to SQLite.
9. **Scoring:** Score Updater periodically recalculates `content_score` from aggregate interactions.

## Algorithm

The feed algorithm is fully transparent and user-controllable:

- **Exploration Rate** (0–100%): Balance between engagement-optimized and random discovery.
- **Clip Duration Bounds**: Minimum and maximum clip lengths.
- **Topic Weights**: Per-topic interest sliders to boost or suppress topics.
- **Saved Filters**: Reusable named filter presets.

The ranking pipeline:
1. SQLite initial sort: `content_score * (1 - exploration_rate) + random * exploration_rate`
2. Topic weight multipliers from user preferences
3. Embedding-based L2R rescoring (cosine similarity of float32 blobs)
4. 24-hour deduplication of recently seen clips

## Ingestion Limits vs User Preferences

- **Env vars** control the worker pipeline (clip duration, download limits, processing mode) — these bound what enters the global pool.
- **Settings page** lets each user filter the global pool to match their current preferences.

Key env vars (in `.env`):

| Variable | Default | Description |
|---|---|---|
| `PROCESSING_MODE` | `transcode` | `transcode` (scale to 720p vertical) or `copy` (fast, keeps original) |
| `MIN_CLIP_SECONDS` | `15` | Minimum clip duration after scene-split |
| `MAX_CLIP_SECONDS` | `90` | Maximum clip duration after scene-split |
| `TARGET_CLIP_SECONDS` | `45` | Target clip length |
| `MAX_VIDEO_DURATION` | `3600` | Maximum source video length in seconds |
| `MAX_DOWNLOAD_SIZE_MB` | `2048` | Maximum download size |
| `MAX_WORKERS` | `4` | Max concurrent ingestion jobs |
| `WHISPER_MODEL` | `medium` | faster-whisper model size |
| `CLIP_TTL_DAYS` | `30` | Days before unprotected clips expire |
| `SCORE_UPDATE_INTERVAL` | `900` | Seconds between score recalculation passes |

## Backup & Restore

```bash
make backup
# Creates backups/YYYYMMDD_HHMMSS/ containing clipfeed.db and minio.tar.gz

make restore BACKUP_DIR=backups/YYYYMMDD_HHMMSS
# Stops services, overwrites DB/storage from backup, restarts services
```

## Storage Lifecycle

Clips auto-expire after `CLIP_TTL_DAYS` (default 30 days). Saving or favoriting a clip sets `is_protected = 1` (via trigger), exempting it from eviction.

```bash
make lifecycle          # run manually
```

Or add to crontab:
```
0 3 * * * cd /path/to/clipfeed && make lifecycle
```

## Alternate Database (Postgres)

ClipFeed defaults to SQLite (WAL mode), which comfortably handles ~30–50 concurrent active users.

For heavier load (100+ concurrent users) or multi-tenant deployments, switch to PostgreSQL:

1. Set `DB_DRIVER=postgres` and `DB_URL=postgres://user:pass@host:5432/clipfeed` in `.env`
2. Restart the API — the backend initializes schema on boot automatically.

## Frontend Configuration

The React frontend reads `window.__CONFIG__` at runtime, so the same build can be pointed at any backend. To deploy the UI on Vercel/Netlify/Pages, edit `web/index.html`:

```html
<script>
  window.__CONFIG__ = {
    API_BASE: 'https://api.yourdomain.com/api',
    STORAGE_BASE: 'https://api.yourdomain.com'
  };
</script>
```

When run behind nginx (default), backend routing is controlled via environment variables:
`API_UPSTREAM`, `WEB_UPSTREAM`, and `MINIO_UPSTREAM`.

## PWA Installation

The frontend is a Progressive Web App. On mobile:
- **Android**: Chrome menu → "Add to Home Screen"
- **iOS**: Safari share → "Add to Home Screen"

No app store needed.

## API Endpoints

### Public
- `GET  /health` - Health check
- `GET  /api/config` - Client configuration flags

### Auth
- `POST /api/auth/register` - Create account
- `POST /api/auth/login` - Sign in

### Feed & Discovery
- `GET  /api/feed` - Personalized feed (supports anonymous access)
- `GET  /api/clips/:id` - Clip details
- `GET  /api/clips/:id/stream` - Presigned streaming URL
- `GET  /api/clips/:id/similar` - Similar clips (embedding-based)
- `GET  /api/clips/:id/summary` - LLM-generated clip summary
- `GET  /api/search` - Full-text search (FTS5)
- `GET  /api/topics` - Top topics
- `GET  /api/topics/tree` - Hierarchical topic graph

### Interactions (auth required)
- `POST   /api/clips/:id/interact` - Record interaction (view, like, skip, etc.)
- `POST   /api/clips/:id/save` - Save/favorite clip
- `DELETE /api/clips/:id/save` - Unsave clip

### Ingestion (auth required)
- `POST /api/ingest` - Submit URL for processing
- `GET  /api/jobs` - List processing jobs
- `GET  /api/jobs/:id` - Job details

### User Profile (auth required)
- `GET  /api/me` - Profile with preferences and topic weights
- `PUT  /api/me/preferences` - Update algorithm preferences
- `GET  /api/me/saved` - Saved clips
- `GET  /api/me/history` - Watch history

### Cookies (auth required)
- `GET    /api/me/cookies` - List cookie status per platform
- `PUT    /api/me/cookies/:platform` - Set platform cookie (for yt-dlp auth)
- `DELETE /api/me/cookies/:platform` - Remove platform cookie

### Collections (auth required)
- `POST   /api/collections` - Create collection
- `GET    /api/collections` - List collections
- `GET    /api/collections/:id/clips` - List clips in collection
- `POST   /api/collections/:id/clips` - Add clip to collection
- `DELETE /api/collections/:id/clips/:clipId` - Remove clip from collection
- `DELETE /api/collections/:id` - Delete collection

### Filters (auth required)
- `POST   /api/filters` - Create saved filter
- `GET    /api/filters` - List saved filters
- `PUT    /api/filters/:id` - Update filter
- `DELETE /api/filters/:id` - Delete filter

### Scout (auth required)
- `POST   /api/scout/sources` - Add scout source (channel/playlist)
- `GET    /api/scout/sources` - List scout sources
- `PATCH  /api/scout/sources/:id` - Update scout source
- `DELETE /api/scout/sources/:id` - Delete scout source
- `POST   /api/scout/sources/:id/trigger` - Force immediate check
- `GET    /api/scout/candidates` - List discovered candidates
- `POST   /api/scout/candidates/:id/approve` - Approve candidate for ingestion
- `GET    /api/scout/profile` - User's interest profile (what Scout optimizes for)

## Development

All builds and tests run inside Docker — do not run Go, npm, or Python on the host.

```bash
# Rebuild a single service after changes
docker compose up -d --build api        # Go API
docker compose up -d --build web        # frontend
docker compose up -d --build worker     # ingestion worker
docker compose up -d --build scout      # scout worker

# Run API tests
make test-api-docker

# Useful make targets
make up                   # start all services (respects .env profiles)
make down                 # stop all services
make logs-api             # tail API logs
make logs-worker          # tail worker logs
make shell-db             # sqlite3 shell into the database
make lifecycle            # expire old clips
make score                # trigger score update
make clean                # stop + remove volumes
```

## LLM Provider Configuration

Scout, clip summaries, and AI-assisted features require an LLM. Two modes:

| Setting | Local (Ollama) | Hosted API |
|---------|----------------|------------|
| `LLM_PROVIDER` | `ollama` | `openai` or `anthropic` |
| `LLM_BASE_URL` | *(auto: internal `llm` service)* | API endpoint URL |
| `LLM_API_KEY` | *(not needed)* | Your API key |
| `LLM_MODEL` | *(uses `OLLAMA_MODEL`)* | Model name |

- Set `COMPOSE_PROFILES=ai` (add `ollama` for local inference).
- Python workers route calls through LiteLLM; any OpenAI-compatible endpoint works.

**Using Claude (Anthropic) as the hosted LLM:**

Option A — native Anthropic provider:
```
LLM_PROVIDER=anthropic
LLM_BASE_URL=https://api.anthropic.com/v1
LLM_API_KEY=<your Anthropic API key>
LLM_MODEL=claude-sonnet-4-6
ANTHROPIC_VERSION=2023-06-01
```

Option B — via Anthropic's OpenAI-compatible endpoint:
```
LLM_PROVIDER=openai
LLM_BASE_URL=https://api.anthropic.com/v1/
LLM_API_KEY=<your Anthropic API key>
LLM_MODEL=claude-sonnet-4-6
```

Both options work. Option B uses Anthropic's OpenAI SDK compatibility layer, which accepts standard OpenAI-format requests and translates them to the Claude API. Note that some advanced Claude features (prompt caching, extended thinking output, citations) are only available through the native Anthropic API (Option A).

## Roadmap

- [x] Phase 1: Core pipeline — ingest, split, serve, basic feed
- [x] Phase 2: Algorithm engine — topic graph, L2R embeddings, preference UI, collections
- [x] Phase 3: Search (FTS5), saved filters, platform cookies, clip summaries, Scout
- [x] Phase 4: Multi-user (auth, per-user preferences/embeddings/collections)

Possible future directions: sharing, federation, public collections.
