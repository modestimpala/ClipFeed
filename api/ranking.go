package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// --- Learning-to-Rank ---

type LTRTree struct {
	FeatureIndex int     `json:"feature_index"`
	Threshold    float64 `json:"threshold"`
	LeftChild    int     `json:"left_child"`
	RightChild   int     `json:"right_child"`
	LeafValue    float64 `json:"leaf_value"`
	IsLeaf       bool    `json:"is_leaf"`
}

type LTRModel struct {
	Trees        [][]LTRTree `json:"trees"`
	FeatureNames []string    `json:"feature_names"`
	NumFeatures  int         `json:"num_features"`
}

func (m *LTRModel) Score(features []float64) float64 {
	if m == nil || len(m.Trees) == 0 {
		return 0.5
	}
	sum := 0.0
	for _, tree := range m.Trees {
		sum += m.scoreTree(tree, features)
	}
	return 1.0 / (1.0 + math.Exp(-sum))
}

func (m *LTRModel) scoreTree(nodes []LTRTree, features []float64) float64 {
	idx := 0
	for idx < len(nodes) {
		n := nodes[idx]
		if n.IsLeaf {
			return n.LeafValue
		}
		if n.FeatureIndex < len(features) && features[n.FeatureIndex] <= n.Threshold {
			idx = n.LeftChild
		} else {
			idx = n.RightChild
		}
	}
	return 0
}

func (a *App) getLTRModel() *LTRModel {
	a.ltrMu.RLock()
	defer a.ltrMu.RUnlock()
	return a.ltrModel
}

func (a *App) loadLTRModel() *LTRModel {
	modelPath := a.cfg.DBPath[:len(a.cfg.DBPath)-len("clipfeed.db")] + "l2r_model.json"
	f, err := os.Open(modelPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}

	var model LTRModel
	if err := json.Unmarshal(data, &model); err != nil {
		log.Printf("LTR model parse error: %v", err)
		return nil
	}
	log.Printf("LTR model loaded: %d trees, %d features", len(model.Trees), model.NumFeatures)
	return &model
}

func (a *App) ltrModelRefreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if m := a.loadLTRModel(); m != nil {
			a.ltrMu.Lock()
			a.ltrModel = m
			a.ltrMu.Unlock()
		}
	}
}

// --- Embeddings ---

func blobToFloat32(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4 : (i+1)*4]))
	}
	return out
}

func float32ToBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:(i+1)*4], math.Float32bits(f))
	}
	return b
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func (a *App) handleSimilarClips(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")
	limitStr := r.URL.Query().Get("limit")
	limit := 10
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 50 {
		limit = n
	}

	var refText, refVisual []byte
	err := a.db.QueryRowContext(r.Context(),
		`SELECT text_embedding, visual_embedding FROM clip_embeddings WHERE clip_id = ?`, clipID,
	).Scan(&refText, &refVisual)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no embeddings for this clip"})
		return
	}

	refTextVec := blobToFloat32(refText)
	refVisualVec := blobToFloat32(refVisual)
	if refTextVec == nil && refVisualVec == nil {
		writeJSON(w, 404, map[string]string{"error": "no embeddings for this clip"})
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT e.clip_id, e.text_embedding, e.visual_embedding,
		       c.title, c.thumbnail_key, c.duration_seconds, c.content_score
		FROM clip_embeddings e
		JOIN clips c ON e.clip_id = c.id AND c.status = 'ready'
		WHERE e.clip_id != ?
	`, clipID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "query failed"})
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
		rows.Scan(&cid, &tBlob, &vBlob, &title, &thumbKey, &dur, &cs)

		textSim := 0.0
		visualSim := 0.0
		hasText := refTextVec != nil && len(tBlob) > 0
		hasVisual := refVisualVec != nil && len(vBlob) > 0

		if hasText {
			textSim = cosineSimilarity(refTextVec, blobToFloat32(tBlob))
		}
		if hasVisual {
			visualSim = cosineSimilarity(refVisualVec, blobToFloat32(vBlob))
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
				"duration_seconds": dur, "content_score": cs, "similarity": math.Round(sim*1000) / 1000,
			},
			score: sim,
		})
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > limit {
		results = results[:limit]
	}

	clips := make([]map[string]interface{}, len(results))
	for i, r := range results {
		clips[i] = r.data
	}
	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

