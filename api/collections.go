package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (a *App) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	id := uuid.New().String()
	_, err := a.db.ExecContext(r.Context(),
		`INSERT INTO collections (id, user_id, title, description) VALUES (?, ?, ?, ?)`,
		id, userID, req.Title, req.Description)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to create collection"})
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (a *App) handleListCollections(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.description, c.is_public, c.created_at,
		       COUNT(cc.clip_id) as clip_count
		FROM collections c
		LEFT JOIN collection_clips cc ON c.id = cc.collection_id
		WHERE c.user_id = ?
		GROUP BY c.id
		ORDER BY c.created_at DESC
	`, userID)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list collections"})
		return
	}
	defer rows.Close()

	var collections []map[string]interface{}
	for rows.Next() {
		var id, title, createdAt string
		var description *string
		var isPublic int
		var clipCount int
		rows.Scan(&id, &title, &description, &isPublic, &createdAt, &clipCount)
		collections = append(collections, map[string]interface{}{
			"id": id, "title": title, "description": description,
			"is_public": isPublic == 1, "clip_count": clipCount, "created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"collections": collections})
}

func (a *App) handleAddToCollection(w http.ResponseWriter, r *http.Request) {
	collectionID := chi.URLParam(r, "id")
	var req struct {
		ClipID string `json:"clip_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	_, err := a.db.ExecContext(r.Context(), `
		INSERT INTO collection_clips (collection_id, clip_id, position)
		VALUES (?, ?, COALESCE((SELECT MAX(position) + 1 FROM collection_clips WHERE collection_id = ?), 0))
		ON CONFLICT DO NOTHING
	`, collectionID, req.ClipID, collectionID)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to add to collection"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "added"})
}

func (a *App) handleRemoveFromCollection(w http.ResponseWriter, r *http.Request) {
	collectionID := chi.URLParam(r, "id")
	clipID := chi.URLParam(r, "clipId")

	a.db.ExecContext(r.Context(),
		`DELETE FROM collection_clips WHERE collection_id = ? AND clip_id = ?`,
		collectionID, clipID)

	writeJSON(w, 200, map[string]string{"status": "removed"})
}
