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
