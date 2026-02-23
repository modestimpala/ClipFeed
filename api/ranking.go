package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
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
	if m == nil || len(m.Trees) == 0 || len(features) == 0 {
		return 0
	}
	sum := 0.0
	for _, tree := range m.Trees {
		sum += m.scoreTree(tree, features)
	}
	return sum
}

func (m *LTRModel) scoreTree(nodes []LTRTree, features []float64) float64 {
	if len(nodes) == 0 {
		return 0
	}
	idx := 0
	for idx < len(nodes) {
		n := nodes[idx]
		if n.IsLeaf {
			return n.LeafValue
		}
		if n.FeatureIndex < 0 || n.FeatureIndex >= len(features) {
			return 0
		}
		if features[n.FeatureIndex] <= n.Threshold {
			if n.LeftChild < 0 || n.LeftChild >= len(nodes) {
				return 0
			}
			idx = n.LeftChild
		} else {
			if n.RightChild < 0 || n.RightChild >= len(nodes) {
				return 0
			}
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
	modelPath := a.cfg.L2RModelPath
	if modelPath == "" {
		modelPath = "/data/l2r_model.json"
	}
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

var ltrFeatureNames = []string{
	"content_score",
	"duration_seconds",
	"topic_count",
	"transcript_length",
	"age_hours",
	"file_size_bytes",
	"topic_overlap",
	"channel_affinity",
	"user_total_views",
	"user_avg_watch_percentage",
	"user_like_rate",
	"user_save_rate",
	"hours_since_last_session",
}

type ltrUserStats struct {
	TotalViews            float64
	AvgWatchPercentage    float64
	LikeRate              float64
	SaveRate              float64
	HoursSinceLastSession float64
	ChannelAffinity       map[string]float64
	TopicAffinities       map[string]struct{}
}

func (a *App) rankFeed(ctx context.Context, clips []map[string]interface{}, userID string, topicWeights map[string]float64) {
	if len(clips) == 0 {
		return
	}

	if model := a.getLTRModel(); model != nil && len(model.Trees) > 0 {
		a.applyLTRRanking(ctx, clips, userID, model)
	} else {
		a.applyTopicBoost(ctx, clips, userID, topicWeights)
	}

	for _, clip := range clips {
		delete(clip, "_source_id")
		delete(clip, "_transcript_length")
		delete(clip, "_file_size_bytes")
		delete(clip, "_age_hours")
		delete(clip, "_l2r_score")
	}
}

func (a *App) applyLTRRanking(ctx context.Context, clips []map[string]interface{}, userID string, model *LTRModel) {
	if model == nil || len(clips) == 0 {
		return
	}

	featureCount := len(ltrFeatureNames)
	if model.NumFeatures > 0 {
		featureCount = model.NumFeatures
	}
	if featureCount <= 0 {
		return
	}

	stats := a.loadLTRUserStats(ctx, userID)
	clipIDs := make([]string, 0, len(clips))
	sourceIDs := make(map[string]string, len(clips))
	for _, clip := range clips {
		clipID, _ := clip["id"].(string)
		if clipID == "" {
			continue
		}
		clipIDs = append(clipIDs, clipID)
		sourceID, _ := clip["_source_id"].(string)
		sourceIDs[clipID] = sourceID
	}

	topicCount, topicOverlap := a.loadClipTopicStats(ctx, clipIDs, stats.TopicAffinities)

	for i := range clips {
		clip := clips[i]
		clipID, _ := clip["id"].(string)
		features := make([]float64, featureCount)
		set := func(idx int, v float64) {
			if idx >= 0 && idx < len(features) {
				features[idx] = v
			}
		}

		contentScore, _ := clip["content_score"].(float64)
		durationSeconds, _ := clip["duration_seconds"].(float64)
		transcriptLength, _ := clip["_transcript_length"].(float64)
		ageHours, _ := clip["_age_hours"].(float64)
		fileSizeBytes, _ := clip["_file_size_bytes"].(float64)

		set(0, contentScore)
		set(1, durationSeconds)
		set(2, float64(topicCount[clipID]))
		set(3, transcriptLength)
		set(4, ageHours)
		set(5, fileSizeBytes)
		set(6, float64(topicOverlap[clipID]))

		if sourceID, ok := sourceIDs[clipID]; ok {
			set(7, stats.ChannelAffinity[sourceID])
		}

		set(8, stats.TotalViews)
		set(9, stats.AvgWatchPercentage)
		set(10, stats.LikeRate)
		set(11, stats.SaveRate)
		set(12, stats.HoursSinceLastSession)

		clip["_l2r_score"] = model.Score(features)
	}

	sort.SliceStable(clips, func(i, j int) bool {
		si, _ := clips[i]["_l2r_score"].(float64)
		sj, _ := clips[j]["_l2r_score"].(float64)
		if si == sj {
			ci, _ := clips[i]["content_score"].(float64)
			cj, _ := clips[j]["content_score"].(float64)
			return ci > cj
		}
		return si > sj
	})
}

func (a *App) loadLTRUserStats(ctx context.Context, userID string) ltrUserStats {
	stats := ltrUserStats{
		HoursSinceLastSession: 24.0 * 7,
		ChannelAffinity:       map[string]float64{},
		TopicAffinities:       map[string]struct{}{},
	}
	if userID == "" {
		return stats
	}

	var totalViews int
	var avgWatch float64
	var likeCount, saveCount int
	_ = a.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(AVG(COALESCE(watch_percentage, 0)), 0),
			COALESCE(SUM(CASE WHEN action IN ('like','watch_full','share') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN action = 'save' THEN 1 ELSE 0 END), 0)
		FROM interactions
		WHERE user_id = ?
	`, userID).Scan(&totalViews, &avgWatch, &likeCount, &saveCount)

	stats.TotalViews = float64(totalViews)
	stats.AvgWatchPercentage = avgWatch
	if totalViews > 0 {
		stats.LikeRate = float64(likeCount) / float64(totalViews)
		stats.SaveRate = float64(saveCount) / float64(totalViews)
	}

	var hoursSince sql.NullFloat64
	_ = a.db.QueryRowContext(ctx, `
		SELECT (julianday('now') - julianday(MAX(created_at))) * 24.0
		FROM interactions
		WHERE user_id = ?
	`, userID).Scan(&hoursSince)
	if hoursSince.Valid && !math.IsNaN(hoursSince.Float64) && !math.IsInf(hoursSince.Float64, 0) && hoursSince.Float64 >= 0 {
		stats.HoursSinceLastSession = hoursSince.Float64
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT COALESCE(c.source_id, ''), COUNT(*)
		FROM interactions i
		JOIN clips c ON c.id = i.clip_id
		WHERE i.user_id = ?
		GROUP BY c.source_id
	`, userID)
	if err == nil {
		for rows.Next() {
			var sourceID string
			var count float64
			if rows.Scan(&sourceID, &count) == nil {
				stats.ChannelAffinity[sourceID] = count
			}
		}
		rows.Close()
	}

	topicRows, err := a.db.QueryContext(ctx, `SELECT topic_id FROM user_topic_affinities WHERE user_id = ?`, userID)
	if err == nil {
		for topicRows.Next() {
			var topicID string
			if topicRows.Scan(&topicID) == nil {
				stats.TopicAffinities[topicID] = struct{}{}
			}
		}
		topicRows.Close()
	}

	return stats
}

func (a *App) loadClipTopicStats(ctx context.Context, clipIDs []string, userTopics map[string]struct{}) (map[string]int, map[string]int) {
	topicCount := make(map[string]int, len(clipIDs))
	topicOverlap := make(map[string]int, len(clipIDs))
	if len(clipIDs) == 0 {
		return topicCount, topicOverlap
	}

	ph := make([]string, len(clipIDs))
	args := make([]interface{}, len(clipIDs))
	for i, id := range clipIDs {
		ph[i] = "?"
		args[i] = id
	}

	rows, err := a.db.QueryContext(ctx,
		`SELECT clip_id, topic_id FROM clip_topics WHERE clip_id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return topicCount, topicOverlap
	}
	defer rows.Close()

	for rows.Next() {
		var clipID, topicID string
		if rows.Scan(&clipID, &topicID) != nil {
			continue
		}
		topicCount[clipID]++
		if _, ok := userTopics[topicID]; ok {
			topicOverlap[clipID]++
		}
	}

	return topicCount, topicOverlap
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

