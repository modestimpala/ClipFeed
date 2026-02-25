package saved

import (
	"encoding/json"
	"net/http"

	"clipfeed/auth"
	"clipfeed/db"
	"clipfeed/httputil"

	"github.com/go-chi/chi/v5"
)

// Handler holds dependencies for saved-clips and history endpoints.
type Handler struct {
	DB          *db.CompatDB
	MinioBucket string
}

// HandleSaveClip saves a clip for the authenticated user.
func (h *Handler) HandleSaveClip(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	clipID := chi.URLParam(r, "id")

	var exists int
	if err := h.DB.QueryRowContext(r.Context(), `SELECT 1 FROM clips WHERE id = ?`, clipID).Scan(&exists); err != nil {
		httputil.WriteJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	_, err := h.DB.ExecContext(r.Context(),
		`INSERT INTO saved_clips (user_id, clip_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		userID, clipID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to save clip"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "saved"})
}

// HandleUnsaveClip removes a saved clip.
func (h *Handler) HandleUnsaveClip(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	clipID := chi.URLParam(r, "id")

	if _, err := h.DB.ExecContext(r.Context(),
		`DELETE FROM saved_clips WHERE user_id = ? AND clip_id = ?`,
		userID, clipID); err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to remove clip"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "removed"})
}

// HandleListSaved lists the user's saved clips.
func (h *Handler) HandleListSaved(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)

	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
		       c.topics, c.created_at, s.platform, s.channel_name, s.url
		FROM saved_clips sc
		JOIN clips c ON sc.clip_id = c.id
		LEFT JOIN sources s ON c.source_id = s.id
		WHERE sc.user_id = ?
		ORDER BY sc.created_at DESC
		LIMIT 200
	`, userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to list saved clips"})
		return
	}
	defer rows.Close()

	var clips []map[string]interface{}
	for rows.Next() {
		var id, title, thumbnailKey, topicsJSON, createdAt string
		var duration float64
		var platform, channelName, sourceURL *string
		if err := rows.Scan(&id, &title, &duration, &thumbnailKey, &topicsJSON, &createdAt,
			&platform, &channelName, &sourceURL); err != nil {
			continue
		}
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey,
			"thumbnail_url": httputil.ThumbnailURL(h.MinioBucket, thumbnailKey),
			"topics": topics, "created_at": createdAt,
			"platform": platform, "channel_name": channelName, "source_url": sourceURL,
		})
	}
	if clips == nil {
		clips = make([]map[string]interface{}, 0)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"clips": clips})
}

// HandleListHistory lists the user's recent interaction history.
func (h *Handler) HandleListHistory(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)

	rows, err := h.DB.QueryContext(r.Context(), `
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
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to list history"})
		return
	}
	defer rows.Close()

	var history []map[string]interface{}
	for rows.Next() {
		var id, title, thumbnailKey, action string
		var duration float64
		var at string
		if err := rows.Scan(&id, &title, &duration, &thumbnailKey, &action, &at); err != nil {
			continue
		}
		history = append(history, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey,
			"thumbnail_url": httputil.ThumbnailURL(h.MinioBucket, thumbnailKey),
			"last_action": action, "at": at,
		})
	}
	if history == nil {
		history = make([]map[string]interface{}, 0)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"history": history})
}
