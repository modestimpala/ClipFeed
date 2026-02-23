package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type App struct {
	db    *sql.DB
	minio *minio.Client
	cfg   Config

	tgMu       sync.RWMutex
	topicGraph *TopicGraph

	ltrMu    sync.RWMutex
	ltrModel *LTRModel
}

type Config struct {
	DBPath        string
	L2RModelPath  string
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
		L2RModelPath:  getEnv("L2R_MODEL_PATH", "/data/l2r_model.json"),
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

	// Column migrations for existing databases (ALTER TABLE is not idempotent in SQLite).
	for _, m := range []string{
		"ALTER TABLE user_preferences ADD COLUMN scout_threshold REAL DEFAULT 6.0",
		"ALTER TABLE user_preferences ADD COLUMN dedupe_seen_24h INTEGER DEFAULT 1",
		"ALTER TABLE scout_sources ADD COLUMN force_check INTEGER DEFAULT 0",
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			log.Fatalf("migration failed (%s): %v", m, err)
		}
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
	app.refreshTopicGraph()
	go app.topicGraphRefreshLoop()
	app.ltrModel = app.loadLTRModel()
	go app.ltrModelRefreshLoop()

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
	r.Get("/api/clips/{id}/similar", app.handleSimilarClips)
	r.Get("/api/clips/{id}/summary", app.handleClipSummary)
	r.Get("/api/search", app.handleSearch)
	r.Get("/api/topics", app.handleGetTopics)
	r.Get("/api/topics/tree", app.handleGetTopicTree)

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
		r.Get("/api/me/cookies", app.handleListCookieStatus)
		r.Put("/api/me/cookies/{platform}", app.handleSetCookie)
		r.Delete("/api/me/cookies/{platform}", app.handleDeleteCookie)
		r.Post("/api/collections", app.handleCreateCollection)
		r.Get("/api/collections", app.handleListCollections)
		r.Post("/api/collections/{id}/clips", app.handleAddToCollection)
		r.Delete("/api/collections/{id}/clips/{clipId}", app.handleRemoveFromCollection)

		// Saved filters
		r.Post("/api/filters", app.handleCreateFilter)
		r.Get("/api/filters", app.handleListFilters)
		r.Put("/api/filters/{id}", app.handleUpdateFilter)
		r.Delete("/api/filters/{id}", app.handleDeleteFilter)

		// Content scout
		r.Post("/api/scout/sources", app.handleCreateScoutSource)
		r.Get("/api/scout/sources", app.handleListScoutSources)
		r.Patch("/api/scout/sources/{id}", app.handleUpdateScoutSource)
		r.Delete("/api/scout/sources/{id}", app.handleDeleteScoutSource)
		r.Post("/api/scout/sources/{id}/trigger", app.handleTriggerScoutSource)
		r.Get("/api/scout/candidates", app.handleListScoutCandidates)
		r.Post("/api/scout/candidates/{id}/approve", app.handleApproveCandidate)
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
