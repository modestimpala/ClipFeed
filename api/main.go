package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/crypto/bcrypt"
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

// --- Topic Graph ---

const topicDecayPerHop = 0.7
const maxLateralHops = 2

type TopicNode struct {
	ID        string
	Name      string
	Slug      string
	Path      string
	ParentID  string
	Depth     int
	ClipCount int
}

type TopicEdge struct {
	TargetID string
	Relation string
	Weight   float64
}

type TopicGraph struct {
	nodes    map[string]*TopicNode
	bySlug   map[string]*TopicNode
	byName   map[string]*TopicNode
	children map[string][]string
	edges    map[string][]TopicEdge
}

func (g *TopicGraph) resolveByName(name string) *TopicNode {
	if g == nil {
		return nil
	}
	if n, ok := g.byName[strings.ToLower(name)]; ok {
		return n
	}
	return nil
}

func (g *TopicGraph) computeBoost(clipTopicIDs []string, userAffinities map[string]float64) float64 {
	if len(clipTopicIDs) == 0 || len(userAffinities) == 0 {
		return 1.0
	}

	totalBoost := 0.0
	matchCount := 0

	for _, ctID := range clipTopicIDs {
		bestBoost := 0.0

		// Direct match
		if w, ok := userAffinities[ctID]; ok {
			bestBoost = w
		}

		node := g.nodes[ctID]
		if node != nil {
			// Walk ancestors: clip tagged "carbonara" matches user affinity for "cooking" with decay
			hops := 0
			current := node
			for current.ParentID != "" {
				hops++
				if w, ok := userAffinities[current.ParentID]; ok {
					decayed := w * math.Pow(topicDecayPerHop, float64(hops))
					if decayed > bestBoost {
						bestBoost = decayed
					}
				}
				current = g.nodes[current.ParentID]
				if current == nil {
					break
				}
			}

			// Walk descendants: user likes "cooking", clip tagged "carbonara" gets boost
			g.walkDescendants(ctID, 1, func(childID string, depth int) {
				if w, ok := userAffinities[childID]; ok {
					decayed := w * math.Pow(topicDecayPerHop, float64(depth))
					if decayed > bestBoost {
						bestBoost = decayed
					}
				}
			})
		}

		// Multi-hop lateral edges
		g.walkLaterals(ctID, maxLateralHops, func(targetID string, hops int, weight float64) {
			if w, ok := userAffinities[targetID]; ok {
				lateral := w * weight * math.Pow(topicDecayPerHop, float64(hops))
				if lateral > bestBoost {
					bestBoost = lateral
				}
			}
		})

		if bestBoost > 0 {
			totalBoost += bestBoost
			matchCount++
		}
	}

	if matchCount == 0 {
		return 1.0
	}
	return totalBoost / float64(matchCount)
}

func (g *TopicGraph) walkDescendants(nodeID string, depth int, fn func(childID string, depth int)) {
	if depth > 3 {
		return
	}
	for _, childID := range g.children[nodeID] {
		fn(childID, depth)
		g.walkDescendants(childID, depth+1, fn)
	}
}

func (g *TopicGraph) walkLaterals(nodeID string, maxHops int, fn func(targetID string, hops int, weight float64)) {
	type visit struct {
		id     string
		hops   int
		weight float64
	}
	seen := map[string]bool{nodeID: true}
	queue := []visit{}
	for _, edge := range g.edges[nodeID] {
		queue = append(queue, visit{edge.TargetID, 1, edge.Weight})
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if seen[v.id] || v.hops > maxHops {
			continue
		}
		seen[v.id] = true
		fn(v.id, v.hops, v.weight)
		if v.hops < maxHops {
			for _, edge := range g.edges[v.id] {
				if !seen[edge.TargetID] {
					queue = append(queue, visit{edge.TargetID, v.hops + 1, v.weight * edge.Weight})
				}
			}
		}
	}
}

func (a *App) getTopicGraph() *TopicGraph {
	a.tgMu.RLock()
	defer a.tgMu.RUnlock()
	return a.topicGraph
}

