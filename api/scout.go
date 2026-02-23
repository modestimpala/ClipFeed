package main

import (
	"encoding/json"
	"fmt"
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
		a.db.ExecContext(r.Context(),
			`UPDATE scout_sources SET is_active = ? WHERE id = ? AND user_id = ?`,
			active, sourceID, userID)
	}
	if req.Interval != nil && *req.Interval > 0 {
		a.db.ExecContext(r.Context(),
			`UPDATE scout_sources SET check_interval_hours = ? WHERE id = ? AND user_id = ?`,
			*req.Interval, sourceID, userID)
	}

	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (a *App) handleDeleteScoutSource(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	sourceID := chi.URLParam(r, "id")

	res, err := a.db.ExecContext(r.Context(),
		`DELETE FROM scout_sources WHERE id = ? AND user_id = ?`, sourceID, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to delete source"})
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		writeJSON(w, 404, map[string]string{"error": "source not found"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
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
