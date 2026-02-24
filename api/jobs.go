package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

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
		if err := rows.Scan(&id, &sourceID, &jobType, &status, &errMsg,
			&attempts, &maxAttempts, &startedAt, &completedAt, &createdAt,
			&url, &platform, &title, &channelName, &thumbnailURL, &externalID, &sourceMetadata); err != nil {
			continue
		}
		var parsedSourceMetadata interface{} = nil
		if sourceMetadata != nil && strings.TrimSpace(*sourceMetadata) != "" {
			if err := json.Unmarshal([]byte(*sourceMetadata), &parsedSourceMetadata); err != nil {
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

	var payload, result interface{}
	if json.Valid([]byte(payloadStr)) {
		payload = json.RawMessage(payloadStr)
	} else {
		payload = payloadStr
	}
	if json.Valid([]byte(resultStr)) {
		result = json.RawMessage(resultStr)
	} else {
		result = resultStr
	}

	writeJSON(w, 200, map[string]interface{}{
		"id": id, "source_id": sourceID, "job_type": jobType,
		"status": status, "payload": payload,
		"result": result, "error": errMsg, "created_at": createdAt,
	})
}
