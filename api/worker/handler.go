package worker

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"clipfeed/crypto"
	"clipfeed/db"
	"clipfeed/httputil"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler holds dependencies for the internal worker API.
type Handler struct {
	DB           *db.CompatDB
	WorkerSecret string
	CookieSecret string
}

// WorkerAuthMiddleware validates requests from the ingestion worker.
func (h *Handler) WorkerAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.WorkerSecret == "" {
			httputil.WriteJSON(w, 503, map[string]string{"error": "worker API not configured (WORKER_SECRET not set)"})
			return
		}
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == authHeader || subtle.ConstantTimeCompare([]byte(token), []byte(h.WorkerSecret)) != 1 {
			httputil.WriteJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HandleClaimJob atomically claims the next queued job.
func (h *Handler) HandleClaimJob(w http.ResponseWriter, r *http.Request) {
	nowExpr := h.DB.NowUTC()

	var id, payload string
	var err error

	if h.DB.IsPostgres() {
		err = h.DB.QueryRowContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = 'running', started_at = %s, attempts = attempts + 1
			WHERE id = (
				SELECT id FROM jobs WHERE status = 'queued' AND (run_after IS NULL OR run_after <= %s)
				ORDER BY priority DESC, created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED
			) RETURNING id, payload
		`, nowExpr, nowExpr)).Scan(&id, &payload)
	} else {
		err = h.DB.QueryRowContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = 'running', started_at = %s, attempts = attempts + 1
			WHERE id = (
				SELECT id FROM jobs WHERE status = 'queued' AND (run_after IS NULL OR run_after <= %s)
				ORDER BY priority DESC, created_at ASC LIMIT 1
			) RETURNING id, payload
		`, nowExpr, nowExpr)).Scan(&id, &payload)
	}

	if err != nil {
		httputil.WriteJSON(w, 204, nil)
		return
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"id": id, "payload": json.RawMessage(payload),
	})
}

// HandleUpdateJob updates a job's status, error, and result.
func (h *Handler) HandleUpdateJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	nowExpr := h.DB.NowUTC()

	var req struct {
		Status   string           `json:"status"`
		Error    *string          `json:"error,omitempty"`
		Result   *json.RawMessage `json:"result,omitempty"`
		RunAfter *string          `json:"run_after,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	switch req.Status {
	case "complete", "failed", "rejected", "cancelled":
		resultStr := "{}"
		if req.Result != nil {
			resultStr = string(*req.Result)
		}
		errStr := ""
		if req.Error != nil {
			errStr = *req.Error
		}
		_, err := h.DB.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = ?, error = ?, result = ?, completed_at = %s WHERE id = ?
		`, nowExpr), req.Status, errStr, resultStr, jobID)
		if err != nil {
			httputil.WriteJSON(w, 500, map[string]string{"error": "failed to update job"})
			return
		}

	case "queued":
		runAfter := ""
		if req.RunAfter != nil {
			runAfter = *req.RunAfter
		}
		errStr := ""
		if req.Error != nil {
			errStr = *req.Error
		}
		_, err := h.DB.ExecContext(r.Context(),
			`UPDATE jobs SET status = 'queued', error = ?, run_after = ? WHERE id = ?`,
			errStr, runAfter, jobID)
		if err != nil {
			httputil.WriteJSON(w, 500, map[string]string{"error": "failed to re-queue job"})
			return
		}

	default:
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid status"})
		return
	}

	httputil.WriteJSON(w, 200, map[string]string{"status": "updated"})
}

// HandleGetJob returns a job's status and attempt info.
func (h *Handler) HandleGetJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	var attempts, maxAttempts int
	var status string
	err := h.DB.QueryRowContext(r.Context(),
		`SELECT status, attempts, max_attempts FROM jobs WHERE id = ?`, jobID,
	).Scan(&status, &attempts, &maxAttempts)
	if err != nil {
		httputil.WriteJSON(w, 404, map[string]string{"error": "job not found"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"id": jobID, "status": status, "attempts": attempts, "max_attempts": maxAttempts,
	})
}

