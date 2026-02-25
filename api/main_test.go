package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	db.SetMaxOpenConns(4)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("pragma: %v", err)
		}
	}
	if err := runMigrations(db, DialectSQLite); err != nil {
		t.Fatalf("schema migration: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &App{
		db:  NewCompatDB(db, DialectSQLite),
		cfg: Config{JWTSecret: "test-secret", AdminJWTSecret: "test-admin-secret", CookieSecret: "test-cookie-secret", MinioBucket: "test-bucket"},
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

func TestBuildBrowserStreamURL(t *testing.T) {
	got, err := buildBrowserStreamURL("http://minio:9000/clips/my/video.mp4?X-Amz-Signature=abc123")
	if err != nil {
		t.Fatalf("buildBrowserStreamURL returned error: %v", err)
	}
	want := "/storage/clips/my/video.mp4?X-Amz-Signature=abc123"
	if got != want {
		t.Fatalf("buildBrowserStreamURL = %q, want %q", got, want)
	}
}

func TestBuildBrowserStreamURL_InvalidURL(t *testing.T) {
	if _, err := buildBrowserStreamURL("://bad-url"); err == nil {
		t.Fatal("expected error for invalid presigned URL, got nil")
	}
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

	app2 := &App{cfg: Config{JWTSecret: "different-secret", CookieSecret: "test-cookie-secret"}}
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

func TestHandleFeed_DedupeSeenToggle(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "dedupeuser", "password123")

	var userID string
	if err := app.db.QueryRow(`SELECT id FROM users WHERE username = 'dedupeuser'`).Scan(&userID); err != nil {
		t.Fatalf("fetch user id: %v", err)
	}

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src-dedupe', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score) VALUES ('c-dedupe', 'src-dedupe', 'Seen Clip', 30.0, 'k', 'ready', 0.8)`)
	app.db.Exec(`INSERT INTO interactions (id, user_id, clip_id, action, created_at) VALUES ('i-dedupe', ?, 'c-dedupe', 'view', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`, userID)

	// Default behavior: dedupe ON, seen clip should be filtered out.
	req := authRequest(t, app, "GET", "/api/feed", nil, token)
	rec := httptest.NewRecorder()
	app.optionalAuth(app.handleFeed)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	clips := resp["clips"].([]interface{})
	if len(clips) != 0 {
		t.Fatalf("got %d clips, want 0 with dedupe_seen_24h enabled", len(clips))
	}

	// Toggle dedupe OFF: seen clip should be returned.
	app.db.Exec(`UPDATE user_preferences SET dedupe_seen_24h = 0 WHERE user_id = ?`, userID)
	req = authRequest(t, app, "GET", "/api/feed", nil, token)
	rec = httptest.NewRecorder()
	app.optionalAuth(app.handleFeed)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp = decodeJSON(t, rec)
	clips = resp["clips"].([]interface{})
	if len(clips) != 1 {
		t.Fatalf("got %d clips, want 1 with dedupe_seen_24h disabled", len(clips))
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

func TestHandleIngest_InvalidURLScheme(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "badscheme", "password123")

	tests := []struct {
		name string
		url  string
	}{
		{"ftp scheme", "ftp://example.com/video.mp4"},
		{"no scheme", "example.com/video.mp4"},
		{"javascript scheme", "javascript:alert(1)"},
		{"file scheme", "file:///etc/passwd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]string{"url": tt.url}
			req := authRequest(t, app, "POST", "/api/ingest", body, token)
			rec := httptest.NewRecorder()
			app.handleIngest(rec, req)
			if rec.Code != 400 {
				t.Errorf("status = %d, want 400 for url %q", rec.Code, tt.url)
			}
		})
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

func TestHandleListCookieStatus(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "cookiestatus", "password123")

	setBody := map[string]string{"cookie_str": "session_id=abc123"}
	setReq := authRequest(t, app, "PUT", "/api/me/cookies/youtube", setBody, token)
	setReq = withChiParam(setReq, "platform", "youtube")
	setRec := httptest.NewRecorder()
	app.handleSetCookie(setRec, setReq)
	if setRec.Code != 200 {
		t.Fatalf("set cookie status = %d, want 200", setRec.Code)
	}

	req := authRequest(t, app, "GET", "/api/me/cookies", nil, token)
	rec := httptest.NewRecorder()
	app.handleListCookieStatus(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	platforms := resp["platforms"].(map[string]interface{})
	youtube := platforms["youtube"].(map[string]interface{})
	if youtube["saved"] != true {
		t.Fatalf("youtube saved = %v, want true", youtube["saved"])
	}
	tiktok := platforms["tiktok"].(map[string]interface{})
	if tiktok["saved"] != false {
		t.Fatalf("tiktok saved = %v, want false", tiktok["saved"])
	}
	if _, hasCookie := youtube["cookie_str"]; hasCookie {
		t.Fatal("cookie_str must not be exposed by cookie status endpoint")
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

func TestHandleListJobs_IncludesSourceMetadataForFailedJobs(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "jobmeta", "password123")
	userID := app.extractUserID(func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}())

	sourceMeta := `{"id":"abc123","title":"Creator Deep Dive","uploader":"ClipFeed Labs","thumbnail":"https://img.example/abc.jpg"}`
	_, err := app.db.Exec(`
		INSERT INTO sources (id, url, platform, title, channel_name, thumbnail_url, metadata, status, submitted_by)
		VALUES ('src-meta', 'https://youtube.com/watch?v=abc123', 'youtube', 'Creator Deep Dive', 'ClipFeed Labs', 'https://img.example/abc.jpg', ?, 'failed', ?)
	`, sourceMeta, userID)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}

	_, err = app.db.Exec(`
		INSERT INTO jobs (
			id, source_id, job_type, status, error, attempts, max_attempts,
			started_at, completed_at
		) VALUES (
			'job-meta', 'src-meta', 'download', 'failed',
			'yt-dlp failed: forbidden', 1, 3,
			strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-3 minutes'),
			strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-2 minutes')
		)
	`)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}

	req := authRequest(t, app, "GET", "/api/jobs", nil, token)
	rec := httptest.NewRecorder()
	app.handleListJobs(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	resp := decodeJSON(t, rec)
	jobs, ok := resp["jobs"].([]interface{})
	if !ok || len(jobs) == 0 {
		t.Fatalf("jobs missing in response: %#v", resp["jobs"])
	}

	job := jobs[0].(map[string]interface{})
	if job["channel_name"] != "ClipFeed Labs" {
		t.Fatalf("channel_name = %v, want %q", job["channel_name"], "ClipFeed Labs")
	}
	if job["thumbnail_url"] != "https://img.example/abc.jpg" {
		t.Fatalf("thumbnail_url = %v", job["thumbnail_url"])
	}
	if job["source_metadata"] == nil {
		t.Fatal("source_metadata missing")
	}
}

func TestHandleGetJob_NotFound(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "jobnotfound", "password123")

	req := authRequest(t, app, "GET", "/api/jobs/nope", nil, token)
	req = withChiParam(req, "id", "nope")
	rec := httptest.NewRecorder()
	app.handleGetJob(rec, req)

	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- Topic Graph ---

func TestTopicGraph_ComputeBoost_DirectMatch(t *testing.T) {
	g := &TopicGraph{
		nodes: map[string]*TopicNode{
			"t1": {ID: "t1", Name: "cooking"},
		},
		edges: map[string][]TopicEdge{},
	}
	boost := g.computeBoost([]string{"t1"}, map[string]float64{"t1": 1.5})
	if boost != 1.5 {
		t.Errorf("boost = %f, want 1.5", boost)
	}
}

func TestTopicGraph_ComputeBoost_AncestorDecay(t *testing.T) {
	g := &TopicGraph{
		nodes: map[string]*TopicNode{
			"cooking":         {ID: "cooking", Name: "cooking"},
			"italian-cuisine": {ID: "italian-cuisine", Name: "italian cuisine", ParentID: "cooking"},
			"carbonara":       {ID: "carbonara", Name: "carbonara", ParentID: "italian-cuisine"},
		},
		edges: map[string][]TopicEdge{},
	}

	// User likes "cooking" (2 hops above "carbonara")
	boost := g.computeBoost([]string{"carbonara"}, map[string]float64{"cooking": 2.0})
	// 2.0 * 0.7^2 = 0.98
	want := 2.0 * 0.7 * 0.7
	if diff := boost - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("boost = %f, want %f", boost, want)
	}
}

func TestTopicGraph_ComputeBoost_LateralEdge(t *testing.T) {
	g := &TopicGraph{
		nodes: map[string]*TopicNode{
			"skating":     {ID: "skating", Name: "skating"},
			"longboarding": {ID: "longboarding", Name: "longboarding"},
		},
		edges: map[string][]TopicEdge{
			"skating": {{TargetID: "longboarding", Relation: "related_to", Weight: 0.8}},
		},
	}

	// User likes longboarding, clip is about skating (lateral edge weight 0.8)
	boost := g.computeBoost([]string{"skating"}, map[string]float64{"longboarding": 1.5})
	// 1.5 * 0.8 * 0.7 = 0.84
	want := 1.5 * 0.8 * 0.7
	if diff := boost - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("boost = %f, want %f", boost, want)
	}
}

func TestTopicGraph_ComputeBoost_NoMatch(t *testing.T) {
	g := &TopicGraph{
		nodes: map[string]*TopicNode{
			"t1": {ID: "t1", Name: "cooking"},
			"t2": {ID: "t2", Name: "music"},
		},
		edges: map[string][]TopicEdge{},
	}
	boost := g.computeBoost([]string{"t1"}, map[string]float64{"t2": 1.5})
	if boost != 1.0 {
		t.Errorf("boost = %f, want 1.0 (no match)", boost)
	}
}

func TestTopicGraph_ResolveByName(t *testing.T) {
	g := &TopicGraph{
		byName: map[string]*TopicNode{
			"cooking": {ID: "t1", Name: "cooking"},
		},
	}
	if n := g.resolveByName("Cooking"); n == nil || n.ID != "t1" {
		t.Errorf("resolveByName(Cooking) = %v, want t1", n)
	}
	if n := g.resolveByName("nonexistent"); n != nil {
		t.Errorf("resolveByName(nonexistent) = %v, want nil", n)
	}
}

func TestHandleFeed_WithTopicGraph(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "graphuser", "password123")

	// Remove SQL randomness so ordering is deterministic
	app.db.Exec(`UPDATE user_preferences SET exploration_rate = 0 WHERE user_id = (SELECT id FROM users WHERE username = 'graphuser')`)

	// Create topic hierarchy: cooking → italian
	app.db.Exec(`INSERT INTO topics (id, name, slug, path, depth) VALUES ('t-cooking', 'cooking', 'cooking', 'cooking', 0)`)
	app.db.Exec(`INSERT INTO topics (id, name, slug, path, parent_id, depth) VALUES ('t-italian', 'italian', 'italian', 'cooking/italian', 't-cooking', 1)`)

	// Create two clips with same content_score; graph boost is the tiebreaker
	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src-g1', 'http://x.com/1', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, topics) VALUES ('cg1', 'src-g1', 'Italian Recipe', 30.0, 'k1', 'ready', 0.5, '["italian"]')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, topics) VALUES ('cg2', 'src-g1', 'Random Clip', 30.0, 'k2', 'ready', 0.5, '[]')`)

	// Link clip to topic graph
	app.db.Exec(`INSERT INTO clip_topics (clip_id, topic_id, confidence, source) VALUES ('cg1', 't-italian', 1.0, 'keybert')`)

	// Set user affinity for parent "cooking" — boost propagates down the tree to "italian"
	app.db.Exec(`INSERT INTO user_topic_affinities (user_id, topic_id, weight, source)
		SELECT u.id, 't-cooking', 2.0, 'explicit' FROM users u WHERE u.username = 'graphuser'`)

	app.refreshTopicGraph()

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

	// Italian recipe: 0.5 * (2.0 * 0.7) = 0.7; Random clip: 0.5 * 1.0 = 0.5
	first := clips[0].(map[string]interface{})
	if first["title"] != "Italian Recipe" {
		t.Errorf("first clip = %v, want 'Italian Recipe' (graph boost should rank it higher)", first["title"])
	}
}

