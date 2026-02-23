package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	limit := 20
	dedupeSeen24h := true
	var topicWeights map[string]float64

	if userID != "" {
		var topicWeightsJSON string
		var dedupeSeen24hRaw int
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT COALESCE(topic_weights, '{}'), COALESCE(dedupe_seen_24h, 1) FROM user_preferences WHERE user_id = ?`,
			userID,
		).Scan(&topicWeightsJSON, &dedupeSeen24hRaw); err == nil {
			json.Unmarshal([]byte(topicWeightsJSON), &topicWeights)
			dedupeSeen24h = dedupeSeen24hRaw == 1
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
					a.rankFeed(r.Context(), clips, userID, topicWeights)
					writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips), "filter_id": filterID})
					return
				}
			}
		}
	}

	var rows *sql.Rows
	var err error

	if userID != "" {
		rows, err = a.db.QueryContext(r.Context(), `
			WITH prefs AS (
				SELECT exploration_rate, min_clip_seconds, max_clip_seconds, dedupe_seen_24h
				FROM user_preferences WHERE user_id = ?
			),
			seen AS (
				SELECT clip_id FROM interactions
				WHERE user_id = ? AND created_at > datetime('now', '-24 hours')
			)
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform, s.url,
			       COALESCE(c.source_id, ''),
			       CAST(LENGTH(COALESCE(c.transcript, '')) AS REAL),
			       CAST(COALESCE(c.file_size_bytes, 0) AS REAL),
			       COALESCE((julianday('now') - julianday(c.created_at)) * 24.0, 0)
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			  AND (COALESCE((SELECT dedupe_seen_24h FROM prefs), 1) = 0 OR c.id NOT IN (SELECT clip_id FROM seen))
			  AND c.duration_seconds >= COALESCE((SELECT min_clip_seconds FROM prefs), 5)
			  AND c.duration_seconds <= COALESCE((SELECT max_clip_seconds FROM prefs), 120)
			ORDER BY
			    (c.content_score * (1.0 - COALESCE((SELECT exploration_rate FROM prefs), 0.3)))
			    + (CAST(ABS(RANDOM()) AS REAL) / 9223372036854775807.0
			       * COALESCE((SELECT exploration_rate FROM prefs), 0.3))
			    DESC
			LIMIT ?
		`, userID, userID, limit)
	} else {
		rows, err = a.db.QueryContext(r.Context(), `
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform, s.url,
			       COALESCE(c.source_id, ''),
			       CAST(LENGTH(COALESCE(c.transcript, '')) AS REAL),
			       CAST(COALESCE(c.file_size_bytes, 0) AS REAL),
			       COALESCE((julianday('now') - julianday(c.created_at)) * 24.0, 0)
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			ORDER BY (c.content_score * 0.7)
			    + (CAST(ABS(RANDOM()) AS REAL) / 9223372036854775807.0 * 0.3) DESC
			LIMIT ?
		`, limit)
	}

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to fetch feed"})
		return
	}
	defer rows.Close()

	clips := scanClips(rows)
	a.rankFeed(r.Context(), clips, userID, topicWeights)
	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, 400, map[string]string{"error": "q required"})
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT c.id, c.title, c.duration_seconds, c.thumbnail_key,
		       c.topics, c.content_score, s.platform, s.channel_name, s.url
		FROM clips_fts
		JOIN clips c ON clips_fts.clip_id = c.id
		LEFT JOIN sources s ON c.source_id = s.id
		WHERE clips_fts MATCH ? AND c.status = 'ready'
		ORDER BY bm25(clips_fts), c.content_score DESC
		LIMIT 20
	`, q)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "search failed"})
		return
	}
	defer rows.Close()

	var hits []map[string]interface{}
	for rows.Next() {
		var id, title, thumbnailKey, topicsJSON string
		var duration, score float64
		var platform, channelName, sourceURL *string
		rows.Scan(&id, &title, &duration, &thumbnailKey, &topicsJSON, &score, &platform, &channelName, &sourceURL)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		hits = append(hits, map[string]interface{}{
			"id": id, "title": title, "duration_seconds": duration,
			"thumbnail_key": thumbnailKey, "topics": topics,
			"content_score": score, "platform": platform, "channel_name": channelName,
			"source_url": sourceURL,
		})
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
