package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	limit := 20
	fetchLimit := limit * 3 // Over-fetch candidates; post-ranking trims to limit
	dedupeSeen24h := true
	var topicWeights map[string]float64
	feedPrefs := FeedPrefs{
		DiversityMix:  0.5,
		TrendingBoost: true,
		FreshnessBias: 0.5,
	}

	if userID != "" {
		var topicWeightsJSON string
		var dedupeSeen24hRaw int
		var diversityMix, freshnessBias float64
		var trendingBoost int
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT COALESCE(topic_weights, '{}'), COALESCE(dedupe_seen_24h, 1),
			        COALESCE(diversity_mix, 0.5), COALESCE(trending_boost, 1), COALESCE(freshness_bias, 0.5)
			 FROM user_preferences WHERE user_id = ?`,
			userID,
		).Scan(&topicWeightsJSON, &dedupeSeen24hRaw, &diversityMix, &trendingBoost, &freshnessBias); err == nil {
			if err := json.Unmarshal([]byte(topicWeightsJSON), &topicWeights); err != nil {
				topicWeights = nil
			}
			dedupeSeen24h = dedupeSeen24hRaw == 1
			feedPrefs.DiversityMix = diversityMix
			feedPrefs.TrendingBoost = trendingBoost == 1
			feedPrefs.FreshnessBias = freshnessBias
		}
	}

	// Check for saved filter
	if filterID := r.URL.Query().Get("filter"); filterID != "" && userID != "" {
		var queryStr string
		err := a.db.QueryRowContext(r.Context(),
			`SELECT query FROM saved_filters WHERE id = ? AND user_id = ?`, filterID, userID,
		).Scan(&queryStr)
		if err == nil {
			var fq FilterQuery
			if json.Unmarshal([]byte(queryStr), &fq) == nil {
				clips, err := a.applyFilterToFeed(r.Context(), &fq, userID, dedupeSeen24h)
				if err == nil {
					a.rankFeed(r.Context(), clips, userID, topicWeights, feedPrefs)
					if len(clips) > limit {
						clips = clips[:limit]
					}
					addThumbnailURLs(clips, a.cfg.MinioBucket)
					writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips), "filter_id": filterID})
					return
				}
			}
		}
	}

	var rows *sql.Rows
	var err error

	if userID != "" {
		// freshness_bias → decay half-life: 0=672h (old ok), 0.5=168h (default), 1=24h (fresh only)
		halfLife := 24.0 + (1.0-feedPrefs.FreshnessBias)*648.0
		ageHours := a.db.AgeHoursExpr("c.created_at")
		randFloat := a.db.RandomFloat()
		seenCutoff := a.db.DatetimeModifier("-24 hours")

		rows, err = a.db.QueryContext(r.Context(), fmt.Sprintf(`
			WITH prefs AS (
				SELECT exploration_rate, min_clip_seconds, max_clip_seconds, dedupe_seen_24h
				FROM user_preferences WHERE user_id = ?
			),
			seen AS (
				SELECT clip_id FROM interactions
				WHERE user_id = ? AND created_at > %s
			)
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform, s.url,
			       COALESCE(c.source_id, ''),
			       CAST(LENGTH(COALESCE(c.transcript, '')) AS REAL),
			       CAST(COALESCE(c.file_size_bytes, 0) AS REAL),
			       COALESCE(%s, 0)
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			  AND (COALESCE((SELECT dedupe_seen_24h FROM prefs), 1) = 0 OR c.id NOT IN (SELECT clip_id FROM seen))
			  AND c.duration_seconds >= COALESCE((SELECT min_clip_seconds FROM prefs), 5)
			  AND c.duration_seconds <= COALESCE((SELECT max_clip_seconds FROM prefs), 120)
			ORDER BY
			    (c.content_score * EXP(-%s / ?) * (1.0 - COALESCE((SELECT exploration_rate FROM prefs), 0.3)))
			    + (%s * COALESCE((SELECT exploration_rate FROM prefs), 0.3))
			    DESC
			LIMIT ?
		`, seenCutoff, ageHours, ageHours, randFloat), userID, userID, halfLife, fetchLimit)
	} else {
		ageHours := a.db.AgeHoursExpr("c.created_at")
		randFloat := a.db.RandomFloat()

		rows, err = a.db.QueryContext(r.Context(), fmt.Sprintf(`
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform, s.url,
			       COALESCE(c.source_id, ''),
			       CAST(LENGTH(COALESCE(c.transcript, '')) AS REAL),
			       CAST(COALESCE(c.file_size_bytes, 0) AS REAL),
			       COALESCE(%s, 0)
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			ORDER BY (c.content_score * EXP(-%s / 168.0) * 0.7)
			    + (%s * 0.3) DESC
			LIMIT ?
		`, ageHours, ageHours, randFloat), fetchLimit)
	}

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to fetch feed"})
		return
	}
	defer rows.Close()

	clips := scanClips(rows)
	a.rankFeed(r.Context(), clips, userID, topicWeights, feedPrefs)
	if len(clips) > limit {
		clips = clips[:limit]
	}
	addThumbnailURLs(clips, a.cfg.MinioBucket)
	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, 400, map[string]string{"error": "q required"})
		return
	}

	var rows *sql.Rows
	var err error

	if a.db.IsPostgres() {
		rows, err = a.db.QueryContext(r.Context(), `
			SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
			       c.topics, c.content_score, s.platform, s.channel_name, s.url
			FROM clips_fts
			JOIN clips c ON clips_fts.clip_id = c.id
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE clips_fts.tsv @@ plainto_tsquery('english', ?) AND c.status = 'ready'
			ORDER BY ts_rank(clips_fts.tsv, plainto_tsquery('english', ?)) DESC, c.content_score DESC
			LIMIT 20
		`, q, q)
	} else {
		// Sanitize FTS5 query: escape double quotes and wrap in double quotes
		// to prevent query syntax injection (AND, OR, NOT, NEAR, etc.)
		ftsQ := `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
		rows, err = a.db.QueryContext(r.Context(), `
			SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
			       c.topics, c.content_score, s.platform, s.channel_name, s.url
			FROM clips_fts
			JOIN clips c ON clips_fts.clip_id = c.id
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE clips_fts MATCH ? AND c.status = 'ready'
			ORDER BY bm25(clips_fts), c.content_score DESC
			LIMIT 20
		`, ftsQ)
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "search failed"})
		return
	}
	defer rows.Close()

	var hits []map[string]interface{}
	for rows.Next() {
		var id, title, topicsJSON string
		var thumbnailKey *string
		var duration, score float64
		var platform, channelName, sourceURL *string
		if err := rows.Scan(&id, &title, &duration, &thumbnailKey, &topicsJSON, &score, &platform, &channelName, &sourceURL); err != nil {
			continue
		}
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		hits = append(hits, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "topics": topics,
			"content_score": score, "platform": platform, "channel_name": channelName,
			"source_url": sourceURL,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("handleSearch: rows iteration error: %v", err)
	}
	writeJSON(w, 200, map[string]interface{}{"hits": hits, "query": q, "total": len(hits)})
}

func computeTopicBoost(clipTopics []string, weights map[string]float64) float64 {
	if len(clipTopics) == 0 {
		return 1.0
	}
	sum := 0.0
	count := 0
	for _, t := range clipTopics {
		if w, ok := weights[t]; ok {
			sum += w
			count++
		}
	}
	if count == 0 {
		return 1.0
	}
	return sum / float64(count)
}
