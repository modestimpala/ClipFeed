package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

type IngestRequest struct {
	URL string `json:"url"`
}

func (a *App) handleIngest(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	maxBody(r, defaultBodyLimit)

	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	if req.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "url is required"})
		return
	}

	parsed, err := url.Parse(req.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeJSON(w, 400, map[string]string{"error": "url must be a valid http or https URL"})
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
		if _, rbErr := conn.ExecContext(r.Context(), "ROLLBACK"); rbErr != nil {
			log.Printf("rollback failed after source insert error: %v", rbErr)
		}
		writeJSON(w, 500, map[string]string{"error": "failed to create source"})
		return
	}

	_, err = conn.ExecContext(r.Context(),
		`INSERT INTO jobs (id, source_id, job_type, payload) VALUES (?, ?, 'download', ?)`,
		jobID, sourceID, payload)
	if err != nil {
		if _, rbErr := conn.ExecContext(r.Context(), "ROLLBACK"); rbErr != nil {
			log.Printf("rollback failed after job insert error: %v", rbErr)
		}
		writeJSON(w, 500, map[string]string{"error": "failed to queue job"})
		return
	}

	if _, err := conn.ExecContext(r.Context(), "COMMIT"); err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to commit"})
		return
	}

	writeJSON(w, 202, map[string]interface{}{
		"source_id": sourceID,
		"job_id":    jobID,
		"status":    "queued",
	})
}

func detectPlatform(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "direct"
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "youtube.com" || host == "www.youtube.com" || host == "m.youtube.com" || host == "youtu.be":
		return "youtube"
	case host == "vimeo.com" || host == "www.vimeo.com":
		return "vimeo"
	case strings.HasSuffix(host, "tiktok.com"):
		return "tiktok"
	case host == "instagram.com" || host == "www.instagram.com":
		return "instagram"
	case host == "twitter.com" || host == "www.twitter.com" || host == "x.com" || host == "www.x.com":
		return "twitter"
	default:
		return "direct"
	}
}
