package feed

import (
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"

	"clipfeed/httputil"

	"github.com/go-chi/chi/v5"
)

// HandleSimilarClips finds clips similar to the given clip by embedding distance.
func (h *Handler) HandleSimilarClips(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")
	limitStr := r.URL.Query().Get("limit")
	limit := 10
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 50 {
		limit = n
	}

	var refText, refVisual []byte
	err := h.DB.QueryRowContext(r.Context(),
		`SELECT text_embedding, visual_embedding FROM clip_embeddings WHERE clip_id = ?`, clipID,
	).Scan(&refText, &refVisual)
	if err != nil {
		httputil.WriteJSON(w, 404, map[string]string{"error": "no embeddings for this clip"})
		return
	}

	refTextVec := BlobToFloat32(refText)
	refVisualVec := BlobToFloat32(refVisual)
	if refTextVec == nil && refVisualVec == nil {
		httputil.WriteJSON(w, 404, map[string]string{"error": "no embeddings for this clip"})
		return
	}

	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT e.clip_id, e.text_embedding, e.visual_embedding,
		       c.title, c.thumbnail_key, c.duration_seconds, c.content_score
		FROM clip_embeddings e
		JOIN clips c ON e.clip_id = c.id AND c.status = 'ready'
		WHERE e.clip_id != ?
		LIMIT 500
	`, clipID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	type scored struct {
		data  map[string]interface{}
		score float64
	}
	var results []scored

	for rows.Next() {
		var cid string
		var tBlob, vBlob []byte
		var title string
		var thumbKey string
		var dur, cs float64
		if err := rows.Scan(&cid, &tBlob, &vBlob, &title, &thumbKey, &dur, &cs); err != nil {
			continue
		}

		textSim := 0.0
		visualSim := 0.0
		hasText := refTextVec != nil && len(tBlob) > 0
		hasVisual := refVisualVec != nil && len(vBlob) > 0

		if hasText {
			textSim = CosineSimilarity(refTextVec, BlobToFloat32(tBlob))
		}
		if hasVisual {
			visualSim = CosineSimilarity(refVisualVec, BlobToFloat32(vBlob))
		}

		var sim float64
		switch {
		case hasText && hasVisual:
			sim = textSim*0.6 + visualSim*0.4
		case hasText:
			sim = textSim
		case hasVisual:
			sim = visualSim
		}

		results = append(results, scored{
			data: map[string]interface{}{
				"id": cid, "title": title, "thumbnail_key": thumbKey,
				"thumbnail_url": httputil.ThumbnailURL(h.MinioBucket, thumbKey),
				"duration_seconds": dur, "content_score": cs, "similarity": math.Round(sim*1000) / 1000,
			},
			score: sim,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("HandleSimilarClips: rows iteration error: %v", err)
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > limit {
		results = results[:limit]
	}

	clips := make([]map[string]interface{}, len(results))
	for i, r := range results {
		clips[i] = r.data
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}
