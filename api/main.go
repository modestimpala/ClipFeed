package main

import (
	"context"
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
	"github.com/jackc/pgx/v5/pgxpool"
	meilisearch "github.com/meilisearch/meilisearch-go"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

type App struct {
	db    *pgxpool.Pool
	rdb   *redis.Client
	minio *minio.Client
	meili *meilisearch.Client
	cfg   Config
}

type Config struct {
	DatabaseURL   string
	RedisURL      string
	MinioEndpoint string
	MinioAccess   string
	MinioSecret   string
	MinioBucket   string
	MinioSSL      bool
	MeiliURL      string
	MeiliKey      string
	JWTSecret     string
	Port          string
}

func loadConfig() Config {
	return Config{
		DatabaseURL:   getEnv("DATABASE_URL", "postgres://clipfeed:changeme@localhost:5432/clipfeed?sslmode=disable"),
		RedisURL:      getEnv("REDIS_URL", "redis://localhost:6379"),
		MinioEndpoint: getEnv("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccess:   getEnv("MINIO_ACCESS_KEY", "clipfeed"),
		MinioSecret:   getEnv("MINIO_SECRET_KEY", "changeme123"),
		MinioBucket:   getEnv("MINIO_BUCKET", "clips"),
		MinioSSL:      getEnv("MINIO_USE_SSL", "false") == "true",
		MeiliURL:      getEnv("MEILI_URL", "http://meilisearch:7700"),
		MeiliKey:      getEnv("MEILI_KEY", ""),
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

	// Postgres
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer pool.Close()

	// Redis
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Fatalf("failed to parse redis url: %v", err)
	}
	rdb := redis.NewClient(opts)

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

	// Meilisearch
	meiliClient := meilisearch.NewClient(meilisearch.ClientConfig{
		Host:   cfg.MeiliURL,
		APIKey: cfg.MeiliKey,
	})

	app := &App{db: pool, rdb: rdb, minio: minioClient, meili: meiliClient, cfg: cfg}

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

	// Health
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	// Public routes
	r.Post("/api/auth/register", app.handleRegister)
	r.Post("/api/auth/login", app.handleLogin)

	// Feed (works for anonymous and authenticated)
	r.Get("/api/feed", app.optionalAuth(app.handleFeed))
	r.Get("/api/clips/{id}", app.handleGetClip)
	r.Get("/api/clips/{id}/stream", app.handleStreamClip)

	// Search (public)
	r.Get("/api/search", app.handleSearch)

	// Authenticated routes
	r.Group(func(r chi.Router) {
		r.Use(app.authMiddleware)

		// Clips
		r.Post("/api/clips/{id}/interact", app.handleInteraction)
		r.Post("/api/clips/{id}/save", app.handleSaveClip)
		r.Delete("/api/clips/{id}/save", app.handleUnsaveClip)

		// Ingestion
		r.Post("/api/ingest", app.handleIngest)
		r.Get("/api/jobs", app.handleListJobs)
		r.Get("/api/jobs/{id}", app.handleGetJob)

		// User
		r.Get("/api/me", app.handleGetProfile)
		r.Put("/api/me/preferences", app.handleUpdatePreferences)
		r.Get("/api/me/saved", app.handleListSaved)
		r.Get("/api/me/history", app.handleListHistory)

		// Platform cookies
		r.Put("/api/me/cookies/{platform}", app.handleSetCookie)
		r.Delete("/api/me/cookies/{platform}", app.handleDeleteCookie)

		// Collections
		r.Post("/api/collections", app.handleCreateCollection)
		r.Get("/api/collections", app.handleListCollections)
		r.Post("/api/collections/{id}/clips", app.handleAddToCollection)
		r.Delete("/api/collections/{id}/clips/{clipId}", app.handleRemoveFromCollection)
	})

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

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

	var userID string
	err = a.db.QueryRow(r.Context(),
		`INSERT INTO users (username, email, password_hash, display_name)
		 VALUES ($1, $2, $3, $1)
		 RETURNING id`,
		req.Username, req.Email, string(hash),
	).Scan(&userID)

	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			writeJSON(w, 409, map[string]string{"error": "username or email already taken"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "failed to create user"})
		return
	}

	// Create default preferences
	a.db.Exec(r.Context(),
		`INSERT INTO user_preferences (user_id) VALUES ($1)`, userID)

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
	err := a.db.QueryRow(r.Context(),
		`SELECT id, password_hash FROM users WHERE username = $1 OR email = $1`,
		req.Username,
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

// --- Search ---

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, 400, map[string]string{"error": "q required"})
		return
	}
	index := a.meili.Index("clips")
	results, err := index.Search(q, &meilisearch.SearchRequest{
		Limit: 20,
		Sort:  []string{"content_score:desc"},
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "search failed"})
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"hits":  results.Hits,
		"query": q,
		"total": results.EstimatedTotalHits,
	})
}