func TestHandleGetTopicTree(t *testing.T) {
	app := newTestApp(t)

	app.db.Exec(`INSERT INTO topics (id, name, slug, path, depth) VALUES ('t1', 'cooking', 'cooking', 'cooking', 0)`)
	app.db.Exec(`INSERT INTO topics (id, name, slug, path, parent_id, depth) VALUES ('t2', 'pasta', 'pasta', 'cooking/pasta', 't1', 1)`)
	app.refreshTopicGraph()

	req := httptest.NewRequest("GET", "/api/topics/tree", nil)
	rec := httptest.NewRecorder()
	app.handleGetTopicTree(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	tree := resp["tree"].([]interface{})
	if len(tree) != 1 {
		t.Fatalf("got %d roots, want 1", len(tree))
	}
	root := tree[0].(map[string]interface{})
	if root["name"] != "cooking" {
		t.Errorf("root name = %v, want cooking", root["name"])
	}
	children := root["children"].([]interface{})
	if len(children) != 1 {
		t.Fatalf("got %d children, want 1", len(children))
	}
	if children[0].(map[string]interface{})["name"] != "pasta" {
		t.Errorf("child name = %v, want pasta", children[0].(map[string]interface{})["name"])
	}
}

// --- Security headers middleware ---

func TestSecurityHeaders(t *testing.T) {
handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
writeJSON(w, 200, map[string]string{"ok": "true"})
})

