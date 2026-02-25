package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// workerAuthMiddleware validates requests from the ingestion worker using a
// shared secret (WORKER_SECRET). This keeps internal endpoints private.
func (a *App) workerAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.WorkerSecret == "" {
			writeJSON(w, 503, map[string]string{"error": "worker API not configured (WORKER_SECRET not set)"})
			return
		}
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth || subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.WorkerSecret)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Job claim ---

// POST /api/internal/jobs/claim
// Atomically claims the next queued job for the worker.
func (a *App) handleWorkerClaimJob(w http.ResponseWriter, r *http.Request) {
	nowExpr := a.db.NowUTC()

	var id, payload string
	var err error

	if a.db.IsPostgres() {
		err = a.db.QueryRowContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs
			SET status = 'running',
			    started_at = %s,
			    attempts = attempts + 1
			WHERE id = (
				SELECT id FROM jobs
				WHERE status = 'queued'
				  AND (run_after IS NULL OR run_after <= %s)
				ORDER BY priority DESC, created_at ASC
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, payload
		`, nowExpr, nowExpr)).Scan(&id, &payload)
	} else {
		err = a.db.QueryRowContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs
			SET status = 'running',
			    started_at = %s,
			    attempts = attempts + 1
			WHERE id = (
				SELECT id FROM jobs
				WHERE status = 'queued'
				  AND (run_after IS NULL OR run_after <= %s)
				ORDER BY priority DESC, created_at ASC
				LIMIT 1
			)
			RETURNING id, payload
		`, nowExpr, nowExpr)).Scan(&id, &payload)
	}

	if err != nil {
		// No job available
		writeJSON(w, 204, nil)
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"id":      id,
		"payload": json.RawMessage(payload),
	})
}

// --- Job update ---

// PUT /api/internal/jobs/{id}
// Updates a job's status, error, and result.
func (a *App) handleWorkerUpdateJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	nowExpr := a.db.NowUTC()

	var req struct {
		Status   string           `json:"status"`
		Error    *string          `json:"error,omitempty"`
		Result   *json.RawMessage `json:"result,omitempty"`
		RunAfter *string          `json:"run_after,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	switch req.Status {
	case "complete", "failed", "rejected":
		resultStr := "{}"
		if req.Result != nil {
			resultStr = string(*req.Result)
		}
		errStr := ""
		if req.Error != nil {
			errStr = *req.Error
		}
		_, err := a.db.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = ?, error = ?, result = ?, completed_at = %s WHERE id = ?
		`, nowExpr), req.Status, errStr, resultStr, jobID)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "failed to update job"})
			return
		}

	case "queued":
		// Re-queue with optional run_after (for retry backoff)
		runAfter := ""
		if req.RunAfter != nil {
			runAfter = *req.RunAfter
		}
		errStr := ""
		if req.Error != nil {
			errStr = *req.Error
		}
		_, err := a.db.ExecContext(r.Context(),
			`UPDATE jobs SET status = 'queued', error = ?, run_after = ? WHERE id = ?`,
			errStr, runAfter, jobID)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "failed to re-queue job"})
			return
		}

	default:
		writeJSON(w, 400, map[string]string{"error": "invalid status"})
		return
	}

	writeJSON(w, 200, map[string]string{"status": "updated"})
}

// --- Job info (attempts) ---

