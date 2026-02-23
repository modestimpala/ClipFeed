package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const topicDecayPerHop = 0.7
const maxLateralHops = 2

type TopicNode struct {
	ID        string
	Name      string
	Slug      string
	Path      string
	ParentID  string
	Depth     int
	ClipCount int
}

type TopicEdge struct {
	TargetID string
	Relation string
	Weight   float64
}

type TopicGraph struct {
	nodes    map[string]*TopicNode
	bySlug   map[string]*TopicNode
	byName   map[string]*TopicNode
	children map[string][]string
	edges    map[string][]TopicEdge
}

func (g *TopicGraph) resolveByName(name string) *TopicNode {
	if g == nil {
		return nil
	}
	if n, ok := g.byName[strings.ToLower(name)]; ok {
		return n
	}
	return nil
}

func (g *TopicGraph) computeBoost(clipTopicIDs []string, userAffinities map[string]float64) float64 {
	if len(clipTopicIDs) == 0 || len(userAffinities) == 0 {
		return 1.0
	}

	totalBoost := 0.0
	matchCount := 0

	for _, ctID := range clipTopicIDs {
		bestBoost := 0.0

		if w, ok := userAffinities[ctID]; ok {
			bestBoost = w
		}

		node := g.nodes[ctID]
		if node != nil {
			// Walk ancestors: clip tagged "carbonara" matches user affinity for "cooking" with decay
			hops := 0
			current := node
			for current.ParentID != "" {
				hops++
				if w, ok := userAffinities[current.ParentID]; ok {
					decayed := w * math.Pow(topicDecayPerHop, float64(hops))
					if decayed > bestBoost {
						bestBoost = decayed
					}
				}
				current = g.nodes[current.ParentID]
				if current == nil {
					break
				}
			}

			// Walk descendants: user likes "cooking", clip tagged "carbonara" gets boost
			g.walkDescendants(ctID, 1, func(childID string, depth int) {
				if w, ok := userAffinities[childID]; ok {
					decayed := w * math.Pow(topicDecayPerHop, float64(depth))
					if decayed > bestBoost {
						bestBoost = decayed
					}
				}
			})
		}

		// Multi-hop lateral edges
		g.walkLaterals(ctID, maxLateralHops, func(targetID string, hops int, weight float64) {
			if w, ok := userAffinities[targetID]; ok {
				lateral := w * weight * math.Pow(topicDecayPerHop, float64(hops))
				if lateral > bestBoost {
					bestBoost = lateral
				}
			}
		})

		if bestBoost > 0 {
			totalBoost += bestBoost
			matchCount++
		}
	}

	if matchCount == 0 {
		return 1.0
	}
	return totalBoost / float64(matchCount)
}

func (g *TopicGraph) walkDescendants(nodeID string, depth int, fn func(childID string, depth int)) {
	if depth > 3 {
		return
	}
	for _, childID := range g.children[nodeID] {
		fn(childID, depth)
		g.walkDescendants(childID, depth+1, fn)
	}
}

func (g *TopicGraph) walkLaterals(nodeID string, maxHops int, fn func(targetID string, hops int, weight float64)) {
	type visit struct {
		id     string
		hops   int
		weight float64
	}
	seen := map[string]bool{nodeID: true}
	queue := []visit{}
	for _, edge := range g.edges[nodeID] {
		queue = append(queue, visit{edge.TargetID, 1, edge.Weight})
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if seen[v.id] || v.hops > maxHops {
			continue
		}
		seen[v.id] = true
		fn(v.id, v.hops, v.weight)
		if v.hops < maxHops {
			for _, edge := range g.edges[v.id] {
				if !seen[edge.TargetID] {
					queue = append(queue, visit{edge.TargetID, v.hops + 1, v.weight * edge.Weight})
				}
			}
		}
	}
}

func (a *App) getTopicGraph() *TopicGraph {
	a.tgMu.RLock()
	defer a.tgMu.RUnlock()
	return a.topicGraph
}

