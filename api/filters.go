package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type FilterQuery struct {
	Topics        *FilterTopics `json:"topics,omitempty"`
	Channels      []string      `json:"channels,omitempty"`
	Duration      *FilterRange  `json:"duration,omitempty"`
	RecencyDays   int           `json:"recency_days,omitempty"`
	MinScore      float64       `json:"min_score,omitempty"`
	SimilarToClip string        `json:"similar_to_clip,omitempty"`
}

type FilterTopics struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
	Mode    string   `json:"mode"`
}

type FilterRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

func (a *App) handleCreateFilter(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	var req struct {
		Name      string          `json:"name"`
		Query     json.RawMessage `json:"query"`
		IsDefault bool            `json:"is_default"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "name and query required"})
		return
	}

	var fq FilterQuery
	if err := json.Unmarshal(req.Query, &fq); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid filter query"})
		return
	}

	id := uuid.New().String()
	isDefault := 0
	if req.IsDefault {
		isDefault = 1
	}

	_, err := a.db.ExecContext(r.Context(),
		`INSERT INTO saved_filters (id, user_id, name, query, is_default) VALUES (?, ?, ?, ?, ?)`,
		id, userID, req.Name, string(req.Query), isDefault)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to create filter"})
		return
	}
	writeJSON(w, 201, map[string]interface{}{"id": id, "name": req.Name})
}

func (a *App) handleListFilters(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, name, query, is_default, created_at FROM saved_filters WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list filters"})
		return
	}
	defer rows.Close()

	var filters []map[string]interface{}
	for rows.Next() {
		var id, name, queryStr, createdAt string
		var isDefault int
		rows.Scan(&id, &name, &queryStr, &isDefault, &createdAt)
		filters = append(filters, map[string]interface{}{
			"id": id, "name": name, "query": json.RawMessage(queryStr),
			"is_default": isDefault == 1, "created_at": createdAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"filters": filters})
}

func (a *App) handleUpdateFilter(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	filterID := chi.URLParam(r, "id")
	var req struct {
		Name      string          `json:"name"`
		Query     json.RawMessage `json:"query"`
		IsDefault *bool           `json:"is_default"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Name != "" {
		a.db.ExecContext(r.Context(), `UPDATE saved_filters SET name = ? WHERE id = ? AND user_id = ?`, req.Name, filterID, userID)
	}
	if req.Query != nil {
		a.db.ExecContext(r.Context(), `UPDATE saved_filters SET query = ? WHERE id = ? AND user_id = ?`, string(req.Query), filterID, userID)
	}
	if req.IsDefault != nil {
		def := 0
		if *req.IsDefault {
			def = 1
		}
		a.db.ExecContext(r.Context(), `UPDATE saved_filters SET is_default = ? WHERE id = ? AND user_id = ?`, def, filterID, userID)
	}
	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (a *App) handleDeleteFilter(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	filterID := chi.URLParam(r, "id")
	a.db.ExecContext(r.Context(), `DELETE FROM saved_filters WHERE id = ? AND user_id = ?`, filterID, userID)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (a *App) applyFilterToFeed(ctx context.Context, fq *FilterQuery, userID string) ([]map[string]interface{}, error) {
	where := []string{"c.status = 'ready'"}
	var args []interface{}

	if fq.Duration != nil {
		if fq.Duration.Min > 0 {
			where = append(where, "c.duration_seconds >= ?")
			args = append(args, fq.Duration.Min)
		}
		if fq.Duration.Max > 0 {
			where = append(where, "c.duration_seconds <= ?")
			args = append(args, fq.Duration.Max)
		}
	}
	if fq.RecencyDays > 0 {
		where = append(where, fmt.Sprintf("c.created_at > datetime('now', '-%d days')", fq.RecencyDays))
	}
	if fq.MinScore > 0 {
		where = append(where, "c.content_score >= ?")
		args = append(args, fq.MinScore)
	}
	if len(fq.Channels) > 0 {
		ph := make([]string, len(fq.Channels))
		for i, ch := range fq.Channels {
			ph[i] = "?"
			args = append(args, ch)
		}
		where = append(where, "s.channel_name IN ("+strings.Join(ph, ",")+")")
	}

	// Topic inclusion via graph descendants
	if fq.Topics != nil && len(fq.Topics.Include) > 0 {
		var topicIDs []string
		if fq.Topics.Mode == "descendants" {
			topicIDs = a.expandTopicDescendants(fq.Topics.Include)
		} else {
			g := a.getTopicGraph()
			if g != nil {
				for _, name := range fq.Topics.Include {
					if n := g.resolveByName(name); n != nil {
						topicIDs = append(topicIDs, n.ID)
					}
				}
			}
		}
		if len(topicIDs) > 0 {
			ph := make([]string, len(topicIDs))
			for i, id := range topicIDs {
				ph[i] = "?"
				args = append(args, id)
			}
			where = append(where, "c.id IN (SELECT clip_id FROM clip_topics WHERE topic_id IN ("+strings.Join(ph, ",")+"))")
		}
	}

	// Topic exclusion
	if fq.Topics != nil && len(fq.Topics.Exclude) > 0 {
		var excludeIDs []string
		if fq.Topics.Mode == "descendants" {
			excludeIDs = a.expandTopicDescendants(fq.Topics.Exclude)
		} else {
			g := a.getTopicGraph()
			if g != nil {
				for _, name := range fq.Topics.Exclude {
					if n := g.resolveByName(name); n != nil {
						excludeIDs = append(excludeIDs, n.ID)
					}
				}
			}
		}
		if len(excludeIDs) > 0 {
			ph := make([]string, len(excludeIDs))
			for i, id := range excludeIDs {
				ph[i] = "?"
				args = append(args, id)
			}
			where = append(where, "c.id NOT IN (SELECT clip_id FROM clip_topics WHERE topic_id IN ("+strings.Join(ph, ",")+"))")
		}
	}

	// Exclude seen
	if userID != "" {
		where = append(where, "c.id NOT IN (SELECT clip_id FROM interactions WHERE user_id = ? AND created_at > datetime('now', '-24 hours'))")
		args = append(args, userID)
	}

	query := `SELECT c.id, c.title, c.description, c.duration_seconds,
	       c.thumbnail_key, c.topics, c.tags, c.content_score,
	       c.created_at, s.channel_name, s.platform, s.url
	FROM clips c LEFT JOIN sources s ON c.source_id = s.id
	WHERE ` + strings.Join(where, " AND ") + `
	ORDER BY c.content_score DESC LIMIT 20`

	args = append(args)
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanClips(rows), nil
}
