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

	// Ensure bucket exists
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
		req.Username, req.Username,
	).Scan(&userID, &hash)
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
		userID := a.extractUserID(r)
		if userID != "" {
			ctx := context.WithValue(r.Context(), userIDKey, userID)
			r = r.WithContext(ctx)
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
	var duration float64
	var score float64

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
		SELECT j.id, j.source_id, j.job_type, j.status, j.error,
		       j.attempts, j.max_attempts, j.started_at, j.completed_at, j.created_at,
		       s.url, s.platform, s.title
		FROM jobs j
		LEFT JOIN sources s ON j.source_id = s.id
		ORDER BY j.created_at DESC LIMIT 50
	`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list jobs"})
		return
	}
	defer rows.Close()

	var jobs []map[string]interface{}
	for rows.Next() {
		var id, jobType, status, createdAt string
		var sourceID, errMsg, startedAt, completedAt, url, platform, title *string
		var attempts, maxAttempts int
		rows.Scan(&id, &sourceID, &jobType, &status, &errMsg,
			&attempts, &maxAttempts, &startedAt, &completedAt, &createdAt,
			&url, &platform, &title)
		job := map[string]interface{}{
			"id": id, "source_id": sourceID, "job_type": jobType,
			"status": status, "error": errMsg,
			"attempts": attempts, "max_attempts": maxAttempts,
			"started_at": startedAt, "completed_at": completedAt, "created_at": createdAt,
			"url": url, "platform": platform, "title": title,
		}
		jobs = append(jobs, job)
	}
	writeJSON(w, 200, map[string]interface{}{"jobs": jobs})
}

func (a *App) handleGetJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	var id, jobType, status, payloadStr, resultStr, createdAt string
	var sourceID *string
	var errMsg *string

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
		var id, title, thumbnailKey, action string
		var duration float64
		var at string
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
	"youtube": true, "tiktok": true, "instagram": true, "twitter": true,
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

	a.db.ExecContext(r.Context(),
		`UPDATE platform_cookies SET is_active = 0, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		 WHERE user_id = ? AND platform = ?`,
		userID, platform)

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
		       COUNT(cc.clip_id) as clip_count
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
		INSERT INTO collection_clips (collection_id, clip_id, position)
		VALUES (?, ?, COALESCE((SELECT MAX(position) + 1 FROM collection_clips WHERE collection_id = ?), 0))
		ON CONFLICT DO NOTHING
	`, collectionID, req.ClipID)

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