func (a *App) loadTopicGraph() *TopicGraph {
	g := &TopicGraph{
		nodes:    make(map[string]*TopicNode),
		bySlug:   make(map[string]*TopicNode),
		byName:   make(map[string]*TopicNode),
		children: make(map[string][]string),
		edges:    make(map[string][]TopicEdge),
	}

	rows, err := a.db.Query("SELECT id, name, slug, path, parent_id, depth, clip_count FROM topics")
	if err != nil {
		log.Printf("topic graph load failed: %v", err)
		return g
	}
	defer rows.Close()

	for rows.Next() {
		var n TopicNode
		var parentID sql.NullString
		rows.Scan(&n.ID, &n.Name, &n.Slug, &n.Path, &parentID, &n.Depth, &n.ClipCount)
		if parentID.Valid {
			n.ParentID = parentID.String
		}
		g.nodes[n.ID] = &n
		g.bySlug[n.Slug] = &n
		g.byName[strings.ToLower(n.Name)] = &n
		if parentID.Valid {
			g.children[parentID.String] = append(g.children[parentID.String], n.ID)
		}
	}

	edgeRows, err := a.db.Query("SELECT source_id, target_id, relation, weight FROM topic_edges")
	if err != nil {
		log.Printf("topic edges load failed: %v", err)
		return g
	}
	defer edgeRows.Close()

	edgeCount := 0
	for edgeRows.Next() {
		var sourceID, targetID, relation string
		var weight float64
		edgeRows.Scan(&sourceID, &targetID, &relation, &weight)
		g.edges[sourceID] = append(g.edges[sourceID], TopicEdge{
			TargetID: targetID,
			Relation: relation,
			Weight:   weight,
		})
		edgeCount++
	}

	log.Printf("Topic graph loaded: %d nodes, %d edges", len(g.nodes), edgeCount)
	return g
}

func (a *App) refreshTopicGraph() {
	g := a.loadTopicGraph()
	a.tgMu.Lock()
	a.topicGraph = g
	a.tgMu.Unlock()
}

func (a *App) topicGraphRefreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		a.refreshTopicGraph()
	}
}

// applyTopicBoost re-ranks clips using graph-aware topic affinity, falling back to legacy string matching.
func (a *App) applyTopicBoost(ctx context.Context, clips []map[string]interface{}, userID string, topicWeights map[string]float64) {
	g := a.getTopicGraph()
	hasGraph := g != nil && len(g.nodes) > 0

	if len(topicWeights) == 0 && !hasGraph {
		return
	}

	userAffinities := make(map[string]float64)

	if hasGraph {
		for name, weight := range topicWeights {
			if node := g.resolveByName(name); node != nil {
				userAffinities[node.ID] = weight
			}
		}
		if userID != "" {
			rows, err := a.db.QueryContext(ctx,
				`SELECT topic_id, weight FROM user_topic_affinities WHERE user_id = ?`, userID)
			if err == nil {
				for rows.Next() {
					var tid string
					var w float64
					rows.Scan(&tid, &w)
					userAffinities[tid] = w
				}
				rows.Close()
			}
		}
	}

	clipTopicMap := make(map[string][]string)
	if hasGraph {
		var ids []string
		for _, c := range clips {
			if id, ok := c["id"].(string); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			ph := make([]string, len(ids))
			args := make([]interface{}, len(ids))
			for i, id := range ids {
				ph[i] = "?"
				args[i] = id
			}
			rows, err := a.db.QueryContext(ctx,
				`SELECT clip_id, topic_id FROM clip_topics WHERE clip_id IN (`+strings.Join(ph, ",")+`)`, args...)
			if err == nil {
				for rows.Next() {
					var cid, tid string
					rows.Scan(&cid, &tid)
					clipTopicMap[cid] = append(clipTopicMap[cid], tid)
				}
				rows.Close()
			}
		}
	}

	// Load user profile embedding for similarity blending
	var userEmb []float32
	if userID != "" {
		var blob []byte
		row := a.db.QueryRowContext(ctx, `SELECT text_embedding FROM user_embeddings WHERE user_id = ?`, userID)
		if row.Scan(&blob) == nil {
			userEmb = blobToFloat32(blob)
		}
	}

	// Load clip embeddings if user embedding exists
	clipEmbMap := make(map[string][]float32)
	if len(userEmb) > 0 {
		var ids []string
		for _, c := range clips {
			if id, ok := c["id"].(string); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			ph := make([]string, len(ids))
			args := make([]interface{}, len(ids))
			for i, id := range ids {
				ph[i] = "?"
				args[i] = id
			}
			embRows, err := a.db.QueryContext(ctx,
				`SELECT clip_id, text_embedding FROM clip_embeddings WHERE clip_id IN (`+strings.Join(ph, ",")+`)`, args...)
			if err == nil {
				for embRows.Next() {
					var cid string
					var blob []byte
					embRows.Scan(&cid, &blob)
					if v := blobToFloat32(blob); v != nil {
						clipEmbMap[cid] = v
					}
				}
				embRows.Close()
			}
		}
	}

	for i, clip := range clips {
		contentScore, _ := clip["content_score"].(float64)
		clipID, _ := clip["id"].(string)

		graphBoost := 1.0
		if graphTopics := clipTopicMap[clipID]; len(graphTopics) > 0 && hasGraph && len(userAffinities) > 0 {
			graphBoost = g.computeBoost(graphTopics, userAffinities)
		} else if len(topicWeights) > 0 {
			topics, _ := clip["topics"].([]string)
			graphBoost = computeTopicBoost(topics, topicWeights)
		}

		embSim := 0.0
		if clipEmb, ok := clipEmbMap[clipID]; ok && len(userEmb) > 0 {
			embSim = cosineSimilarity(userEmb, clipEmb)
			if embSim < 0 {
				embSim = 0
			}
		}

		var boost float64
		if embSim > 0 {
			boost = graphBoost*0.6 + embSim*0.4
		} else {
			boost = graphBoost
		}

		clips[i]["_score"] = contentScore * boost
	}

	sort.SliceStable(clips, func(i, j int) bool {
		si, _ := clips[i]["_score"].(float64)
		sj, _ := clips[j]["_score"].(float64)
		return si > sj
	})
	for _, clip := range clips {
		delete(clip, "_score")
	}
}

