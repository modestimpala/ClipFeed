package feed

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

	"clipfeed/httputil"
)

const topicDecayPerHop = 0.7
const maxLateralHops = 2

// normalizeTopicStem reduces topic names to a canonical stem for consolidation.
func normalizeTopicStem(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	if len(s) > 4 && strings.HasSuffix(s, "ies") {
		s = s[:len(s)-3] + "y"
	} else if len(s) > 3 && s[len(s)-1] == 's' && s[len(s)-2] != 's' && s[len(s)-2] != 'u' && s[len(s)-2] != 'i' {
		s = s[:len(s)-1]
	}
	return strings.Join(strings.Fields(s), " ")
}

// TopicNode represents a single topic in the hierarchy.
type TopicNode struct {
	ID        string
	Name      string
	Slug      string
	Path      string
	ParentID  string
	Depth     int
	ClipCount int
}

// TopicEdge represents a lateral relationship between topics.
type TopicEdge struct {
	TargetID string
	Relation string
	Weight   float64
}

// TopicGraph holds the in-memory topic hierarchy and edge graph.
type TopicGraph struct {
	Nodes     map[string]*TopicNode
	BySlug    map[string]*TopicNode
	ByName    map[string]*TopicNode
	Children  map[string][]string
	Edges     map[string][]TopicEdge
	Canonical map[string]string // topic_id → canonical topic_id for consolidated topics
}

// ResolveByName finds a topic node by its lowercase name.
func (g *TopicGraph) ResolveByName(name string) *TopicNode {
	if g == nil {
		return nil
	}
	if n, ok := g.ByName[strings.ToLower(name)]; ok {
		return n
	}
	return nil
}

// ComputeBoost computes the topic-affinity boost for a clip's topics.
func (g *TopicGraph) ComputeBoost(clipTopicIDs []string, userAffinities map[string]float64) float64 {
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
		if canonID, ok := g.Canonical[ctID]; ok {
			if w, ok := userAffinities[canonID]; ok && w > bestBoost {
				bestBoost = w
			}
		}

		node := g.Nodes[ctID]
		if node != nil {
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
				current = g.Nodes[current.ParentID]
				if current == nil {
					break
				}
			}

			g.walkDescendants(ctID, 1, func(childID string, depth int) {
				if w, ok := userAffinities[childID]; ok {
					decayed := w * math.Pow(topicDecayPerHop, float64(depth))
					if decayed > bestBoost {
						bestBoost = decayed
					}
				}
			})
		}

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
	for _, childID := range g.Children[nodeID] {
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
	for _, edge := range g.Edges[nodeID] {
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
			for _, edge := range g.Edges[v.id] {
				if !seen[edge.TargetID] {
					queue = append(queue, visit{edge.TargetID, v.hops + 1, v.weight * edge.Weight})
				}
			}
		}
	}
}

// GetTopicGraph returns the current in-memory topic graph (thread-safe).
func (h *Handler) GetTopicGraph() *TopicGraph {
	h.tgMu.RLock()
	defer h.tgMu.RUnlock()
	return h.topicGraph
}

