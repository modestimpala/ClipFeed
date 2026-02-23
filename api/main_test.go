package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"
)

// --- helpers ---

func newTestApp(t *testing.T) *App {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("pragma: %v", err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &App{
		db:  db,
		cfg: Config{JWTSecret: "test-secret", MinioBucket: "test-bucket"},
	}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return m
}

func registerUser(t *testing.T, app *App, username, password string) string {
	t.Helper()
	body := map[string]string{"username": username, "email": username + "@test.com", "password": password}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	app.handleRegister(rec, req)
	if rec.Code != 201 {
		t.Fatalf("register failed: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	return resp["token"].(string)
}

func authRequest(t *testing.T, app *App, method, url string, body interface{}, token string) *http.Request {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		if uid := app.extractUserID(req); uid != "" {
			ctx := context.WithValue(req.Context(), userIDKey, uid)
			req = req.WithContext(ctx)
		}
	}
	return req
}

// withChiParam sets a chi URL parameter on the request context.
func withChiParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// --- detectPlatform ---

func TestDetectPlatform(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		{"https://www.youtube.com/watch?v=Aq5WXmQQooo", "youtube"},
		{"https://youtu.be/UtdGSaJNb-g", "youtube"},
		{"https://vimeo.com/85923309", "vimeo"},
		{"https://www.tiktok.com/@lamininefennec/video/7578731780082044190", "tiktok"},
		{"https://www.instagram.com/reel/DBOC0Z4hOR1/", "instagram"},
		{"https://twitter.com/grok/status/2025666577988018674", "twitter"},
		{"https://x.com/grok/status/2025666577988018674", "twitter"},
		{"https://commondatastorage.googleapis.com/gtv-videos-bucket/sample/BigBuckBunny.mp4", "direct"},
		{"", "direct"},
	}

	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			got := detectPlatform(tc.url)
			if got != tc.expected {
				t.Errorf("detectPlatform(%q) = %q, want %q", tc.url, got, tc.expected)
			}
		})
	}
}

// --- getEnv ---

func TestGetEnv(t *testing.T) {
	got := getEnv("CLIPFEED_NONEXISTENT_VAR_12345", "fallback")
	if got != "fallback" {
		t.Errorf("getEnv returned %q, want %q", got, "fallback")
	}

	t.Setenv("CLIPFEED_TEST_VAR", "real_value")
	got = getEnv("CLIPFEED_TEST_VAR", "fallback")
	if got != "real_value" {
		t.Errorf("getEnv returned %q, want %q", got, "real_value")
	}
}

// --- writeJSON ---

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, 201, map[string]string{"msg": "created"})

	if rec.Code != 201 {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("content-type = %q, want %q", ct, "application/json")
	}
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["msg"] != "created" {
		t.Errorf("body msg = %q, want %q", resp["msg"], "created")
	}
}

// --- Auth: Register ---

