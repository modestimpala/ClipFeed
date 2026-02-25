package feed

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// --- Learning-to-Rank ---

// LTRTree represents a single node in a gradient-boosted decision tree.
type LTRTree struct {
	FeatureIndex int     `json:"feature_index"`
	Threshold    float64 `json:"threshold"`
	LeftChild    int     `json:"left_child"`
	RightChild   int     `json:"right_child"`
	LeafValue    float64 `json:"leaf_value"`
	IsLeaf       bool    `json:"is_leaf"`
}

// LTRModel is a trained learning-to-rank model (gradient-boosted trees).
type LTRModel struct {
	Trees        [][]LTRTree `json:"trees"`
	FeatureNames []string    `json:"feature_names"`
	NumFeatures  int         `json:"num_features"`
}

// Score returns the model's prediction for the given feature vector.
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
	// steps is bounded by len(nodes): a valid tree of N nodes has at most N−1
	// edges on any root-to-leaf path. Exceeding that means a cyclic reference.
	for steps := 0; steps < len(nodes) && idx < len(nodes); steps++ {
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

// GetLTRModel returns the current in-memory LTR model (thread-safe).
func (h *Handler) GetLTRModel() *LTRModel {
	h.ltrMu.RLock()
	defer h.ltrMu.RUnlock()
	return h.ltrModel
}

// LoadLTRModel reads the LTR model from disk.
func (h *Handler) LoadLTRModel() *LTRModel {
	modelPath := h.LTRModelPath
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

// SetLTRModel replaces the in-memory LTR model (thread-safe).
func (h *Handler) SetLTRModel(m *LTRModel) {
	h.ltrMu.Lock()
	h.ltrModel = m
	h.ltrMu.Unlock()
}

// LTRModelRefreshLoop periodically reloads the LTR model from disk.
func (h *Handler) LTRModelRefreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if m := h.LoadLTRModel(); m != nil {
			h.SetLTRModel(m)
		}
	}
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

// RankFeed post-processes the candidate clip list with LTR, topic boosts,
// trending signals, and diversity reranking.
func (h *Handler) RankFeed(ctx context.Context, clips []map[string]interface{}, userID string, topicWeights map[string]float64, fp FeedPrefs) {
	if len(clips) == 0 {
		return
	}

	if model := h.GetLTRModel(); model != nil && len(model.Trees) > 0 {
		h.applyLTRRanking(ctx, clips, userID, model)
	} else {
		h.applyTopicBoost(ctx, clips, userID, topicWeights)
	}

	if fp.TrendingBoost {
		h.applyTrendingBoost(ctx, clips)
	}

	if fp.DiversityMix > 0 {
		h.applyDiversityPenalty(clips, fp.DiversityMix)
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

func (h *Handler) applyDiversityPenalty(clips []map[string]interface{}, diversityMix float64) {
	if len(clips) <= 1 {
		return
	}

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

func (h *Handler) applyTrendingBoost(ctx context.Context, clips []map[string]interface{}) {
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

	dtExpr := h.DB.DatetimeModifier("-6 hours")
	rows, err := h.DB.QueryContext(ctx,
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
			trendBoost := 1.0 + math.Log1p(v)*0.1
			if s, ok := clip["_l2r_score"].(float64); ok {
				clip["_l2r_score"] = s * trendBoost
			} else if s, ok := clip["_score"].(float64); ok {
				clip["_score"] = s * trendBoost
			}
		}
	}
}

func (h *Handler) applyLTRRanking(ctx context.Context, clips []map[string]interface{}, userID string, model *LTRModel) {
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

	stats := h.loadLTRUserStats(ctx, userID)
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

	topicCount, topicOverlap := h.loadClipTopicStats(ctx, clipIDs, stats.TopicAffinities)

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

func (h *Handler) loadLTRUserStats(ctx context.Context, userID string) ltrUserStats {
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
	if err := h.DB.QueryRowContext(ctx, `
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

	ageExpr := h.DB.AgeHoursExpr("MAX(created_at)")
	var hoursSince sql.NullFloat64
	if err := h.DB.QueryRowContext(ctx, `
		SELECT `+ageExpr+`
		FROM interactions
		WHERE user_id = ?
	`, userID).Scan(&hoursSince); err != nil {
		log.Printf("loadLTRUserStats: hours-since query failed: %v", err)
	}
	if hoursSince.Valid && !math.IsNaN(hoursSince.Float64) && !math.IsInf(hoursSince.Float64, 0) && hoursSince.Float64 >= 0 {
		stats.HoursSinceLastSession = hoursSince.Float64
	}

	rows, err := h.DB.QueryContext(ctx, `
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

	topicRows, err := h.DB.QueryContext(ctx, `SELECT topic_id FROM user_topic_affinities WHERE user_id = ?`, userID)
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

func (h *Handler) loadClipTopicStats(ctx context.Context, clipIDs []string, userTopics map[string]struct{}) (map[string]int, map[string]int) {
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

	rows, err := h.DB.QueryContext(ctx,
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