// LoadTopicGraph reads topics and edges from the database.
func (h *Handler) LoadTopicGraph() *TopicGraph {
	g := &TopicGraph{
		Nodes:     make(map[string]*TopicNode),
		BySlug:    make(map[string]*TopicNode),
		ByName:    make(map[string]*TopicNode),
		Children:  make(map[string][]string),
		Edges:     make(map[string][]TopicEdge),
		Canonical: make(map[string]string),
	}

	rows, err := h.DB.Query("SELECT id, name, slug, path, parent_id, depth, clip_count FROM topics")
	if err != nil {
		log.Printf("topic graph load failed: %v", err)
		return g
	}
	defer rows.Close()

	for rows.Next() {
		var n TopicNode
		var parentID sql.NullString
		if err := rows.Scan(&n.ID, &n.Name, &n.Slug, &n.Path, &parentID, &n.Depth, &n.ClipCount); err != nil {
			continue
		}
		if parentID.Valid {
			n.ParentID = parentID.String
		}
		g.Nodes[n.ID] = &n
		g.BySlug[n.Slug] = &n
		g.ByName[strings.ToLower(n.Name)] = &n
		if parentID.Valid {
			g.Children[parentID.String] = append(g.Children[parentID.String], n.ID)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("topic graph node iteration error: %v", err)
	}

	edgeRows, err := h.DB.Query("SELECT source_id, target_id, relation, weight FROM topic_edges")
	if err != nil {
		log.Printf("topic edges load failed: %v", err)
		return g
	}
	defer edgeRows.Close()

	edgeCount := 0
	for edgeRows.Next() {
		var sourceID, targetID, relation string
		var weight float64
		if err := edgeRows.Scan(&sourceID, &targetID, &relation, &weight); err != nil {
			continue
		}
		g.Edges[sourceID] = append(g.Edges[sourceID], TopicEdge{
			TargetID: targetID,
			Relation: relation,
			Weight:   weight,
		})
		edgeCount++
	}
	if err := edgeRows.Err(); err != nil {
		log.Printf("topic graph edge iteration error: %v", err)
	}

	log.Printf("Topic graph loaded: %d nodes, %d edges", len(g.Nodes), edgeCount)

	// Topic consolidation
	stemGroups := make(map[string][]*TopicNode)
	for _, node := range g.Nodes {
		stem := normalizeTopicStem(node.Name)
		stemGroups[stem] = append(stemGroups[stem], node)
	}
	mergeCount := 0
	for _, group := range stemGroups {
		if len(group) <= 1 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			return group[i].ClipCount > group[j].ClipCount
		})
		canon := group[0]
		for _, node := range group[1:] {
			g.Canonical[node.ID] = canon.ID
			mergeCount++
		}
	}
	if mergeCount > 0 {
		log.Printf("Topic consolidation: %d topics merged into canonical forms", mergeCount)
	}

	return g
}

// RefreshTopicGraph reloads the topic graph from the database.
func (h *Handler) RefreshTopicGraph() {
	g := h.LoadTopicGraph()
	h.tgMu.Lock()
	h.topicGraph = g
	h.tgMu.Unlock()
}

// TopicGraphRefreshLoop periodically refreshes the topic graph.
func (h *Handler) TopicGraphRefreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		h.RefreshTopicGraph()
	}
}