func TestRegister_Success(t *testing.T) {
	app := newTestApp(t)
	body := `{"username":"testuser","email":"test@example.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleRegister(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["token"] == nil || resp["token"] == "" {
		t.Error("expected token in response")
	}
	if resp["user_id"] == nil || resp["user_id"] == "" {
		t.Error("expected user_id in response")
	}
}

func TestRegister_ShortUsername(t *testing.T) {
	app := newTestApp(t)
	body := `{"username":"ab","email":"a@b.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleRegister(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRegister_ShortPassword(t *testing.T) {
	app := newTestApp(t)
	body := `{"username":"testuser","email":"a@b.com","password":"short"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleRegister(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRegister_DuplicateUsername(t *testing.T) {
	app := newTestApp(t)
	registerUser(t, app, "duplicate", "password123")

	body := `{"username":"duplicate","email":"other@test.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleRegister(rec, req)

	if rec.Code != 409 {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestRegister_InvalidJSON(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()

	app.handleRegister(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Auth: Login ---

func TestLogin_Success(t *testing.T) {
	app := newTestApp(t)
	registerUser(t, app, "loginuser", "password123")

	body := `{"username":"loginuser","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleLogin(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["token"] == nil || resp["token"] == "" {
		t.Error("expected token")
	}
}

func TestLogin_ByEmail(t *testing.T) {
	app := newTestApp(t)
	registerUser(t, app, "emailuser", "password123")

	body := `{"username":"emailuser@test.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleLogin(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	app := newTestApp(t)
	registerUser(t, app, "wrongpw", "password123")

	body := `{"username":"wrongpw","password":"wrong"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleLogin(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestLogin_NonexistentUser(t *testing.T) {
	app := newTestApp(t)

	body := `{"username":"nobody","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	app.handleLogin(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// --- JWT / Middleware ---

func TestGenerateToken_And_ExtractUserID(t *testing.T) {
	app := newTestApp(t)

	token, err := app.generateToken("user-123")
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	userID := app.extractUserID(req)
	if userID != "user-123" {
		t.Errorf("extractUserID = %q, want %q", userID, "user-123")
	}
}

func TestExtractUserID_NoHeader(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest("GET", "/", nil)
	if got := app.extractUserID(req); got != "" {
		t.Errorf("extractUserID = %q, want empty", got)
	}
}

func TestExtractUserID_InvalidToken(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	if got := app.extractUserID(req); got != "" {
		t.Errorf("extractUserID = %q, want empty", got)
	}
}

func TestExtractUserID_WrongSecret(t *testing.T) {
	app := newTestApp(t)
	token, _ := app.generateToken("user-123")

	app2 := &App{cfg: Config{JWTSecret: "different-secret"}}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if got := app2.extractUserID(req); got != "" {
		t.Errorf("extractUserID = %q, want empty (wrong secret)", got)
	}
}

func TestAuthMiddleware_Unauthorized(t *testing.T) {
	app := newTestApp(t)
	handler := app.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_Authorized(t *testing.T) {
	app := newTestApp(t)
	token, _ := app.generateToken("user-abc")

	var capturedUID string
	handler := app.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUID = r.Context().Value(userIDKey).(string)
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if capturedUID != "user-abc" {
		t.Errorf("user_id = %q, want %q", capturedUID, "user-abc")
	}
}

// --- Interactions ---

func TestHandleInteraction_ValidActions(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "interactor", "password123")

	// Insert a clip so the FK constraint is met
	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src1', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('clip1', 'src1', 30.0, 'key1', 'ready')`)

	for _, action := range []string{"view", "like", "dislike", "save", "share", "skip", "watch_full"} {
		t.Run(action, func(t *testing.T) {
			body := map[string]interface{}{"action": action, "watch_duration_seconds": 10.0, "watch_percentage": 0.5}
			req := authRequest(t, app, "POST", "/api/clips/clip1/interact", body, token)
			req = withChiParam(req, "id", "clip1")
			rec := httptest.NewRecorder()

			app.handleInteraction(rec, req)

			if rec.Code != 200 {
				t.Errorf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleInteraction_InvalidAction(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "badaction", "password123")

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src2', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('clip2', 'src2', 30.0, 'key2', 'ready')`)

	body := map[string]interface{}{"action": "invalid_action"}
	req := authRequest(t, app, "POST", "/api/clips/clip2/interact", body, token)
	req = withChiParam(req, "id", "clip2")
	rec := httptest.NewRecorder()

	app.handleInteraction(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Feed ---

func TestHandleFeed_Anonymous(t *testing.T) {
	app := newTestApp(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src3', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score) VALUES ('c1', 'src3', 'Test Clip', 30.0, 'key', 'ready', 0.8)`)

	req := httptest.NewRequest("GET", "/api/feed", nil)
	rec := httptest.NewRecorder()
	app.handleFeed(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	clips := resp["clips"].([]interface{})
	if len(clips) != 1 {
		t.Errorf("got %d clips, want 1", len(clips))
	}
}

func TestHandleFeed_Authenticated(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "feeduser", "password123")

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src4', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score) VALUES ('c2', 'src4', 'Test Clip', 30.0, 'key', 'ready', 0.8)`)

	req := authRequest(t, app, "GET", "/api/feed", nil, token)
	rec := httptest.NewRecorder()
	app.optionalAuth(app.handleFeed)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	clips := resp["clips"].([]interface{})
	if len(clips) != 1 {
		t.Errorf("got %d clips, want 1", len(clips))
	}
}

func TestHandleFeed_FiltersProcessingClips(t *testing.T) {
	app := newTestApp(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src5', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status) VALUES ('c3', 'src5', 'Processing', 30.0, 'key', 'processing')`)

	req := httptest.NewRequest("GET", "/api/feed", nil)
	rec := httptest.NewRecorder()
	app.handleFeed(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["clips"] != nil {
		clips := resp["clips"].([]interface{})
		if len(clips) != 0 {
			t.Errorf("got %d clips, want 0 (processing clips filtered)", len(clips))
		}
	}
}

// --- GetClip ---

func TestHandleGetClip_Found(t *testing.T) {
	app := newTestApp(t)
	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src6', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, description, duration_seconds, storage_key, thumbnail_key, status) VALUES ('c4', 'src6', 'My Clip', '', 42.0, 'key', '', 'ready')`)

	req := httptest.NewRequest("GET", "/api/clips/c4", nil)
	req = withChiParam(req, "id", "c4")
	rec := httptest.NewRecorder()

	app.handleGetClip(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["title"] != "My Clip" {
		t.Errorf("title = %v, want %q", resp["title"], "My Clip")
	}
}

func TestHandleGetClip_NotFound(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest("GET", "/api/clips/nonexistent", nil)
	req = withChiParam(req, "id", "nonexistent")
	rec := httptest.NewRecorder()

	app.handleGetClip(rec, req)

	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- Save / Unsave clips ---

func TestSaveAndUnsaveClip(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "saver", "password123")

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src7', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('c5', 'src7', 30.0, 'key', 'ready')`)

	// Save
	req := authRequest(t, app, "POST", "/api/clips/c5/save", nil, token)
	req = withChiParam(req, "id", "c5")
	rec := httptest.NewRecorder()
	app.handleSaveClip(rec, req)
	if rec.Code != 200 {
		t.Fatalf("save: status = %d, want 200", rec.Code)
	}

	// Verify is_protected trigger fired
	var isProtected int
	app.db.QueryRow("SELECT is_protected FROM clips WHERE id = 'c5'").Scan(&isProtected)
	if isProtected != 1 {
		t.Errorf("is_protected = %d after save, want 1", isProtected)
	}

	// Unsave
	req = authRequest(t, app, "DELETE", "/api/clips/c5/save", nil, token)
	req = withChiParam(req, "id", "c5")
	rec = httptest.NewRecorder()
	app.handleUnsaveClip(rec, req)
	if rec.Code != 200 {
		t.Fatalf("unsave: status = %d, want 200", rec.Code)
	}

	// Verify trigger unprotected
	app.db.QueryRow("SELECT is_protected FROM clips WHERE id = 'c5'").Scan(&isProtected)
	if isProtected != 0 {
		t.Errorf("is_protected = %d after unsave, want 0", isProtected)
	}
}

// --- Ingest ---

func TestHandleIngest_Success(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "ingester", "password123")

	body := map[string]string{"url": "https://www.youtube.com/watch?v=abc123"}
	req := authRequest(t, app, "POST", "/api/ingest", body, token)
	rec := httptest.NewRecorder()

	app.handleIngest(rec, req)

	if rec.Code != 202 {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["status"] != "queued" {
		t.Errorf("status = %v, want queued", resp["status"])
	}
	if resp["source_id"] == nil {
		t.Error("expected source_id")
	}
	if resp["job_id"] == nil {
		t.Error("expected job_id")
	}

	// Verify source was created with correct platform
	var platform string
	app.db.QueryRow("SELECT platform FROM sources WHERE id = ?", resp["source_id"]).Scan(&platform)
	if platform != "youtube" {
		t.Errorf("platform = %q, want youtube", platform)
	}
}

func TestHandleIngest_EmptyURL(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "nourling", "password123")

	body := map[string]string{"url": ""}
	req := authRequest(t, app, "POST", "/api/ingest", body, token)
	rec := httptest.NewRecorder()

	app.handleIngest(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Platform Cookies ---

func TestHandleSetCookie_ValidPlatform(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "cookieuser", "password123")

	body := map[string]string{"cookie_str": "session_id=abc123"}
	req := authRequest(t, app, "PUT", "/api/me/cookies/tiktok", body, token)
	req = withChiParam(req, "platform", "tiktok")
	rec := httptest.NewRecorder()

	app.handleSetCookie(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetCookie_InvalidPlatform(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "badplat", "password123")

	body := map[string]string{"cookie_str": "session_id=abc123"}
	req := authRequest(t, app, "PUT", "/api/me/cookies/reddit", body, token)
	req = withChiParam(req, "platform", "reddit")
	rec := httptest.NewRecorder()

	app.handleSetCookie(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleDeleteCookie(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "delcookie", "password123")

	// First set a cookie
	body := map[string]string{"cookie_str": "session_id=abc123"}
	req := authRequest(t, app, "PUT", "/api/me/cookies/instagram", body, token)
	req = withChiParam(req, "platform", "instagram")
	rec := httptest.NewRecorder()
	app.handleSetCookie(rec, req)

	// Delete it
	req = authRequest(t, app, "DELETE", "/api/me/cookies/instagram", nil, token)
	req = withChiParam(req, "platform", "instagram")
	rec = httptest.NewRecorder()
	app.handleDeleteCookie(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- Collections ---

func TestCollectionCRUD(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "collector", "password123")

	// Create collection
	body := map[string]string{"title": "My Favorites", "description": "Best clips"}
	req := authRequest(t, app, "POST", "/api/collections", body, token)
	rec := httptest.NewRecorder()
	app.handleCreateCollection(rec, req)

	if rec.Code != 201 {
		t.Fatalf("create: status = %d, want 201", rec.Code)
	}
	resp := decodeJSON(t, rec)
	collectionID := resp["id"].(string)

	// List collections
	req = authRequest(t, app, "GET", "/api/collections", nil, token)
	rec = httptest.NewRecorder()
	app.handleListCollections(rec, req)

	if rec.Code != 200 {
		t.Fatalf("list: status = %d, want 200", rec.Code)
	}
	resp = decodeJSON(t, rec)
	collections := resp["collections"].([]interface{})
	if len(collections) != 1 {
		t.Fatalf("got %d collections, want 1", len(collections))
	}
	first := collections[0].(map[string]interface{})
	if first["title"] != "My Favorites" {
		t.Errorf("title = %v, want %q", first["title"], "My Favorites")
	}

	// Add a clip to collection
	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src8', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('c6', 'src8', 30.0, 'key', 'ready')`)

	addBody := map[string]string{"clip_id": "c6"}
	req = authRequest(t, app, "POST", "/api/collections/"+collectionID+"/clips", addBody, token)
	req = withChiParam(req, "id", collectionID)
	rec = httptest.NewRecorder()
	app.handleAddToCollection(rec, req)

	if rec.Code != 200 {
		t.Fatalf("add: status = %d, want 200", rec.Code)
	}

	// Remove clip from collection
	req = authRequest(t, app, "DELETE", "/api/collections/"+collectionID+"/clips/c6", nil, token)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", collectionID)
	rctx.URLParams.Add("clipId", "c6")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec = httptest.NewRecorder()
	app.handleRemoveFromCollection(rec, req)

	if rec.Code != 200 {
		t.Fatalf("remove: status = %d, want 200", rec.Code)
	}
}

// --- Search (FTS5) ---

func TestHandleSearch_RequiresQuery(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest("GET", "/api/search", nil)
	rec := httptest.NewRecorder()
	app.handleSearch(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSearch_ReturnsResults(t *testing.T) {
	app := newTestApp(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform, channel_name) VALUES ('src9', 'http://x.com', 'youtube', 'TestChannel')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status) VALUES ('c7', 'src9', 'Amazing Cooking Video', 45.0, 'key', 'ready')`)
	app.db.Exec(`INSERT INTO clips_fts (clip_id, title, transcript, platform, channel_name) VALUES ('c7', 'Amazing Cooking Video', 'today we cook pasta', 'youtube', 'TestChannel')`)

	req := httptest.NewRequest("GET", "/api/search?q=cooking", nil)
	rec := httptest.NewRecorder()
	app.handleSearch(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	total := resp["total"].(float64)
	if total != 1 {
		t.Errorf("total = %v, want 1", total)
	}
}

// --- Profile ---

func TestHandleGetProfile(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "profuser", "password123")

	req := authRequest(t, app, "GET", "/api/me", nil, token)
	rec := httptest.NewRecorder()
	app.handleGetProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["username"] != "profuser" {
		t.Errorf("username = %v, want %q", resp["username"], "profuser")
	}
}

// --- List Saved ---

func TestHandleListSaved_Empty(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "emptysaver", "password123")

	req := authRequest(t, app, "GET", "/api/me/saved", nil, token)
	rec := httptest.NewRecorder()
	app.handleListSaved(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// --- Jobs ---

func TestHandleListJobs(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "jobuser", "password123")

	req := authRequest(t, app, "GET", "/api/jobs", nil, token)
	rec := httptest.NewRecorder()
	app.handleListJobs(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGetJob_NotFound(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest("GET", "/api/jobs/nope", nil)
	req = withChiParam(req, "id", "nope")
	rec := httptest.NewRecorder()
	app.handleGetJob(rec, req)

	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
