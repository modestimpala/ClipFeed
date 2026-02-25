package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (a *App) handleListJobs(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT j.id, j.source_id, j.job_type, j.status, j.error,
		       j.attempts, j.max_attempts, j.started_at, j.completed_at, j.created_at,
		       s.url, s.platform, s.title, s.channel_name, s.thumbnail_url, s.external_id, s.metadata
		FROM jobs j
		LEFT JOIN sources s ON j.source_id = s.id
		WHERE s.submitted_by = ?
		ORDER BY j.created_at DESC LIMIT 50
	`, userID)
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
	userID := r.Context().Value(userIDKey).(string)
	jobID := chi.URLParam(r, "id")
	var id, jobType, status, payloadStr, resultStr, createdAt string
	var sourceID *string
	var errMsg *string

	err := a.db.QueryRowContext(r.Context(), `
		SELECT j.id, j.source_id, j.job_type, j.status, j.payload, j.result, j.error, j.created_at
		FROM jobs j
		LEFT JOIN sources s ON j.source_id = s.id
		WHERE j.id = ? AND s.submitted_by = ?
	`, jobID, userID).Scan(&id, &sourceID, &jobType, &status, &payloadStr, &resultStr, &errMsg, &createdAt)

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

// POST /api/jobs/{id}/cancel — cancel a queued or running job
func (a *App) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	jobID := chi.URLParam(r, "id")
	nowExpr := a.db.NowUTC()

	res, err := a.db.ExecContext(r.Context(), fmt.Sprintf(`
		UPDATE jobs SET status = 'cancelled', error = 'Cancelled by user', completed_at = %s
		WHERE id = ? AND status IN ('queued', 'running')
		  AND source_id IN (SELECT id FROM sources WHERE submitted_by = ?)
	`, nowExpr), jobID, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to cancel job"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, 404, map[string]string{"error": "job not found or not cancellable"})
		return
	}
	// Also reset the source so it doesn't stay in a stuck state
	a.db.ExecContext(r.Context(),
		`UPDATE sources SET status = 'cancelled' WHERE id = (SELECT source_id FROM jobs WHERE id = ?)`, jobID)
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

// POST /api/jobs/{id}/retry — retry a failed/cancelled/rejected job
func (a *App) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	jobID := chi.URLParam(r, "id")

	res, err := a.db.ExecContext(r.Context(), `
		UPDATE jobs SET status = 'queued', error = NULL, run_after = NULL,
		       attempts = 0, started_at = NULL, completed_at = NULL
		WHERE id = ? AND status IN ('failed', 'cancelled', 'rejected')
		  AND source_id IN (SELECT id FROM sources WHERE submitted_by = ?)
	`, jobID, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to retry job"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, 404, map[string]string{"error": "job not found or not retryable"})
		return
	}
	// Reset source status so the worker picks it up
	a.db.ExecContext(r.Context(),
		`UPDATE sources SET status = 'pending' WHERE id = (SELECT source_id FROM jobs WHERE id = ?)`, jobID)
	writeJSON(w, 200, map[string]string{"status": "queued"})
}

// DELETE /api/jobs/{id} — dismiss a completed/failed/cancelled job from the list
func (a *App) handleDismissJob(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	jobID := chi.URLParam(r, "id")

	res, err := a.db.ExecContext(r.Context(), `
		DELETE FROM jobs
		WHERE id = ? AND status IN ('complete', 'failed', 'cancelled', 'rejected')
		  AND source_id IN (SELECT id FROM sources WHERE submitted_by = ?)
	`, jobID, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to dismiss job"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, 404, map[string]string{"error": "job not found or still active"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "dismissed"})
}