// --- Feed ---

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	limit := 20

	var clips []map[string]interface{}

	if userID != "" {
		// Personalized feed based on user preferences and interaction history
		rows, err := a.db.Query(r.Context(), `
			WITH user_prefs AS (
				SELECT exploration_rate, topic_weights, min_clip_seconds, max_clip_seconds
				FROM user_preferences WHERE user_id = $1
			),
			seen AS (
				SELECT clip_id FROM interactions
				WHERE user_id = $1 AND created_at > now() - interval '24 hours'
			)
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			  AND c.id NOT IN (SELECT clip_id FROM seen)
			  AND c.duration_seconds >= COALESCE((SELECT min_clip_seconds FROM user_prefs), 5)
			  AND c.duration_seconds <= COALESCE((SELECT max_clip_seconds FROM user_prefs), 120)
			ORDER BY
			    (c.content_score * (1 - COALESCE((SELECT exploration_rate FROM user_prefs), 0.3)))
			    + (random() * COALESCE((SELECT exploration_rate FROM user_prefs), 0.3))
			    DESC
			LIMIT $2
		`, userID, limit)

		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "failed to fetch feed"})
			return
		}
		defer rows.Close()
		clips = scanClips(rows)
	} else {
		// Anonymous feed - ranked by content score with randomness
		rows, err := a.db.Query(r.Context(), `
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			ORDER BY (c.content_score * 0.7) + (random() * 0.3) DESC
			LIMIT $1
		`, limit)

		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "failed to fetch feed"})
			return
		}
		defer rows.Close()
		clips = scanClips(rows)
	}

	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

func (a *App) handleGetClip(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var clip map[string]interface{}
	var id, title, description, thumbnailKey, status string
	var duration float64
	var topics, tags []string
	var score float64
	var createdAt time.Time

	err := a.db.QueryRow(r.Context(), `
		SELECT c.id, c.title, c.description, c.duration_seconds,
		       c.thumbnail_key, c.topics, c.tags, c.content_score,
		       c.status, c.created_at
		FROM clips c WHERE c.id = $1
	`, clipID).Scan(&id, &title, &description, &duration, &thumbnailKey,
		&topics, &tags, &score, &status, &createdAt)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	clip = map[string]interface{}{
		"id": id, "title": title, "description": description,
		"duration_seconds": duration, "thumbnail_key": thumbnailKey,
		"topics": topics, "tags": tags, "content_score": score,
		"status": status, "created_at": createdAt,
	}
	writeJSON(w, 200, clip)
}

