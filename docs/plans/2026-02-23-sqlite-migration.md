# SQLite Migration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace Postgres + Redis + Meilisearch (4 extra Docker services) with SQLite, shrinking the stack from 8 services to 4 (api, worker, minio, nginx).

**Architecture:** SQLite file at `/data/clipfeed.db` mounted as a named Docker volume into both the Go API and Python worker. WAL mode allows concurrent reads; `BEGIN IMMEDIATE` + `busy_timeout=5000` prevents write conflicts. FTS5 virtual table replaces Meilisearch. The worker polls the `jobs` table instead of using Redis `BLPOP`.

**Tech Stack:** `modernc.org/sqlite` (pure-Go, no CGO), `database/sql`, `github.com/google/uuid`, Python `sqlite3` stdlib, SQLite WAL+FTS5.

---

### Task 1: Create SQLite schema (`api/schema.sql`)

**Files:**
- Create: `api/schema.sql`
- Delete (no longer executed): `migrations/002_score_function.sql`, `migrations/003_platform_cookies.sql`

The Go API will embed this file and run it on every startup. All `CREATE` statements use `IF NOT EXISTS` so it's idempotent.

**Step 1: Write the schema file**

```sql
-- api/schema.sql
-- SQLite schema for ClipFeed (WAL mode set via PRAGMA at runtime)

CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    username        TEXT UNIQUE NOT NULL,
    email           TEXT UNIQUE NOT NULL,
    password_hash   TEXT NOT NULL,
    display_name    TEXT,
    avatar_url      TEXT,
    created_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS user_preferences (
    user_id             TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    exploration_rate    REAL DEFAULT 0.3,
    topic_weights       TEXT DEFAULT '{}',
    min_clip_seconds    INTEGER DEFAULT 5,
    max_clip_seconds    INTEGER DEFAULT 120,
    autoplay            INTEGER DEFAULT 1,
    nsfw_filter         INTEGER DEFAULT 1,
    updated_at          TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS sources (
    id              TEXT PRIMARY KEY,
    url             TEXT NOT NULL,
    platform        TEXT NOT NULL,
    external_id     TEXT,
    title           TEXT,
    description     TEXT,
    duration_seconds REAL,
    thumbnail_url   TEXT,
    channel_name    TEXT,
    channel_id      TEXT,
    metadata        TEXT DEFAULT '{}',
    status          TEXT DEFAULT 'pending',
    submitted_by    TEXT REFERENCES users(id),
    created_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(platform, external_id)
);

CREATE TABLE IF NOT EXISTS clips (
    id              TEXT PRIMARY KEY,
    source_id       TEXT REFERENCES sources(id) ON DELETE SET NULL,
    title           TEXT,
    description     TEXT,
    duration_seconds REAL NOT NULL,
    start_time      REAL,
    end_time        REAL,
    storage_key     TEXT NOT NULL,
    thumbnail_key   TEXT,
    hls_key         TEXT,
    width           INTEGER,
    height          INTEGER,
    file_size_bytes INTEGER,
    transcript      TEXT,
    language        TEXT,
    topics          TEXT DEFAULT '[]',
    tags            TEXT DEFAULT '[]',
    content_score   REAL DEFAULT 0.5,
    expires_at      TEXT,
    is_protected    INTEGER DEFAULT 0,
    status          TEXT DEFAULT 'processing',
    created_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_clips_status ON clips(status);
CREATE INDEX IF NOT EXISTS idx_clips_expires ON clips(expires_at)
    WHERE expires_at IS NOT NULL AND is_protected = 0;
CREATE INDEX IF NOT EXISTS idx_clips_score ON clips(content_score DESC);

-- FTS5 full-text search (replaces Meilisearch)
CREATE VIRTUAL TABLE IF NOT EXISTS clips_fts USING fts5(
    clip_id UNINDEXED,
    title,
    transcript,
    platform UNINDEXED,
    channel_name UNINDEXED
);

CREATE TABLE IF NOT EXISTS interactions (
    id                     TEXT PRIMARY KEY,
    user_id                TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id                TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    action                 TEXT NOT NULL,
    watch_duration_seconds REAL,
    watch_percentage       REAL,
    created_at             TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_interactions_user ON interactions(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_interactions_clip ON interactions(clip_id);
CREATE INDEX IF NOT EXISTS idx_interactions_action ON interactions(user_id, action);

CREATE TABLE IF NOT EXISTS saved_clips (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id    TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (user_id, clip_id)
);

CREATE TABLE IF NOT EXISTS collections (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    description TEXT,
    is_public   INTEGER DEFAULT 0,
    created_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS collection_clips (
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    clip_id       TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    position      INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (collection_id, clip_id)
);

CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,
    source_id    TEXT REFERENCES sources(id),
    job_type     TEXT NOT NULL,
    status       TEXT DEFAULT 'queued',
    priority     INTEGER DEFAULT 5,
    payload      TEXT DEFAULT '{}',
    result       TEXT DEFAULT '{}',
    error        TEXT,
    attempts     INTEGER DEFAULT 0,
    max_attempts INTEGER DEFAULT 3,
    locked_at    TEXT,
    started_at   TEXT,
    completed_at TEXT,
    created_at   TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status, priority DESC, created_at ASC);

CREATE TABLE IF NOT EXISTS platform_cookies (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform   TEXT NOT NULL,
    cookie_str TEXT NOT NULL,
    is_active  INTEGER DEFAULT 1,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(user_id, platform)
);

CREATE INDEX IF NOT EXISTS idx_platform_cookies_user ON platform_cookies(user_id, platform);

-- Protect clip when saved
CREATE TRIGGER IF NOT EXISTS trg_protect_saved
    AFTER INSERT ON saved_clips FOR EACH ROW
BEGIN
    UPDATE clips SET is_protected = 1 WHERE id = NEW.clip_id;
END;

-- Unprotect clip when last save is removed
CREATE TRIGGER IF NOT EXISTS trg_check_unprotect
    AFTER DELETE ON saved_clips FOR EACH ROW
BEGIN
    UPDATE clips SET is_protected = 0
    WHERE id = OLD.clip_id
      AND NOT EXISTS (SELECT 1 FROM saved_clips WHERE clip_id = OLD.clip_id);
END;
```

**Step 2: Verify it's valid SQLite**

Run: `sqlite3 /tmp/test_schema.db < api/schema.sql && echo "OK"`
Expected: `OK` with no errors.

**Step 3: Commit**

```bash
git add api/schema.sql
git commit -m "feat: add SQLite schema with FTS5 (replaces Postgres)"
```

---

### Task 2: Update `api/go.mod`

**Files:**
- Modify: `api/go.mod`

Remove `pgx/v5`, `go-redis/v9`, `meilisearch-go`. Add `modernc.org/sqlite`, `github.com/google/uuid`.

**Step 1: Replace go.mod content**

```
module clipfeed

go 1.22

require (
	github.com/go-chi/chi/v5     v5.0.12
	github.com/go-chi/cors       v1.2.1
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/google/uuid       v1.6.0
	github.com/minio/minio-go/v7 v7.0.70
	golang.org/x/crypto          v0.22.0
	modernc.org/sqlite           v1.29.5
)
```

**Step 2: Verify the Dockerfile still builds**

The `api/Dockerfile` already runs `go mod tidy` which will regenerate `go.sum` for the new deps.

Run: `docker compose build api`
Expected: build succeeds (tidy downloads modernc.org/sqlite, google/uuid).

**Step 3: Commit**

```bash
git add api/go.mod
git commit -m "chore: swap Go deps to sqlite (drop pgx, redis, meilisearch)"
```

---

### Task 3: Rewrite `api/main.go`

**Files:**
- Modify: `api/main.go`

Complete rewrite of the file. The HTTP routes, middleware, JWT, and MinIO logic are unchanged. What changes: imports, App struct, Config, DB init, all SQL queries (`$N` → `?`, Postgres functions → SQLite equivalents), array columns (TEXT JSON), removed Redis push in handleIngest, FTS5 search.

