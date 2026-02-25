package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

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
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT s.id, s.source_type, s.platform, s.identifier, s.is_active,
		       s.last_checked, s.check_interval_hours, s.force_check, s.created_at,
		       COALESCE(SUM(CASE WHEN c.status = 'pending'  THEN 1 ELSE 0 END), 0) AS cnt_pending,
		       COALESCE(SUM(CASE WHEN c.status = 'approved' THEN 1 ELSE 0 END), 0) AS cnt_approved,
		       COALESCE(SUM(CASE WHEN c.status = 'rejected' THEN 1 ELSE 0 END), 0) AS cnt_rejected,
		       COALESCE(SUM(CASE WHEN c.status = 'ingested' THEN 1 ELSE 0 END), 0) AS cnt_ingested
		FROM scout_sources s
		LEFT JOIN scout_candidates c ON c.scout_source_id = s.id
		WHERE s.user_id = ?
		GROUP BY s.id
		ORDER BY s.created_at DESC`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	var sources []map[string]interface{}
	for rows.Next() {
		var id, srcType, platform, identifier, createdAt string
		var isActive, interval, forceCheck int
		var lastChecked *string
		var cntPending, cntApproved, cntRejected, cntIngested int
		if err := rows.Scan(&id, &srcType, &platform, &identifier, &isActive,
			&lastChecked, &interval, &forceCheck, &createdAt,
			&cntPending, &cntApproved, &cntRejected, &cntIngested); err != nil {
			continue
		}
		sources = append(sources, map[string]interface{}{
			"id": id, "source_type": srcType, "platform": platform,
			"identifier": identifier, "is_active": isActive == 1,
			"last_checked": lastChecked, "check_interval_hours": interval,
			"force_check": forceCheck == 1, "created_at": createdAt,
			"candidates": map[string]int{
				"pending": cntPending, "approved": cntApproved,
				"rejected": cntRejected, "ingested": cntIngested,
			},
		})
	}
	if sources == nil {
		sources = make([]map[string]interface{}, 0)
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
		if err := rows.Scan(&id, &urlStr, &platform, &extID, &title, &channelName, &duration, &llmScore, &status, &createdAt); err != nil {
			continue
		}
		candidates = append(candidates, map[string]interface{}{
			"id": id, "url": urlStr, "platform": platform, "external_id": extID,
			"title": title, "channel_name": channelName,
			"duration_seconds": duration, "llm_score": llmScore,
			"status": status, "created_at": createdAt,
		})
	}
	if candidates == nil {
		candidates = make([]map[string]interface{}, 0)
	}
	writeJSON(w, 200, map[string]interface{}{"candidates": candidates})
}

func (a *App) handleUpdateScoutSource(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	sourceID := chi.URLParam(r, "id")

	var req struct {
		IsActive *bool `json:"is_active"`
		Interval *int  `json:"check_interval_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	if req.IsActive != nil {
		active := 0
		if *req.IsActive {
			active = 1
		}
		if _, err := a.db.ExecContext(r.Context(),
			`UPDATE scout_sources SET is_active = ? WHERE id = ? AND user_id = ?`,
			active, sourceID, userID); err != nil {
			writeJSON(w, 500, map[string]string{"error": "failed to update source"})
			return
		}
	}
	if req.Interval != nil && *req.Interval > 0 {
		if _, err := a.db.ExecContext(r.Context(),
			`UPDATE scout_sources SET check_interval_hours = ? WHERE id = ? AND user_id = ?`,
			*req.Interval, sourceID, userID); err != nil {
			writeJSON(w, 500, map[string]string{"error": "failed to update source"})
			return
		}
	}

	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (a *App) handleDeleteScoutSource(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	sourceID := chi.URLParam(r, "id")

	// Verify ownership before touching anything.
	var count int
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM scout_sources WHERE id = ? AND user_id = ?`,
		sourceID, userID).Scan(&count); err != nil || count == 0 {
		writeJSON(w, 404, map[string]string{"error": "source not found"})
		return
	}

	if err := withTx(r.Context(), a.db, func(conn *CompatConn) error {
		// Delete candidates first (FK references scout_sources with no cascade).
		if _, err := conn.ExecContext(r.Context(),
			`DELETE FROM scout_candidates WHERE scout_source_id = ?`, sourceID); err != nil {
			return fmt.Errorf("delete candidates: %w", err)
		}
		if _, err := conn.ExecContext(r.Context(),
			`DELETE FROM scout_sources WHERE id = ? AND user_id = ?`, sourceID, userID); err != nil {
			return fmt.Errorf("delete source: %w", err)
		}
		return nil
	}); err != nil {
		log.Printf("delete scout source %s: %v", sourceID, err)
		writeJSON(w, 500, map[string]string{"error": "failed to delete source"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// handleGetScoutProfile returns the user's interest profile that Scout uses for
// personalized content discovery. This includes top topics (from interactions +
// explicit weights), favorite channels, and interaction stats.
func (a *App) handleGetScoutProfile(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	// 1. Top topics from interactions (liked/saved clips)
	topicRows, err := a.db.QueryContext(r.Context(), `
		SELECT t.name, COUNT(*) AS cnt,
		       COALESCE(uta.weight, 1.0) AS user_weight
		FROM interactions i
		JOIN clips c ON i.clip_id = c.id
		JOIN clip_topics ct ON ct.clip_id = c.id
		JOIN topics t ON ct.topic_id = t.id
		LEFT JOIN user_topic_affinities uta ON uta.topic_id = t.id AND uta.user_id = i.user_id
		WHERE i.user_id = ?
		  AND i.action IN ('like', 'save', 'share')
		GROUP BY t.id
		ORDER BY (cnt * COALESCE(uta.weight, 1.0)) DESC
		LIMIT 15
	`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	defer topicRows.Close()

	var topics []map[string]interface{}
	for topicRows.Next() {
		var name string
		var cnt int
		var weight float64
		if err := topicRows.Scan(&name, &cnt, &weight); err != nil {
			continue
		}
		topics = append(topics, map[string]interface{}{
			"name": name, "interaction_count": cnt, "weight": weight,
		})
	}
	if topics == nil {
		topics = make([]map[string]interface{}, 0)
	}

	// 2. Explicit topic weights from user preferences
	var topicWeightsJSON string
	a.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(topic_weights, '{}') FROM user_preferences WHERE user_id = ?`,
		userID).Scan(&topicWeightsJSON)

	var explicitWeights map[string]interface{}
	json.Unmarshal([]byte(topicWeightsJSON), &explicitWeights)
	if explicitWeights == nil {
		explicitWeights = make(map[string]interface{})
	}

	// 3. Favorite channels (most interacted)
	channelRows, err := a.db.QueryContext(r.Context(), `
		SELECT s.channel_name, COUNT(*) AS cnt
		FROM interactions i
		JOIN clips c ON i.clip_id = c.id
		JOIN sources s ON c.source_id = s.id
		WHERE i.user_id = ?
		  AND i.action IN ('like', 'save', 'share', 'complete')
		  AND s.channel_name IS NOT NULL AND TRIM(s.channel_name) <> ''
		GROUP BY s.channel_name
		ORDER BY cnt DESC
		LIMIT 10
	`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	defer channelRows.Close()

	var channels []map[string]interface{}
	for channelRows.Next() {
		var name string
		var cnt int
		if err := channelRows.Scan(&name, &cnt); err != nil {
			continue
		}
		channels = append(channels, map[string]interface{}{
			"name": name, "interaction_count": cnt,
		})
	}
	if channels == nil {
		channels = make([]map[string]interface{}, 0)
	}

	// 4. Interaction summary
	var totalLikes, totalSaves, totalViews int
	a.db.QueryRowContext(r.Context(), `
		SELECT
			COALESCE(SUM(CASE WHEN action = 'like' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN action = 'save' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN action = 'view' THEN 1 ELSE 0 END), 0)
		FROM interactions WHERE user_id = ?
	`, userID).Scan(&totalLikes, &totalSaves, &totalViews)

	// 5. User preferences relevant to scout
	var scoutThreshold float64
	var scoutAutoIngest int
	a.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(scout_threshold, 6.0), COALESCE(scout_auto_ingest, 1) FROM user_preferences WHERE user_id = ?`,
		userID).Scan(&scoutThreshold, &scoutAutoIngest)

	writeJSON(w, 200, map[string]interface{}{
		"topics":           topics,
		"explicit_weights": explicitWeights,
		"channels":         channels,
		"stats": map[string]int{
			"likes": totalLikes, "saves": totalSaves, "views": totalViews,
		},
		"scout_threshold":    scoutThreshold,
		"scout_auto_ingest":  scoutAutoIngest == 1,
	})
}

func (a *App) handleTriggerScoutSource(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	sourceID := chi.URLParam(r, "id")

	res, err := a.db.ExecContext(r.Context(),
		`UPDATE scout_sources SET force_check = 1, last_checked = NULL
		 WHERE id = ? AND user_id = ?`, sourceID, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "trigger failed"})
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		writeJSON(w, 404, map[string]string{"error": "source not found"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "triggered"})
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

	if err := withTx(r.Context(), a.db, func(conn *CompatConn) error {
		if _, err := conn.ExecContext(r.Context(),
			`INSERT INTO sources (id, url, platform, submitted_by, status) VALUES (?, ?, ?, ?, 'pending')`,
			sourceID, urlStr, platform, userID); err != nil {
			return fmt.Errorf("create source: %w", err)
		}
		if _, err := conn.ExecContext(r.Context(),
			`INSERT INTO jobs (id, source_id, job_type, payload) VALUES (?, ?, 'download', ?)`,
			jobID, sourceID, payload); err != nil {
			return fmt.Errorf("queue job: %w", err)
		}
		if _, err := conn.ExecContext(r.Context(),
			`UPDATE scout_candidates SET status = 'ingested' WHERE id = ?`, candidateID); err != nil {
			return fmt.Errorf("update candidate: %w", err)
		}
		return nil
	}); err != nil {
		log.Printf("approve candidate tx failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "failed to approve candidate"})
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"status": "approved", "source_id": sourceID, "job_id": jobID,
	})
}
