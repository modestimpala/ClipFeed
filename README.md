# ClipFeed

Self-hosted short-form video platform with a transparent, user-controllable algorithm.

## Architecture

```
                    +----------+
                    |  nginx   |  :80
                    +----+-----+
                         |
              +----------+----------+
              |                     |
        +-----+-----+       +------+------+
        |  React PWA |       |   Go API    |  :8080
        |    :3000   |       +------+------+
        +------------+              |
                          +---------+---------+
                          |         |         |
                    +-----++  +-----++  +-----++
                    |SQLite|  | SQLite|  | MinIO |
                    | (WAL)|  | FTS5  |  | :9000 |
                    +------+  +---+--+  +-------+
                                  |
              +-------------------+-------------------+
              |                   |                   |
       +------+-------+   +------+-------+   +-------+------+
       |   Worker     |   | Score Updater|   |   Scout *    |
       | yt-dlp       |   | (periodic)   |   | LLM-backed  |
       | ffmpeg       |   +--------------+   +--------------+
       | whisper      |                            |
       +--------------+                      +-----+------+
                                             |   LLM *   |
                                             |  (Ollama)  |
                                             +------------+
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
| Database | SQLite (WAL) | Single-connection DB: users, clips, interactions, jobs, topic graph, embeddings |
| Storage | MinIO | S3-compatible object storage for video and thumbnail files |
| Search | SQLite FTS5 | Full-text search across clip titles, transcripts, channels |
| Proxy | nginx | Reverse proxy, SPA routing, streaming optimization |

## Quick Start

```bash
# Clone and configure
cp .env.example .env
# Edit .env with your secrets

# Launch core services
docker compose up -d

# Watch logs
docker compose logs -f worker
```

**With AI features (Scout + LLM):**
```bash
docker compose --profile ai up -d
```

**GPU Acceleration (requires NVIDIA Container Toolkit):**
```bash
docker compose -f docker-compose.yml -f docker-compose.gpu.yml --profile ai up -d
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

# Ingestion Limits and Settings vs User Preferences

- The Env Vars ensure the worker chops everything into healthy, manageable bite-sized files (e.g., ~45 seconds each).
- The Settings Page lets you filter out any clips from the global pool that don't match your current mood.

## Storage Lifecycle

Clips auto-expire after a configurable TTL (default 30 days). Saving or favoriting a clip sets `is_protected = 1` (via trigger), exempting it from eviction.

```bash
make lifecycle          # run manually
```

Or add to crontab:
```
0 3 * * * cd /path/to/clipfeed && make lifecycle
```

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
make up                   # start all services
make down                 # stop all services
make ai-up                # start with ai profile (Scout + LLM)
make ai-down              # stop ai profile
make gpu-up               # GPU-accelerated stack
make gpu-down
make logs-api             # tail API logs
make logs-worker          # tail worker logs
make shell-db             # sqlite3 shell into the database
make lifecycle            # expire old clips
make score                # trigger score update
make clean                # stop + remove volumes
```

## LLM Provider Configuration

Scout, clip summaries, and AI-assisted features require an LLM. Two modes:

| Setting | Local (default) | Hosted API |
|---------|----------------|------------|
| `LLM_PROVIDER` | `ollama` | `openai` or `anthropic` |
| `LLM_BASE_URL` | *(auto: internal LLM service)* | API endpoint URL |
| `LLM_API_KEY` | *(not needed)* | Your API key |
| `LLM_MODEL` | *(uses `OLLAMA_MODEL`)* | Model name |

- Set `ENABLE_AI=true` (automatic with GPU compose or `--profile ai`).
- Python workers route calls through LiteLLM; any OpenAI-compatible endpoint works.
- Anthropic: set `LLM_BASE_URL=https://api.anthropic.com/v1`.

## Roadmap

- [x] Phase 1: Core pipeline — ingest, split, serve, basic feed
- [x] Phase 2: Algorithm engine — topic graph, L2R embeddings, preference UI, collections
- [x] Phase 3: Search (FTS5), saved filters, platform cookies, clip summaries, Scout
- [x] Phase 4: Multi-user (auth, per-user preferences/embeddings/collections)

 Possible Features: Sharing, federation, public collections?