**Step 1: Replace api/main.go**

```go
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	_ "modernc.org/sqlite"
	"golang.org/x/crypto/bcrypt"
)

//go:embed schema.sql
var schemaSQL string

type App struct {
	db    *sql.DB
	minio *minio.Client
	cfg   Config
}

type Config struct {
	DBPath        string
	MinioEndpoint string
	MinioAccess   string
	MinioSecret   string
	MinioBucket   string
	MinioSSL      bool
	JWTSecret     string
	Port          string
}

func loadConfig() Config {
	return Config{
		DBPath:        getEnv("DB_PATH", "/data/clipfeed.db"),
		MinioEndpoint: getEnv("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccess:   getEnv("MINIO_ACCESS_KEY", "clipfeed"),
		MinioSecret:   getEnv("MINIO_SECRET_KEY", "changeme123"),
		MinioBucket:   getEnv("MINIO_BUCKET", "clips"),
		MinioSSL:      getEnv("MINIO_USE_SSL", "false") == "true",
		JWTSecret:     getEnv("JWT_SECRET", "supersecretkey"),
		Port:          getEnv("PORT", "8080"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := loadConfig()

	// SQLite
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Single connection: prevents concurrent write conflicts
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			log.Fatalf("pragma failed (%s): %v", pragma, err)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		log.Fatalf("failed to init schema: %v", err)
	}

	// MinIO
	minioClient, err := minio.New(cfg.MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinioAccess, cfg.MinioSecret, ""),
		Secure: cfg.MinioSSL,
	})
	if err != nil {
		log.Fatalf("failed to connect to minio: %v", err)
	}

	ctx := context.Background()
	exists, err := minioClient.BucketExists(ctx, cfg.MinioBucket)
	if err != nil {
		log.Fatalf("failed to check bucket: %v", err)
	}
	if !exists {
		if err := minioClient.MakeBucket(ctx, cfg.MinioBucket, minio.MakeBucketOptions{}); err != nil {
			log.Fatalf("failed to create bucket: %v", err)
		}
		log.Printf("created bucket: %s", cfg.MinioBucket)
	}

	app := &App{db: db, minio: minioClient, cfg: cfg}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	r.Post("/api/auth/register", app.handleRegister)
	r.Post("/api/auth/login", app.handleLogin)
	r.Get("/api/feed", app.optionalAuth(app.handleFeed))
	r.Get("/api/clips/{id}", app.handleGetClip)
	r.Get("/api/clips/{id}/stream", app.handleStreamClip)
	r.Get("/api/search", app.handleSearch)

	r.Group(func(r chi.Router) {
		r.Use(app.authMiddleware)
		r.Post("/api/clips/{id}/interact", app.handleInteraction)
		r.Post("/api/clips/{id}/save", app.handleSaveClip)
		r.Delete("/api/clips/{id}/save", app.handleUnsaveClip)
		r.Post("/api/ingest", app.handleIngest)
		r.Get("/api/jobs", app.handleListJobs)
		r.Get("/api/jobs/{id}", app.handleGetJob)
		r.Get("/api/me", app.handleGetProfile)
		r.Put("/api/me/preferences", app.handleUpdatePreferences)
		r.Get("/api/me/saved", app.handleListSaved)
		r.Get("/api/me/history", app.handleListHistory)
		r.Put("/api/me/cookies/{platform}", app.handleSetCookie)
		r.Delete("/api/me/cookies/{platform}", app.handleDeleteCookie)
		r.Post("/api/collections", app.handleCreateCollection)
		r.Get("/api/collections", app.handleListCollections)
		r.Post("/api/collections/{id}/clips", app.handleAddToCollection)
		r.Delete("/api/collections/{id}/clips/{clipId}", app.handleRemoveFromCollection)
	})

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}

	go func() {
		log.Printf("ClipFeed API listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	log.Println("server shut down")
}

// --- Auth ---

type RegisterRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}
	if len(req.Username) < 3 || len(req.Password) < 8 {
		writeJSON(w, 400, map[string]string{"error": "username must be 3+ chars, password 8+ chars"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "internal error"})
		return
	}

	userID := uuid.New().String()
	_, err = a.db.ExecContext(r.Context(),
		`INSERT INTO users (id, username, email, password_hash, display_name) VALUES (?, ?, ?, ?, ?)`,
		userID, req.Username, req.Email, string(hash), req.Username)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeJSON(w, 409, map[string]string{"error": "username or email already taken"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "failed to create user"})
		return
	}

	a.db.ExecContext(r.Context(), `INSERT OR IGNORE INTO user_preferences (user_id) VALUES (?)`, userID)

	token, err := a.generateToken(userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to generate token"})
		return
	}
	writeJSON(w, 201, map[string]string{"token": token, "user_id": userID})
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	var userID, hash string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT id, password_hash FROM users WHERE username = ? OR email = ?`,
		req.Username, req.Username).Scan(&userID, &hash)
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}

	token, err := a.generateToken(userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to generate token"})
		return
	}
	writeJSON(w, 200, map[string]string{"token": token, "user_id": userID})
}

func (a *App) generateToken(userID string) (string, error) {
	claims := jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(7 * 24 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(a.cfg.JWTSecret))
}

// --- Middleware ---

type contextKey string

const userIDKey contextKey = "user_id"

func (a *App) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := a.extractUserID(r)
		if userID == "" {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userID := a.extractUserID(r); userID != "" {
			r = r.WithContext(context.WithValue(r.Context(), userIDKey, userID))
		}
		next(w, r)
	}
}

func (a *App) extractUserID(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	tokenStr := strings.TrimPrefix(auth, "Bearer ")
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(a.cfg.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		return ""
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return ""
	}
	sub, ok := claims["sub"].(string)
	if !ok {
		return ""
	}
	return sub
}

// --- Search (FTS5) ---

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, 400, map[string]string{"error": "q required"})
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
		       c.topics, c.content_score, s.platform, s.channel_name
		FROM clips_fts
		JOIN clips c ON clips_fts.clip_id = c.id
		LEFT JOIN sources s ON c.source_id = s.id
		WHERE clips_fts MATCH ? AND c.status = 'ready'
		ORDER BY bm25(clips_fts), c.content_score DESC
		LIMIT 20
	`, q)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "search failed"})
		return
	}
	defer rows.Close()

	var hits []map[string]interface{}
	for rows.Next() {
		var id, title, thumbnailKey, topicsJSON string
		var duration, score float64
		var platform, channelName *string
		rows.Scan(&id, &title, &duration, &thumbnailKey, &topicsJSON, &score, &platform, &channelName)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		hits = append(hits, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "topics": topics,
			"content_score": score, "platform": platform, "channel_name": channelName,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"hits": hits, "query": q, "total": len(hits)})
}

// --- Feed ---

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	limit := 20

	var rows *sql.Rows
	var err error

	if userID != "" {
		rows, err = a.db.QueryContext(r.Context(), `
			WITH prefs AS (
				SELECT exploration_rate, min_clip_seconds, max_clip_seconds
				FROM user_preferences WHERE user_id = ?
			),
			seen AS (
				SELECT clip_id FROM interactions
				WHERE user_id = ? AND created_at > datetime('now', '-24 hours')
			)
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			  AND c.id NOT IN (SELECT clip_id FROM seen)
			  AND c.duration_seconds >= COALESCE((SELECT min_clip_seconds FROM prefs), 5)
			  AND c.duration_seconds <= COALESCE((SELECT max_clip_seconds FROM prefs), 120)
			ORDER BY
			    (c.content_score * (1.0 - COALESCE((SELECT exploration_rate FROM prefs), 0.3)))
			    + (CAST(ABS(RANDOM()) AS REAL) / 9223372036854775807.0
			       * COALESCE((SELECT exploration_rate FROM prefs), 0.3))
			    DESC
			LIMIT ?
		`, userID, userID, limit)
	} else {
		rows, err = a.db.QueryContext(r.Context(), `
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			ORDER BY (c.content_score * 0.7)
			    + (CAST(ABS(RANDOM()) AS REAL) / 9223372036854775807.0 * 0.3) DESC
			LIMIT ?
		`, limit)
	}

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to fetch feed"})
		return
	}
	defer rows.Close()

	clips := scanClips(rows)
	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

func (a *App) handleGetClip(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var id, title, description, thumbnailKey, topicsJSON, tagsJSON, status, createdAt string
	var duration, score float64

	err := a.db.QueryRowContext(r.Context(), `
		SELECT id, title, description, duration_seconds,
		       thumbnail_key, topics, tags, content_score, status, created_at
		FROM clips WHERE id = ?
	`, clipID).Scan(&id, &title, &description, &duration,
		&thumbnailKey, &topicsJSON, &tagsJSON, &score, &status, &createdAt)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	var topics, tags []string
	json.Unmarshal([]byte(topicsJSON), &topics)
	json.Unmarshal([]byte(tagsJSON), &tags)

	writeJSON(w, 200, map[string]interface{}{
		"id": id, "title": title, "description": description,
		"duration_seconds": duration, "thumbnail_key": thumbnailKey,
		"topics": topics, "tags": tags, "content_score": score,
		"status": status, "created_at": createdAt,
	})
}

func (a *App) handleStreamClip(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var storageKey string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT storage_key FROM clips WHERE id = ? AND status = 'ready'`,
		clipID).Scan(&storageKey)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	presignedURL, err := a.minio.PresignedGetObject(r.Context(),
		a.cfg.MinioBucket, storageKey, 2*time.Hour, nil)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to generate stream URL"})
		return
	}
	writeJSON(w, 200, map[string]string{"url": presignedURL.String()})
}