// --- Embeddings ---

func blobToFloat32(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4 : (i+1)*4]))
	}
	return out
}

func float32ToBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:(i+1)*4], math.Float32bits(f))
	}
	return b
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func (a *App) handleSimilarClips(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")
	limitStr := r.URL.Query().Get("limit")
	limit := 10
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 50 {
		limit = n
	}

	var refText, refVisual []byte
	err := a.db.QueryRowContext(r.Context(),
		`SELECT text_embedding, visual_embedding FROM clip_embeddings WHERE clip_id = ?`, clipID,
	).Scan(&refText, &refVisual)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no embeddings for this clip"})
		return
	}

	refTextVec := blobToFloat32(refText)
	refVisualVec := blobToFloat32(refVisual)
	if refTextVec == nil && refVisualVec == nil {
		writeJSON(w, 404, map[string]string{"error": "no embeddings for this clip"})
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT e.clip_id, e.text_embedding, e.visual_embedding,
		       c.title, c.thumbnail_key, c.duration_seconds, c.content_score
		FROM clip_embeddings e
		JOIN clips c ON e.clip_id = c.id AND c.status = 'ready'
		WHERE e.clip_id != ?
	`, clipID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	type scored struct {
		data  map[string]interface{}
		score float64
	}
	var results []scored

	for rows.Next() {
		var cid string
		var tBlob, vBlob []byte
		var title string
		var thumbKey string
		var dur, cs float64
		rows.Scan(&cid, &tBlob, &vBlob, &title, &thumbKey, &dur, &cs)

		textSim := 0.0
		visualSim := 0.0
		hasText := refTextVec != nil && len(tBlob) > 0
		hasVisual := refVisualVec != nil && len(vBlob) > 0

		if hasText {
			textSim = cosineSimilarity(refTextVec, blobToFloat32(tBlob))
		}
		if hasVisual {
			visualSim = cosineSimilarity(refVisualVec, blobToFloat32(vBlob))
		}

		var sim float64
		switch {
		case hasText && hasVisual:
			sim = textSim*0.6 + visualSim*0.4
		case hasText:
			sim = textSim
		case hasVisual:
			sim = visualSim
		}

		results = append(results, scored{
			data: map[string]interface{}{
				"id": cid, "title": title, "thumbnail_key": thumbKey,
				"duration_seconds": dur, "content_score": cs, "similarity": math.Round(sim*1000) / 1000,
			},
			score: sim,
		})
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > limit {
		results = results[:limit]
	}

	clips := make([]map[string]interface{}, len(results))
	for i, r := range results {
		clips[i] = r.data
	}
	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

// --- Learning-to-Rank ---

type LTRTree struct {
	FeatureIndex int       `json:"feature_index"`
	Threshold    float64   `json:"threshold"`
	LeftChild    int       `json:"left_child"`
	RightChild   int       `json:"right_child"`
	LeafValue    float64   `json:"leaf_value"`
	IsLeaf       bool      `json:"is_leaf"`
}

type LTRModel struct {
	Trees        [][]LTRTree `json:"trees"`
	FeatureNames []string    `json:"feature_names"`
	NumFeatures  int         `json:"num_features"`
}

func (m *LTRModel) Score(features []float64) float64 {
	if m == nil || len(m.Trees) == 0 {
		return 0.5
	}
	sum := 0.0
	for _, tree := range m.Trees {
		sum += m.scoreTree(tree, features)
	}
	return 1.0 / (1.0 + math.Exp(-sum))
}

func (m *LTRModel) scoreTree(nodes []LTRTree, features []float64) float64 {
	idx := 0
	for idx < len(nodes) {
		n := nodes[idx]
		if n.IsLeaf {
			return n.LeafValue
		}
		if n.FeatureIndex < len(features) && features[n.FeatureIndex] <= n.Threshold {
			idx = n.LeftChild
		} else {
			idx = n.RightChild
		}
	}
	return 0
}

func (a *App) getLTRModel() *LTRModel {
	a.ltrMu.RLock()
	defer a.ltrMu.RUnlock()
	return a.ltrModel
}

func (a *App) loadLTRModel() *LTRModel {
	modelPath := a.cfg.DBPath[:len(a.cfg.DBPath)-len("clipfeed.db")] + "l2r_model.json"
	f, err := os.Open(modelPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}

	var model LTRModel
	if err := json.Unmarshal(data, &model); err != nil {
		log.Printf("LTR model parse error: %v", err)
		return nil
	}
	log.Printf("LTR model loaded: %d trees, %d features", len(model.Trees), model.NumFeatures)
	return &model
}

func (a *App) ltrModelRefreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if m := a.loadLTRModel(); m != nil {
			a.ltrMu.Lock()
			a.ltrModel = m
			a.ltrMu.Unlock()
		}
	}
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
		       c.topics, c.content_score, s.platform, s.channel_name, s.url
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
		var platform, channelName, sourceURL *string
		rows.Scan(&id, &title, &duration, &thumbnailKey, &topicsJSON, &score, &platform, &channelName, &sourceURL)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		hits = append(hits, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "topics": topics,
			"content_score": score, "platform": platform, "channel_name": channelName,
			"source_url": sourceURL,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"hits": hits, "query": q, "total": len(hits)})
}

// --- Feed ---

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	limit := 20

	// Check for saved filter
	if filterID := r.URL.Query().Get("filter"); filterID != "" && userID != "" {
		var queryStr string
		err := a.db.QueryRowContext(r.Context(),
			`SELECT query FROM saved_filters WHERE id = ? AND user_id = ?`, filterID, userID,
		).Scan(&queryStr)
		if err == nil {
			var fq FilterQuery
			if json.Unmarshal([]byte(queryStr), &fq) == nil {
				clips, err := a.applyFilterToFeed(r.Context(), &fq, userID)
				if err == nil {
					writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips), "filter_id": filterID})
					return
				}
			}
		}
	}

	var rows *sql.Rows
	var err error
	var topicWeights map[string]float64

	if userID != "" {
		var topicWeightsJSON string
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT COALESCE(topic_weights, '{}') FROM user_preferences WHERE user_id = ?`,
			userID,
		).Scan(&topicWeightsJSON); err == nil {
			json.Unmarshal([]byte(topicWeightsJSON), &topicWeights)
		}

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
			       c.created_at, s.channel_name, s.platform, s.url
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
			       c.created_at, s.channel_name, s.platform, s.url
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
	a.applyTopicBoost(r.Context(), clips, userID, topicWeights)
	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