// GET /api/internal/jobs/{id}
func (a *App) handleWorkerGetJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	var attempts, maxAttempts int
	var status string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT status, attempts, max_attempts FROM jobs WHERE id = ?`, jobID,
	).Scan(&status, &attempts, &maxAttempts)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": jobID, "status": status, "attempts": attempts, "max_attempts": maxAttempts,
	})
}

// --- Stale job reclamation ---

// POST /api/internal/jobs/reclaim
func (a *App) handleWorkerReclaimStale(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StaleMinutes int `json:"stale_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StaleMinutes <= 0 {
		req.StaleMinutes = 120
	}

	nowExpr := a.db.NowUTC()
	staleMsg := fmt.Sprintf("stale watchdog: recovered running job older than %dm", req.StaleMinutes)

	var requeuedCount, failedCount int

	if a.db.IsPostgres() {
		cutoffExpr := fmt.Sprintf("now() - interval '%d minutes'", req.StaleMinutes)
		// Re-queue retryable
		res, _ := a.db.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs
			SET status = 'queued', run_after = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN $1 ELSE error || ' | ' || $1 END
			WHERE status = 'running'
			  AND started_at IS NOT NULL
			  AND started_at::timestamptz <= %s
			  AND attempts < max_attempts
		`, nowExpr, cutoffExpr), staleMsg)
		if res != nil {
			n, _ := res.RowsAffected()
			requeuedCount = int(n)
		}
		// Fail exhausted
		res, _ = a.db.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs
			SET status = 'failed', completed_at = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN $1 ELSE error || ' | ' || $1 END
			WHERE status = 'running'
			  AND started_at IS NOT NULL
			  AND started_at::timestamptz <= %s
			  AND attempts >= max_attempts
		`, nowExpr, cutoffExpr), staleMsg)
		if res != nil {
			n, _ := res.RowsAffected()
			failedCount = int(n)
		}
	} else {
		cutoff := fmt.Sprintf("-%d minutes", req.StaleMinutes)
		res, _ := a.db.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs
			SET status = 'queued', run_after = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN ? ELSE error || ' | ' || ? END
			WHERE status = 'running'
			  AND started_at IS NOT NULL
			  AND datetime(started_at) <= datetime('now', ?)
			  AND attempts < max_attempts
		`, nowExpr), staleMsg, staleMsg, cutoff)
		if res != nil {
			n, _ := res.RowsAffected()
			requeuedCount = int(n)
		}
		res, _ = a.db.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs
			SET status = 'failed', completed_at = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN ? ELSE error || ' | ' || ? END
			WHERE status = 'running'
			  AND started_at IS NOT NULL
			  AND datetime(started_at) <= datetime('now', ?)
			  AND attempts >= max_attempts
		`, nowExpr), staleMsg, staleMsg, cutoff)
		if res != nil {
			n, _ := res.RowsAffected()
			failedCount = int(n)
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"requeued": requeuedCount, "failed": failedCount,
	})
}

// --- Source update ---