// --- Interactions ---

type InteractionRequest struct {
	Action          string  `json:"action"`
	WatchDuration   float64 `json:"watch_duration_seconds"`
	WatchPercentage float64 `json:"watch_percentage"`
}

func (a *App) handleInteraction(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	clipID := chi.URLParam(r, "id")

	var req InteractionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	validActions := map[string]bool{
		"view": true, "like": true, "dislike": true,
		"save": true, "share": true, "skip": true, "watch_full": true,
	}
	if !validActions[req.Action] {
		writeJSON(w, 400, map[string]string{"error": "invalid action"})
		return
	}

	interactionID := uuid.New().String()
	_, err := a.db.ExecContext(r.Context(), `
		INSERT INTO interactions (id, user_id, clip_id, action, watch_duration_seconds, watch_percentage)
		VALUES (?, ?, ?, ?, ?, ?)
	`, interactionID, userID, clipID, req.Action, req.WatchDuration, req.WatchPercentage)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to record interaction"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "recorded"})
}

// --- Ingestion ---

type IngestRequest struct {
	URL string `json:"url"`
}

func (a *App) handleIngest(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}
	if req.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "url is required"})
		return
	}

	platform := detectPlatform(req.URL)
	sourceID := uuid.New().String()
	jobID := uuid.New().String()
	payload := fmt.Sprintf(`{"url":%q,"source_id":%q,"platform":%q}`, req.URL, sourceID, platform)

	// Atomic: insert source + job together
	conn, err := a.db.Conn(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "db error"})
		return
	}
	defer conn.Close()

	if _, err := conn.ExecContext(r.Context(), "BEGIN IMMEDIATE"); err != nil {
		writeJSON(w, 500, map[string]string{"error": "db error"})
		return
	}

	_, err = conn.ExecContext(r.Context(),
		`INSERT INTO sources (id, url, platform, submitted_by, status) VALUES (?, ?, ?, ?, 'pending')`,
		sourceID, req.URL, platform, userID)
	if err != nil {
		conn.ExecContext(r.Context(), "ROLLBACK")
		writeJSON(w, 500, map[string]string{"error": "failed to create source"})
		return
	}

	_, err = conn.ExecContext(r.Context(),
		`INSERT INTO jobs (id, source_id, job_type, payload) VALUES (?, ?, 'download', ?)`,
		jobID, sourceID, payload)
	if err != nil {
		conn.ExecContext(r.Context(), "ROLLBACK")
		writeJSON(w, 500, map[string]string{"error": "failed to queue job"})
		return
	}

	conn.ExecContext(r.Context(), "COMMIT")

	writeJSON(w, 202, map[string]interface{}{
		"source_id": sourceID,
		"job_id":    jobID,
		"status":    "queued",
	})
}

func detectPlatform(url string) string {
	switch {
	case strings.Contains(url, "youtube.com") || strings.Contains(url, "youtu.be"):
		return "youtube"
	case strings.Contains(url, "vimeo.com"):
		return "vimeo"
	case strings.Contains(url, "tiktok.com"):
		return "instagram"
	case strings.Contains(url, "instagram.com"):
		return "instagram"
	case strings.Contains(url, "twitter.com") || strings.Contains(url, "x.com"):
		return "twitter"
	default:
		return "direct"
	}
}

// --- Jobs ---

func (a *App) handleListJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, source_id, job_type, status, created_at, completed_at
		FROM jobs ORDER BY created_at DESC LIMIT 50
	`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list jobs"})
		return
	}
	defer rows.Close()

	var jobs []map[string]interface{}
	for rows.Next() {
		var id, jobType, status, createdAt string
		var sourceID, completedAt *string
		rows.Scan(&id, &sourceID, &jobType, &status, &createdAt, &completedAt)
		jobs = append(jobs, map[string]interface{}{
			"id": id, "source_id": sourceID, "job_type": jobType,
			"status": status, "created_at": createdAt, "completed_at": completedAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"jobs": jobs})
}

func (a *App) handleGetJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")

	var id, jobType, status, payloadStr, resultStr, createdAt string
	var sourceID, errMsg *string

	err := a.db.QueryRowContext(r.Context(), `
		SELECT id, source_id, job_type, status, payload, result, error, created_at
		FROM jobs WHERE id = ?
	`, jobID).Scan(&id, &sourceID, &jobType, &status, &payloadStr, &resultStr, &errMsg, &createdAt)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "job not found"})
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"id": id, "source_id": sourceID, "job_type": jobType,
		"status": status, "payload": json.RawMessage(payloadStr),
		"result": json.RawMessage(resultStr), "error": errMsg, "created_at": createdAt,
	})
}

// --- User Profile ---

func (a *App) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	var username, email, displayName, createdAt string
	var avatarURL *string

	err := a.db.QueryRowContext(r.Context(), `
		SELECT username, email, display_name, avatar_url, created_at
		FROM users WHERE id = ?
	`, userID).Scan(&username, &email, &displayName, &avatarURL, &createdAt)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "user not found"})
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"id": userID, "username": username, "email": email,
		"display_name": displayName, "avatar_url": avatarURL,
		"created_at": createdAt,
	})
}

func (a *App) handleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	var prefs map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&prefs); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	topicWeights, _ := json.Marshal(prefs["topic_weights"])

	_, err := a.db.ExecContext(r.Context(), `
		INSERT INTO user_preferences (user_id, exploration_rate, topic_weights, min_clip_seconds, max_clip_seconds, autoplay)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			exploration_rate = COALESCE(excluded.exploration_rate, user_preferences.exploration_rate),
			topic_weights    = COALESCE(excluded.topic_weights,    user_preferences.topic_weights),
			min_clip_seconds = COALESCE(excluded.min_clip_seconds, user_preferences.min_clip_seconds),
			max_clip_seconds = COALESCE(excluded.max_clip_seconds, user_preferences.max_clip_seconds),
			autoplay         = COALESCE(excluded.autoplay,         user_preferences.autoplay),
			updated_at       = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
	`, userID,
		prefs["exploration_rate"],
		string(topicWeights),
		prefs["min_clip_seconds"],
		prefs["max_clip_seconds"],
		prefs["autoplay"],
	)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to update preferences"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "updated"})
}

// --- Saved Clips ---

func (a *App) handleSaveClip(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	clipID := chi.URLParam(r, "id")

	_, err := a.db.ExecContext(r.Context(),
		`INSERT OR IGNORE INTO saved_clips (user_id, clip_id) VALUES (?, ?)`,
		userID, clipID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to save clip"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "saved"})
}

func (a *App) handleUnsaveClip(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	clipID := chi.URLParam(r, "id")
	a.db.ExecContext(r.Context(),
		`DELETE FROM saved_clips WHERE user_id = ? AND clip_id = ?`,
		userID, clipID)
	writeJSON(w, 200, map[string]string{"status": "removed"})
}

func (a *App) handleListSaved(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key, c.topics, c.created_at
		FROM saved_clips sc
		JOIN clips c ON sc.clip_id = c.id
		WHERE sc.user_id = ?
		ORDER BY sc.created_at DESC
	`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list saved clips"})
		return
	}
	defer rows.Close()

	var clips []map[string]interface{}
	for rows.Next() {
		var id, title, thumbnailKey, topicsJSON, createdAt string
		var duration float64
		rows.Scan(&id, &title, &duration, &thumbnailKey, &topicsJSON, &createdAt)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "topics": topics, "created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"clips": clips})
}

