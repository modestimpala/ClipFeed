package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"clipfeed/admin"
	"clipfeed/auth"
	"clipfeed/clips"
	"clipfeed/collections"
	"clipfeed/db"
	"clipfeed/feed"
	"clipfeed/httputil"
	"clipfeed/ingest"
	"clipfeed/jobs"
	"clipfeed/profile"
	"clipfeed/saved"
	"clipfeed/scout"
	"clipfeed/worker"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"
)

// testHandlers holds all handler instances for integration tests.
type testHandlers struct {
	db          *db.CompatDB
	authH       *auth.Handler
	feedH       *feed.Handler
	clipsH      *clips.Handler
	adminH      *admin.Handler
	workerH     *worker.Handler
	ingestH     *ingest.Handler
	savedH      *saved.Handler
	collectionsH *collections.Handler
	jobsH       *jobs.Handler
	profileH    *profile.Handler
	scoutH      *scout.Handler
}

func newTestHandlers(t *testing.T) *testHandlers {
	t.Helper()
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	rawDB.SetMaxOpenConns(4)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := rawDB.Exec(p); err != nil {
			t.Fatalf("pragma: %v", err)
		}
	}
	if err := db.RunMigrations(rawDB, db.DialectSQLite); err != nil {
		t.Fatalf("schema migration: %v", err)
	}
	t.Cleanup(func() { rawDB.Close() })

	compatDB := db.NewCompatDB(rawDB, db.DialectSQLite)

	return &testHandlers{
		db:           compatDB,
		authH:        &auth.Handler{DB: compatDB, JWTSecret: "test-secret"},
		feedH:        &feed.Handler{DB: compatDB, MinioBucket: "test-bucket", LTRModelPath: ""},
		clipsH:       &clips.Handler{DB: compatDB, Minio: nil, MinioBucket: "test-bucket"},
		adminH:       &admin.Handler{DB: compatDB, AdminUsername: "admin", AdminPassword: "admin-pw", AdminJWTSecret: "test-admin-secret"},
		workerH:      &worker.Handler{DB: compatDB, WorkerSecret: "test-worker-secret", CookieSecret: "test-cookie-secret"},
		ingestH:      &ingest.Handler{DB: compatDB},
		savedH:       &saved.Handler{DB: compatDB, MinioBucket: "test-bucket"},
		collectionsH: &collections.Handler{DB: compatDB, MinioBucket: "test-bucket"},
		jobsH:        &jobs.Handler{DB: compatDB},
		profileH:     &profile.Handler{DB: compatDB, CookieSecret: "test-cookie-secret"},
		scoutH:       &scout.Handler{DB: compatDB},
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

func registerUser(t *testing.T, h *testHandlers, username, password string) string {
	t.Helper()
	body := map[string]string{"username": username, "email": username + "@test.com", "password": password}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.authH.HandleRegister(rec, req)
	if rec.Code != 201 {
		t.Fatalf("register failed: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	return resp["token"].(string)
}

func authRequest(t *testing.T, h *testHandlers, method, url string, body interface{}, token string) *http.Request {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		if uid := auth.ExtractUserIDFromToken(req, h.authH.JWTSecret); uid != "" {
			ctx := context.WithValue(req.Context(), auth.UserIDKey, uid)
			req = req.WithContext(ctx)
		}
	}
	return req
}

func withChiParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// --- buildBrowserStreamURL ---

func TestBuildBrowserStreamURL(t *testing.T) {
	got, err := clips.BuildBrowserStreamURL("http://minio:9000/clips/my/video.mp4?X-Amz-Signature=abc123")
	if err != nil {
		t.Fatalf("BuildBrowserStreamURL returned error: %v", err)
	}
	want := "/storage/clips/my/video.mp4?X-Amz-Signature=abc123"
	if got != want {
		t.Fatalf("BuildBrowserStreamURL = %q, want %q", got, want)
	}
}

func TestBuildBrowserStreamURL_InvalidURL(t *testing.T) {
	if _, err := clips.BuildBrowserStreamURL("://bad-url"); err == nil {
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
			got := ingest.DetectPlatform(tc.url)
			if got != tc.expected {
				t.Errorf("DetectPlatform(%q) = %q, want %q", tc.url, got, tc.expected)
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
	httputil.WriteJSON(rec, 201, map[string]string{"msg": "created"})

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
	h := newTestHandlers(t)
	body := `{"username":"testuser","email":"test@example.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleRegister(rec, req)

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
	h := newTestHandlers(t)
	body := `{"username":"ab","email":"a@b.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleRegister(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRegister_ShortPassword(t *testing.T) {
	h := newTestHandlers(t)
	body := `{"username":"testuser","email":"a@b.com","password":"short"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleRegister(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRegister_DuplicateUsername(t *testing.T) {
	h := newTestHandlers(t)
	registerUser(t, h, "duplicate", "password123")

	body := `{"username":"duplicate","email":"other@test.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleRegister(rec, req)

	if rec.Code != 409 {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestRegister_InvalidJSON(t *testing.T) {
	h := newTestHandlers(t)
	req := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()

	h.authH.HandleRegister(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Auth: Login ---

func TestLogin_Success(t *testing.T) {
	h := newTestHandlers(t)
	registerUser(t, h, "loginuser", "password123")

	body := `{"username":"loginuser","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleLogin(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["token"] == nil || resp["token"] == "" {
		t.Error("expected token")
	}
}

func TestLogin_ByEmail(t *testing.T) {
	h := newTestHandlers(t)
	registerUser(t, h, "emailuser", "password123")

	body := `{"username":"emailuser@test.com","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleLogin(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	h := newTestHandlers(t)
	registerUser(t, h, "wrongpw", "password123")

	body := `{"username":"wrongpw","password":"wrong"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleLogin(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestLogin_NonexistentUser(t *testing.T) {
	h := newTestHandlers(t)

	body := `{"username":"nobody","password":"password123"}`
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	h.authH.HandleLogin(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// --- JWT / Middleware ---

func TestGenerateToken_And_ExtractUserID(t *testing.T) {
	h := newTestHandlers(t)
	token := auth.GenerateToken("user-123", h.authH.JWTSecret)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	userID := auth.ExtractUserIDFromToken(req, h.authH.JWTSecret)
	if userID != "user-123" {
		t.Errorf("ExtractUserIDFromToken = %q, want %q", userID, "user-123")
	}
}

func TestExtractUserID_NoHeader(t *testing.T) {
	h := newTestHandlers(t)
	req := httptest.NewRequest("GET", "/", nil)
	if got := auth.ExtractUserIDFromToken(req, h.authH.JWTSecret); got != "" {
		t.Errorf("ExtractUserIDFromToken = %q, want empty", got)
	}
}

func TestExtractUserID_InvalidToken(t *testing.T) {
	h := newTestHandlers(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	if got := auth.ExtractUserIDFromToken(req, h.authH.JWTSecret); got != "" {
		t.Errorf("ExtractUserIDFromToken = %q, want empty", got)
	}
}

func TestExtractUserID_WrongSecret(t *testing.T) {
	token := auth.GenerateToken("user-123", "test-secret")

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if got := auth.ExtractUserIDFromToken(req, "different-secret"); got != "" {
		t.Errorf("ExtractUserIDFromToken = %q, want empty (wrong secret)", got)
	}
}

func TestAuthMiddleware_Unauthorized(t *testing.T) {
	h := newTestHandlers(t)
	handler := h.authH.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_Authorized(t *testing.T) {
	h := newTestHandlers(t)
	token := auth.GenerateToken("user-abc", h.authH.JWTSecret)

	var capturedUID string
	handler := h.authH.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUID = r.Context().Value(auth.UserIDKey).(string)
		httputil.WriteJSON(w, 200, map[string]string{"ok": "true"})
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
	h := newTestHandlers(t)
	token := registerUser(t, h, "interactor", "password123")

	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src1', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('clip1', 'src1', 30.0, 'key1', 'ready')`)

	for _, action := range []string{"view", "like", "dislike", "save", "share", "skip", "watch_full"} {
		t.Run(action, func(t *testing.T) {
			body := map[string]interface{}{"action": action, "watch_duration_seconds": 10.0, "watch_percentage": 0.5}
			req := authRequest(t, h, "POST", "/api/clips/clip1/interact", body, token)
			req = withChiParam(req, "id", "clip1")
			rec := httptest.NewRecorder()

			h.clipsH.HandleInteraction(rec, req)

			if rec.Code != 200 {
				t.Errorf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleInteraction_InvalidAction(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "badaction", "password123")

	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src2', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('clip2', 'src2', 30.0, 'key2', 'ready')`)

	body := map[string]interface{}{"action": "invalid_action"}
	req := authRequest(t, h, "POST", "/api/clips/clip2/interact", body, token)
	req = withChiParam(req, "id", "clip2")
	rec := httptest.NewRecorder()

	h.clipsH.HandleInteraction(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Feed ---

func TestHandleFeed_Anonymous(t *testing.T) {
	h := newTestHandlers(t)

	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src3', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score) VALUES ('c1', 'src3', 'Test Clip', 30.0, 'key', 'ready', 0.8)`)

	req := httptest.NewRequest("GET", "/api/feed", nil)
	rec := httptest.NewRecorder()
	h.feedH.HandleFeed(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	clipsList := resp["clips"].([]interface{})
	if len(clipsList) != 1 {
		t.Errorf("got %d clips, want 1", len(clipsList))
	}
}

func TestHandleFeed_Authenticated(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "feeduser", "password123")

	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src4', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score) VALUES ('c2', 'src4', 'Test Clip', 30.0, 'key', 'ready', 0.8)`)

	req := authRequest(t, h, "GET", "/api/feed", nil, token)
	rec := httptest.NewRecorder()
	h.authH.OptionalAuth(h.feedH.HandleFeed)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	clipsList := resp["clips"].([]interface{})
	if len(clipsList) != 1 {
		t.Errorf("got %d clips, want 1", len(clipsList))
	}
}

func TestHandleFeed_DedupeSeenToggle(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "dedupeuser", "password123")

	var userID string
	if err := h.db.QueryRow(`SELECT id FROM users WHERE username = 'dedupeuser'`).Scan(&userID); err != nil {
		t.Fatalf("fetch user id: %v", err)
	}

	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src-dedupe', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score) VALUES ('c-dedupe', 'src-dedupe', 'Seen Clip', 30.0, 'k', 'ready', 0.8)`)
	h.db.Exec(`INSERT INTO interactions (id, user_id, clip_id, action, created_at) VALUES ('i-dedupe', ?, 'c-dedupe', 'view', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`, userID)

	req := authRequest(t, h, "GET", "/api/feed", nil, token)
	rec := httptest.NewRecorder()
	h.authH.OptionalAuth(h.feedH.HandleFeed)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	clipsList := resp["clips"].([]interface{})
	if len(clipsList) != 0 {
		t.Fatalf("got %d clips, want 0 with dedupe_seen_24h enabled", len(clipsList))
	}

	h.db.Exec(`UPDATE user_preferences SET dedupe_seen_24h = 0 WHERE user_id = ?`, userID)
	req = authRequest(t, h, "GET", "/api/feed", nil, token)
	rec = httptest.NewRecorder()
	h.authH.OptionalAuth(h.feedH.HandleFeed)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp = decodeJSON(t, rec)
	clipsList = resp["clips"].([]interface{})
	if len(clipsList) != 1 {
		t.Fatalf("got %d clips, want 1 with dedupe_seen_24h disabled", len(clipsList))
	}
}

func TestHandleFeed_FiltersProcessingClips(t *testing.T) {
	h := newTestHandlers(t)

	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src5', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status) VALUES ('c3', 'src5', 'Processing', 30.0, 'key', 'processing')`)

	req := httptest.NewRequest("GET", "/api/feed", nil)
	rec := httptest.NewRecorder()
	h.feedH.HandleFeed(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["clips"] != nil {
		clipsList := resp["clips"].([]interface{})
		if len(clipsList) != 0 {
			t.Errorf("got %d clips, want 0 (processing clips filtered)", len(clipsList))
		}
	}
}

// --- GetClip ---

func TestHandleGetClip_Found(t *testing.T) {
	h := newTestHandlers(t)
	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src6', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, title, description, duration_seconds, storage_key, thumbnail_key, status) VALUES ('c4', 'src6', 'My Clip', '', 42.0, 'key', '', 'ready')`)

	req := httptest.NewRequest("GET", "/api/clips/c4", nil)
	req = withChiParam(req, "id", "c4")
	rec := httptest.NewRecorder()

	h.clipsH.HandleGetClip(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeJSON(t, rec)
	if resp["title"] != "My Clip" {
		t.Errorf("title = %v, want %q", resp["title"], "My Clip")
	}
}

func TestHandleGetClip_NotFound(t *testing.T) {
	h := newTestHandlers(t)

	req := httptest.NewRequest("GET", "/api/clips/nonexistent", nil)
	req = withChiParam(req, "id", "nonexistent")
	rec := httptest.NewRecorder()

	h.clipsH.HandleGetClip(rec, req)

	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- Save / Unsave ---

func TestSaveAndUnsaveClip(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "saver", "password123")

	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src7', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, duration_seconds, storage_key, status) VALUES ('c5', 'src7', 30.0, 'key', 'ready')`)

	// Save
	req := authRequest(t, h, "POST", "/api/clips/c5/save", nil, token)
	req = withChiParam(req, "id", "c5")
	rec := httptest.NewRecorder()
	h.savedH.HandleSaveClip(rec, req)
	if rec.Code != 200 {
		t.Fatalf("save: status = %d, want 200", rec.Code)
	}

	// Unsave
	req = authRequest(t, h, "DELETE", "/api/clips/c5/save", nil, token)
	req = withChiParam(req, "id", "c5")
	rec = httptest.NewRecorder()
	h.savedH.HandleUnsaveClip(rec, req)
	if rec.Code != 200 {
		t.Fatalf("unsave: status = %d, want 200", rec.Code)
	}
}

func TestSaveClip_NotFound(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "saver2", "password123")

	req := authRequest(t, h, "POST", "/api/clips/nonexistent/save", nil, token)
	req = withChiParam(req, "id", "nonexistent")
	rec := httptest.NewRecorder()
	h.savedH.HandleSaveClip(rec, req)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- Ingest ---

func TestHandleIngest_ValidURL(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "ingestor", "password123")

	body := map[string]string{"url": "https://www.youtube.com/watch?v=test123"}
	req := authRequest(t, h, "POST", "/api/ingest", body, token)
	rec := httptest.NewRecorder()
	h.ingestH.HandleIngest(rec, req)

	if rec.Code != 202 {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["source_id"] == nil || resp["job_id"] == nil {
		t.Error("expected source_id and job_id in response")
	}
}

func TestHandleIngest_InvalidURL(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "badingest", "password123")

	body := map[string]string{"url": "not-a-url"}
	req := authRequest(t, h, "POST", "/api/ingest", body, token)
	rec := httptest.NewRecorder()
	h.ingestH.HandleIngest(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Jobs ---

func TestHandleListJobs_Empty(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "jobuser", "password123")

	req := authRequest(t, h, "GET", "/api/jobs", nil, token)
	rec := httptest.NewRecorder()
	h.jobsH.HandleListJobs(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// --- Profile ---

func TestHandleGetProfile(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "profuser", "password123")

	req := authRequest(t, h, "GET", "/api/me", nil, token)
	rec := httptest.NewRecorder()
	h.profileH.HandleGetProfile(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["username"] != "profuser" {
		t.Errorf("username = %v, want %q", resp["username"], "profuser")
	}
}

func TestHandleUpdatePreferences(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "prefuser", "password123")

	body := map[string]interface{}{"exploration_rate": 0.5, "autoplay": true}
	req := authRequest(t, h, "PUT", "/api/me/preferences", body, token)
	rec := httptest.NewRecorder()
	h.profileH.HandleUpdatePreferences(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdatePreferences_InvalidRange(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "badpref", "password123")

	body := map[string]interface{}{"exploration_rate": 5.0}
	req := authRequest(t, h, "PUT", "/api/me/preferences", body, token)
	rec := httptest.NewRecorder()
	h.profileH.HandleUpdatePreferences(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Collections ---

func TestCollectionsCRUD(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "collector", "password123")

	// Create
	body := map[string]string{"title": "My Collection", "description": "Test desc"}
	req := authRequest(t, h, "POST", "/api/collections", body, token)
	rec := httptest.NewRecorder()
	h.collectionsH.HandleCreateCollection(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	colID := resp["id"].(string)

	// List
	req = authRequest(t, h, "GET", "/api/collections", nil, token)
	rec = httptest.NewRecorder()
	h.collectionsH.HandleListCollections(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: status = %d, want 200", rec.Code)
	}

	// Delete
	req = authRequest(t, h, "DELETE", "/api/collections/"+colID, nil, token)
	req = withChiParam(req, "id", colID)
	rec = httptest.NewRecorder()
	h.collectionsH.HandleDeleteCollection(rec, req)
	if rec.Code != 200 {
		t.Fatalf("delete: status = %d, want 200", rec.Code)
	}
}

// --- LTR Model ---

func TestLTRModelScore_SumsLeafValues(t *testing.T) {
	model := &feed.LTRModel{
		Trees: [][]feed.LTRTree{
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
	h := newTestHandlers(t)
	token := registerUser(t, h, "l2r-user", "password123")

	h.db.Exec(`UPDATE user_preferences SET exploration_rate = 0 WHERE user_id = (SELECT id FROM users WHERE username = 'l2r-user')`)
	h.db.Exec(`INSERT INTO sources (id, url, platform) VALUES ('src-l2r', 'http://x.com', 'direct')`)
	h.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, transcript, file_size_bytes) VALUES ('l2r-a', 'src-l2r', 'Long High Score', 40.0, 'k1', 'ready', 0.9, '', 100)`)
	h.db.Exec(`INSERT INTO clips (id, source_id, title, duration_seconds, storage_key, status, content_score, transcript, file_size_bytes) VALUES ('l2r-b', 'src-l2r', 'Short Lower Score', 8.0, 'k2', 'ready', 0.4, '', 100)`)

	h.feedH.SetLTRModel(&feed.LTRModel{
		NumFeatures: 13,
		Trees: [][]feed.LTRTree{
			{
				{FeatureIndex: 0, Threshold: 0.5, LeftChild: 1, RightChild: 2},
				{LeafValue: 2.0, IsLeaf: true},
				{LeafValue: -1.0, IsLeaf: true},
			},
		},
	})

	req := authRequest(t, h, "GET", "/api/feed", nil, token)
	rec := httptest.NewRecorder()
	h.authH.OptionalAuth(h.feedH.HandleFeed)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	clipsList := resp["clips"].([]interface{})
	if len(clipsList) < 2 {
		t.Fatalf("got %d clips, want at least 2", len(clipsList))
	}
	first := clipsList[0].(map[string]interface{})
	if first["title"] != "Short Lower Score" {
		t.Fatalf("first clip = %v, want Short Lower Score", first["title"])
	}
	if _, ok := first["_source_id"]; ok {
		t.Fatal("internal L2R fields should not be exposed in feed response")
	}
}

// --- Worker API ---

func TestWorkerAuth_ValidSecret(t *testing.T) {
	h := newTestHandlers(t)
	called := false
	handler := h.workerH.WorkerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		httputil.WriteJSON(w, 200, map[string]string{"ok": "true"})
	}))

	req := httptest.NewRequest("POST", "/api/internal/jobs/claim", nil)
	req.Header.Set("Authorization", "Bearer test-worker-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Error("inner handler not called")
	}
}

func TestWorkerAuth_InvalidSecret(t *testing.T) {
	h := newTestHandlers(t)
	handler := h.workerH.WorkerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not be called")
	}))

	req := httptest.NewRequest("POST", "/api/internal/jobs/claim", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// --- Scout ---

func TestScoutSourceCRUD(t *testing.T) {
	h := newTestHandlers(t)
	token := registerUser(t, h, "scoutuser", "password123")

	// Create source
	body := map[string]interface{}{"source_type": "channel", "platform": "youtube", "identifier": "@test"}
	req := authRequest(t, h, "POST", "/api/scout/sources", body, token)
	rec := httptest.NewRecorder()
	h.scoutH.HandleCreateScoutSource(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	sourceID := resp["id"].(string)

	// List sources
	req = authRequest(t, h, "GET", "/api/scout/sources", nil, token)
	rec = httptest.NewRecorder()
	h.scoutH.HandleListScoutSources(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: status = %d, want 200", rec.Code)
	}

	// Delete source
	req = authRequest(t, h, "DELETE", "/api/scout/sources/"+sourceID, nil, token)
	req = withChiParam(req, "id", sourceID)
	rec = httptest.NewRecorder()
	h.scoutH.HandleDeleteScoutSource(rec, req)
	if rec.Code != 200 {
		t.Fatalf("delete: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// --- Slugify / Truncate ---

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Machine Learning", "machine-learning"},
		{"  AI & ML  ", "ai-ml"},
		{"hello", "hello"},
	}
	for _, tc := range tests {
		got := worker.Slugify(tc.input)
		if got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	got := worker.Truncate("hello world", 5)
	if got != "hello" {
		t.Errorf("Truncate = %q, want %q", got, "hello")
	}
	got = worker.Truncate("hi", 10)
	if got != "hi" {
		t.Errorf("Truncate = %q, want %q", got, "hi")
	}
}