// PUT /api/internal/sources/{id}
func (a *App) handleWorkerUpdateSource(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")

	var req struct {
		Status          *string `json:"status,omitempty"`
		ExternalID      *string `json:"external_id,omitempty"`
		Title           *string `json:"title,omitempty"`
		ChannelName     *string `json:"channel_name,omitempty"`
		ThumbnailURL    *string `json:"thumbnail_url,omitempty"`
		DurationSeconds *float64 `json:"duration_seconds,omitempty"`
		Metadata        *string `json:"metadata,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	// Build SET clause dynamically
	var sets []string
	var args []interface{}
	addSet := func(col string, val interface{}) {
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}

	if req.Status != nil {
		addSet("status", *req.Status)
	}
	if req.ExternalID != nil {
		addSet("external_id", *req.ExternalID)
	}
	if req.Title != nil {
		addSet("title", *req.Title)
	}
	if req.ChannelName != nil {
		addSet("channel_name", *req.ChannelName)
	}
	if req.ThumbnailURL != nil {
		addSet("thumbnail_url", *req.ThumbnailURL)
	}
	if req.DurationSeconds != nil {
		addSet("duration_seconds", *req.DurationSeconds)
	}
	if req.Metadata != nil {
		addSet("metadata", *req.Metadata)
	}

	if len(sets) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no fields to update"})
		return
	}

	args = append(args, sourceID)
	query := "UPDATE sources SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	if _, err := a.db.ExecContext(r.Context(), query, args...); err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to update source"})
		return
	}

	writeJSON(w, 200, map[string]string{"status": "updated"})
}

// --- Platform cookie ---

// GET /api/internal/sources/{id}/cookie?platform=youtube
func (a *App) handleWorkerGetCookie(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")
	platform := r.URL.Query().Get("platform")

	var encrypted string
	err := a.db.QueryRowContext(r.Context(), `
		SELECT pc.cookie_str FROM platform_cookies pc
		JOIN sources s ON pc.user_id = s.submitted_by
		WHERE s.id = ? AND pc.platform = ? AND pc.is_active = 1
	`, sourceID, platform).Scan(&encrypted)
	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"cookie": nil})
		return
	}

	decrypted, err := decryptCookie(encrypted, a.cfg.JWTSecret)
	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"cookie": nil})
		return
	}

	writeJSON(w, 200, map[string]interface{}{"cookie": decrypted})
}

// --- Clip creation ---

// POST /api/internal/clips
// Creates a clip with associated topics, embeddings, and FTS in one transaction.
func (a *App) handleWorkerCreateClip(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID              string   `json:"id"`
		SourceID        string   `json:"source_id"`
		Title           string   `json:"title"`
		DurationSeconds float64  `json:"duration_seconds"`
		StartTime       float64  `json:"start_time"`
		EndTime         float64  `json:"end_time"`
		StorageKey      string   `json:"storage_key"`
		ThumbnailKey    string   `json:"thumbnail_key"`
		Width           int      `json:"width"`
		Height          int      `json:"height"`
		FileSizeBytes   int64    `json:"file_size_bytes"`
		Transcript      string   `json:"transcript"`
		Topics          []string `json:"topics"`
		ContentScore    float64  `json:"content_score"`
		ExpiresAt       string   `json:"expires_at"`
		Platform        string   `json:"platform"`
		ChannelName     string   `json:"channel_name"`
		// Embeddings as base64-encoded raw bytes
		TextEmbedding   string `json:"text_embedding,omitempty"`
		VisualEmbedding string `json:"visual_embedding,omitempty"`
		ModelVersion    string `json:"model_version,omitempty"`
	}
	// Allow larger body for clip creation (transcript + embeddings)
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10 MB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	if err := withTx(r.Context(), a.db, func(conn *CompatConn) error {
		topicsJSON, _ := json.Marshal(req.Topics)

		// Insert clip
		if _, err := conn.ExecContext(r.Context(), `
			INSERT INTO clips (
				id, source_id, title, duration_seconds, start_time, end_time,
				storage_key, thumbnail_key, width, height, file_size_bytes,
				transcript, topics, content_score, expires_at, status
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')
		`, req.ID, req.SourceID, req.Title, req.DurationSeconds, req.StartTime, req.EndTime,
			req.StorageKey, req.ThumbnailKey, req.Width, req.Height, req.FileSizeBytes,
			req.Transcript, string(topicsJSON), req.ContentScore, req.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert clip: %w", err)
		}

		// Link topics
		for _, topicName := range req.Topics {
			topicID := a.resolveOrCreateTopicTx(r.Context(), conn, topicName)
			if topicID != "" {
				conn.ExecContext(r.Context(),
					`INSERT INTO clip_topics (clip_id, topic_id, confidence, source) VALUES (?, ?, 1.0, 'keybert') ON CONFLICT DO NOTHING`,
					req.ID, topicID)
			}
		}

		// Index FTS
		if a.db.IsPostgres() {
			conn.ExecContext(r.Context(),
				`INSERT INTO clips_fts(clip_id, title, transcript, platform, channel_name) VALUES (?, ?, ?, ?, ?)`,
				req.ID, req.Title, truncate(req.Transcript, 2000), req.Platform, req.ChannelName)
		} else {
			conn.ExecContext(r.Context(),
				`INSERT INTO clips_fts(clip_id, title, transcript, platform, channel_name) VALUES (?, ?, ?, ?, ?)`,
				req.ID, req.Title, truncate(req.Transcript, 2000), req.Platform, req.ChannelName)
		}

		// Embeddings
		if req.TextEmbedding != "" || req.VisualEmbedding != "" {
			var textEmb, visEmb []byte
			if req.TextEmbedding != "" {
				textEmb, _ = base64.StdEncoding.DecodeString(req.TextEmbedding)
			}
			if req.VisualEmbedding != "" {
				visEmb, _ = base64.StdEncoding.DecodeString(req.VisualEmbedding)
			}
			conn.ExecContext(r.Context(),
				`INSERT INTO clip_embeddings (clip_id, text_embedding, visual_embedding, model_version) VALUES (?, ?, ?, ?)
				 ON CONFLICT(clip_id) DO UPDATE SET text_embedding = EXCLUDED.text_embedding, visual_embedding = EXCLUDED.visual_embedding, model_version = EXCLUDED.model_version`,
				req.ID, textEmb, visEmb, req.ModelVersion)
		}

		return nil
	}); err != nil {
		log.Printf("worker create clip failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "failed to create clip"})
		return
	}

	writeJSON(w, 201, map[string]interface{}{"id": req.ID})
}

// resolveOrCreateTopicTx finds or creates a topic within a transaction.
func (a *App) resolveOrCreateTopicTx(ctx context.Context, conn *CompatConn, name string) string {
	slug := slugify(name)
	var id string
	err := conn.QueryRowContext(ctx,
		"SELECT id FROM topics WHERE slug = ? OR LOWER(name) = LOWER(?)", slug, name,
	).Scan(&id)
	if err == nil {
		return id
	}

	id = uuid.New().String()
	conn.ExecContext(ctx,
		"INSERT INTO topics (id, name, slug, path, depth) VALUES (?, ?, ?, ?, 0) ON CONFLICT DO NOTHING",
		id, name, slug, slug)

	// Re-read in case another process created it concurrently
	conn.QueryRowContext(ctx, "SELECT id FROM topics WHERE slug = ?", slug).Scan(&id)
	return id
}

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ' ' || r == '-' {
			return r
		}
		return -1
	}, s)
	parts := strings.Fields(s)
	return strings.Join(parts, "-")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// --- Topic resolution ---

// POST /api/internal/topics/resolve
func (a *App) handleWorkerResolveTopic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "name required"})
		return
	}

	slug := slugify(req.Name)
	var id string
	err := a.db.QueryRowContext(r.Context(),
		"SELECT id FROM topics WHERE slug = ? OR LOWER(name) = LOWER(?)", slug, req.Name,
	).Scan(&id)
	if err == nil {
		writeJSON(w, 200, map[string]interface{}{"id": id, "created": false})
		return
	}

	id = uuid.New().String()
	a.db.ExecContext(r.Context(),
		"INSERT INTO topics (id, name, slug, path, depth) VALUES (?, ?, ?, ?, 0) ON CONFLICT DO NOTHING",
		id, req.Name, slug, slug)
	// Re-read
	a.db.QueryRowContext(r.Context(), "SELECT id FROM topics WHERE slug = ?", slug).Scan(&id)

	writeJSON(w, 201, map[string]interface{}{"id": id, "created": true})
}

// --- Score update (for score-updater HTTP mode) ---

// POST /api/internal/scores/update
func (a *App) handleWorkerScoreUpdate(w http.ResponseWriter, r *http.Request) {
	nowExpr := a.db.NowUTC()
	_ = nowExpr

	// Update content scores from interaction signals
	var count int64
	if a.db.IsPostgres() {
		res, err := a.db.ExecContext(r.Context(), `
			UPDATE clips
			SET content_score = sub.new_score
			FROM (
				SELECT clip_id,
					MAX(0.0, LEAST(1.0,
						COALESCE(AVG(CASE WHEN action='view' THEN watch_percentage END), 0.5) * 0.35
						+ COALESCE(CAST(SUM(CASE WHEN action='like' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.25
						+ COALESCE(CAST(SUM(CASE WHEN action='save' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.20
						+ COALESCE(CAST(SUM(CASE WHEN action='watch_full' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.15
						- COALESCE(CAST(SUM(CASE WHEN action='skip' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.30
						- COALESCE(CAST(SUM(CASE WHEN action='dislike' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.15
					)) AS new_score
				FROM interactions
				GROUP BY clip_id
				HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5
			) sub
			WHERE clips.id = sub.clip_id AND clips.status = 'ready'
		`)
		if err == nil {
			count, _ = res.RowsAffected()
		}
	} else {
		res, err := a.db.ExecContext(r.Context(), `
			UPDATE clips
			SET content_score = (
				SELECT MAX(0.0, MIN(1.0,
					COALESCE(AVG(CASE WHEN action='view' THEN watch_percentage END), 0.5) * 0.35
					+ COALESCE(CAST(SUM(CASE WHEN action='like' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.25
					+ COALESCE(CAST(SUM(CASE WHEN action='save' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.20
					+ COALESCE(CAST(SUM(CASE WHEN action='watch_full' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.15
					- COALESCE(CAST(SUM(CASE WHEN action='skip' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.30
					- COALESCE(CAST(SUM(CASE WHEN action='dislike' THEN 1.0 ELSE 0 END) AS REAL) / NULLIF(SUM(CASE WHEN action='view' THEN 1 ELSE 0 END), 0), 0) * 0.15
				))
				FROM interactions WHERE interactions.clip_id = clips.id
				HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5
			)
			WHERE status = 'ready'
			  AND id IN (SELECT clip_id FROM interactions GROUP BY clip_id HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5)
		`)
		if err == nil {
			count, _ = res.RowsAffected()
		}
	}

	writeJSON(w, 200, map[string]interface{}{"updated": count})
}