func computeTopicBoost(clipTopics []string, weights map[string]float64) float64 {
	if len(clipTopics) == 0 {
		return 1.0
	}
	sum := 0.0
	count := 0
	for _, t := range clipTopics {
		if w, ok := weights[t]; ok {
			sum += w
			count++
		}
	}
	if count == 0 {
		return 1.0
	}
	return sum / float64(count)
}

func (a *App) handleGetClip(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var id, title, description, thumbnailKey, topicsJSON, tagsJSON, status, createdAt string
	var duration, score float64
	var width, height, fileSize *int64
	var channelName, platform, sourceURL *string

	err := a.db.QueryRowContext(r.Context(), `
		SELECT c.id, c.title, c.description, c.duration_seconds,
		       c.thumbnail_key, c.topics, c.tags, c.content_score,
		       c.status, c.created_at, c.width, c.height, c.file_size_bytes,
		       s.channel_name, s.platform, s.url
		FROM clips c
		LEFT JOIN sources s ON c.source_id = s.id
		WHERE c.id = ?
	`, clipID).Scan(&id, &title, &description, &duration,
		&thumbnailKey, &topicsJSON, &tagsJSON, &score,
		&status, &createdAt, &width, &height, &fileSize,
		&channelName, &platform, &sourceURL)

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
		"width": width, "height": height, "file_size_bytes": fileSize,
		"channel_name": channelName, "platform": platform,
		"source_url": sourceURL,
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
	streamURL, err := buildBrowserStreamURL(presignedURL.String())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to build stream URL"})
		return
	}

	writeJSON(w, 200, map[string]string{"url": streamURL})
}