// Wrap with security headers middleware (same logic as in main()).
secured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("X-Content-Type-Options", "nosniff")
w.Header().Set("X-Frame-Options", "DENY")
w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
handler.ServeHTTP(w, r)
})

req := httptest.NewRequest("GET", "/health", nil)
rec := httptest.NewRecorder()
secured.ServeHTTP(rec, req)

if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
}
if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
}
if got := rec.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
t.Errorf("Referrer-Policy = %q, want %q", got, "strict-origin-when-cross-origin")
}
}

// --- CORS config ---

func TestLoadConfig_AllowedOriginsDefault(t *testing.T) {
t.Setenv("ALLOWED_ORIGINS", "")
cfg := loadConfig()
if cfg.AllowedOrigins != "*" {
t.Errorf("AllowedOrigins = %q, want %q", cfg.AllowedOrigins, "*")
}
}

func TestLoadConfig_AllowedOriginsCustom(t *testing.T) {
t.Setenv("ALLOWED_ORIGINS", "https://example.com,https://app.example.com")
cfg := loadConfig()
if cfg.AllowedOrigins != "https://example.com,https://app.example.com" {
t.Errorf("AllowedOrigins = %q", cfg.AllowedOrigins)
}
}

// ===========================================================================
// Worker API endpoint tests
// ===========================================================================

