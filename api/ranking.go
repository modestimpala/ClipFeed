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

// FeedPrefs holds per-user algorithm tuning preferences.
type FeedPrefs struct {
	DiversityMix  float64 // 0 = no diversity reranking, 1 = maximum diversity
	TrendingBoost bool    // whether to boost trending clips
	FreshnessBias float64 // 0 = old content ok, 1 = strongly prefer fresh
}

func (a *App) rankFeed(ctx context.Context, clips []map[string]interface{}, userID string, topicWeights map[string]float64, fp FeedPrefs) {
	if len(clips) == 0 {
		return
	}

	if model := a.getLTRModel(); model != nil && len(model.Trees) > 0 {
		a.applyLTRRanking(ctx, clips, userID, model)
	} else {
		a.applyTopicBoost(ctx, clips, userID, topicWeights)
	}

	if fp.TrendingBoost {
		a.applyTrendingBoost(ctx, clips)
	}

	if fp.DiversityMix > 0 {
		a.applyDiversityPenalty(clips, fp.DiversityMix)
	}

	for _, clip := range clips {
		delete(clip, "_source_id")
		delete(clip, "_transcript_length")
		delete(clip, "_file_size_bytes")
		delete(clip, "_age_hours")
		delete(clip, "_l2r_score")
		delete(clip, "_score")
	}
}

func (a *App) applyDiversityPenalty(clips []map[string]interface{}, diversityMix float64) {
	if len(clips) <= 1 {
		return
	}

	// Scale penalty strengths by diversity_mix: 0=no penalty, 1=max penalty
	// Base decay rates at mix=1: topic 0.6, channel 0.5, platform 0.84
	topicDecay := 1.0 - diversityMix*0.4
	channelDecay := 1.0 - diversityMix*0.5
	platformDecay := 1.0 - diversityMix*0.16

	candidates := make([]map[string]interface{}, len(clips))
	copy(candidates, clips)

	for i, clip := range candidates {
		score := float64(len(candidates) - i)
		if s, ok := clip["_l2r_score"].(float64); ok {
			score = s
		} else if s, ok := clip["_score"].(float64); ok {
			score = s
		}
		if score <= 0 {
			score = 0.0001
		}
		clip["_div_score"] = score
	}

	seenTopics := make(map[string]int)
	seenChannels := make(map[string]int)
	seenPlatforms := make(map[string]int)

	for i := 0; i < len(clips); i++ {
		bestIdx := 0
		bestScore := -1.0

		for j, clip := range candidates {
			score, _ := clip["_div_score"].(float64)

			topicPenalty := 1.0
			if topics, ok := clip["topics"].([]string); ok {
				for _, t := range topics {
					if count, ok := seenTopics[t]; ok {
						topicPenalty *= math.Pow(topicDecay, float64(count))
					}
				}
			}

			channelPenalty := 1.0
			if ch, ok := clip["channel_name"].(*string); ok && ch != nil && *ch != "" {
				if count, ok := seenChannels[*ch]; ok {
					channelPenalty *= math.Pow(channelDecay, float64(count))
				}
			}

			// Platform diversity: gentle penalty to avoid all-YouTube or all-TikTok feeds
			platformPenalty := 1.0
			if p, ok := clip["platform"].(*string); ok && p != nil && *p != "" {
				if count, ok := seenPlatforms[*p]; ok {
					platformPenalty *= math.Pow(platformDecay, float64(count))
				}
			}

			finalScore := score * topicPenalty * channelPenalty * platformPenalty
			if finalScore > bestScore {
				bestScore = finalScore
				bestIdx = j
			}
		}

		bestClip := candidates[bestIdx]
		clips[i] = bestClip

		if topics, ok := bestClip["topics"].([]string); ok {
			for _, t := range topics {
				seenTopics[t]++
			}
		}
		if ch, ok := bestClip["channel_name"].(*string); ok && ch != nil && *ch != "" {
			seenChannels[*ch]++
		}
		if p, ok := bestClip["platform"].(*string); ok && p != nil && *p != "" {
			seenPlatforms[*p]++
		}

		candidates = append(candidates[:bestIdx], candidates[bestIdx+1:]...)
	}

	for _, clip := range clips {
		delete(clip, "_div_score")
	}
}