// HandleReclaimStale re-queues or fails stale running jobs.
func (h *Handler) HandleReclaimStale(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StaleMinutes int `json:"stale_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StaleMinutes <= 0 {
		req.StaleMinutes = 120
	}

	nowExpr := h.DB.NowUTC()
	staleMsg := fmt.Sprintf("stale watchdog: recovered running job older than %dm", req.StaleMinutes)

	var requeuedCount, failedCount int

	if h.DB.IsPostgres() {
		cutoffExpr := fmt.Sprintf("now() - interval '%d minutes'", req.StaleMinutes)
		res, _ := h.DB.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = 'queued', run_after = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN $1 ELSE error || ' | ' || $1 END
			WHERE status = 'running' AND started_at IS NOT NULL
			  AND started_at::timestamptz <= %s AND attempts < max_attempts
		`, nowExpr, cutoffExpr), staleMsg)
		if res != nil {
			n, _ := res.RowsAffected()
			requeuedCount = int(n)
		}
		res, _ = h.DB.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = 'failed', completed_at = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN $1 ELSE error || ' | ' || $1 END
			WHERE status = 'running' AND started_at IS NOT NULL
			  AND started_at::timestamptz <= %s AND attempts >= max_attempts
		`, nowExpr, cutoffExpr), staleMsg)
		if res != nil {
			n, _ := res.RowsAffected()
			failedCount = int(n)
		}
	} else {
		cutoff := fmt.Sprintf("-%d minutes", req.StaleMinutes)
		staleExpr := h.DB.PurgeDatetimeComparison("started_at", cutoff)
		res, _ := h.DB.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = 'queued', run_after = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN ? ELSE error || ' | ' || ? END
			WHERE status = 'running' AND started_at IS NOT NULL
			  AND %s AND attempts < max_attempts
		`, nowExpr, staleExpr), staleMsg, staleMsg)
		if res != nil {
			n, _ := res.RowsAffected()
			requeuedCount = int(n)
		}
		res, _ = h.DB.ExecContext(r.Context(), fmt.Sprintf(`
			UPDATE jobs SET status = 'failed', completed_at = %s,
			    error = CASE WHEN error IS NULL OR error = '' THEN ? ELSE error || ' | ' || ? END
			WHERE status = 'running' AND started_at IS NOT NULL
			  AND %s AND attempts >= max_attempts
		`, nowExpr, staleExpr), staleMsg, staleMsg)
		if res != nil {
			n, _ := res.RowsAffected()
			failedCount = int(n)
		}
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"requeued": requeuedCount, "failed": failedCount,
	})
}

// HandleUpdateSource updates source metadata from the worker.
func (h *Handler) HandleUpdateSource(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")

	var req struct {
		Status          *string  `json:"status,omitempty"`
		ExternalID      *string  `json:"external_id,omitempty"`
		Title           *string  `json:"title,omitempty"`
		ChannelName     *string  `json:"channel_name,omitempty"`
		ThumbnailURL    *string  `json:"thumbnail_url,omitempty"`
		DurationSeconds *float64 `json:"duration_seconds,omitempty"`
		Metadata        *string  `json:"metadata,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

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
		httputil.WriteJSON(w, 400, map[string]string{"error": "no fields to update"})
		return
	}

	args = append(args, sourceID)
	query := "UPDATE sources SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	if _, err := h.DB.ExecContext(r.Context(), query, args...); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "UNIQUE constraint") || strings.Contains(errMsg, "duplicate key") {
			httputil.WriteJSON(w, 409, map[string]string{"error": "duplicate source: a source with the same platform and external_id already exists"})
			return
		}
		log.Printf("worker update source %s failed: %v", sourceID, err)
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to update source"})
		return
	}

	httputil.WriteJSON(w, 200, map[string]string{"status": "updated"})
}

// HandleGetCookie returns a decrypted platform cookie for a source.
func (h *Handler) HandleGetCookie(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")
	platform := r.URL.Query().Get("platform")

	var encrypted string
	err := h.DB.QueryRowContext(r.Context(), `
		SELECT pc.cookie_str FROM platform_cookies pc
		JOIN sources s ON pc.user_id = s.submitted_by
		WHERE s.id = ? AND pc.platform = ? AND pc.is_active = 1
	`, sourceID, platform).Scan(&encrypted)
	if err != nil {
		httputil.WriteJSON(w, 200, map[string]interface{}{"cookie": nil})
		return
	}

	decrypted, err := crypto.DecryptCookie(encrypted, h.CookieSecret)
	if err != nil {
		httputil.WriteJSON(w, 200, map[string]interface{}{"cookie": nil})
		return
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{"cookie": decrypted})
}

// HandleCreateClip creates a clip with associated topics, embeddings, and FTS.
func (h *Handler) HandleCreateClip(w http.ResponseWriter, r *http.Request) {
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
		TextEmbedding   string   `json:"text_embedding,omitempty"`
		VisualEmbedding string   `json:"visual_embedding,omitempty"`
		ModelVersion    string   `json:"model_version,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	if err := db.WithTx(r.Context(), h.DB, func(conn *db.CompatConn) error {
		topicsJSON, _ := json.Marshal(req.Topics)

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

		for _, topicName := range req.Topics {
			topicID := ResolveOrCreateTopicTx(r.Context(), conn, topicName)
			if topicID != "" {
				if _, err := conn.ExecContext(r.Context(),
					`INSERT INTO clip_topics (clip_id, topic_id, confidence, source) VALUES (?, ?, 1.0, 'keybert') ON CONFLICT DO NOTHING`,
					req.ID, topicID); err != nil {
					return fmt.Errorf("insert clip_topics: %w", err)
				}
			}
		}

		if _, err := conn.ExecContext(r.Context(),
			`INSERT INTO clips_fts(clip_id, title, transcript, platform, channel_name) VALUES (?, ?, ?, ?, ?)`,
			req.ID, req.Title, Truncate(req.Transcript, 2000), req.Platform, req.ChannelName); err != nil {
			return fmt.Errorf("insert clips_fts: %w", err)
		}

		if req.TextEmbedding != "" || req.VisualEmbedding != "" {
			var textEmb, visEmb []byte
			if req.TextEmbedding != "" {
				textEmb, _ = base64.StdEncoding.DecodeString(req.TextEmbedding)
			}
			if req.VisualEmbedding != "" {
				visEmb, _ = base64.StdEncoding.DecodeString(req.VisualEmbedding)
			}
			if _, err := conn.ExecContext(r.Context(),
				`INSERT INTO clip_embeddings (clip_id, text_embedding, visual_embedding, model_version) VALUES (?, ?, ?, ?)
				 ON CONFLICT(clip_id) DO UPDATE SET text_embedding = EXCLUDED.text_embedding, visual_embedding = EXCLUDED.visual_embedding, model_version = EXCLUDED.model_version`,
				req.ID, textEmb, visEmb, req.ModelVersion); err != nil {
				return fmt.Errorf("insert clip_embeddings: %w", err)
			}
		}

		return nil
	}); err != nil {
		log.Printf("worker create clip failed: %v", err)
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to create clip"})
		return
	}

	httputil.WriteJSON(w, 201, map[string]interface{}{"id": req.ID})
}

