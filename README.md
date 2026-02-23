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
                    |Postgres| | Redis | | MinIO |
                    | :5432  | | :6379 | | :9000 |
                    +--------+ +---+---+ +-------+
                                   |
                            +------+-------+
                            |   Worker     |
                            | yt-dlp       |
                            | ffmpeg       |
                            | whisper      |
                            +--------------+
```

## Stack

| Component | Tech | Purpose |
|-----------|------|---------|
| API | Go + Chi | REST API, auth, feed algorithm |
| Frontend | React + Vite PWA | Mobile-first swipe feed |
| Worker | Python | Video download, split, transcode, transcribe |
| Database | PostgreSQL 16 | Users, clips, interactions, algorithm state |
| Queue | Redis 7 | Job queue for async processing |
| Storage | MinIO | S3-compatible object storage for video files |
| Search | Meilisearch | Full-text search across clips and transcripts |
| Proxy | nginx | Reverse proxy, streaming optimization |

## Quick Start

```bash
# Clone and configure
cp .env.example .env
# Edit .env with your secrets

# Launch everything
docker compose up -d

# Watch logs
docker compose logs -f worker
```

The app will be available at `http://localhost`.

## Content Pipeline

1. User submits a video URL (YouTube, Vimeo, TikTok, Instagram, etc.)
2. Worker downloads via yt-dlp
3. ffmpeg detects scene changes and splits into 15-90s clips
4. Each clip is transcoded to mobile-optimized mp4
5. Whisper transcribes audio for search and topic extraction
6. Clips are uploaded to MinIO and made available in the feed

## Algorithm

The feed algorithm is fully transparent and user-controllable:

- **Exploration Rate** (0-100%): Controls the balance between showing content similar to what you've liked vs discovering new topics. At 0% you get a pure comfort zone feed, at 100% everything is random discovery.
- **Clip Duration Bounds**: Set minimum and maximum clip lengths you want to see.
- **Topic Weights**: (Phase 2) Explicit per-topic interest sliders.

The algorithm combines:
- Content score (predicted engagement based on aggregate interactions)
- User preference matching (topics, duration, source)
- Controlled randomness (scaled by exploration rate)
- Recency bias (recent content weighted slightly higher)
- Deduplication (seen clips in last 24h are filtered)

## Storage Lifecycle

Clips auto-expire after a configurable TTL (default 30 days). Saving/favoriting a clip protects it from deletion. When storage exceeds the configured limit, the oldest unprotected clips are evicted first.

Run the lifecycle script periodically:
```bash
docker compose exec worker python /app/../scripts/lifecycle.py
```

Or add to crontab:
```
0 3 * * * cd /path/to/clipfeed && docker compose exec -T worker python /app/../scripts/lifecycle.py
```

## PWA Installation

The frontend is a Progressive Web App. On mobile:
- **Android**: Chrome menu -> "Add to Home Screen" (installs like a native app)
- **iOS**: Safari share button -> "Add to Home Screen"

No app store needed. For actual native builds later, Capacitor can wrap the same codebase.

## API Endpoints

### Auth
- `POST /api/auth/register` - Create account
- `POST /api/auth/login` - Sign in

### Feed
- `GET /api/feed` - Get personalized feed (or anonymous)
- `GET /api/clips/:id` - Get clip details
- `GET /api/clips/:id/stream` - Get streaming URL

### Interactions
- `POST /api/clips/:id/interact` - Record interaction (view, like, skip, etc.)
- `POST /api/clips/:id/save` - Save/favorite clip
- `DELETE /api/clips/:id/save` - Unsave clip

### Ingestion
- `POST /api/ingest` - Submit URL for processing
- `GET /api/jobs` - List processing jobs
- `GET /api/jobs/:id` - Get job details

### User
- `GET /api/me` - Get profile
- `PUT /api/me/preferences` - Update algorithm preferences
- `GET /api/me/saved` - List saved clips
- `GET /api/me/history` - View watch history

### Collections
- `POST /api/collections` - Create collection
- `GET /api/collections` - List collections
- `POST /api/collections/:id/clips` - Add clip to collection
- `DELETE /api/collections/:id/clips/:clipId` - Remove from collection

## Development

```bash
# API (Go)
cd api && go run .

# Worker (Python)
cd ingestion && pip install -r requirements.txt && python worker.py

# Frontend (React)
cd web && npm install && npm run dev
```

## Roadmap

- [ ] Phase 1: Core pipeline (this) - ingest, split, serve, basic feed
- [ ] Phase 2: Algorithm engine - topic extraction, collaborative filtering, preference UI
- [ ] Phase 3: Polish - HLS adaptive streaming, search, refined UI
- [ ] Phase 4: Multi-source (Reels/TikTok via cookies), federation