func (a *App) handleListHistory(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	// ROW_NUMBER() window function replaces Postgres DISTINCT ON
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key, i.action, i.created_at
		FROM (
			SELECT clip_id, action, created_at,
			       ROW_NUMBER() OVER (PARTITION BY clip_id ORDER BY created_at DESC) AS rn
			FROM interactions WHERE user_id = ?
		) i
		JOIN clips c ON i.clip_id = c.id
		WHERE i.rn = 1
		ORDER BY i.created_at DESC
		LIMIT 100
	`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list history"})
		return
	}
	defer rows.Close()

	var history []map[string]interface{}
	for rows.Next() {
		var id, title, thumbnailKey, action, at string
		var duration float64
		rows.Scan(&id, &title, &duration, &thumbnailKey, &action, &at)
		history = append(history, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "last_action": action, "at": at,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"history": history})
}

// --- Platform Cookies ---

type CookieRequest struct {
	CookieStr string `json:"cookie_str"`
}

var validPlatforms = map[string]bool{
	"tiktok": true, "instagram": true, "twitter": true,
}

func (a *App) handleSetCookie(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	platform := chi.URLParam(r, "platform")

	if !validPlatforms[platform] {
		writeJSON(w, 400, map[string]string{"error": "invalid platform (tiktok, instagram, twitter)"})
		return
	}

	var req CookieRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CookieStr == "" {
		writeJSON(w, 400, map[string]string{"error": "cookie_str required"})
		return
	}

	cookieID := uuid.New().String()
	_, err := a.db.ExecContext(r.Context(), `
		INSERT INTO platform_cookies (id, user_id, platform, cookie_str, is_active, updated_at)
		VALUES (?, ?, ?, ?, 1, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
		ON CONFLICT(user_id, platform) DO UPDATE SET
			cookie_str = excluded.cookie_str,
			is_active  = 1,
			updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
	`, cookieID, userID, platform, req.CookieStr)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to save cookie"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "saved", "platform": platform})
}

func (a *App) handleDeleteCookie(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	platform := chi.URLParam(r, "platform")

	if !validPlatforms[platform] {
		writeJSON(w, 400, map[string]string{"error": "invalid platform"})
		return
	}

	a.db.ExecContext(r.Context(), `
		UPDATE platform_cookies SET is_active = 0, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		WHERE user_id = ? AND platform = ?
	`, userID, platform)
	writeJSON(w, 200, map[string]string{"status": "removed", "platform": platform})
}

// --- Collections ---

func (a *App) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	id := uuid.New().String()
	_, err := a.db.ExecContext(r.Context(),
		`INSERT INTO collections (id, user_id, title, description) VALUES (?, ?, ?, ?)`,
		id, userID, req.Title, req.Description)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to create collection"})
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (a *App) handleListCollections(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.description, c.is_public, c.created_at,
		       COUNT(cc.clip_id) AS clip_count
		FROM collections c
		LEFT JOIN collection_clips cc ON c.id = cc.collection_id
		WHERE c.user_id = ?
		GROUP BY c.id
		ORDER BY c.created_at DESC
	`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list collections"})
		return
	}
	defer rows.Close()

	var collections []map[string]interface{}
	for rows.Next() {
		var id, title, createdAt string
		var description *string
		var isPublic int
		var clipCount int
		rows.Scan(&id, &title, &description, &isPublic, &createdAt, &clipCount)
		collections = append(collections, map[string]interface{}{
			"id": id, "title": title, "description": description,
			"is_public": isPublic == 1, "clip_count": clipCount, "created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"collections": collections})
}

func (a *App) handleAddToCollection(w http.ResponseWriter, r *http.Request) {
	collectionID := chi.URLParam(r, "id")
	var req struct {
		ClipID string `json:"clip_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	_, err := a.db.ExecContext(r.Context(), `
		INSERT OR IGNORE INTO collection_clips (collection_id, clip_id, position)
		VALUES (?, ?, COALESCE((SELECT MAX(position) + 1 FROM collection_clips WHERE collection_id = ?), 0))
	`, collectionID, req.ClipID, collectionID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to add to collection"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "added"})
}

func (a *App) handleRemoveFromCollection(w http.ResponseWriter, r *http.Request) {
	collectionID := chi.URLParam(r, "id")
	clipID := chi.URLParam(r, "clipId")
	a.db.ExecContext(r.Context(),
		`DELETE FROM collection_clips WHERE collection_id = ? AND clip_id = ?`,
		collectionID, clipID)
	writeJSON(w, 200, map[string]string{"status": "removed"})
}

// --- Helpers ---

func scanClips(rows *sql.Rows) []map[string]interface{} {
	var clips []map[string]interface{}
	for rows.Next() {
		var id, title, description, thumbnailKey, topicsJSON, tagsJSON, createdAt string
		var duration, score float64
		var channelName, platform *string

		rows.Scan(&id, &title, &description, &duration,
			&thumbnailKey, &topicsJSON, &tagsJSON, &score,
			&createdAt, &channelName, &platform)

		var topics, tags []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		json.Unmarshal([]byte(tagsJSON), &tags)

		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "description": description,
			"duration_seconds": duration, "thumbnail_key": thumbnailKey,
			"topics": topics, "tags": tags, "content_score": score,
			"created_at": createdAt, "channel_name": channelName,
			"platform": platform,
		})
	}
	return clips
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
```

**Step 2: Verify Go compiles**

Run: `docker compose build api`
Expected: build succeeds with no errors (the `go mod tidy` step in the Dockerfile will fetch modernc.org/sqlite and google/uuid).

**Step 3: Commit**

```bash
git add api/main.go
git commit -m "feat: rewrite api/main.go for SQLite (drop pgx, redis, meilisearch)"
```

---

### Task 4: Update `ingestion/requirements.txt`

**Files:**
- Modify: `ingestion/requirements.txt`

Remove `redis`, `psycopg2-binary`, `meilisearch`. Everything else stays.

**Step 1: Replace requirements.txt**

```
minio==7.2.7
yt-dlp==2024.4.9
faster-whisper==1.0.3
keybert==0.8.4
sentence-transformers==2.7.0
```

**Step 2: Verify**

Run: `docker compose build worker`
Expected: build succeeds (pip install is faster with fewer packages).

**Step 3: Commit**

```bash
git add ingestion/requirements.txt
git commit -m "chore: remove redis/psycopg2/meilisearch from Python deps"
```

---

### Task 5: Rewrite `ingestion/worker.py`

**Files:**
- Modify: `ingestion/worker.py`

Remove Redis (replaced by DB polling), psycopg2 (replaced by sqlite3), meilisearch (replaced by FTS5 insert). Each worker thread opens its own SQLite connection to avoid cross-thread sharing. The main loop polls `jobs` with `BEGIN IMMEDIATE` for atomic claim.

**Step 1: Replace ingestion/worker.py**

```python
#!/usr/bin/env python3
"""
ClipFeed Ingestion Worker
Processes video sources: download -> analyze -> split -> transcode -> transcribe -> upload
"""

import os
import sys
import json
import time
import uuid
import sqlite3
import signal
import logging
import subprocess
import tempfile
from pathlib import Path
from datetime import datetime, timedelta
from concurrent.futures import ThreadPoolExecutor

from minio import Minio
from faster_whisper import WhisperModel
from keybert import KeyBERT

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s"
)
log = logging.getLogger("worker")

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
MINIO_ENDPOINT = os.getenv("MINIO_ENDPOINT", "localhost:9000")
MINIO_ACCESS = os.getenv("MINIO_ACCESS_KEY", "clipfeed")
MINIO_SECRET = os.getenv("MINIO_SECRET_KEY", "changeme123")
MINIO_BUCKET = os.getenv("MINIO_BUCKET", "clips")
MINIO_SSL = os.getenv("MINIO_USE_SSL", "false") == "true"
WHISPER_MODEL = os.getenv("WHISPER_MODEL", "base")
MAX_CONCURRENT = int(os.getenv("MAX_CONCURRENT_JOBS", "2"))
CLIP_TTL_DAYS = int(os.getenv("CLIP_TTL_DAYS", "30"))
WORK_DIR = Path(os.getenv("WORK_DIR", "/tmp/clipfeed"))

MIN_CLIP_SECONDS = 15
MAX_CLIP_SECONDS = 90
TARGET_CLIP_SECONDS = 45
SCENE_THRESHOLD = 0.3

shutdown = False


def signal_handler(sig, frame):
    global shutdown
    log.info("Shutdown signal received, finishing current jobs...")
    shutdown = True


signal.signal(signal.SIGINT, signal_handler)
signal.signal(signal.SIGTERM, signal_handler)


def open_db():
    """Open a SQLite connection with WAL mode and row factory."""
    db = sqlite3.connect(DB_PATH, isolation_level=None, check_same_thread=False)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA busy_timeout=5000")
    db.execute("PRAGMA foreign_keys=ON")
    db.execute("PRAGMA synchronous=NORMAL")
    db.row_factory = sqlite3.Row
    return db


class Worker:
    def __init__(self):
        # Main-thread connection used only for job popping
        self.db = open_db()
        self.minio = Minio(
            MINIO_ENDPOINT,
            access_key=MINIO_ACCESS,
            secret_key=MINIO_SECRET,
            secure=MINIO_SSL,
        )
        WORK_DIR.mkdir(parents=True, exist_ok=True)

        if not self.minio.bucket_exists(MINIO_BUCKET):
            self.minio.make_bucket(MINIO_BUCKET)

        self.whisper = WhisperModel(WHISPER_MODEL, device="cpu", compute_type="int8")
        self.kw_model = KeyBERT(model='all-MiniLM-L6-v2')

    def _pop_job(self):
        """Atomically claim one pending job. Returns sqlite3.Row or None."""
        try:
            self.db.execute("BEGIN IMMEDIATE")
            row = self.db.execute("""
                UPDATE jobs
                SET status = 'running',
                    started_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
                    attempts = attempts + 1
                WHERE id = (
                    SELECT id FROM jobs
                    WHERE status = 'queued'
                    ORDER BY priority DESC, created_at ASC
                    LIMIT 1
                )
                RETURNING id, payload
            """).fetchone()
            self.db.execute("COMMIT")
            return row
        except Exception as e:
            try:
                self.db.execute("ROLLBACK")
            except Exception:
                pass
            raise

    def run(self):
        log.info(f"Worker started (max_concurrent={MAX_CONCURRENT})")
        with ThreadPoolExecutor(max_workers=MAX_CONCURRENT) as pool:
            while not shutdown:
                try:
                    row = self._pop_job()
                    if row is None:
                        time.sleep(2)
                        continue
                    job_id = row["id"]
                    payload = json.loads(row["payload"])
                    log.info(f"Claimed job {job_id}")
                    pool.submit(self.process_job, job_id, payload)
                except Exception as e:
                    log.error(f"Job pop failed: {e}")
                    time.sleep(5)
        log.info("Worker shut down")

    def process_job(self, job_id: str, payload: dict):
        """Each thread gets its own DB connection."""
        db = open_db()
        try:
            source_id = payload.get("source_id")
            platform = payload.get("platform", "")
            url = payload.get("url", "")

            db.execute("UPDATE sources SET status = 'downloading' WHERE id = ?", (source_id,))

            work_path = WORK_DIR / job_id
            work_path.mkdir(parents=True, exist_ok=True)

            try:
                # Fetch platform cookie if applicable
                cookie_str = None
                if platform in ("tiktok", "instagram", "twitter"):
                    row = db.execute("""
                        SELECT cookie_str FROM platform_cookies
                        WHERE user_id = (SELECT submitted_by FROM sources WHERE id = ?)
                          AND platform = ? AND is_active = 1
                    """, (source_id, platform)).fetchone()
                    if row:
                        cookie_str = row["cookie_str"]

                # Step 1: Download
                source_file = self.download(url, work_path, cookie_str=cookie_str)
                db.execute("UPDATE sources SET status = 'processing' WHERE id = ?", (source_id,))

                # Step 2: Extract metadata
                metadata = self.extract_metadata(source_file)
                db.execute(
                    "UPDATE sources SET title = ?, duration_seconds = ?, metadata = ? WHERE id = ?",
                    (metadata.get("title"), metadata.get("duration"), json.dumps(metadata), source_id),
                )

                # Step 3: Detect scenes and split
                segments = self.detect_scenes(source_file, metadata.get("duration", 0))

                # Step 4: Process each segment
                clip_ids = []
                for i, seg in enumerate(segments):
                    clip_id = self.process_segment(db, source_file, source_id, seg, i, work_path, metadata)
                    if clip_id:
                        clip_ids.append(clip_id)

                db.execute("UPDATE sources SET status = 'complete' WHERE id = ?", (source_id,))
                db.execute(
                    "UPDATE jobs SET status = 'complete', completed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), result = ? WHERE id = ?",
                    (json.dumps({"clip_ids": clip_ids, "clip_count": len(clip_ids)}), job_id),
                )
                log.info(f"Job {job_id} complete: {len(clip_ids)} clips created")

            except Exception as e:
                log.error(f"Job {job_id} failed: {e}")
                db.execute(
                    "UPDATE jobs SET status = 'failed', error = ? WHERE id = ?",
                    (str(e), job_id),
                )
                db.execute("UPDATE sources SET status = 'failed' WHERE id = ?", (source_id,))

            finally:
                subprocess.run(["rm", "-rf", str(work_path)], check=False)

        except Exception as e:
            log.error(f"Fatal error processing job {job_id}: {e}")
        finally:
            db.close()

    def download(self, url: str, work_path: Path, cookie_str: str = None) -> Path:
        output_template = str(work_path / "source.%(ext)s")
        cmd = [
            "yt-dlp",
            "--no-playlist",
            "--format", "bestvideo[height<=1080]+bestaudio/best[height<=1080]",
            "--merge-output-format", "mp4",
            "--output", output_template,
            "--no-overwrites",
            "--socket-timeout", "30",
        ]
        if cookie_str:
            cookie_file = work_path / "cookies.txt"
            cookie_file.write_text(cookie_str)
            cmd += ["--cookies", str(cookie_file)]
        cmd.append(url)

        log.info(f"Downloading: {url}")
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=600)
        if result.returncode != 0:
            raise RuntimeError(f"yt-dlp failed: {result.stderr[:500]}")

        for f in work_path.glob("source.*"):
            if f.suffix in (".mp4", ".mkv", ".webm"):
                return f

        raise RuntimeError("Download completed but no video file found")

    def extract_metadata(self, video_path: Path) -> dict:
        cmd = [
            "ffprobe", "-v", "quiet",
            "-print_format", "json",
            "-show_format", "-show_streams",
            str(video_path),
        ]
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
        if result.returncode != 0:
            return {}
        probe = json.loads(result.stdout)
        fmt = probe.get("format", {})
        video_stream = next(
            (s for s in probe.get("streams", []) if s.get("codec_type") == "video"), {}
        )
        return {
            "title": fmt.get("tags", {}).get("title", video_path.stem),
            "duration": float(fmt.get("duration", 0)),
            "width": int(video_stream.get("width", 0)),
            "height": int(video_stream.get("height", 0)),
            "codec": video_stream.get("codec_name"),
            "bitrate": int(fmt.get("bit_rate", 0)),
        }

    def detect_scenes(self, video_path: Path, total_duration: float) -> list:
        if total_duration <= MAX_CLIP_SECONDS:
            return [{"start": 0, "end": total_duration}]
        try:
            cmd = [
                "ffmpeg", "-i", str(video_path),
                "-filter:v", f"select='gt(scene,{SCENE_THRESHOLD})',showinfo",
                "-f", "null", "-",
            ]
            result = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
            scene_times = [0.0]
            for line in result.stderr.split("\n"):
                if "pts_time:" in line:
                    try:
                        pts = float(line.split("pts_time:")[1].split()[0])
                        scene_times.append(pts)
                    except (ValueError, IndexError):
                        continue
            scene_times.append(total_duration)
            scene_times = sorted(set(scene_times))
            segments = self._merge_scenes(scene_times, total_duration)
            if segments:
                return segments
        except Exception as e:
            log.warning(f"Scene detection failed, using fixed intervals: {e}")
        return self._fixed_split(total_duration)

    def _merge_scenes(self, scene_times: list, total_duration: float) -> list:
        segments = []
        start = 0.0
        for i in range(1, len(scene_times)):
            duration = scene_times[i] - start
            if duration >= TARGET_CLIP_SECONDS:
                end = scene_times[i]
                if duration > MAX_CLIP_SECONDS:
                    while start + TARGET_CLIP_SECONDS < end:
                        segments.append({"start": round(start, 2), "end": round(start + TARGET_CLIP_SECONDS, 2)})
                        start += TARGET_CLIP_SECONDS
                    if end - start >= MIN_CLIP_SECONDS:
                        segments.append({"start": round(start, 2), "end": round(end, 2)})
                else:
                    segments.append({"start": round(start, 2), "end": round(end, 2)})
                start = end
        if total_duration - start >= MIN_CLIP_SECONDS:
            segments.append({"start": round(start, 2), "end": round(total_duration, 2)})
        return segments

    def _fixed_split(self, total_duration: float) -> list:
        segments = []
        pos = 0.0
        while pos < total_duration:
            end = min(pos + TARGET_CLIP_SECONDS, total_duration)
            if end - pos >= MIN_CLIP_SECONDS:
                segments.append({"start": round(pos, 2), "end": round(end, 2)})
            pos = end
        return segments

    def _extract_topics(self, transcript: str, source_title: str = "") -> list:
        if not transcript or len(transcript.split()) < 10:
            return []
        text = f"{source_title}\n{transcript}".strip()
        try:
            keywords = self.kw_model.extract_keywords(
                text, keyphrase_ngram_range=(1, 2), stop_words='english',
                top_n=5, diversity=0.5, use_mmr=True,
            )
            return [kw for kw, score in keywords if score > 0.25][:5]
        except Exception as e:
            log.warning(f"Topic extraction failed: {e}")
            return []

    def _index_clip_fts(self, db, clip_id, title, transcript, platform, channel_name):
        """Insert into FTS5 table (replaces Meilisearch)."""
        try:
            db.execute(
                "INSERT INTO clips_fts(clip_id, title, transcript, platform, channel_name) VALUES (?, ?, ?, ?, ?)",
                (clip_id, title or '', (transcript or '')[:2000], platform or '', channel_name or ''),
            )
        except Exception as e:
            log.warning(f"FTS index failed for {clip_id}: {e}")

    def process_segment(
        self, db, source_file: Path, source_id: str,
        segment: dict, index: int, work_path: Path, metadata: dict
    ) -> str:
        clip_id = str(uuid.uuid4())
        start = segment["start"]
        end = segment["end"]
        duration = end - start

        clip_filename = f"clip_{index:04d}.mp4"
        clip_path = work_path / clip_filename
        thumb_path = work_path / f"thumb_{index:04d}.jpg"

        try:
            self._transcode_clip(source_file, clip_path, start, duration, metadata)
            self._generate_thumbnail(clip_path, thumb_path)
            transcript = self._transcribe(clip_path)
            topics = self._extract_topics(transcript, metadata.get("title", ""))

            clip_key = f"clips/{clip_id}/{clip_filename}"
            thumb_key = f"clips/{clip_id}/thumbnail.jpg"
            file_size = clip_path.stat().st_size

            self.minio.fput_object(MINIO_BUCKET, clip_key, str(clip_path), content_type="video/mp4")
            if thumb_path.exists():
                self.minio.fput_object(MINIO_BUCKET, thumb_key, str(thumb_path), content_type="image/jpeg")

            clip_meta = self.extract_metadata(clip_path)
            expires_at = (datetime.utcnow() + timedelta(days=CLIP_TTL_DAYS)).strftime('%Y-%m-%dT%H:%M:%SZ')
            title = self._generate_clip_title(transcript, metadata.get("title", ""), index)

            row = db.execute("SELECT platform, channel_name FROM sources WHERE id = ?", (source_id,)).fetchone()
            platform = row["platform"] if row else ""
            channel_name = row["channel_name"] if row else ""
            content_score = 0.5

            db.execute("BEGIN IMMEDIATE")
            db.execute("""
                INSERT INTO clips (
                    id, source_id, title, duration_seconds, start_time, end_time,
                    storage_key, thumbnail_key, width, height, file_size_bytes,
                    transcript, topics, content_score, expires_at, status
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')
            """, (
                clip_id, source_id, title, duration, start, end,
                clip_key, thumb_key,
                clip_meta.get("width", 0), clip_meta.get("height", 0),
                file_size, transcript, json.dumps(topics), content_score, expires_at,
            ))
            self._index_clip_fts(db, clip_id, title, transcript, platform, channel_name)
            db.execute("COMMIT")

            log.info(f"Clip {clip_id} created ({duration:.1f}s, topics={topics})")
            return clip_id

        except Exception as e:
            try:
                db.execute("ROLLBACK")
            except Exception:
                pass
            log.error(f"Failed to process segment {index}: {e}")
            return None

    def _transcode_clip(self, source: Path, output: Path, start: float, duration: float, metadata: dict):
        scale_filter = "scale='min(720,iw)':'min(1280,ih)':force_original_aspect_ratio=decrease"
        cmd = [
            "ffmpeg", "-y",
            "-ss", str(start),
            "-i", str(source),
            "-t", str(duration),
            "-vf", scale_filter,
            "-c:v", "libx264",
            "-preset", "fast",
            "-crf", "23",
            "-c:a", "aac",
            "-b:a", "128k",
            "-movflags", "+faststart",
            "-avoid_negative_ts", "make_zero",
            str(output),
        ]
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
        if result.returncode != 0:
            raise RuntimeError(f"Transcode failed: {result.stderr[:300]}")

    def _generate_thumbnail(self, clip_path: Path, thumb_path: Path):
        cmd = [
            "ffmpeg", "-y",
            "-i", str(clip_path),
            "-vf", "thumbnail,scale=480:-1",
            "-frames:v", "1",
            str(thumb_path),
        ]
        subprocess.run(cmd, capture_output=True, timeout=60)

    def _transcribe(self, clip_path: Path) -> str:
        try:
            segments, _ = self.whisper.transcribe(str(clip_path), language="en")
            return " ".join(seg.text.strip() for seg in segments)
        except Exception as e:
            log.warning(f"Transcription failed: {e}")
            return ""

    def _generate_clip_title(self, transcript: str, source_title: str, index: int) -> str:
        if transcript:
            words = transcript.split()[:10]
            if len(words) >= 3:
                return " ".join(words) + "..."
        if source_title:
            return f"{source_title} (Part {index + 1})"
        return f"Clip {index + 1}"


if __name__ == "__main__":
    worker = Worker()
    worker.run()
```

**Step 2: Verify Python syntax**

Run: `python3 -c "import ast; ast.parse(open('ingestion/worker.py').read()); print('OK')`
Expected: `OK`

**Step 3: Commit**

```bash
git add ingestion/worker.py
git commit -m "feat: rewrite worker.py for SQLite + DB polling (drop redis, psycopg2, meilisearch)"
```

---

### Task 6: Rewrite `ingestion/score_updater.py` and `ingestion/lifecycle.py`

**Files:**
- Modify: `ingestion/score_updater.py`
- Modify: `ingestion/lifecycle.py`

Both replace psycopg2 with sqlite3. score_updater replaces the PL/pgSQL `update_content_scores()` call with a pure SQL UPDATE. lifecycle.py replaces Postgres-specific functions (`now()`, `interval`) with SQLite equivalents.

**Step 1: Replace ingestion/score_updater.py**

```python
#!/usr/bin/env python3
import os
import time
import sqlite3
import logging

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("score_updater")

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
INTERVAL = int(os.getenv("SCORE_UPDATE_INTERVAL", "900"))


def main():
    db = sqlite3.connect(DB_PATH)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA busy_timeout=5000")
    log.info(f"Score updater started (interval={INTERVAL}s)")

    while True:
        try:
            db.execute("BEGIN IMMEDIATE")
            db.execute("""
                UPDATE clips
                SET content_score = (
                    SELECT MAX(0.0, MIN(1.0,
                        COALESCE(AVG(CASE WHEN action='view' THEN watch_percentage END), 0.5) * 0.35
                        + COALESCE(
                            CAST(SUM(CASE WHEN action='like'       THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.25
                        + COALESCE(
                            CAST(SUM(CASE WHEN action='save'       THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.20
                        + COALESCE(
                            CAST(SUM(CASE WHEN action='watch_full' THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.15
                        - COALESCE(
                            CAST(SUM(CASE WHEN action='skip'       THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.30
                        - COALESCE(
                            CAST(SUM(CASE WHEN action='dislike'    THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.15
                    ))
                    FROM interactions
                    WHERE interactions.clip_id = clips.id
                    HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5
                )
                WHERE status = 'ready'
                  AND id IN (
                    SELECT clip_id FROM interactions
                    GROUP BY clip_id
                    HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5
                  )
            """)
            count = db.execute("SELECT changes()").fetchone()[0]
            db.execute("COMMIT")
            log.info(f"Updated scores for {count} clips")
        except Exception as e:
            log.error(f"Score update failed: {e}")
            try:
                db.execute("ROLLBACK")
            except Exception:
                pass
            try:
                db = sqlite3.connect(DB_PATH)
                db.execute("PRAGMA journal_mode=WAL")
                db.execute("PRAGMA busy_timeout=5000")
            except Exception:
                pass
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
```

**Step 2: Replace ingestion/lifecycle.py**

```python
#!/usr/bin/env python3
"""
Storage lifecycle manager.
Run via cron or `make lifecycle` to clean up expired clips.
"""

import os
import sqlite3
import logging
from minio import Minio

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("lifecycle")

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
MINIO_ENDPOINT = os.getenv("MINIO_ENDPOINT", "localhost:9000")
MINIO_ACCESS = os.getenv("MINIO_ACCESS_KEY", "clipfeed")
MINIO_SECRET = os.getenv("MINIO_SECRET_KEY", "changeme123")
MINIO_BUCKET = os.getenv("MINIO_BUCKET", "clips")
MINIO_SSL = os.getenv("MINIO_USE_SSL", "false") == "true"
STORAGE_LIMIT_GB = float(os.getenv("STORAGE_LIMIT_GB", "50"))


def main():
    db = sqlite3.connect(DB_PATH)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA busy_timeout=5000")
    db.row_factory = sqlite3.Row

    minio_client = Minio(
        MINIO_ENDPOINT,
        access_key=MINIO_ACCESS,
        secret_key=MINIO_SECRET,
        secure=MINIO_SSL,
    )

    # Phase 1: Delete expired clips that aren't protected
    expired = db.execute("""
        SELECT id, storage_key, thumbnail_key, file_size_bytes
        FROM clips
        WHERE expires_at < strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
          AND is_protected = 0
          AND status = 'ready'
    """).fetchall()

    deleted_count = 0
    freed_bytes = 0
    for clip in expired:
        try:
            if clip["storage_key"]:
                minio_client.remove_object(MINIO_BUCKET, clip["storage_key"])
            if clip["thumbnail_key"]:
                minio_client.remove_object(MINIO_BUCKET, clip["thumbnail_key"])
            db.execute("UPDATE clips SET status = 'expired' WHERE id = ?", (clip["id"],))
            deleted_count += 1
            freed_bytes += clip["file_size_bytes"] or 0
        except Exception as e:
            log.error(f"Failed to clean up clip {clip['id']}: {e}")

    log.info(f"Expired {deleted_count} clips, freed {freed_bytes / (1024**3):.2f} GB")

    # Phase 2: Check total storage usage and evict oldest if over limit
    total_bytes = db.execute(
        "SELECT COALESCE(SUM(file_size_bytes), 0) FROM clips WHERE status = 'ready'"
    ).fetchone()[0]
    total_gb = total_bytes / (1024 ** 3)

    if total_gb > STORAGE_LIMIT_GB:
        overage_bytes = total_bytes - int(STORAGE_LIMIT_GB * (1024 ** 3))
        log.info(f"Storage at {total_gb:.2f} GB (limit {STORAGE_LIMIT_GB} GB), need to free {overage_bytes / (1024**3):.2f} GB")

        candidates = db.execute("""
            SELECT id, storage_key, thumbnail_key, file_size_bytes
            FROM clips
            WHERE is_protected = 0 AND status = 'ready'
            ORDER BY created_at ASC
        """).fetchall()

        evicted = 0
        for clip in candidates:
            if overage_bytes <= 0:
                break
            try:
                if clip["storage_key"]:
                    minio_client.remove_object(MINIO_BUCKET, clip["storage_key"])
                if clip["thumbnail_key"]:
                    minio_client.remove_object(MINIO_BUCKET, clip["thumbnail_key"])
                db.execute("UPDATE clips SET status = 'evicted' WHERE id = ?", (clip["id"],))
                overage_bytes -= clip["file_size_bytes"] or 0
                evicted += 1
            except Exception as e:
                log.error(f"Failed to evict clip {clip['id']}: {e}")

        log.info(f"Evicted {evicted} clips for storage management")

    # Phase 3: Clean up failed jobs older than 7 days
    db.execute("""
        DELETE FROM jobs
        WHERE status IN ('failed', 'complete')
          AND created_at < datetime('now', '-7 days')
    """)

    db.close()
    log.info("Lifecycle cleanup complete")


if __name__ == "__main__":
    main()
```

**Step 3: Verify Python syntax for both files**

Run:
```bash
python3 -c "import ast; ast.parse(open('ingestion/score_updater.py').read()); print('score_updater OK')"
python3 -c "import ast; ast.parse(open('ingestion/lifecycle.py').read()); print('lifecycle OK')"
```
Expected: both print OK.

**Step 4: Commit**

```bash
git add ingestion/score_updater.py ingestion/lifecycle.py
git commit -m "feat: rewrite score_updater and lifecycle for SQLite"
```

---

### Task 7: Update `docker-compose.yml`

**Files:**
- Modify: `docker-compose.yml`

Remove postgres, redis, meilisearch services and their volumes. Add `db_data` named volume mounted at `/data` in api, worker, and score-updater. Update environment variables. Update `depends_on` (api and worker now only depend on minio).

**Step 1: Replace docker-compose.yml**

```yaml
services:
  # --- Object Storage (S3-compatible) ---
  minio:
    image: minio/minio:latest
    container_name: clipfeed-minio
    restart: unless-stopped
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: ${MINIO_USER:-clipfeed}
      MINIO_ROOT_PASSWORD: ${MINIO_PASSWORD:-changeme123}
    volumes:
      - minio_data:/data
    ports:
      - "9000:9000"
      - "9001:9001"
    healthcheck:
      test: ["CMD", "mc", "ready", "local"]
      interval: 10s
      timeout: 5s
      retries: 3

  # --- API Server ---
  api:
    build:
      context: ./api
      dockerfile: Dockerfile
    container_name: clipfeed-api
    restart: unless-stopped
    depends_on:
      minio:
        condition: service_healthy
    environment:
      DB_PATH: /data/clipfeed.db
      MINIO_ENDPOINT: minio:9000
      MINIO_ACCESS_KEY: ${MINIO_USER:-clipfeed}
      MINIO_SECRET_KEY: ${MINIO_PASSWORD:-changeme123}
      MINIO_BUCKET: clips
      MINIO_USE_SSL: "false"
      JWT_SECRET: ${JWT_SECRET:-supersecretkey}
    volumes:
      - db_data:/data
    ports:
      - "8080:8080"

  # --- Ingestion Worker ---
  worker:
    build:
      context: ./ingestion
      dockerfile: Dockerfile
    container_name: clipfeed-worker
    restart: unless-stopped
    depends_on:
      minio:
        condition: service_healthy
    environment:
      DB_PATH: /data/clipfeed.db
      MINIO_ENDPOINT: minio:9000
      MINIO_ACCESS_KEY: ${MINIO_USER:-clipfeed}
      MINIO_SECRET_KEY: ${MINIO_PASSWORD:-changeme123}
      MINIO_BUCKET: clips
      MINIO_USE_SSL: "false"
      WHISPER_MODEL: ${WHISPER_MODEL:-base}
      MAX_CONCURRENT_JOBS: ${MAX_WORKERS:-2}
      CLIP_TTL_DAYS: ${CLIP_TTL_DAYS:-30}
    volumes:
      - db_data:/data
      - worker_tmp:/tmp/clipfeed

  # --- Score Updater ---
  score-updater:
    build:
      context: ./ingestion
      dockerfile: Dockerfile
    container_name: clipfeed-scorer
    restart: unless-stopped
    environment:
      DB_PATH: /data/clipfeed.db
      SCORE_UPDATE_INTERVAL: "${SCORE_UPDATE_INTERVAL:-900}"
    volumes:
      - db_data:/data
    command: ["python", "score_updater.py"]

  # --- Web Frontend ---
  web:
    build:
      context: ./web
      dockerfile: Dockerfile
    container_name: clipfeed-web
    restart: unless-stopped
    depends_on:
      - api
    ports:
      - "3000:3000"

  # --- Reverse Proxy ---
  nginx:
    image: nginx:alpine
    container_name: clipfeed-proxy
    restart: unless-stopped
    depends_on:
      - api
      - web
      - minio
    ports:
      - "80:80"
    volumes:
      - ./nginx/nginx.conf:/etc/nginx/nginx.conf:ro

volumes:
  minio_data:
  db_data:
  worker_tmp:
```

**Step 2: Verify services count**

Run: `docker compose config --services`
Expected output lists exactly: minio, api, worker, score-updater, web, nginx (6 services, down from 8).

**Step 3: Build all services**

Run: `docker compose build`
Expected: all 3 custom services build successfully (api, worker, web).

**Step 4: Commit**

```bash
git add docker-compose.yml
git commit -m "feat: replace postgres/redis/meilisearch with SQLite in compose"
```

---

### Task 8: Update `Makefile` and `.env.example`

**Files:**
- Modify: `Makefile`
- Modify: `.env.example`

Remove postgres/redis/meilisearch references. Update shell-db target to use sqlite3.

**Step 1: Replace Makefile**

```makefile
.PHONY: up down build logs shell-api shell-worker shell-db lifecycle score clean

up:
	docker compose up -d

down:
	docker compose down

build:
	docker compose build --no-cache

logs:
	docker compose logs -f

logs-worker:
	docker compose logs -f worker

logs-api:
	docker compose logs -f api

shell-api:
	docker compose exec api sh

shell-worker:
	docker compose exec worker bash

shell-db:
	docker compose exec api sqlite3 /data/clipfeed.db

lifecycle:
	docker compose exec worker python lifecycle.py

score:
	docker compose exec score-updater python score_updater.py

dev-api:
	cd api && go run .

dev-web:
	cd web && npm run dev

clean:
	docker compose down -v --remove-orphans
```

**Step 2: Replace .env.example**

```bash
# MinIO object storage
MINIO_USER=clipfeed
MINIO_PASSWORD=changeme_strong_password_here

# JWT signing key — generate with: openssl rand -base64 32
JWT_SECRET=changeme_generate_with_openssl

# Storage management
STORAGE_LIMIT_GB=50
CLIP_TTL_DAYS=30

# Worker settings (tune to your NAS hardware)
MAX_WORKERS=4
WHISPER_MODEL=medium

# Score updater interval (seconds)
SCORE_UPDATE_INTERVAL=900
```

**Step 3: Verify full stack starts**

Run:
```bash
docker compose up -d
sleep 10
curl -s http://localhost/health
```
Expected: `{"status":"ok"}`

Run:
```bash
docker compose logs api | grep "listening"
docker compose logs worker | grep "Worker started"
```
Expected: both log lines appear.

**Step 4: Commit**

```bash
git add Makefile .env.example
git commit -m "chore: update Makefile and .env for SQLite-only stack"
```

---

## Verification Checklist

After all 8 tasks complete:

1. **`docker compose ps`** → all 6 services Up (minio, api, worker, score-updater, web, nginx)
2. **`curl http://localhost/health`** → `{"status":"ok"}`
3. **`make shell-db`** → opens sqlite3 REPL on `/data/clipfeed.db`; `.tables` shows all tables
4. **Register + login:** `POST /api/auth/register` then `POST /api/auth/login` → returns JWT token
5. **Ingest test:** `POST /api/ingest` with a YouTube URL → `docker compose logs worker` shows job processing
6. **Search test:** `GET /api/search?q=test` → returns JSON hits from FTS5 (may be empty initially)
7. **`make score`** → logs "Updated scores for N clips"
8. **`make lifecycle`** → logs "Lifecycle cleanup complete"
9. **Memory check:** `docker stats` → API + worker combined under 200MB RAM (vs ~1.5GB with Postgres+Redis+Meili)

## Files Modified Summary

| File | Action |
|------|--------|
| `api/schema.sql` | Create — SQLite schema with FTS5, triggers |
| `api/go.mod` | Modify — drop pgx/redis/meilisearch, add sqlite/uuid |
| `api/main.go` | Rewrite — database/sql + SQLite, FTS5 search, no Redis |
| `ingestion/requirements.txt` | Modify — drop redis/psycopg2/meilisearch |
| `ingestion/worker.py` | Rewrite — sqlite3, DB polling, FTS5 insert |
| `ingestion/score_updater.py` | Rewrite — sqlite3, pure SQL scoring |
| `ingestion/lifecycle.py` | Rewrite — sqlite3, SQLite date functions |
| `docker-compose.yml` | Rewrite — 6 services (drop 3), db_data volume |
| `Makefile` | Modify — drop postgres/redis/meili targets |
| `.env.example` | Modify — drop DB_PASSWORD/MEILI_KEY |