// ResolveOrCreateTopicTx finds or creates a topic within a transaction.
func ResolveOrCreateTopicTx(ctx context.Context, conn *db.CompatConn, name string) string {
	slug := Slugify(name)
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
	conn.QueryRowContext(ctx, "SELECT id FROM topics WHERE slug = ?", slug).Scan(&id)
	return id
}

// HandleResolveTopic resolves or creates a topic by name.
func (h *Handler) HandleResolveTopic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httputil.WriteJSON(w, 400, map[string]string{"error": "name required"})
		return
	}

	slug := Slugify(req.Name)
	var id string
	err := h.DB.QueryRowContext(r.Context(),
		"SELECT id FROM topics WHERE slug = ? OR LOWER(name) = LOWER(?)", slug, req.Name,
	).Scan(&id)
	if err == nil {
		httputil.WriteJSON(w, 200, map[string]interface{}{"id": id, "created": false})
		return
	}

	id = uuid.New().String()
	h.DB.ExecContext(r.Context(),
		"INSERT INTO topics (id, name, slug, path, depth) VALUES (?, ?, ?, ?, 0) ON CONFLICT DO NOTHING",
		id, req.Name, slug, slug)
	h.DB.QueryRowContext(r.Context(), "SELECT id FROM topics WHERE slug = ?", slug).Scan(&id)

	httputil.WriteJSON(w, 201, map[string]interface{}{"id": id, "created": true})
}

// HandleScoreUpdate recalculates content scores from interaction signals.
func (h *Handler) HandleScoreUpdate(w http.ResponseWriter, r *http.Request) {
	var count int64
	if h.DB.IsPostgres() {
		res, err := h.DB.ExecContext(r.Context(), `
			UPDATE clips SET content_score = sub.new_score
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
				FROM interactions GROUP BY clip_id
				HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5
			) sub
			WHERE clips.id = sub.clip_id AND clips.status = 'ready'
		`)
		if err == nil {
			count, _ = res.RowsAffected()
		}
	} else {
		res, err := h.DB.ExecContext(r.Context(), `
			UPDATE clips SET content_score = (
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

	httputil.WriteJSON(w, 200, map[string]interface{}{"updated": count})
}

// Slugify converts a topic name to a URL-safe slug.
func Slugify(name string) string {
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

// Truncate shortens a string to maxLen runes.
func Truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}