// newTestAppWithWorkerSecret returns an App with WORKER_SECRET configured.
func newTestAppWithWorkerSecret(t *testing.T) *App {
	t.Helper()
	app := newTestApp(t)
	app.cfg.WorkerSecret = "test-worker-secret"
	return app
}

func workerRequest(method, url string, body interface{}, secret string) *http.Request {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(b))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return req
}

// --- Worker Auth Middleware ---

func TestWorkerAuth_NoSecretConfigured(t *testing.T) {
	app := newTestApp(t) // WorkerSecret defaults to ""
	handler := app.workerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 when WORKER_SECRET not configured", rec.Code)
	}
}

func TestWorkerAuth_ValidSecret(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)
	handler := app.workerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer test-worker-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestWorkerAuth_InvalidSecret(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)
	handler := app.workerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWorkerAuth_NoHeader(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)
	handler := app.workerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWorkerAuth_NoBearerPrefix(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)
	handler := app.workerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "test-worker-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401 when missing Bearer prefix", rec.Code)
	}
}

// --- Worker Claim Job ---

func TestWorkerClaimJob_NoJobs(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	req := workerRequest("POST", "/api/internal/jobs/claim", nil, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerClaimJob(rec, req)

	if rec.Code != 204 {
		t.Errorf("status = %d, want 204 when no jobs", rec.Code)
	}
}

func TestWorkerClaimJob_ClaimsQueuedJob(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc1', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload) VALUES ('wj1', 'wsrc1', 'ingest', 'queued', '{"url":"http://x.com"}')`)

	req := workerRequest("POST", "/api/internal/jobs/claim", nil, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerClaimJob(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["id"] != "wj1" {
		t.Errorf("id = %v, want wj1", resp["id"])
	}

	// Verify job is now running
	var status string
	app.db.QueryRow("SELECT status FROM jobs WHERE id = 'wj1'").Scan(&status)
	if status != "running" {
		t.Errorf("job status = %q, want running", status)
	}
}

func TestWorkerClaimJob_SkipsFutureRunAfter(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc2', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload, run_after)
		VALUES ('wj2', 'wsrc2', 'ingest', 'queued', '{}', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '+1 hour'))`)

	req := workerRequest("POST", "/api/internal/jobs/claim", nil, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerClaimJob(rec, req)

	if rec.Code != 204 {
		t.Errorf("status = %d, want 204 (job in backoff)", rec.Code)
	}
}

func TestWorkerClaimJob_PriorityOrdering(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc3', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, priority, payload) VALUES ('wj-low', 'wsrc3', 'ingest', 'queued', 1, '{}')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, priority, payload) VALUES ('wj-high', 'wsrc3', 'ingest', 'queued', 10, '{}')`)

	req := workerRequest("POST", "/api/internal/jobs/claim", nil, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerClaimJob(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["id"] != "wj-high" {
		t.Errorf("claimed job id = %v, want wj-high (higher priority)", resp["id"])
	}
}

// --- Worker Update Job ---

func TestWorkerUpdateJob_Complete(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc4', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload) VALUES ('wj4', 'wsrc4', 'ingest', 'running', '{}')`)

	body := map[string]interface{}{"status": "complete", "result": map[string]string{"clips": "3"}}
	req := workerRequest("PUT", "/api/internal/jobs/wj4", body, "test-worker-secret")
	req = withChiParam(req, "id", "wj4")
	rec := httptest.NewRecorder()
	app.handleWorkerUpdateJob(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var status string
	app.db.QueryRow("SELECT status FROM jobs WHERE id = 'wj4'").Scan(&status)
	if status != "complete" {
		t.Errorf("job status = %q, want complete", status)
	}
}

func TestWorkerUpdateJob_Failed(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc5', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload) VALUES ('wj5', 'wsrc5', 'ingest', 'running', '{}')`)

	errMsg := "download timeout"
	body := map[string]interface{}{"status": "failed", "error": errMsg}
	req := workerRequest("PUT", "/api/internal/jobs/wj5", body, "test-worker-secret")
	req = withChiParam(req, "id", "wj5")
	rec := httptest.NewRecorder()
	app.handleWorkerUpdateJob(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var status, jobError string
	app.db.QueryRow("SELECT status, error FROM jobs WHERE id = 'wj5'").Scan(&status, &jobError)
	if status != "failed" {
		t.Errorf("job status = %q, want failed", status)
	}
	if jobError != errMsg {
		t.Errorf("job error = %q, want %q", jobError, errMsg)
	}
}

func TestWorkerUpdateJob_Requeue(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc6', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload) VALUES ('wj6', 'wsrc6', 'ingest', 'running', '{}')`)

	body := map[string]interface{}{
		"status":    "queued",
		"error":     "rate limited",
		"run_after": "2099-01-01T00:00:00Z",
	}
	req := workerRequest("PUT", "/api/internal/jobs/wj6", body, "test-worker-secret")
	req = withChiParam(req, "id", "wj6")
	rec := httptest.NewRecorder()
	app.handleWorkerUpdateJob(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var status, runAfter string
	app.db.QueryRow("SELECT status, run_after FROM jobs WHERE id = 'wj6'").Scan(&status, &runAfter)
	if status != "queued" {
		t.Errorf("job status = %q, want queued", status)
	}
	if runAfter != "2099-01-01T00:00:00Z" {
		t.Errorf("run_after = %q, want 2099-01-01T00:00:00Z", runAfter)
	}
}

func TestWorkerUpdateJob_InvalidStatus(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc7', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload) VALUES ('wj7', 'wsrc7', 'ingest', 'running', '{}')`)

	body := map[string]interface{}{"status": "bogus"}
	req := workerRequest("PUT", "/api/internal/jobs/wj7", body, "test-worker-secret")
	req = withChiParam(req, "id", "wj7")
	rec := httptest.NewRecorder()
	app.handleWorkerUpdateJob(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestWorkerUpdateJob_InvalidBody(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	req := httptest.NewRequest("PUT", "/api/internal/jobs/wj99", bytes.NewBufferString("{bad"))
	req = withChiParam(req, "id", "wj99")
	rec := httptest.NewRecorder()
	app.handleWorkerUpdateJob(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Worker Get Job ---

func TestWorkerGetJob_Found(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc8', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload, attempts, max_attempts) VALUES ('wj8', 'wsrc8', 'ingest', 'running', '{}', 2, 5)`)

	req := workerRequest("GET", "/api/internal/jobs/wj8", nil, "test-worker-secret")
	req = withChiParam(req, "id", "wj8")
	rec := httptest.NewRecorder()
	app.handleWorkerGetJob(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}
	if resp["attempts"].(float64) != 2 {
		t.Errorf("attempts = %v, want 2", resp["attempts"])
	}
	if resp["max_attempts"].(float64) != 5 {
		t.Errorf("max_attempts = %v, want 5", resp["max_attempts"])
	}
}

func TestWorkerGetJob_NotFound(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	req := workerRequest("GET", "/api/internal/jobs/nonexistent", nil, "test-worker-secret")
	req = withChiParam(req, "id", "nonexistent")
	rec := httptest.NewRecorder()
	app.handleWorkerGetJob(rec, req)

	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- Worker Reclaim Stale ---

func TestWorkerReclaimStale_RequeueBelowMaxAttempts(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc9', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload, attempts, max_attempts, started_at)
		VALUES ('wj9', 'wsrc9', 'ingest', 'running', '{}', 1, 3, strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-3 hours'))`)

	body := map[string]interface{}{"stale_minutes": 60}
	req := workerRequest("POST", "/api/internal/jobs/reclaim", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerReclaimStale(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["requeued"].(float64) != 1 {
		t.Errorf("requeued = %v, want 1", resp["requeued"])
	}

	var status string
	app.db.QueryRow("SELECT status FROM jobs WHERE id = 'wj9'").Scan(&status)
	if status != "queued" {
		t.Errorf("job status = %q, want queued", status)
	}
}

func TestWorkerReclaimStale_FailAtMaxAttempts(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc10', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload, attempts, max_attempts, started_at)
		VALUES ('wj10', 'wsrc10', 'ingest', 'running', '{}', 3, 3, strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-3 hours'))`)

	body := map[string]interface{}{"stale_minutes": 60}
	req := workerRequest("POST", "/api/internal/jobs/reclaim", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerReclaimStale(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["failed"].(float64) != 1 {
		t.Errorf("failed = %v, want 1", resp["failed"])
	}

	var status string
	app.db.QueryRow("SELECT status FROM jobs WHERE id = 'wj10'").Scan(&status)
	if status != "failed" {
		t.Errorf("job status = %q, want failed", status)
	}
}

func TestWorkerReclaimStale_NoStaleJobs(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	// Fresh running job — should not be reclaimed
	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc11', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload, attempts, max_attempts, started_at)
		VALUES ('wj11', 'wsrc11', 'ingest', 'running', '{}', 1, 3, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`)

	body := map[string]interface{}{"stale_minutes": 120}
	req := workerRequest("POST", "/api/internal/jobs/reclaim", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerReclaimStale(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["requeued"].(float64) != 0 || resp["failed"].(float64) != 0 {
		t.Errorf("expected no reclaimed jobs, got requeued=%v failed=%v", resp["requeued"], resp["failed"])
	}
}

// --- Worker Update Source ---

func TestWorkerUpdateSource_Success(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc-upd', 'http://x.com', 'direct')`)

	body := map[string]interface{}{
		"status":       "ready",
		"title":        "Cool Video",
		"channel_name": "TestChannel",
	}
	req := workerRequest("PUT", "/api/internal/sources/wsrc-upd", body, "test-worker-secret")
	req = withChiParam(req, "id", "wsrc-upd")
	rec := httptest.NewRecorder()
	app.handleWorkerUpdateSource(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var title, channelName string
	app.db.QueryRow("SELECT title, channel_name FROM sources WHERE id = 'wsrc-upd'").Scan(&title, &channelName)
	if title != "Cool Video" {
		t.Errorf("title = %q, want Cool Video", title)
	}
	if channelName != "TestChannel" {
		t.Errorf("channel_name = %q, want TestChannel", channelName)
	}
}

func TestWorkerUpdateSource_EmptyFields(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc-empty', 'http://x.com', 'direct')`)

	body := map[string]interface{}{}
	req := workerRequest("PUT", "/api/internal/sources/wsrc-empty", body, "test-worker-secret")
	req = withChiParam(req, "id", "wsrc-empty")
	rec := httptest.NewRecorder()
	app.handleWorkerUpdateSource(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 when no fields to update", rec.Code)
	}
}

// --- Worker Get Cookie ---

func TestWorkerGetCookie_NoCookie(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc-cookie', 'http://x.com', 'youtube')`)

	req := workerRequest("GET", "/api/internal/sources/wsrc-cookie/cookie?platform=youtube", nil, "test-worker-secret")
	req = withChiParam(req, "id", "wsrc-cookie")
	rec := httptest.NewRecorder()
	app.handleWorkerGetCookie(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["cookie"] != nil {
		t.Errorf("cookie = %v, want nil when no cookie set", resp["cookie"])
	}
}

// --- Worker Create Clip ---

func TestWorkerCreateClip_Success(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc-clip', 'http://x.com', 'youtube')`)

	body := map[string]interface{}{
		"id":               "wclip-1",
		"source_id":        "wsrc-clip",
		"title":            "Test Clip",
		"duration_seconds": 30.0,
		"start_time":       0.0,
		"end_time":         30.0,
		"storage_key":      "clips/wclip-1/video.mp4",
		"thumbnail_key":    "clips/wclip-1/thumb.jpg",
		"width":            1080,
		"height":           1920,
		"file_size_bytes":  5000000,
		"transcript":       "Hello world this is a test",
		"topics":           []string{"testing", "hello"},
		"content_score":    0.5,
		"expires_at":       "2099-12-31T00:00:00Z",
		"platform":         "youtube",
		"channel_name":     "TestChannel",
	}
	req := workerRequest("POST", "/api/internal/clips", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerCreateClip(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	if resp["id"] != "wclip-1" {
		t.Errorf("id = %v, want wclip-1", resp["id"])
	}

	// Verify clip exists in DB
	var title, status string
	app.db.QueryRow("SELECT title, status FROM clips WHERE id = 'wclip-1'").Scan(&title, &status)
	if title != "Test Clip" {
		t.Errorf("clip title = %q, want Test Clip", title)
	}
	if status != "ready" {
		t.Errorf("clip status = %q, want ready", status)
	}

	// Verify topics were created
	var topicCount int
	app.db.QueryRow("SELECT COUNT(*) FROM clip_topics WHERE clip_id = 'wclip-1'").Scan(&topicCount)
	if topicCount != 2 {
		t.Errorf("topic count = %d, want 2", topicCount)
	}

	// Verify FTS entry
	var ftsCount int
	app.db.QueryRow("SELECT COUNT(*) FROM clips_fts WHERE clip_id = 'wclip-1'").Scan(&ftsCount)
	if ftsCount != 1 {
		t.Errorf("FTS count = %d, want 1", ftsCount)
	}
}

func TestWorkerCreateClip_WithEmbeddings(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc-emb', 'http://x.com', 'direct')`)

	// Create a small fake embedding (4 float32s = 16 bytes)
	embBytes := make([]byte, 16)
	for i := range embBytes {
		embBytes[i] = byte(i)
	}
	embB64 := "AAECAwQFBgcICQoLDA0ODw==" // base64 of bytes 0-15

	body := map[string]interface{}{
		"id":               "wclip-emb",
		"source_id":        "wsrc-emb",
		"title":            "Embedded Clip",
		"duration_seconds": 20.0,
		"start_time":       0.0,
		"end_time":         20.0,
		"storage_key":      "clips/wclip-emb/video.mp4",
		"thumbnail_key":    "",
		"width":            720,
		"height":           1280,
		"file_size_bytes":  2000000,
		"transcript":       "test transcript",
		"topics":           []string{},
		"content_score":    0.6,
		"expires_at":       "2099-12-31T00:00:00Z",
		"text_embedding":   embB64,
		"visual_embedding": embB64,
		"model_version":    "v1",
	}
	req := workerRequest("POST", "/api/internal/clips", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerCreateClip(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	// Verify embeddings were stored
	var modelVersion string
	err := app.db.QueryRow("SELECT model_version FROM clip_embeddings WHERE clip_id = 'wclip-emb'").Scan(&modelVersion)
	if err != nil {
		t.Fatalf("embedding not found: %v", err)
	}
	if modelVersion != "v1" {
		t.Errorf("model_version = %q, want v1", modelVersion)
	}
}

func TestWorkerCreateClip_InvalidBody(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	req := httptest.NewRequest("POST", "/api/internal/clips", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	app.handleWorkerCreateClip(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Worker Resolve Topic ---

func TestWorkerResolveTopic_Create(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	body := map[string]string{"name": "Machine Learning"}
	req := workerRequest("POST", "/api/internal/topics/resolve", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerResolveTopic(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["created"] != true {
		t.Errorf("created = %v, want true", resp["created"])
	}
	if resp["id"] == nil || resp["id"] == "" {
		t.Error("expected id in response")
	}
}

func TestWorkerResolveTopic_ExistingTopic(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	app.db.Exec(`INSERT INTO topics (id, name, slug, path, depth) VALUES ('existing-t', 'cooking', 'cooking', 'cooking', 0)`)

	body := map[string]string{"name": "cooking"}
	req := workerRequest("POST", "/api/internal/topics/resolve", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerResolveTopic(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["created"] != false {
		t.Errorf("created = %v, want false", resp["created"])
	}
	if resp["id"] != "existing-t" {
		t.Errorf("id = %v, want existing-t", resp["id"])
	}
}

func TestWorkerResolveTopic_EmptyName(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	body := map[string]string{"name": ""}
	req := workerRequest("POST", "/api/internal/topics/resolve", body, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerResolveTopic(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Worker Score Update ---

func TestWorkerScoreUpdate_NoInteractions(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)

	req := workerRequest("POST", "/api/internal/scores/update", nil, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerScoreUpdate(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["updated"].(float64) != 0 {
		t.Errorf("updated = %v, want 0", resp["updated"])
	}
}

func TestWorkerScoreUpdate_UpdatesWithSufficientViews(t *testing.T) {
	app := newTestAppWithWorkerSecret(t)
	token := registerUser(t, app, "scorer", "password123")
	userID := app.extractUserID(func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}())

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('wsrc-score', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status, content_score) VALUES ('wc-score', 'wsrc-score', 30.0, 'key', 'ready', 0.5)`)

	// Add 5+ views with 100% watch — should push score up
	for i := 0; i < 6; i++ {
		app.db.Exec(`INSERT INTO interactions (id, user_id, clip_id, action, watch_percentage)
			VALUES (?, ?, 'wc-score', 'view', 1.0)`, fmt.Sprintf("int-v%d", i), userID)
	}
	for i := 0; i < 6; i++ {
		app.db.Exec(`INSERT INTO interactions (id, user_id, clip_id, action)
			VALUES (?, ?, 'wc-score', 'like')`, fmt.Sprintf("int-l%d", i), userID)
	}

	req := workerRequest("POST", "/api/internal/scores/update", nil, "test-worker-secret")
	rec := httptest.NewRecorder()
	app.handleWorkerScoreUpdate(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["updated"].(float64) < 1 {
		t.Errorf("updated = %v, want >= 1", resp["updated"])
	}

	var newScore float64
	app.db.QueryRow("SELECT content_score FROM clips WHERE id = 'wc-score'").Scan(&newScore)
	if newScore <= 0.5 {
		t.Errorf("score = %f, want > 0.5 after positive engagement", newScore)
	}
}

// ===========================================================================
// Admin authentication tests
// ===========================================================================

func TestAdminLogin_Success(t *testing.T) {
	app := newTestApp(t)
	app.cfg.AdminUsername = "admin"
	app.cfg.AdminPassword = "secret-admin-pw"

	body := map[string]string{"username": "admin", "password": "secret-admin-pw"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewReader(b))
	rec := httptest.NewRecorder()

	app.handleAdminLogin(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["token"] == nil || resp["token"] == "" {
		t.Error("expected token in response")
	}
}

func TestAdminLogin_WrongUsername(t *testing.T) {
	app := newTestApp(t)
	app.cfg.AdminUsername = "admin"
	app.cfg.AdminPassword = "secret-admin-pw"

	body := map[string]string{"username": "notadmin", "password": "secret-admin-pw"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	app.handleAdminLogin(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAdminLogin_WrongPassword(t *testing.T) {
	app := newTestApp(t)
	app.cfg.AdminUsername = "admin"
	app.cfg.AdminPassword = "secret-admin-pw"

	body := map[string]string{"username": "admin", "password": "wrong"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	app.handleAdminLogin(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAdminLogin_InvalidBody(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	app.handleAdminLogin(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestIsAdminToken_ValidAdminToken(t *testing.T) {
	app := newTestApp(t)
	app.cfg.AdminUsername = "admin"
	app.cfg.AdminPassword = "secret-admin-pw"

	// Login to get admin token
	body := map[string]string{"username": "admin", "password": "secret-admin-pw"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	app.handleAdminLogin(rec, req)
	resp := decodeJSON(t, rec)
	adminToken := resp["token"].(string)

	// Verify isAdminToken returns true
	checkReq := httptest.NewRequest("GET", "/", nil)
	checkReq.Header.Set("Authorization", "Bearer "+adminToken)
	if !app.isAdminToken(checkReq) {
		t.Error("isAdminToken returned false for valid admin token")
	}
}

func TestIsAdminToken_UserTokenRejected(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "regularuser", "password123")

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if app.isAdminToken(req) {
		t.Error("isAdminToken returned true for a regular user token")
	}
}

func TestIsAdminToken_InvalidToken(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer not.a.valid.jwt")
	if app.isAdminToken(req) {
		t.Error("isAdminToken returned true for invalid token")
	}
}

func TestIsAdminToken_NoHeader(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest("GET", "/", nil)
	if app.isAdminToken(req) {
		t.Error("isAdminToken returned true with no auth header")
	}
}

func TestIsAdminToken_WrongSecret(t *testing.T) {
	app := newTestApp(t)
	app.cfg.AdminUsername = "admin"
	app.cfg.AdminPassword = "pw1"

	// Generate admin token
	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "pw1"})
	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	app.handleAdminLogin(rec, req)
	resp := decodeJSON(t, rec)
	adminToken := resp["token"].(string)

	// Check with a different admin JWT secret
	app2 := newTestApp(t)
	app2.cfg.AdminJWTSecret = "completely-different-secret"
	checkReq := httptest.NewRequest("GET", "/", nil)
	checkReq.Header.Set("Authorization", "Bearer "+adminToken)
	if app2.isAdminToken(checkReq) {
		t.Error("isAdminToken returned true with a different JWT secret")
	}
}

func TestAdminAuthMiddleware_BlocksNonAdmin(t *testing.T) {
	app := newTestApp(t)
	token := registerUser(t, app, "nonadmin", "password123")

	handler := app.adminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401 for non-admin user", rec.Code)
	}
}

func TestAdminAuthMiddleware_AllowsAdmin(t *testing.T) {
	app := newTestApp(t)
	app.cfg.AdminUsername = "admin"
	app.cfg.AdminPassword = "pw"

	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "pw"})
	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	app.handleAdminLogin(rec, req)
	adminToken := decodeJSON(t, rec)["token"].(string)

	var capturedUID string
	handler := app.adminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUID = r.Context().Value(userIDKey).(string)
		writeJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req = httptest.NewRequest("GET", "/admin/status", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if capturedUID != "admin" {
		t.Errorf("user_id = %q, want admin", capturedUID)
	}
}

// --- Admin Status ---

func TestAdminStatus_ReturnsStats(t *testing.T) {
	app := newTestApp(t)
	app.cfg.AdminUsername = "admin"
	app.cfg.AdminPassword = "pw"

	// Create some data
	registerUser(t, app, "statuser", "password123")
	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('stat-src', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('stat-clip', 'stat-src', 30.0, 'key', 'ready')`)

	// Get admin token
	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "pw"})
	req := httptest.NewRequest("POST", "/api/admin/login", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	app.handleAdminLogin(rec, req)
	adminToken := decodeJSON(t, rec)["token"].(string)

	// Call status
	req = httptest.NewRequest("GET", "/api/admin/status", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	app.handleAdminStatus(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["system"] == nil {
		t.Error("missing system stats")
	}
	if resp["database"] == nil {
		t.Error("missing database stats")
	}
	content := resp["content"].(map[string]interface{})
	if content["ready"].(float64) < 1 {
		t.Errorf("ready clips = %v, want >= 1", content["ready"])
	}
}

// --- Admin Clear Failed Jobs ---

func TestAdminClearFailedJobs(t *testing.T) {
	app := newTestApp(t)

	app.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('fail-src', 'http://x.com', 'direct')`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, error, attempts, max_attempts) VALUES ('fail-j1', 'fail-src', 'ingest', 'failed', 'some error', 3, 3)`)
	app.db.Exec(`INSERT INTO jobs (id, source_id, job_type, status, payload) VALUES ('ok-j1', 'fail-src', 'ingest', 'queued', '{}')`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/admin/clear-failed", nil)
	app.handleClearFailedJobs(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["cleared"].(float64) < 1 {
		t.Errorf("cleared = %v, want >= 1", resp["cleared"])
	}

	// Queued job should not be affected
	var queuedCount int
	app.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = 'queued'").Scan(&queuedCount)
	if queuedCount != 1 {
		t.Errorf("queued jobs = %d, want 1 (should not be cleared)", queuedCount)
	}

	// Source should be reset to pending for re-ingestion
	var srcStatus string
	app.db.QueryRow("SELECT status FROM sources WHERE id = 'fail-src'").Scan(&srcStatus)
	if srcStatus != "pending" {
		t.Errorf("source status = %q, want pending after clear", srcStatus)
	}
}

// --- Slugify ---

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Machine Learning", "machine-learning"},
		{"cooking", "cooking"},
		{"  spaces  around  ", "spaces-around"},
		{"Special@#Characters!", "specialcharacters"},
		{"hello-world", "hello-world"},
		{"UPPER CASE", "upper-case"},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := slugify(tc.input)
			if got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// --- Truncate ---

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello" {
		t.Errorf("truncate(11 chars, 5) = %q, want hello", got)
	}
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate(short, 100) = %q, want short", got)
	}
	if got := truncate("", 5); got != "" {
		t.Errorf("truncate(empty, 5) = %q, want empty", got)
	}
}