func buildBrowserStreamURL(presigned string) (string, error) {
	u, err := url.Parse(presigned)
	if err != nil || u.Path == "" {
		return "", fmt.Errorf("invalid presigned URL")
	}

	streamPath := "/storage" + u.EscapedPath()
	if u.RawQuery != "" {
		streamPath += "?" + u.RawQuery
	}
	return streamPath, nil
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
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT j.id, j.source_id, j.job_type, j.status, j.error,
		       j.attempts, j.max_attempts, j.started_at, j.completed_at, j.created_at,
		       s.url, s.platform, s.title, s.channel_name, s.thumbnail_url, s.external_id, s.metadata
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
		var sourceID, errMsg, startedAt, completedAt, url, platform, title, channelName, thumbnailURL, externalID, sourceMetadata *string
		var attempts, maxAttempts int
		rows.Scan(&id, &sourceID, &jobType, &status, &errMsg,
			&attempts, &maxAttempts, &startedAt, &completedAt, &createdAt,
			&url, &platform, &title, &channelName, &thumbnailURL, &externalID, &sourceMetadata)
		var parsedSourceMetadata interface{} = nil
		if sourceMetadata != nil && strings.TrimSpace(*sourceMetadata) != "" {
			if err := json.Unmarshal([]byte(*sourceMetadata), &parsedSourceMetadata); err != nil {
				// Keep the original string if metadata is malformed JSON.
				parsedSourceMetadata = *sourceMetadata
			}
		}
		job := map[string]interface{}{
			"id": id, "source_id": sourceID, "job_type": jobType,
			"status": status, "error": errMsg,
			"attempts": attempts, "max_attempts": maxAttempts,
			"started_at": startedAt, "completed_at": completedAt, "created_at": createdAt,
			"url": url, "platform": platform, "title": title,
			"channel_name": channelName, "thumbnail_url": thumbnailURL,
			"external_id": externalID, "source_metadata": parsedSourceMetadata,
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
	var explorationRate float64
	var topicWeightsJSON string
	var minClip, maxClip int
	var autoplay int

	err := a.db.QueryRowContext(r.Context(), `
		SELECT u.username, u.email, u.display_name, u.avatar_url, u.created_at,
		       COALESCE(p.exploration_rate, 0.3),
		       COALESCE(p.topic_weights, '{}'),
		       COALESCE(p.min_clip_seconds, 5),
		       COALESCE(p.max_clip_seconds, 120),
		       COALESCE(p.autoplay, 1)
		FROM users u
		LEFT JOIN user_preferences p ON u.id = p.user_id
		WHERE u.id = ?
	`, userID).Scan(&username, &email, &displayName, &avatarURL, &createdAt,
		&explorationRate, &topicWeightsJSON, &minClip, &maxClip, &autoplay)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "user not found"})
		return
	}

	var topicWeights map[string]interface{}
	json.Unmarshal([]byte(topicWeightsJSON), &topicWeights)
	if topicWeights == nil {
		topicWeights = make(map[string]interface{})
	}

	writeJSON(w, 200, map[string]interface{}{
		"id": userID, "username": username, "email": email,
		"display_name": displayName, "avatar_url": avatarURL,
		"created_at": createdAt,
		"preferences": map[string]interface{}{
			"exploration_rate": explorationRate,
			"topic_weights":    topicWeights,
			"min_clip_seconds": minClip,
			"max_clip_seconds": maxClip,
			"autoplay":         autoplay == 1,
		},
	})
}

func (a *App) handleGetTopics(w http.ResponseWriter, r *http.Request) {
	// Use topics table when populated; otherwise fall back to legacy JSON scan
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, name, slug, path, parent_id, depth, clip_count
		FROM topics ORDER BY clip_count DESC LIMIT 50
	`)
	if err == nil {
		defer rows.Close()
		var topics []map[string]interface{}
		for rows.Next() {
			var id, name, slug, path string
			var parentID sql.NullString
			var depth, clipCount int
			rows.Scan(&id, &name, &slug, &path, &parentID, &depth, &clipCount)
			t := map[string]interface{}{
				"id": id, "name": name, "slug": slug,
				"path": path, "depth": depth, "clip_count": clipCount,
			}
			if parentID.Valid {
				t["parent_id"] = parentID.String
			}
			topics = append(topics, t)
		}
		if len(topics) > 0 {
			writeJSON(w, 200, map[string]interface{}{"topics": topics})
			return
		}
	}

	a.handleGetTopicsLegacy(w, r)
}

func (a *App) handleGetTopicsLegacy(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT topics FROM clips WHERE status = 'ready' AND topics != '[]'
	`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to fetch topics"})
		return
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var topicsJSON string
		rows.Scan(&topicsJSON)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		for _, t := range topics {
			if t != "" {
				counts[t]++
			}
		}
	}

	type topicEntry struct {
		Name      string `json:"name"`
		ClipCount int    `json:"clip_count"`
	}
	var result []topicEntry
	for name, count := range counts {
		result = append(result, topicEntry{Name: name, ClipCount: count})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ClipCount > result[j].ClipCount
	})
	if len(result) > 20 {
		result = result[:20]
	}

	writeJSON(w, 200, map[string]interface{}{"topics": result})
}