// applyTrendingBoost applies a velocity-based boost to clips with recent engagement.
// Inspired by TikTok/YouTube trending signals: content gaining traction gets a lift.
func (a *App) applyTrendingBoost(ctx context.Context, clips []map[string]interface{}) {
	if len(clips) == 0 {
		return
	}

	var ids []string
	for _, c := range clips {
		if id, ok := c["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}

	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}

	dtExpr := a.db.DatetimeModifier("-6 hours")
	rows, err := a.db.QueryContext(ctx,
		`SELECT clip_id, COUNT(*) FROM interactions
		 WHERE clip_id IN (`+strings.Join(ph, ",")+`)
		   AND created_at > `+dtExpr+`
		 GROUP BY clip_id`, args...)
	if err != nil {
		return
	}
	defer rows.Close()

	velocity := make(map[string]float64)
	for rows.Next() {
		var cid string
		var count float64
		if err := rows.Scan(&cid, &count); err != nil {
			continue
		}
		velocity[cid] = count
	}
	if err := rows.Err(); err != nil {
		log.Printf("applyTrendingBoost: rows iteration error: %v", err)
	}

	if len(velocity) == 0 {
		return
	}

	for _, clip := range clips {
		id, _ := clip["id"].(string)
		if v, ok := velocity[id]; ok && v > 0 {
			// Log-scaled trending boost: avoids runaway popularity while rewarding traction
			trendBoost := 1.0 + math.Log1p(v)*0.1
			if s, ok := clip["_l2r_score"].(float64); ok {
				clip["_l2r_score"] = s * trendBoost
			} else if s, ok := clip["_score"].(float64); ok {
				clip["_score"] = s * trendBoost
			}
		}
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
	if err := a.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(AVG(COALESCE(watch_percentage, 0)), 0),
			COALESCE(SUM(CASE WHEN action IN ('like','watch_full','share') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN action = 'save' THEN 1 ELSE 0 END), 0)
		FROM interactions
		WHERE user_id = ?
	`, userID).Scan(&totalViews, &avgWatch, &likeCount, &saveCount); err != nil {
		log.Printf("loadLTRUserStats: user stats query failed: %v", err)
	}
	stats.TotalViews = float64(totalViews)
	stats.AvgWatchPercentage = avgWatch
	if totalViews > 0 {
		stats.LikeRate = float64(likeCount) / float64(totalViews)
		stats.SaveRate = float64(saveCount) / float64(totalViews)
	}

	ageExpr := a.db.AgeHoursExpr("MAX(created_at)")
	var hoursSince sql.NullFloat64
	if err := a.db.QueryRowContext(ctx, `
		SELECT `+ageExpr+`
		FROM interactions
		WHERE user_id = ?
	`, userID).Scan(&hoursSince); err != nil {
		log.Printf("loadLTRUserStats: hours-since query failed: %v", err)
	}
	if hoursSince.Valid && !math.IsNaN(hoursSince.Float64) && !math.IsInf(hoursSince.Float64, 0) && hoursSince.Float64 >= 0 {
		stats.HoursSinceLastSession = hoursSince.Float64
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT COALESCE(c.source_id, ''),
		       SUM(CASE
		           WHEN i.action IN ('dislike', 'skip') THEN -0.5
		           WHEN i.action IN ('like', 'save', 'share') THEN 2.0
		           WHEN i.action = 'watch_full' THEN 1.5
		           WHEN COALESCE(i.watch_percentage, 0) >= 0.75 THEN 1.0 + COALESCE(i.watch_percentage, 0)
		           WHEN COALESCE(i.watch_percentage, 0) < 0.25 AND COALESCE(i.watch_percentage, 0) > 0 THEN -0.3
		           ELSE 0.5
		       END)
		FROM interactions i
		JOIN clips c ON c.id = i.clip_id
		WHERE i.user_id = ?
		GROUP BY c.source_id
	`, userID)
	if err == nil {
		for rows.Next() {
			var sourceID string
			var affinity float64
			if rows.Scan(&sourceID, &affinity) == nil {
				stats.ChannelAffinity[sourceID] = affinity
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("loadLTRUserStats: channel affinity rows error: %v", err)
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
		if err := topicRows.Err(); err != nil {
			log.Printf("loadLTRUserStats: topic affinity rows error: %v", err)
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
	if err := rows.Err(); err != nil {
		log.Printf("loadClipTopicStats: rows iteration error: %v", err)
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

	// TODO: For production scale (>10k clips), this brute-force text/visual embedding
	// cosine similarity check should be replaced with an ANN index (e.g. pgvector or sqlite-vss).
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT e.clip_id, e.text_embedding, e.visual_embedding,
		       c.title, c.thumbnail_key, c.duration_seconds, c.content_score
		FROM clip_embeddings e
		JOIN clips c ON e.clip_id = c.id AND c.status = 'ready'
		WHERE e.clip_id != ?
		LIMIT 500
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
		if err := rows.Scan(&cid, &tBlob, &vBlob, &title, &thumbKey, &dur, &cs); err != nil {
			continue
		}

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
				"thumbnail_url": thumbnailURL(a.cfg.MinioBucket, thumbKey),
				"duration_seconds": dur, "content_score": cs, "similarity": math.Round(sim*1000) / 1000,
			},
			score: sim,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("handleSimilarClips: rows iteration error: %v", err)
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