// applyTopicBoost re-ranks clips using graph-aware topic affinity.
func (h *Handler) applyTopicBoost(ctx context.Context, clips []map[string]interface{}, userID string, topicWeights map[string]float64) {
	g := h.GetTopicGraph()
	hasGraph := g != nil && len(g.Nodes) > 0

	if len(topicWeights) == 0 && !hasGraph {
		return
	}

	userAffinities := make(map[string]float64)

	if hasGraph {
		for name, weight := range topicWeights {
			if node := g.ResolveByName(name); node != nil {
				userAffinities[node.ID] = weight
			}
		}
		if userID != "" {
			rows, err := h.DB.QueryContext(ctx,
				`SELECT topic_id, weight FROM user_topic_affinities WHERE user_id = ?`, userID)
			if err == nil {
				for rows.Next() {
					var tid string
					var w float64
					if err := rows.Scan(&tid, &w); err != nil {
						continue
					}
					userAffinities[tid] = w
				}
				if err := rows.Err(); err != nil {
					log.Printf("applyTopicBoost: user affinity rows error: %v", err)
				}
				rows.Close()
			}
		}
		if len(g.Canonical) > 0 {
			for tid, w := range userAffinities {
				if canonID, ok := g.Canonical[tid]; ok {
					if existing, exists := userAffinities[canonID]; !exists || w > existing {
						userAffinities[canonID] = w
					}
				}
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
			rows, err := h.DB.QueryContext(ctx,
				`SELECT clip_id, topic_id FROM clip_topics WHERE clip_id IN (`+strings.Join(ph, ",")+`)`, args...)
			if err == nil {
				for rows.Next() {
					var cid, tid string
					if err := rows.Scan(&cid, &tid); err != nil {
						continue
					}
					clipTopicMap[cid] = append(clipTopicMap[cid], tid)
				}
				if err := rows.Err(); err != nil {
					log.Printf("applyTopicBoost: clip topic rows error: %v", err)
				}
				rows.Close()
			}
		}
	}

	// Load user profile embedding for similarity blending
	var userEmb []float32
	if userID != "" {
		var blob []byte
		row := h.DB.QueryRowContext(ctx, `SELECT text_embedding FROM user_embeddings WHERE user_id = ?`, userID)
		if row.Scan(&blob) == nil {
			userEmb = BlobToFloat32(blob)
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
			embRows, err := h.DB.QueryContext(ctx,
				`SELECT clip_id, text_embedding FROM clip_embeddings WHERE clip_id IN (`+strings.Join(ph, ",")+`)`, args...)
			if err == nil {
				for embRows.Next() {
					var cid string
					var blob []byte
					if err := embRows.Scan(&cid, &blob); err != nil {
						continue
					}
					if v := BlobToFloat32(blob); v != nil {
						clipEmbMap[cid] = v
					}
				}
				if err := embRows.Err(); err != nil {
					log.Printf("applyTopicBoost: embedding rows error: %v", err)
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
			graphBoost = g.ComputeBoost(graphTopics, userAffinities)
		} else if len(topicWeights) > 0 {
			topics, _ := clip["topics"].([]string)
			graphBoost = ComputeTopicBoost(topics, topicWeights)
		}

		embSim := 0.0
		if clipEmb, ok := clipEmbMap[clipID]; ok && len(userEmb) > 0 {
			embSim = CosineSimilarity(userEmb, clipEmb)
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
}

// HandleGetTopics returns topics from the topics table, falling back to legacy JSON scan.
func (h *Handler) HandleGetTopics(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(), `
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
			if err := rows.Scan(&id, &name, &slug, &path, &parentID, &depth, &clipCount); err != nil {
				continue
			}
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
			httputil.WriteJSON(w, 200, map[string]interface{}{"topics": topics})
			return
		}
	}

	h.handleGetTopicsLegacy(w, r)
}

func (h *Handler) handleGetTopicsLegacy(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT topics FROM clips WHERE status = 'ready' AND topics != '[]'
	`)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to fetch topics"})
		return
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var topicsJSON string
		if err := rows.Scan(&topicsJSON); err != nil {
			continue
		}
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

	httputil.WriteJSON(w, 200, map[string]interface{}{"topics": result})
}

// HandleGetTopicTree returns the topic hierarchy as a nested tree.
func (h *Handler) HandleGetTopicTree(w http.ResponseWriter, r *http.Request) {
	g := h.GetTopicGraph()
	if g == nil || len(g.Nodes) == 0 {
		httputil.WriteJSON(w, 200, map[string]interface{}{"tree": []interface{}{}})
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
	for id, n := range g.Nodes {
		nodeMap[id] = &treeNode{
			ID: n.ID, Name: n.Name, Slug: n.Slug, ClipCount: n.ClipCount,
		}
	}

	var roots []*treeNode
	for id, n := range g.Nodes {
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

	httputil.WriteJSON(w, 200, map[string]interface{}{"tree": roots})
}

// ExpandTopicDescendants returns all topic IDs that are descendants of the given names.
func (h *Handler) ExpandTopicDescendants(topicNames []string) []string {
	g := h.GetTopicGraph()
	if g == nil {
		return nil
	}
	seen := make(map[string]bool)
	var ids []string
	for _, name := range topicNames {
		node := g.ResolveByName(name)
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
			for _, child := range g.Children[id] {
				walk(child)
			}
		}
		walk(node.ID)
	}
	return ids
}