func (a *App) handleGetTopicTree(w http.ResponseWriter, r *http.Request) {
	g := a.getTopicGraph()
	if g == nil || len(g.nodes) == 0 {
		writeJSON(w, 200, map[string]interface{}{"tree": []interface{}{}})
		return
	}

	type treeNode struct {
		ID        string      `json:"id"`
		Name      string      `json:"name"`
		Slug      string      `json:"slug"`
		ClipCount int         `json:"clip_count"`
		Children  []*treeNode `json:"children,omitempty"`
	}

	nodeMap := make(map[string]*treeNode)
	for id, n := range g.nodes {
		nodeMap[id] = &treeNode{
			ID: n.ID, Name: n.Name, Slug: n.Slug, ClipCount: n.ClipCount,
		}
	}

	var roots []*treeNode
	for id, n := range g.nodes {
		tn := nodeMap[id]
		if n.ParentID == "" {
			roots = append(roots, tn)
		} else if parent, ok := nodeMap[n.ParentID]; ok {
			parent.Children = append(parent.Children, tn)
		} else {
			roots = append(roots, tn)
		}
	}

	sort.Slice(roots, func(i, j int) bool {
		return roots[i].ClipCount > roots[j].ClipCount
	})

	writeJSON(w, 200, map[string]interface{}{"tree": roots})
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
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
		       c.topics, c.created_at, s.platform, s.channel_name, s.url
		FROM saved_clips sc
		JOIN clips c ON sc.clip_id = c.id
		LEFT JOIN sources s ON c.source_id = s.id
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
		var platform, channelName, sourceURL *string
		rows.Scan(&id, &title, &duration, &thumbnailKey, &topicsJSON, &createdAt,
			&platform, &channelName, &sourceURL)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "topics": topics, "created_at": createdAt,
			"platform": platform, "channel_name": channelName, "source_url": sourceURL,
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

// --- Clip Summary (LLM) ---

func (a *App) handleClipSummary(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var summary, model string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT summary, model FROM clip_summaries WHERE clip_id = ?`, clipID,
	).Scan(&summary, &model)
	if err == nil {
		writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": summary, "model": model, "cached": true})
		return
	}

	var transcript string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(transcript, '') FROM clips WHERE id = ?`, clipID,
	).Scan(&transcript)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}
	if transcript == "" {
		writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": "", "model": "", "cached": false})
		return
	}

	ollamaURL := getEnv("OLLAMA_URL", "http://ollama:11434")
	prompt := fmt.Sprintf("Summarize this video transcript in 1-2 sentences:\n\n%s", transcript)
	if len(prompt) > 4000 {
		prompt = prompt[:4000]
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"model": getEnv("OLLAMA_MODEL", "llama3.2:3b"), "prompt": prompt, "stream": false,
	})

	resp, err := http.Post(ollamaURL+"/api/generate", "application/json",
		strings.NewReader(string(reqBody)))
	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": "", "error": "LLM unavailable"})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Response string `json:"response"`
	}
	json.Unmarshal(body, &result)

	if result.Response != "" {
		modelName := getEnv("OLLAMA_MODEL", "llama3.2:3b")
		a.db.ExecContext(r.Context(),
			`INSERT OR REPLACE INTO clip_summaries (clip_id, summary, model) VALUES (?, ?, ?)`,
			clipID, result.Response, modelName)
	}

	writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": result.Response, "cached": false})
}

// --- Saved Filters ---

type FilterQuery struct {
	Topics       *FilterTopics  `json:"topics,omitempty"`
	Channels     []string       `json:"channels,omitempty"`
	Duration     *FilterRange   `json:"duration,omitempty"`
	RecencyDays  int            `json:"recency_days,omitempty"`
	MinScore     float64        `json:"min_score,omitempty"`
	SimilarToClip string        `json:"similar_to_clip,omitempty"`
}

type FilterTopics struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
	Mode    string   `json:"mode"`
}

type FilterRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