func (a *App) loadTopicGraph() *TopicGraph {
	g := &TopicGraph{
		nodes:    make(map[string]*TopicNode),
		bySlug:   make(map[string]*TopicNode),
		byName:   make(map[string]*TopicNode),
		children: make(map[string][]string),
		edges:    make(map[string][]TopicEdge),
	}

	rows, err := a.db.Query("SELECT id, name, slug, path, parent_id, depth, clip_count FROM topics")
	if err != nil {
		log.Printf("topic graph load failed: %v", err)
		return g
	}
	defer rows.Close()

	for rows.Next() {
		var n TopicNode
		var parentID sql.NullString
		rows.Scan(&n.ID, &n.Name, &n.Slug, &n.Path, &parentID, &n.Depth, &n.ClipCount)
		if parentID.Valid {
			n.ParentID = parentID.String
		}
		g.nodes[n.ID] = &n
		g.bySlug[n.Slug] = &n
		g.byName[strings.ToLower(n.Name)] = &n
		if parentID.Valid {
			g.children[parentID.String] = append(g.children[parentID.String], n.ID)
		}
	}

	edgeRows, err := a.db.Query("SELECT source_id, target_id, relation, weight FROM topic_edges")
	if err != nil {
		log.Printf("topic edges load failed: %v", err)
		return g
	}
	defer edgeRows.Close()

	edgeCount := 0
	for edgeRows.Next() {
		var sourceID, targetID, relation string
		var weight float64
		edgeRows.Scan(&sourceID, &targetID, &relation, &weight)
		g.edges[sourceID] = append(g.edges[sourceID], TopicEdge{
			TargetID: targetID,
			Relation: relation,
			Weight:   weight,
		})
		edgeCount++
	}

	log.Printf("Topic graph loaded: %d nodes, %d edges", len(g.nodes), edgeCount)
	return g
}

func (a *App) refreshTopicGraph() {
	g := a.loadTopicGraph()
	a.tgMu.Lock()
	a.topicGraph = g
	a.tgMu.Unlock()
}

func (a *App) topicGraphRefreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		a.refreshTopicGraph()
	}
}

// applyTopicBoost re-ranks clips using graph-aware topic affinity, falling back to legacy string matching.
func (a *App) applyTopicBoost(ctx context.Context, clips []map[string]interface{}, userID string, topicWeights map[string]float64) {
	g := a.getTopicGraph()
	hasGraph := g != nil && len(g.nodes) > 0

	if len(topicWeights) == 0 && !hasGraph {
		return
	}

	userAffinities := make(map[string]float64)

	if hasGraph {
		for name, weight := range topicWeights {
			if node := g.resolveByName(name); node != nil {
				userAffinities[node.ID] = weight
			}
		}
		if userID != "" {
			rows, err := a.db.QueryContext(ctx,
				`SELECT topic_id, weight FROM user_topic_affinities WHERE user_id = ?`, userID)
			if err == nil {
				for rows.Next() {
					var tid string
					var w float64
					rows.Scan(&tid, &w)
					userAffinities[tid] = w
				}
				rows.Close()
			}
		}
	}

	clipTopicMap := make(map[string][]string)
	if hasGraph {
		var ids []string
		for _, c := range clips {
			if id, ok := c["id"].(string); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			ph := make([]string, len(ids))
			args := make([]interface{}, len(ids))
			for i, id := range ids {
				ph[i] = "?"
				args[i] = id
			}
			rows, err := a.db.QueryContext(ctx,
				`SELECT clip_id, topic_id FROM clip_topics WHERE clip_id IN (`+strings.Join(ph, ",")+`)`, args...)
			if err == nil {
				for rows.Next() {
					var cid, tid string
					rows.Scan(&cid, &tid)
					clipTopicMap[cid] = append(clipTopicMap[cid], tid)
				}
				rows.Close()
			}
		}
	}

	// Load user profile embedding for similarity blending
	var userEmb []float32
	if userID != "" {
		var blob []byte
		row := a.db.QueryRowContext(ctx, `SELECT text_embedding FROM user_embeddings WHERE user_id = ?`, userID)
		if row.Scan(&blob) == nil {
			userEmb = blobToFloat32(blob)
		}
	}

	// Load clip embeddings if user embedding exists
	clipEmbMap := make(map[string][]float32)
	if len(userEmb) > 0 {
		var ids []string
		for _, c := range clips {
			if id, ok := c["id"].(string); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			ph := make([]string, len(ids))
			args := make([]interface{}, len(ids))
			for i, id := range ids {
				ph[i] = "?"
				args[i] = id
			}
			embRows, err := a.db.QueryContext(ctx,
				`SELECT clip_id, text_embedding FROM clip_embeddings WHERE clip_id IN (`+strings.Join(ph, ",")+`)`, args...)
			if err == nil {
				for embRows.Next() {
					var cid string
					var blob []byte
					embRows.Scan(&cid, &blob)
					if v := blobToFloat32(blob); v != nil {
						clipEmbMap[cid] = v
					}
				}
				embRows.Close()
			}
		}
	}

	for i, clip := range clips {
		contentScore, _ := clip["content_score"].(float64)
		clipID, _ := clip["id"].(string)

		graphBoost := 1.0
		if graphTopics := clipTopicMap[clipID]; len(graphTopics) > 0 && hasGraph && len(userAffinities) > 0 {
			graphBoost = g.computeBoost(graphTopics, userAffinities)
		} else if len(topicWeights) > 0 {
			topics, _ := clip["topics"].([]string)
			graphBoost = computeTopicBoost(topics, topicWeights)
		}

		embSim := 0.0
		if clipEmb, ok := clipEmbMap[clipID]; ok && len(userEmb) > 0 {
			embSim = cosineSimilarity(userEmb, clipEmb)
			if embSim < 0 {
				embSim = 0
			}
		}

		var boost float64
		if embSim > 0 {
			boost = graphBoost*0.6 + embSim*0.4
		} else {
			boost = graphBoost
		}

		clips[i]["_score"] = contentScore * boost
	}

	sort.SliceStable(clips, func(i, j int) bool {
		si, _ := clips[i]["_score"].(float64)
		sj, _ := clips[j]["_score"].(float64)
		return si > sj
	})
	for _, clip := range clips {
		delete(clip, "_score")
	}
}

