package main

import (
	"net/http/httptest"
	"testing"
)

func TestLTRModelScore_SumsLeafValues(t *testing.T) {
	model := &LTRModel{
		Trees: [][]LTRTree{
			{
				{FeatureIndex: 0, Threshold: 0.5, LeftChild: 1, RightChild: 2},
				{LeafValue: 1.25, IsLeaf: true},
				{LeafValue: -0.75, IsLeaf: true},
			},
			{
				{FeatureIndex: 1, Threshold: 10, LeftChild: 1, RightChild: 2},
				{LeafValue: 0.5, IsLeaf: true},
				{LeafValue: 0.2, IsLeaf: true},
			},
		},
	}

	score := model.Score([]float64{0.2, 20})
	want := 1.25 + 0.2
	if score != want {
		t.Fatalf("score = %f, want %f", score, want)
	}
}

func TestHandleFeed_UsesLTRRanking(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "l2r-user", "password123")

	app.db.Exec(`UPDATE user_preferences SET exploration_rate = 0 WHERE user_id = (SELECT id FROM users WHERE username = 'l2r-user')`)
	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src-l2r', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, transcript, file_size_bytes) VALUES ('l2r-a', 'src-l2r', 'Long High Score', 40.0, 'k1', 'ready', 0.9, '', 100)`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, transcript, file_size_bytes) VALUES ('l2r-b', 'src-l2r', 'Short Lower Score', 8.0, 'k2', 'ready', 0.4, '', 100)`)

	app.ltrModel = &LTRModel{
		NumFeatures: 13,
		Trees: [][]LTRTree{
			{
				{FeatureIndex: 1, Threshold: 10, LeftChild: 1, RightChild: 2},
				{LeafValue: 2.0, IsLeaf: true},
				{LeafValue: -1.0, IsLeaf: true},
			},
		},
	}

	req := authRequest(t, app, "GET", "/api/feed", nil, token)
	rec := httptest.NewRecorder()
	app.optionalAuth(app.handleFeed)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	clips := resp["clips"].([]interface{})
	if len(clips) < 2 {
		t.Fatalf("got %d clips, want at least 2", len(clips))
	}
	first := clips[0].(map[string]interface{})
	if first["title"] != "Short Lower Score" {
		t.Fatalf("first clip = %v, want Short Lower Score", first["title"])
	}
	if _, ok := first["_source_id"]; ok {
		t.Fatal("internal L2R fields should not be exposed in feed response")
	}
}

func TestHandleFeed_Filtered_UsesLTRRanking(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "l2r-filter-user", "password123")

	var userID string
	if err := app.db.QueryRow(`SELECT id FROM users WHERE username = 'l2r-filter-user'`).Scan(&userID); err != nil {
		t.Fatalf("fetch user id: %v", err)
	}

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src-l2r-f', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, transcript, file_size_bytes) VALUES ('l2rf-a', 'src-l2r-f', 'Long Filter Clip', 45.0, 'k1', 'ready', 0.95, '', 100)`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, transcript, file_size_bytes) VALUES ('l2rf-b', 'src-l2r-f', 'Short Filter Clip', 7.0, 'k2', 'ready', 0.3, '', 100)`)
	app.db.Exec(`INSERT INTO saved_filters (id, user_id, name, query, is_default) VALUES ('f-l2r', ?, 'all', '{}', 1)`, userID)

	app.ltrModel = &LTRModel{
		NumFeatures: 13,
		Trees: [][]LTRTree{
			{
				{FeatureIndex: 1, Threshold: 10, LeftChild: 1, RightChild: 2},
				{LeafValue: 3.0, IsLeaf: true},
				{LeafValue: -1.0, IsLeaf: true},
			},
		},
	}

	req := authRequest(t, app, "GET", "/api/feed?filter=f-l2r", nil, token)
	rec := httptest.NewRecorder()
	app.optionalAuth(app.handleFeed)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	clips := resp["clips"].([]interface{})
	if len(clips) < 2 {
		t.Fatalf("got %d clips, want at least 2", len(clips))
	}
	first := clips[0].(map[string]interface{})
	if first["title"] != "Short Filter Clip" {
		t.Fatalf("first clip = %v, want Short Filter Clip", first["title"])
	}
}
