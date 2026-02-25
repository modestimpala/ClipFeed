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

	if err := withTx(r.Context(), a.db, func(conn *CompatConn) error {
		if _, err := conn.ExecContext(r.Context(),
			`INSERT INTO sources (id, url, platform, submitted_by, status) VALUES (?, ?, ?, ?, 'pending')`,
			sourceID, req.URL, platform, userID); err != nil {
			return fmt.Errorf("create source: %w", err)
		}
		if _, err := conn.ExecContext(r.Context(),
			`INSERT INTO jobs (id, source_id, job_type, payload) VALUES (?, ?, 'download', ?)`,
			jobID, sourceID, payload); err != nil {
			return fmt.Errorf("queue job: %w", err)
		}
		return nil
	}); err != nil {
		log.Printf("ingest tx failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "failed to queue ingestion"})
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