func (a *App) handleCreateFilter(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	var req struct {
		Name      string          `json:"name"`
		Query     json.RawMessage `json:"query"`
		IsDefault bool            `json:"is_default"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "name and query required"})
		return
	}

	var fq FilterQuery
	if err := json.Unmarshal(req.Query, &fq); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid filter query"})
		return
	}

	id := uuid.New().String()
	isDefault := 0
	if req.IsDefault {
		isDefault = 1
	}

	_, err := a.db.ExecContext(r.Context(),
		`INSERT INTO saved_filters (id, user_id, name, query, is_default) VALUES (?, ?, ?, ?, ?)`,
		id, userID, req.Name, string(req.Query), isDefault)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to create filter"})
		return
	}
	writeJSON(w, 201, map[string]interface{}{"id": id, "name": req.Name})
}

func (a *App) handleListFilters(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, name, query, is_default, created_at FROM saved_filters WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list filters"})
		return
	}
	defer rows.Close()

	var filters []map[string]interface{}
	for rows.Next() {
		var id, name, queryStr, createdAt string
		var isDefault int
		rows.Scan(&id, &name, &queryStr, &isDefault, &createdAt)
		filters = append(filters, map[string]interface{}{
			"id": id, "name": name, "query": json.RawMessage(queryStr),
			"is_default": isDefault == 1, "created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"filters": filters})
}

func (a *App) handleUpdateFilter(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	filterID := chi.URLParam(r, "id")
	var req struct {
		Name      string          `json:"name"`
		Query     json.RawMessage `json:"query"`
		IsDefault *bool           `json:"is_default"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Name != "" {
		a.db.ExecContext(r.Context(), `UPDATE saved_filters SET name = ? WHERE id = ? AND user_id = ?`, req.Name, filterID, userID)
	}
	if req.Query != nil {
		a.db.ExecContext(r.Context(), `UPDATE saved_filters SET query = ? WHERE id = ? AND user_id = ?`, string(req.Query), filterID, userID)
	}
	if req.IsDefault != nil {
		def := 0
		if *req.IsDefault {
			def = 1
		}
		a.db.ExecContext(r.Context(), `UPDATE saved_filters SET is_default = ? WHERE id = ? AND user_id = ?`, def, filterID, userID)
	}
	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (a *App) handleDeleteFilter(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	filterID := chi.URLParam(r, "id")
	a.db.ExecContext(r.Context(), `DELETE FROM saved_filters WHERE id = ? AND user_id = ?`, filterID, userID)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (a *App) expandTopicDescendants(topicNames []string) []string {
	g := a.getTopicGraph()
	if g == nil {
		return nil
	}
	seen := make(map[string]bool)
	var ids []string
	for _, name := range topicNames {
		node := g.resolveByName(name)
		if node == nil {
			continue
		}
		var walk func(id string)
		walk = func(id string) {
			if seen[id] {
				return
			}
			seen[id] = true
			ids = append(ids, id)
			for _, child := range g.children[id] {
				walk(child)
			}
		}
		walk(node.ID)
	}
	return ids
}

func (a *App) applyFilterToFeed(ctx context.Context, fq *FilterQuery, userID string) ([]map[string]interface{}, error) {
	where := []string{"c.status = 'ready'"}
	var args []interface{}

	if fq.Duration != nil {
		if fq.Duration.Min > 0 {
			where = append(where, "c.duration_seconds >= ?")
			args = append(args, fq.Duration.Min)
		}
		if fq.Duration.Max > 0 {
			where = append(where, "c.duration_seconds <= ?")
			args = append(args, fq.Duration.Max)
		}
	}
	if fq.RecencyDays > 0 {
		where = append(where, fmt.Sprintf("c.created_at > datetime('now', '-%d days')", fq.RecencyDays))
	}
	if fq.MinScore > 0 {
		where = append(where, "c.content_score >= ?")
		args = append(args, fq.MinScore)
	}
	if len(fq.Channels) > 0 {
		ph := make([]string, len(fq.Channels))
		for i, ch := range fq.Channels {
			ph[i] = "?"
			args = append(args, ch)
		}
		where = append(where, "s.channel_name IN ("+strings.Join(ph, ",")+")")
	}

	// Topic inclusion via graph descendants
	if fq.Topics != nil && len(fq.Topics.Include) > 0 {
		var topicIDs []string
		if fq.Topics.Mode == "descendants" {
			topicIDs = a.expandTopicDescendants(fq.Topics.Include)
		} else {
			g := a.getTopicGraph()
			if g != nil {
				for _, name := range fq.Topics.Include {
					if n := g.resolveByName(name); n != nil {
						topicIDs = append(topicIDs, n.ID)
					}
				}
			}
		}
		if len(topicIDs) > 0 {
			ph := make([]string, len(topicIDs))
			for i, id := range topicIDs {
				ph[i] = "?"
				args = append(args, id)
			}
			where = append(where, "c.id IN (SELECT clip_id FROM clip_topics WHERE topic_id IN ("+strings.Join(ph, ",")+"))")
		}
	}

	// Topic exclusion
	if fq.Topics != nil && len(fq.Topics.Exclude) > 0 {
		var excludeIDs []string
		if fq.Topics.Mode == "descendants" {
			excludeIDs = a.expandTopicDescendants(fq.Topics.Exclude)
		} else {
			g := a.getTopicGraph()
			if g != nil {
				for _, name := range fq.Topics.Exclude {
					if n := g.resolveByName(name); n != nil {
						excludeIDs = append(excludeIDs, n.ID)
					}
				}
			}
		}
		if len(excludeIDs) > 0 {
			ph := make([]string, len(excludeIDs))
			for i, id := range excludeIDs {
				ph[i] = "?"
				args = append(args, id)
			}
			where = append(where, "c.id NOT IN (SELECT clip_id FROM clip_topics WHERE topic_id IN ("+strings.Join(ph, ",")+"))")
		}
	}

	// Exclude seen
	if userID != "" {
		where = append(where, "c.id NOT IN (SELECT clip_id FROM interactions WHERE user_id = ? AND created_at > datetime('now', '-24 hours'))")
		args = append(args, userID)
	}

	query := `SELECT c.id, c.title, c.description, c.duration_seconds,
	       c.thumbnail_key, c.topics, c.tags, c.content_score,
	       c.created_at, s.channel_name, s.platform, s.url
	FROM clips c LEFT JOIN sources s ON c.source_id = s.id
	WHERE ` + strings.Join(where, " AND ") + `
	ORDER BY c.content_score DESC LIMIT 20`

	args = append(args)
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanClips(rows), nil
}

// --- Content Scout ---

func (a *App) handleCreateScoutSource(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	var req struct {
		SourceType string `json:"source_type"`
		Platform   string `json:"platform"`
		Identifier string `json:"identifier"`
		Interval   int    `json:"check_interval_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}
	if req.SourceType == "" || req.Platform == "" || req.Identifier == "" {
		writeJSON(w, 400, map[string]string{"error": "source_type, platform, identifier required"})
		return
	}
	interval := req.Interval
	if interval <= 0 {
		interval = 24
	}

	id := uuid.New().String()
	_, err := a.db.ExecContext(r.Context(),
		`INSERT INTO scout_sources (id, user_id, source_type, platform, identifier, check_interval_hours)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, req.SourceType, req.Platform, req.Identifier, interval)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeJSON(w, 409, map[string]string{"error": "source already exists"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "failed to create scout source"})
		return
	}
	writeJSON(w, 201, map[string]interface{}{"id": id})
}

func (a *App) handleListScoutSources(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, source_type, platform, identifier, is_active, last_checked, check_interval_hours, created_at
		 FROM scout_sources WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	var sources []map[string]interface{}
	for rows.Next() {
		var id, srcType, platform, identifier, createdAt string
		var isActive, interval int
		var lastChecked *string
		rows.Scan(&id, &srcType, &platform, &identifier, &isActive, &lastChecked, &interval, &createdAt)
		sources = append(sources, map[string]interface{}{
			"id": id, "source_type": srcType, "platform": platform,
			"identifier": identifier, "is_active": isActive == 1,
			"last_checked": lastChecked, "check_interval_hours": interval,
			"created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"sources": sources})
}

func (a *App) handleListScoutCandidates(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "pending"
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT sc.id, sc.url, sc.platform, sc.external_id, sc.title,
		       sc.channel_name, sc.duration_seconds, sc.llm_score, sc.status, sc.created_at
		FROM scout_candidates sc
		JOIN scout_sources ss ON sc.scout_source_id = ss.id
		WHERE ss.user_id = ? AND sc.status = ?
		ORDER BY sc.created_at DESC LIMIT 50
	`, userID, statusFilter)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	var candidates []map[string]interface{}
	for rows.Next() {
		var id, urlStr, platform, extID, status, createdAt string
		var title, channelName *string
		var duration, llmScore *float64
		rows.Scan(&id, &urlStr, &platform, &extID, &title, &channelName, &duration, &llmScore, &status, &createdAt)
		candidates = append(candidates, map[string]interface{}{
			"id": id, "url": urlStr, "platform": platform, "external_id": extID,
			"title": title, "channel_name": channelName,
			"duration_seconds": duration, "llm_score": llmScore,
			"status": status, "created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"candidates": candidates})
}

func (a *App) handleApproveCandidate(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	candidateID := chi.URLParam(r, "id")

	var urlStr, platform string
	err := a.db.QueryRowContext(r.Context(), `
		SELECT sc.url, sc.platform FROM scout_candidates sc
		JOIN scout_sources ss ON sc.scout_source_id = ss.id
		WHERE sc.id = ? AND ss.user_id = ? AND sc.status = 'pending'
	`, candidateID, userID).Scan(&urlStr, &platform)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "candidate not found or already processed"})
		return
	}

	sourceID := uuid.New().String()
	jobID := uuid.New().String()
	payload := fmt.Sprintf(`{"url":%q,"source_id":%q,"platform":%q}`, urlStr, sourceID, platform)

	conn, err := a.db.Conn(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "db error"})
		return
	}
	defer conn.Close()

	conn.ExecContext(r.Context(), "BEGIN IMMEDIATE")
	conn.ExecContext(r.Context(),
		`INSERT INTO sources (id, url, platform, submitted_by, status) VALUES (?, ?, ?, ?, 'pending')`,
		sourceID, urlStr, platform, userID)
	conn.ExecContext(r.Context(),
		`INSERT INTO jobs (id, source_id, job_type, payload) VALUES (?, ?, 'download', ?)`,
		jobID, sourceID, payload)
	conn.ExecContext(r.Context(),
		`UPDATE scout_candidates SET status = 'ingested' WHERE id = ?`, candidateID)
	conn.ExecContext(r.Context(), "COMMIT")

	writeJSON(w, 200, map[string]interface{}{
		"status": "approved", "source_id": sourceID, "job_id": jobID,
	})
}

// --- Helpers ---

func scanClips(rows *sql.Rows) []map[string]interface{} {
	var clips []map[string]interface{}
	for rows.Next() {
		var id, title, description, thumbnailKey, topicsJSON, tagsJSON, createdAt string
		var duration, score float64
		var channelName, platform, sourceURL *string

		rows.Scan(&id, &title, &description, &duration,
			&thumbnailKey, &topicsJSON, &tagsJSON, &score,
			&createdAt, &channelName, &platform, &sourceURL)

		var topics, tags []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		json.Unmarshal([]byte(tagsJSON), &tags)

		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "description": description,
			"duration_seconds": duration, "thumbnail_key": thumbnailKey,
			"topics": topics, "tags": tags, "content_score": score,
			"created_at": createdAt, "channel_name": channelName,
			"platform": platform, "source_url": sourceURL,
		})
	}
	return clips
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