func (a *App) handleStreamClip(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var storageKey string
	err := a.db.QueryRow(r.Context(),
		`SELECT storage_key FROM clips WHERE id = $1 AND status = 'ready'`,
		clipID).Scan(&storageKey)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	// Generate a presigned URL for direct streaming from MinIO
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

	_, err := a.db.Exec(r.Context(), `
		INSERT INTO interactions (user_id, clip_id, action, watch_duration_seconds, watch_percentage)
		VALUES ($1, $2, $3, $4, $5)
	`, userID, clipID, req.Action, req.WatchDuration, req.WatchPercentage)

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

	// Detect platform
	platform := detectPlatform(req.URL)

	// Insert source
	var sourceID string
	err := a.db.QueryRow(r.Context(), `
		INSERT INTO sources (url, platform, submitted_by, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id
	`, req.URL, platform, userID).Scan(&sourceID)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to create source"})
		return
	}

	// Create job with platform in payload
	var jobID string
	payload := fmt.Sprintf(
		`{"url": "%s", "source_id": "%s", "platform": "%s"}`,
		req.URL, sourceID, platform,
	)
	err = a.db.QueryRow(r.Context(), `
		INSERT INTO jobs (source_id, job_type, payload)
		VALUES ($1, 'download', $2)
		RETURNING id
	`, sourceID, payload).Scan(&jobID)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to queue job"})
		return
	}

	// Push to Redis queue
	a.rdb.RPush(r.Context(), "clipfeed:jobs", jobID)

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
		return "tiktok"
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
	rows, err := a.db.Query(r.Context(), `
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
		var id, jobType, status string
		var sourceID *string
		var createdAt time.Time
		var completedAt *time.Time
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
	var id, jobType, status string
	var sourceID *string
	var payload, result json.RawMessage
	var errMsg *string
	var createdAt time.Time

	err := a.db.QueryRow(r.Context(), `
		SELECT id, source_id, job_type, status, payload, result, error, created_at
		FROM jobs WHERE id = $1
	`, jobID).Scan(&id, &sourceID, &jobType, &status, &payload, &result, &errMsg, &createdAt)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "job not found"})
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"id": id, "source_id": sourceID, "job_type": jobType,
		"status": status, "payload": payload, "result": result,
		"error": errMsg, "created_at": createdAt,
	})
}

// --- User Profile ---

func (a *App) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	var username, email, displayName string
	var avatarURL *string
	var createdAt time.Time

	err := a.db.QueryRow(r.Context(), `
		SELECT username, email, display_name, avatar_url, created_at
		FROM users WHERE id = $1
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

	_, err := a.db.Exec(r.Context(), `
		INSERT INTO user_preferences (user_id, exploration_rate, topic_weights, min_clip_seconds, max_clip_seconds, autoplay)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id) DO UPDATE SET
			exploration_rate = COALESCE($2, user_preferences.exploration_rate),
			topic_weights = COALESCE($3, user_preferences.topic_weights),
			min_clip_seconds = COALESCE($4, user_preferences.min_clip_seconds),
			max_clip_seconds = COALESCE($5, user_preferences.max_clip_seconds),
			autoplay = COALESCE($6, user_preferences.autoplay),
			updated_at = now()
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

	_, err := a.db.Exec(r.Context(),
		`INSERT INTO saved_clips (user_id, clip_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
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

	a.db.Exec(r.Context(),
		`DELETE FROM saved_clips WHERE user_id = $1 AND clip_id = $2`,
		userID, clipID)

	writeJSON(w, 200, map[string]string{"status": "removed"})
}

func (a *App) handleListSaved(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	rows, err := a.db.Query(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key, c.topics, c.created_at
		FROM saved_clips sc
		JOIN clips c ON sc.clip_id = c.id
		WHERE sc.user_id = $1
		ORDER BY sc.created_at DESC
	`, userID)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list saved clips"})
		return
	}
	defer rows.Close()

	var clips []map[string]interface{}
	for rows.Next() {
		var id, title, thumbnailKey string
		var duration float64
		var topics []string
		var createdAt time.Time
		rows.Scan(&id, &title, &duration, &thumbnailKey, &topics, &createdAt)
		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "topics": topics, "created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"clips": clips})
}

func (a *App) handleListHistory(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	rows, err := a.db.Query(r.Context(), `
		SELECT DISTINCT ON (i.clip_id) c.id, c.title, c.duration_seconds,
		       c.thumbnail_key, i.action, i.created_at
		FROM interactions i
		JOIN clips c ON i.clip_id = c.id
		WHERE i.user_id = $1
		ORDER BY i.clip_id, i.created_at DESC
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
		var at time.Time
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

	_, err := a.db.Exec(r.Context(), `
		INSERT INTO platform_cookies (user_id, platform, cookie_str, is_active, updated_at)
		VALUES ($1, $2, $3, true, now())
		ON CONFLICT (user_id, platform) DO UPDATE SET
			cookie_str = $3, is_active = true, updated_at = now()
	`, userID, platform, req.CookieStr)

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

	a.db.Exec(r.Context(),
		`UPDATE platform_cookies SET is_active = false, updated_at = now()
		 WHERE user_id = $1 AND platform = $2`,
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

	var id string
	err := a.db.QueryRow(r.Context(), `
		INSERT INTO collections (user_id, title, description) VALUES ($1, $2, $3) RETURNING id
	`, userID, req.Title, req.Description).Scan(&id)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to create collection"})
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (a *App) handleListCollections(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	rows, err := a.db.Query(r.Context(), `
		SELECT c.id, c.title, c.description, c.is_public, c.created_at,
		       COUNT(cc.clip_id) as clip_count
		FROM collections c
		LEFT JOIN collection_clips cc ON c.id = cc.collection_id
		WHERE c.user_id = $1
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
		var id, title string
		var description *string
		var isPublic bool
		var createdAt time.Time
		var clipCount int
		rows.Scan(&id, &title, &description, &isPublic, &createdAt, &clipCount)
		collections = append(collections, map[string]interface{}{
			"id": id, "title": title, "description": description,
			"is_public": isPublic, "clip_count": clipCount, "created_at": createdAt,
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

	_, err := a.db.Exec(r.Context(), `
		INSERT INTO collection_clips (collection_id, clip_id, position)
		VALUES ($1, $2, COALESCE((SELECT MAX(position) + 1 FROM collection_clips WHERE collection_id = $1), 0))
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

	a.db.Exec(r.Context(),
		`DELETE FROM collection_clips WHERE collection_id = $1 AND clip_id = $2`,
		collectionID, clipID)

	writeJSON(w, 200, map[string]string{"status": "removed"})
}

// --- Helpers ---

func scanClips(rows interface {
	Next() bool
	Scan(...interface{}) error
}) []map[string]interface{} {
	var clips []map[string]interface{}
	for rows.Next() {
		var id, title, description, thumbnailKey string
		var duration, score float64
		var topics, tags []string
		var createdAt time.Time
		var channelName, platform *string

		rows.Scan(&id, &title, &description, &duration,
			&thumbnailKey, &topics, &tags, &score,
			&createdAt, &channelName, &platform)

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
