package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
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
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	_ "modernc.org/sqlite"
)

type App struct {
	db    *CompatDB
	minio *minio.Client
	cfg   Config

	tgMu       sync.RWMutex
	topicGraph *TopicGraph

	ltrMu    sync.RWMutex
	ltrModel *LTRModel
}

type Config struct {
	DBDriver       string
	DBPath         string
	DBURL          string
	L2RModelPath   string
	MinioEndpoint  string
	MinioAccess    string
	MinioSecret    string
	MinioBucket    string
	MinioSSL       bool
	JWTSecret      string
	AdminJWTSecret string
	CookieSecret   string
	AdminUsername  string
	AdminPassword  string
	Port           string
	AllowedOrigins string
	WorkerSecret   string
}

// defaultSecrets lists the baked-in placeholder values that MUST be changed
// before running in production.  When any of these remain, the server refuses
// to start unless ALLOW_INSECURE_DEFAULTS=true (for local development only).
var defaultSecrets = map[string]string{
	"JWT_SECRET":      "supersecretkey",
	"MINIO_SECRET_KEY": "changeme123",
	"ADMIN_PASSWORD":  "changeme_admin_password",
}

func loadConfig() Config {
	adminJWT := getEnv("ADMIN_JWT_SECRET", "")
	if adminJWT == "" {
		// Fall back to JWT_SECRET so existing deployments keep working,
		// but operators should set a separate key.
		adminJWT = getEnv("JWT_SECRET", "supersecretkey")
	}
	return Config{
		DBDriver:       getEnv("DB_DRIVER", "sqlite"),
		DBPath:         getEnv("DB_PATH", "/data/clipfeed.db"),
		DBURL:          getEnv("DB_URL", ""),
		L2RModelPath:   getEnv("L2R_MODEL_PATH", "/data/l2r_model.json"),
		MinioEndpoint:  getEnv("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccess:    getEnv("MINIO_ACCESS_KEY", "clipfeed"),
		MinioSecret:    getEnv("MINIO_SECRET_KEY", "changeme123"),
		MinioBucket:    getEnv("MINIO_BUCKET", "clips"),
		MinioSSL:       getEnv("MINIO_USE_SSL", "false") == "true",
		JWTSecret:      getEnv("JWT_SECRET", "supersecretkey"),
		AdminJWTSecret: adminJWT,
		CookieSecret:   getEnv("COOKIE_SECRET", getEnv("JWT_SECRET", "supersecretkey")),
		AdminUsername:  getEnv("ADMIN_USERNAME", "admin"),
		AdminPassword:  getEnv("ADMIN_PASSWORD", "changeme_admin_password"),
		Port:           getEnv("PORT", "8080"),
		AllowedOrigins: getEnv("ALLOWED_ORIGINS", "*"),
		WorkerSecret:   getEnv("WORKER_SECRET", ""),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func isInsecureDefaultsAllowed() bool {
	v := strings.ToLower(os.Getenv("ALLOW_INSECURE_DEFAULTS"))
	return v == "true" || v == "1" || v == "yes"
}

func main() {
	cfg := loadConfig()

	// Refuse to start with known default secrets unless explicitly overridden.
	if !isInsecureDefaultsAllowed() {
		var insecure []string
		for envKey, placeholder := range defaultSecrets {
			if getEnv(envKey, placeholder) == placeholder {
				insecure = append(insecure, envKey)
			}
		}
		if len(insecure) > 0 {
			log.Fatalf("FATAL: the following secrets still use insecure defaults: %v\n"+
				"Set them in your .env file or pass ALLOW_INSECURE_DEFAULTS=true for local development.",
				insecure)
		}
	} else {
		log.Println("WARNING: ALLOW_INSECURE_DEFAULTS=true — running with default secrets (development mode)")
	}

	// Database
	var dialect Dialect
	var rawDB *sql.DB

	switch strings.ToLower(cfg.DBDriver) {
	case "postgres", "postgresql":
		dialect = DialectPostgres
		if cfg.DBURL == "" {
			log.Fatal("DB_URL is required when DB_DRIVER=postgres")
		}
		var err error
		rawDB, err = sql.Open("pgx", cfg.DBURL)
		if err != nil {
			log.Fatalf("failed to open postgres: %v", err)
		}
		rawDB.SetMaxOpenConns(10)
		rawDB.SetMaxIdleConns(5)
		rawDB.SetConnMaxLifetime(5 * time.Minute)

		if err := runMigrations(rawDB, dialect); err != nil {
			log.Fatalf("failed to init postgres schema: %v", err)
		}
		log.Println("Using Postgres database")

	default:
		dialect = DialectSQLite
		var err error
		rawDB, err = sql.Open("sqlite", cfg.DBPath)
		if err != nil {
			log.Fatalf("failed to open database: %v", err)
		}
		// Allow concurrent reads in WAL mode: writes still serialize naturally
		rawDB.SetMaxOpenConns(4)
		rawDB.SetMaxIdleConns(4)
		rawDB.SetConnMaxLifetime(0)

		for _, pragma := range []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA busy_timeout=5000",
			"PRAGMA foreign_keys=ON",
			"PRAGMA synchronous=NORMAL",
		} {
			if _, err := rawDB.Exec(pragma); err != nil {
				log.Fatalf("pragma failed (%s): %v", pragma, err)
			}
		}

		if err := runMigrations(rawDB, dialect); err != nil {
			log.Fatalf("failed to init schema: %v", err)
		}
		log.Println("Using SQLite database")
	}

	db := NewCompatDB(rawDB, dialect)
	defer db.Close()

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

	// Set public-read policy scoped to thumbnail prefix only — video streams
	// remain protected behind presigned URLs.
	publicPolicy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/clips/*/thumbnail.jpg"]}]}`, cfg.MinioBucket)
	if err := minioClient.SetBucketPolicy(ctx, cfg.MinioBucket, publicPolicy); err != nil {
		log.Printf("warning: failed to set public-read policy on bucket: %v", err)
	}

	app := &App{db: db, minio: minioClient, cfg: cfg}
	app.refreshTopicGraph()
	go app.topicGraphRefreshLoop()
	app.ltrModel = app.loadLTRModel()
	go app.ltrModelRefreshLoop()

	// Rate limiters: auth endpoints get a tight limit, general API is more generous.
	authRL := newRateLimiter(10, 1*time.Minute)  // 10 attempts per IP per minute

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	// Global request body size limit (1 MB) to prevent oversized payloads.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, defaultBodyLimit)
			next.ServeHTTP(w, r)
		})
	})

	// Security headers
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			next.ServeHTTP(w, r)
		})
	})

	// CORS — AllowedOrigins is configurable via ALLOWED_ORIGINS (comma-separated).
	// AllowCredentials is intentionally false: JWT tokens are sent via the
	// Authorization header and do not require browser credential mode.
	allowedOrigins := strings.Split(cfg.AllowedOrigins, ",")
	for i := range allowedOrigins {
		allowedOrigins[i] = strings.TrimSpace(allowedOrigins[i])
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	r.Get("/api/config", func(w http.ResponseWriter, r *http.Request) {
		aiEnabled := os.Getenv("ENABLE_AI") == "true"
		w.Header().Set("Cache-Control", "public, max-age=300")
		writeJSON(w, 200, map[string]interface{}{
			"ai_enabled": aiEnabled,
		})
	})

	// Auth routes with rate limiting
	r.Group(func(r chi.Router) {
		r.Use(rateLimitMiddleware(authRL))
		r.Post("/api/auth/register", app.handleRegister)
		r.Post("/api/auth/login", app.handleLogin)
		r.Post("/api/admin/login", app.handleAdminLogin)
	})
	r.Get("/api/feed", app.optionalAuth(app.handleFeed))
	r.Get("/api/clips/{id}", app.handleGetClip)
	r.Get("/api/clips/{id}/stream", app.handleStreamClip)
	r.Get("/api/clips/{id}/similar", app.handleSimilarClips)
	r.Get("/api/search", app.handleSearch)
	r.Get("/api/topics", app.handleGetTopics)
	r.Get("/api/topics/tree", app.handleGetTopicTree)

	r.Group(func(r chi.Router) {
		r.Use(app.adminAuthMiddleware)
		r.Get("/api/admin/status", app.handleAdminStatus)
		r.Get("/api/admin/llm_logs", app.handleAdminLLMLogs)
		r.Post("/api/admin/clear-failed", app.handleClearFailedJobs)
	})

	r.Group(func(r chi.Router) {
		r.Use(app.authMiddleware)
		r.Post("/api/clips/{id}/summary", app.handleClipSummary)
		r.Post("/api/clips/{id}/interact", app.handleInteraction)
		r.Post("/api/clips/{id}/save", app.handleSaveClip)
		r.Delete("/api/clips/{id}/save", app.handleUnsaveClip)
		r.Post("/api/ingest", app.handleIngest)
		r.Get("/api/jobs", app.handleListJobs)
		r.Get("/api/jobs/{id}", app.handleGetJob)
		r.Post("/api/jobs/{id}/cancel", app.handleCancelJob)
		r.Post("/api/jobs/{id}/retry", app.handleRetryJob)
		r.Delete("/api/jobs/{id}", app.handleDismissJob)
		r.Get("/api/me", app.handleGetProfile)
		r.Put("/api/me/preferences", app.handleUpdatePreferences)
		r.Get("/api/me/saved", app.handleListSaved)
		r.Get("/api/me/history", app.handleListHistory)
		r.Get("/api/me/cookies", app.handleListCookieStatus)
		r.Put("/api/me/cookies/{platform}", app.handleSetCookie)
		r.Delete("/api/me/cookies/{platform}", app.handleDeleteCookie)
		r.Post("/api/collections", app.handleCreateCollection)
		r.Get("/api/collections", app.handleListCollections)
		r.Get("/api/collections/{id}/clips", app.handleGetCollectionClips)
		r.Post("/api/collections/{id}/clips", app.handleAddToCollection)
		r.Delete("/api/collections/{id}/clips/{clipId}", app.handleRemoveFromCollection)
		r.Delete("/api/collections/{id}", app.handleDeleteCollection)

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
		r.Get("/api/scout/profile", app.handleGetScoutProfile)
	})

	// Internal worker API (authenticated via WORKER_SECRET)
	r.Group(func(r chi.Router) {
		r.Use(app.workerAuthMiddleware)
		r.Post("/api/internal/jobs/claim", app.handleWorkerClaimJob)
		r.Put("/api/internal/jobs/{id}", app.handleWorkerUpdateJob)
		r.Get("/api/internal/jobs/{id}", app.handleWorkerGetJob)
		r.Post("/api/internal/jobs/reclaim", app.handleWorkerReclaimStale)
		r.Put("/api/internal/sources/{id}", app.handleWorkerUpdateSource)
		r.Get("/api/internal/sources/{id}/cookie", app.handleWorkerGetCookie)
		r.Post("/api/internal/clips", app.handleWorkerCreateClip)
		r.Post("/api/internal/topics/resolve", app.handleWorkerResolveTopic)
		r.Post("/api/internal/scores/update", app.handleWorkerScoreUpdate)
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
