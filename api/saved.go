package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (a *App) handleSaveClip(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	clipID := chi.URLParam(r, "id")

	_, err := a.db.ExecContext(r.Context(),
		`INSERT OR IGNORE INTO saved_clips (user_id, clip_id) VALUES (?, ?)`,
		userID, clipID)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to save clip"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "saved"})
}

func (a *App) handleUnsaveClip(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	clipID := chi.URLParam(r, "id")

	if _, err := a.db.ExecContext(r.Context(),
		`DELETE FROM saved_clips WHERE user_id = ? AND clip_id = ?`,
		userID, clipID); err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to remove clip"})
		return
	}

	writeJSON(w, 200, map[string]string{"status": "removed"})
}

func (a *App) handleListSaved(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
		       c.topics, c.created_at, s.platform, s.channel_name, s.url
		FROM saved_clips sc
		JOIN clips c ON sc.clip_id = c.id
		LEFT JOIN sources s ON c.source_id = s.id
		WHERE sc.user_id = ?
		ORDER BY sc.created_at DESC
	`, userID)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list saved clips"})
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
			"thumbnail_url": thumbnailURL(a.cfg.MinioBucket, thumbnailKey),
			"topics": topics, "created_at": createdAt,
			"platform": platform, "channel_name": channelName, "source_url": sourceURL,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"clips": clips})
}

func (a *App) handleListHistory(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	rows, err := a.db.QueryContext(r.Context(), `
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
		writeJSON(w, 500, map[string]string{"error": "failed to list history"})
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
			"thumbnail_url": thumbnailURL(a.cfg.MinioBucket, thumbnailKey),
			"last_action": action, "at": at,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"history": history})
}