func (a *App) handleGetTopics(w http.ResponseWriter, r *http.Request) {
	// Use topics table when populated; otherwise fall back to legacy JSON scan
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, name, slug, path, parent_id, depth, clip_count
		FROM topics ORDER BY clip_count DESC LIMIT 50
	`)
	if err == nil {
		defer rows.Close()
		var topics []map[string]interface{}
		for rows.Next() {
			var id, name, slug, path string
			var parentID sql.NullString
			var depth, clipCount int
			rows.Scan(&id, &name, &slug, &path, &parentID, &depth, &clipCount)
			t := map[string]interface{}{
				"id": id, "name": name, "slug": slug,
				"path": path, "depth": depth, "clip_count": clipCount,
			}
			if parentID.Valid {
				t["parent_id"] = parentID.String
			}
			topics = append(topics, t)
		}
		if len(topics) > 0 {
			writeJSON(w, 200, map[string]interface{}{"topics": topics})
			return
		}
	}

	a.handleGetTopicsLegacy(w, r)
}

func (a *App) handleGetTopicsLegacy(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT topics FROM clips WHERE status = 'ready' AND topics != '[]'
	`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to fetch topics"})
		return
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var topicsJSON string
		rows.Scan(&topicsJSON)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		for _, t := range topics {
			if t != "" {
				counts[t]++
			}
		}
	}

	type topicEntry struct {
		Name      string `json:"name"`
		ClipCount int    `json:"clip_count"`
	}
	var result []topicEntry
	for name, count := range counts {
		result = append(result, topicEntry{Name: name, ClipCount: count})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ClipCount > result[j].ClipCount
	})
	if len(result) > 20 {
		result = result[:20]
	}

	writeJSON(w, 200, map[string]interface{}{"topics": result})
}

func (a *App) handleGetTopicTree(w http.ResponseWriter, r *http.Request) {
	g := a.getTopicGraph()
	if g == nil || len(g.nodes) == 0 {
		writeJSON(w, 200, map[string]interface{}{"tree": []interface{}{}})
		return
	}

	type treeNode struct {
		ID        string      `json:"id"`
		Name      string      `json:"name"`
		Slug      string      `json:"slug"`
		ClipCount int         `json:"clip_count"`
		Children  []*treeNode `json:"children,omitempty"`
	}

	nodeMap := make(map[string]*treeNode)
	for id, n := range g.nodes {
		nodeMap[id] = &treeNode{
			ID: n.ID, Name: n.Name, Slug: n.Slug, ClipCount: n.ClipCount,
		}
	}

	var roots []*treeNode
	for id, n := range g.nodes {
		tn := nodeMap[id]
		if n.ParentID == "" {
			roots = append(roots, tn)
		} else if parent, ok := nodeMap[n.ParentID]; ok {
			parent.Children = append(parent.Children, tn)
		} else {
			roots = append(roots, tn)
		}
	}

	sort.Slice(roots, func(i, j int) bool {
		return roots[i].ClipCount > roots[j].ClipCount
	})

	writeJSON(w, 200, map[string]interface{}{"tree": roots})
}

func (a *App) expandTopicDescendants(topicNames []string) []string {
	g := a.getTopicGraph()
	if g == nil {
		return nil
	}
	seen := make(map[string]bool)
	var ids []string
	for _, name := range topicNames {
		node := g.resolveByName(name)
		if node == nil {
			continue
		}
		var walk func(id string)
		walk = func(id string) {
			if seen[id] {
				return
			}
			seen[id] = true
			ids = append(ids, id)
			for _, child := range g.children[id] {
				walk(child)
			}
		}
		walk(node.ID)
	}
	return ids
}
