package collections

import (
	"encoding/json"
	"net/http"
	"strings"

	"clipfeed/auth"
	"clipfeed/db"
	"clipfeed/httputil"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler holds dependencies for collection endpoints.
type Handler struct {
	DB          *db.CompatDB
	MinioBucket string
}

// HandleCreateCollection creates a new collection.
func (h *Handler) HandleCreateCollection(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	httputil.MaxBody(r, httputil.DefaultBodyLimit)

	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || len(req.Title) > 200 {
		httputil.WriteJSON(w, 400, map[string]string{"error": "title is required and must be under 200 characters"})
		return
	}
	if len(req.Description) > 2000 {
		httputil.WriteJSON(w, 400, map[string]string{"error": "description must be under 2000 characters"})
		return
	}

	id := uuid.New().String()
	_, err := h.DB.ExecContext(r.Context(),
		`INSERT INTO collections (id, user_id, title, description) VALUES (?, ?, ?, ?)`,
		id, userID, req.Title, req.Description)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to create collection"})
		return
	}
	httputil.WriteJSON(w, 201, map[string]string{"id": id})
}

// HandleListCollections lists the user's collections with clip counts.
func (h *Handler) HandleListCollections(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.description, c.is_public, c.created_at,
		       COUNT(cc.clip_id) as clip_count
		FROM collections c
		LEFT JOIN collection_clips cc ON c.id = cc.collection_id
		WHERE c.user_id = ?
		GROUP BY c.id
		ORDER BY c.created_at DESC
		LIMIT 100
	`, userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to list collections"})
		return
	}
	defer rows.Close()

	var cols []map[string]interface{}
	for rows.Next() {
		var id, title, createdAt string
		var description *string
		var isPublic int
		var clipCount int
		if err := rows.Scan(&id, &title, &description, &isPublic, &createdAt, &clipCount); err != nil {
			continue
		}
		cols = append(cols, map[string]interface{}{
			"id": id, "title": title, "description": description,
			"is_public": isPublic == 1, "clip_count": clipCount, "created_at": createdAt,
		})
	}
	if cols == nil {
		cols = make([]map[string]interface{}, 0)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"collections": cols})
}

// HandleAddToCollection adds a clip to a collection.
func (h *Handler) HandleAddToCollection(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	collectionID := chi.URLParam(r, "id")
	var req struct {
		ClipID string `json:"clip_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	var count int
	if err := h.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM collections WHERE id = ? AND user_id = ?`, collectionID, userID,
	).Scan(&count); err != nil || count == 0 {
		httputil.WriteJSON(w, 404, map[string]string{"error": "collection not found"})
		return
	}

	_, err := h.DB.ExecContext(r.Context(), `
		INSERT INTO collection_clips (collection_id, clip_id, position)
		VALUES (?, ?, COALESCE((SELECT MAX(position) + 1 FROM collection_clips WHERE collection_id = ?), 0))
		ON CONFLICT DO NOTHING
	`, collectionID, req.ClipID, collectionID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to add to collection"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "added"})
}

// HandleRemoveFromCollection removes a clip from a collection.
func (h *Handler) HandleRemoveFromCollection(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	collectionID := chi.URLParam(r, "id")
	clipID := chi.URLParam(r, "clipId")

	var count int
	if err := h.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM collections WHERE id = ? AND user_id = ?`, collectionID, userID,
	).Scan(&count); err != nil || count == 0 {
		httputil.WriteJSON(w, 404, map[string]string{"error": "collection not found"})
		return
	}

	if _, err := h.DB.ExecContext(r.Context(),
		`DELETE FROM collection_clips WHERE collection_id = ? AND clip_id = ?`,
		collectionID, clipID); err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to remove from collection"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "removed"})
}

// HandleGetCollectionClips returns clips in a collection.
func (h *Handler) HandleGetCollectionClips(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	collectionID := chi.URLParam(r, "id")

	var colTitle string
	var colDesc *string
	if err := h.DB.QueryRowContext(r.Context(),
		`SELECT title, description FROM collections WHERE id = ? AND user_id = ?`, collectionID, userID,
	).Scan(&colTitle, &colDesc); err != nil {
		httputil.WriteJSON(w, 404, map[string]string{"error": "collection not found"})
		return
	}

	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
		       c.topics, c.created_at, s.platform, s.channel_name, s.url
		FROM collection_clips cc
		JOIN clips c ON cc.clip_id = c.id
		LEFT JOIN sources s ON c.source_id = s.id
		WHERE cc.collection_id = ?
		ORDER BY cc.position ASC, cc.added_at DESC
		LIMIT 200
	`, collectionID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to list collection clips"})
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
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"collection": map[string]interface{}{"id": collectionID, "title": colTitle, "description": colDesc},
		"clips":      clips,
	})
}

// HandleDeleteCollection deletes a collection.
func (h *Handler) HandleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	collectionID := chi.URLParam(r, "id")

	res, err := h.DB.ExecContext(r.Context(),
		`DELETE FROM collections WHERE id = ? AND user_id = ?`, collectionID, userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to delete collection"})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		httputil.WriteJSON(w, 404, map[string]string{"error": "collection not found"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "deleted"})
}
