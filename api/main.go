package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"clipfeed/admin"
	"clipfeed/auth"
	"clipfeed/clips"
	"clipfeed/collections"
	"clipfeed/db"
	"clipfeed/feed"
	"clipfeed/httputil"
	"clipfeed/ingest"
	"clipfeed/jobs"
	"clipfeed/profile"
	"clipfeed/ratelimit"
	"clipfeed/saved"
	"clipfeed/scout"
	"clipfeed/worker"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	_ "modernc.org/sqlite"
)

// Config holds all environment-derived configuration.
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
	AdminUsername   string
	AdminPassword  string
	Port           string
	AllowedOrigins string
	WorkerSecret   string
}

// defaultSecrets lists the baked-in placeholder values that MUST be changed
// before running in production.
var defaultSecrets = map[string]string{
	"JWT_SECRET":       "supersecretkey",
	"MINIO_SECRET_KEY": "changeme123",
	"ADMIN_PASSWORD":   "changeme_admin_password",
}

func loadConfig() Config {
	adminJWT := getEnv("ADMIN_JWT_SECRET", "")
	if adminJWT == "" {
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
		AdminUsername:   getEnv("ADMIN_USERNAME", "admin"),
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
		log.Println("WARNING: ALLOW_INSECURE_DEFAULTS=true -- running with default secrets (development mode)")
	}

	// --- Database ---
	var dialect db.Dialect
	var rawDB *sql.DB

	switch strings.ToLower(cfg.DBDriver) {
	case "postgres", "postgresql":
		dialect = db.DialectPostgres
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

		if err := db.RunMigrations(rawDB, dialect); err != nil {
			log.Fatalf("failed to init postgres schema: %v", err)
		}
		log.Println("Using Postgres database")

	default:
		dialect = db.DialectSQLite
		var err error
		rawDB, err = sql.Open("sqlite", cfg.DBPath)
		if err != nil {
			log.Fatalf("failed to open database: %v", err)
		}
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

		if err := db.RunMigrations(rawDB, dialect); err != nil {
			log.Fatalf("failed to init schema: %v", err)
		}
		log.Println("Using SQLite database")
	}

	compatDB := db.NewCompatDB(rawDB, dialect)
	defer compatDB.Close()

	// --- MinIO ---
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

	publicPolicy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/clips/*/thumbnail.jpg"]}]}`, cfg.MinioBucket)
	if err := minioClient.SetBucketPolicy(ctx, cfg.MinioBucket, publicPolicy); err != nil {
		log.Printf("warning: failed to set public-read policy on bucket: %v", err)
	}

	// --- Handlers ---
	authH := &auth.Handler{DB: compatDB, JWTSecret: cfg.JWTSecret}
	feedH := &feed.Handler{DB: compatDB, MinioBucket: cfg.MinioBucket, LTRModelPath: cfg.L2RModelPath}
	feedH.RefreshTopicGraph()
	go feedH.TopicGraphRefreshLoop()
	feedH.SetLTRModel(feedH.LoadLTRModel())
	go feedH.LTRModelRefreshLoop()

	clipsH := &clips.Handler{DB: compatDB, Minio: minioClient, MinioBucket: cfg.MinioBucket}
	adminH := &admin.Handler{DB: compatDB, AdminUsername: cfg.AdminUsername, AdminPassword: cfg.AdminPassword, AdminJWTSecret: cfg.AdminJWTSecret}
	workerH := &worker.Handler{DB: compatDB, WorkerSecret: cfg.WorkerSecret, CookieSecret: cfg.CookieSecret}
	ingestH := &ingest.Handler{DB: compatDB}
	savedH := &saved.Handler{DB: compatDB, MinioBucket: cfg.MinioBucket}
	collectionsH := &collections.Handler{DB: compatDB, MinioBucket: cfg.MinioBucket}
	jobsH := &jobs.Handler{DB: compatDB}
	profileH := &profile.Handler{DB: compatDB, CookieSecret: cfg.CookieSecret}
	scoutH := &scout.Handler{DB: compatDB}

	// --- Rate limiters ---
	authRL := ratelimit.New(10, 1*time.Minute)

	// --- Router ---
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	// Global request body size limit (1 MB).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Body = http.MaxBytesReader(w, req.Body, httputil.DefaultBodyLimit)
			next.ServeHTTP(w, req)
		})
	})

	// Security headers
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			next.ServeHTTP(w, req)
		})
	})

	// CORS
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

	// Health / config
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	r.Get("/api/config", func(w http.ResponseWriter, r *http.Request) {
		provider := os.Getenv("LLM_PROVIDER")
		apiKey := os.Getenv("LLM_API_KEY")
		aiEnabled := provider != "" && (provider == "ollama" || apiKey != "")
		w.Header().Set("Cache-Control", "public, max-age=300")
		httputil.WriteJSON(w, 200, map[string]interface{}{"ai_enabled": aiEnabled})
	})

	// Auth routes (rate limited)
	r.Group(func(r chi.Router) {
		r.Use(ratelimit.Middleware(authRL))
		r.Post("/api/auth/register", authH.HandleRegister)
		r.Post("/api/auth/login", authH.HandleLogin)
		r.Post("/api/admin/login", adminH.HandleAdminLogin)
	})

	// Public routes
	r.Get("/api/feed", authH.OptionalAuth(feedH.HandleFeed))
	r.Get("/api/clips/{id}", clipsH.HandleGetClip)
	r.Get("/api/clips/{id}/stream", clipsH.HandleStreamClip)
	r.Get("/api/clips/{id}/similar", feedH.HandleSimilarClips)
	r.Get("/api/search", feedH.HandleSearch)
	r.Get("/api/topics", feedH.HandleGetTopics)
	r.Get("/api/topics/tree", feedH.HandleGetTopicTree)

	// Admin routes
	r.Group(func(r chi.Router) {
		r.Use(adminH.AdminAuthMiddleware)
		r.Get("/api/admin/status", adminH.HandleAdminStatus)
		r.Get("/api/admin/llm_logs", adminH.HandleAdminLLMLogs)
		r.Post("/api/admin/clear-failed", adminH.HandleClearFailedJobs)
	})

	// Authenticated user routes
	r.Group(func(r chi.Router) {
		r.Use(authH.AuthMiddleware)
		r.Post("/api/clips/{id}/summary", clipsH.HandleClipSummary)
		r.Post("/api/clips/{id}/interact", clipsH.HandleInteraction)
		r.Post("/api/clips/{id}/save", savedH.HandleSaveClip)
		r.Delete("/api/clips/{id}/save", savedH.HandleUnsaveClip)
		r.Post("/api/ingest", ingestH.HandleIngest)
		r.Get("/api/jobs", jobsH.HandleListJobs)
		r.Get("/api/jobs/{id}", jobsH.HandleGetJob)
		r.Post("/api/jobs/{id}/cancel", jobsH.HandleCancelJob)
		r.Post("/api/jobs/{id}/retry", jobsH.HandleRetryJob)
		r.Delete("/api/jobs/{id}", jobsH.HandleDismissJob)
		r.Get("/api/me", profileH.HandleGetProfile)
		r.Put("/api/me/preferences", profileH.HandleUpdatePreferences)
		r.Get("/api/me/saved", savedH.HandleListSaved)
		r.Get("/api/me/history", savedH.HandleListHistory)
		r.Get("/api/me/cookies", profileH.HandleListCookieStatus)
		r.Put("/api/me/cookies/{platform}", profileH.HandleSetCookie)
		r.Delete("/api/me/cookies/{platform}", profileH.HandleDeleteCookie)
		r.Post("/api/collections", collectionsH.HandleCreateCollection)
		r.Get("/api/collections", collectionsH.HandleListCollections)
		r.Get("/api/collections/{id}/clips", collectionsH.HandleGetCollectionClips)
		r.Post("/api/collections/{id}/clips", collectionsH.HandleAddToCollection)
		r.Delete("/api/collections/{id}/clips/{clipId}", collectionsH.HandleRemoveFromCollection)
		r.Delete("/api/collections/{id}", collectionsH.HandleDeleteCollection)

		// Saved filters
		r.Post("/api/filters", feedH.HandleCreateFilter)
		r.Get("/api/filters", feedH.HandleListFilters)
		r.Put("/api/filters/{id}", feedH.HandleUpdateFilter)
		r.Delete("/api/filters/{id}", feedH.HandleDeleteFilter)

		// Content scout
		r.Post("/api/scout/sources", scoutH.HandleCreateScoutSource)
		r.Get("/api/scout/sources", scoutH.HandleListScoutSources)
		r.Patch("/api/scout/sources/{id}", scoutH.HandleUpdateScoutSource)
		r.Delete("/api/scout/sources/{id}", scoutH.HandleDeleteScoutSource)
		r.Post("/api/scout/sources/{id}/trigger", scoutH.HandleTriggerScoutSource)
		r.Get("/api/scout/candidates", scoutH.HandleListScoutCandidates)
		r.Post("/api/scout/candidates/{id}/approve", scoutH.HandleApproveCandidate)
		r.Get("/api/scout/profile", scoutH.HandleGetScoutProfile)
	})

	// Internal worker API
	r.Group(func(r chi.Router) {
		r.Use(workerH.WorkerAuthMiddleware)
		r.Post("/api/internal/jobs/claim", workerH.HandleClaimJob)
		r.Put("/api/internal/jobs/{id}", workerH.HandleUpdateJob)
		r.Get("/api/internal/jobs/{id}", workerH.HandleGetJob)
		r.Post("/api/internal/jobs/{id}/heartbeat", workerH.HandleHeartbeat)
		r.Post("/api/internal/jobs/reclaim", workerH.HandleReclaimStale)
		r.Put("/api/internal/sources/{id}", workerH.HandleUpdateSource)
		r.Get("/api/internal/sources/{id}/cookie", workerH.HandleGetCookie)
		r.Post("/api/internal/clips", workerH.HandleCreateClip)
		r.Post("/api/internal/topics/resolve", workerH.HandleResolveTopic)
		r.Post("/api/internal/scores/update", workerH.HandleScoreUpdate)
		r.Post("/api/internal/llm-logs", workerH.HandleCreateLLMLog)
	})

	// --- Start server ---
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
