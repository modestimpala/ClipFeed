package jobs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"clipfeed/auth"
	"clipfeed/db"
	"clipfeed/httputil"

	"github.com/go-chi/chi/v5"
)

// Handler holds dependencies for user-facing job endpoints.
type Handler struct {
	DB *db.CompatDB
}

// HandleListJobs lists jobs for the authenticated user.
func (h *Handler) HandleListJobs(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT j.id, j.source_id, j.job_type, j.status, j.error,
		       j.attempts, j.max_attempts, j.started_at, j.completed_at, j.created_at,
		       s.url, s.platform, s.title, s.channel_name, s.thumbnail_url, s.external_id, s.metadata
		FROM jobs j
		LEFT JOIN sources s ON j.source_id = s.id
		WHERE s.submitted_by = ?
		ORDER BY j.created_at DESC LIMIT 50
	`, userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to list jobs"})
		return
	}
	defer rows.Close()

	var jobList []map[string]interface{}
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
		jobList = append(jobList, job)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"jobs": jobList})
}

// HandleGetJob returns a single job by ID (owned by the authenticated user).
func (h *Handler) HandleGetJob(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	jobID := chi.URLParam(r, "id")
	var id, jobType, status, payloadStr, resultStr, createdAt string
	var sourceID *string
	var errMsg *string

	err := h.DB.QueryRowContext(r.Context(), `
		SELECT j.id, j.source_id, j.job_type, j.status, j.payload, j.result, j.error, j.created_at
		FROM jobs j
		LEFT JOIN sources s ON j.source_id = s.id
		WHERE j.id = ? AND s.submitted_by = ?
	`, jobID, userID).Scan(&id, &sourceID, &jobType, &status, &payloadStr, &resultStr, &errMsg, &createdAt)
	if err != nil {
		httputil.WriteJSON(w, 404, map[string]string{"error": "job not found"})
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

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"id": id, "source_id": sourceID, "job_type": jobType,
		"status": status, "payload": payload,
		"result": result, "error": errMsg, "created_at": createdAt,
	})
}

// HandleCancelJob cancels a queued or running job.
func (h *Handler) HandleCancelJob(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	jobID := chi.URLParam(r, "id")
	nowExpr := h.DB.NowUTC()

	res, err := h.DB.ExecContext(r.Context(), fmt.Sprintf(`
		UPDATE jobs SET status = 'cancelled', error = 'Cancelled by user', completed_at = %s
		WHERE id = ? AND status IN ('queued', 'running')
		  AND source_id IN (SELECT id FROM sources WHERE submitted_by = ?)
	`, nowExpr), jobID, userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to cancel job"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		httputil.WriteJSON(w, 404, map[string]string{"error": "job not found or not cancellable"})
		return
	}
	h.DB.ExecContext(r.Context(),
		`UPDATE sources SET status = 'cancelled' WHERE id = (SELECT source_id FROM jobs WHERE id = ?)`, jobID)
	httputil.WriteJSON(w, 200, map[string]string{"status": "cancelled"})
}

// HandleRetryJob re-queues a failed/cancelled/rejected job.
func (h *Handler) HandleRetryJob(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	jobID := chi.URLParam(r, "id")

	res, err := h.DB.ExecContext(r.Context(), `
		UPDATE jobs SET status = 'queued', error = NULL, run_after = NULL,
		       attempts = 0, started_at = NULL, completed_at = NULL
		WHERE id = ? AND status IN ('failed', 'cancelled', 'rejected')
		  AND source_id IN (SELECT id FROM sources WHERE submitted_by = ?)
	`, jobID, userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to retry job"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		httputil.WriteJSON(w, 404, map[string]string{"error": "job not found or not retryable"})
		return
	}
	h.DB.ExecContext(r.Context(),
		`UPDATE sources SET status = 'pending' WHERE id = (SELECT source_id FROM jobs WHERE id = ?)`, jobID)
	httputil.WriteJSON(w, 200, map[string]string{"status": "queued"})
}

// HandleDismissJob removes a completed/failed/cancelled job.
func (h *Handler) HandleDismissJob(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	jobID := chi.URLParam(r, "id")

	res, err := h.DB.ExecContext(r.Context(), `
		DELETE FROM jobs
		WHERE id = ? AND status IN ('complete', 'failed', 'cancelled', 'rejected')
		  AND source_id IN (SELECT id FROM sources WHERE submitted_by = ?)
	`, jobID, userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to dismiss job"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		httputil.WriteJSON(w, 404, map[string]string{"error": "job not found or still active"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "dismissed"})
}
