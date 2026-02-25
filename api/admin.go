package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func (a *App) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}

	usernameOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(a.cfg.AdminUsername)) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.cfg.AdminPassword)) == 1
	if !usernameOK || !passwordOK {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}

	claims := jwt.MapClaims{
		"sub":   "admin",
		"admin": true,
		"exp":   time.Now().Add(24 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString([]byte(a.cfg.JWTSecret))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to generate token"})
		return
	}

	writeJSON(w, 200, map[string]string{"token": tokenStr})
}

// isAdminToken validates the Bearer JWT and checks the admin:true claim explicitly.
// This is intentionally separate from extractUserID so admin access is never
// accidentally granted by a non-admin token whose sub happens to equal "admin".
func (a *App) isAdminToken(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	tokenStr := strings.TrimPrefix(auth, "Bearer ")
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(a.cfg.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		return false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	isAdmin, _ := claims["admin"].(bool)
	return isAdmin
}

func (a *App) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.isAdminToken(r) {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, "admin")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	stats := map[string]interface{}{
		"system": map[string]interface{}{
			"goroutines":    runtime.NumGoroutine(),
			"memory_mb":     m.Alloc / 1024 / 1024,
			"os_threads":    runtime.GOMAXPROCS(0),
			"go_version":    runtime.Version(),
		},
	}

	// Database Stats
	var totalUsers, totalInteractions int
	a.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&totalUsers)
	a.db.QueryRow("SELECT COUNT(*) FROM interactions").Scan(&totalInteractions)
	
	// Get DB file size
	var dbSizeMB float64 = 0
	if a.cfg.DBPath != "" {
		a.db.QueryRow("SELECT page_count * page_size / 1024.0 / 1024.0 FROM pragma_page_count(), pragma_page_size()").Scan(&dbSizeMB)
	}

	stats["database"] = map[string]interface{}{
		"total_users":        totalUsers,
		"total_interactions": totalInteractions,
		"size_mb":            dbSizeMB,
	}

	// Content Stats
	var readyClips, processingClips, failedClips, expiredClips, evictedClips int
	a.db.QueryRow("SELECT COUNT(*) FROM clips WHERE status = 'ready'").Scan(&readyClips)
	a.db.QueryRow("SELECT COUNT(*) FROM clips WHERE status = 'processing'").Scan(&processingClips)
	a.db.QueryRow("SELECT COUNT(*) FROM clips WHERE status = 'failed'").Scan(&failedClips)
	a.db.QueryRow("SELECT COUNT(*) FROM clips WHERE status = 'expired'").Scan(&expiredClips)
	a.db.QueryRow("SELECT COUNT(*) FROM clips WHERE status = 'evicted'").Scan(&evictedClips)

	var totalBytes int64
	a.db.QueryRow("SELECT COALESCE(SUM(file_size_bytes), 0) FROM clips WHERE status = 'ready'").Scan(&totalBytes)

	stats["content"] = map[string]interface{}{
		"ready":      readyClips,
		"processing": processingClips,
		"failed":     failedClips,
		"expired":    expiredClips,
		"evicted":    evictedClips,
		"storage_gb": float64(totalBytes) / (1024 * 1024 * 1024),
	}

	// Queue Stats
	var queuedJobs, runningJobs, completeJobs, failedJobs int
	a.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = 'queued'").Scan(&queuedJobs)
	a.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = 'running'").Scan(&runningJobs)
	a.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = 'complete'").Scan(&completeJobs)
	a.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = 'failed'").Scan(&failedJobs)

	stats["queue"] = map[string]interface{}{
		"queued":   queuedJobs,
		"running":  runningJobs,
		"complete": completeJobs,
		"failed":   failedJobs,
	}

	// Time-series for Graphs (last 7 days)
	type DailyStat struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	}

	fetchDailyStats := func(query string) []DailyStat {
		rows, err := a.db.Query(query)
		if err != nil {
			return []DailyStat{}
		}
		defer rows.Close()
		
		var res []DailyStat
		for rows.Next() {
			var ds DailyStat
			if err := rows.Scan(&ds.Date, &ds.Count); err == nil {
				res = append(res, ds)
			}
		}
		return res
	}

	stats["graphs"] = map[string]interface{}{
		"interactions_7d": fetchDailyStats(`
			SELECT date(created_at) as d, COUNT(*) 
			FROM interactions 
			WHERE created_at >= date('now', '-7 days') 
			GROUP BY d ORDER BY d ASC`),
		"clips_7d": fetchDailyStats(`
			SELECT date(created_at) as d, COUNT(*) 
			FROM clips 
			WHERE created_at >= date('now', '-7 days') AND status = 'ready'
			GROUP BY d ORDER BY d ASC`),
	}

	// LLM / AI Stats
	var totalSummaries, evaluatedCandidates, approvedCandidates int
	var avgScore float64
	
	a.db.QueryRow("SELECT COUNT(*) FROM clip_summaries").Scan(&totalSummaries)
	a.db.QueryRow("SELECT COUNT(*) FROM scout_candidates WHERE llm_score IS NOT NULL").Scan(&evaluatedCandidates)
	a.db.QueryRow("SELECT COUNT(*) FROM scout_candidates WHERE status = 'ingested'").Scan(&approvedCandidates)
	a.db.QueryRow("SELECT COALESCE(AVG(llm_score), 0) FROM scout_candidates WHERE llm_score IS NOT NULL").Scan(&avgScore)

	stats["ai"] = map[string]interface{}{
		"clip_summaries":      totalSummaries,
		"scout_evaluated":     evaluatedCandidates,
		"scout_approved":      approvedCandidates,
		"avg_scout_llm_score": avgScore,
	}

	writeJSON(w, 200, stats)
}

func (a *App) handleAdminLLMLogs(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`
		SELECT id, system, model, prompt, COALESCE(response, ''), COALESCE(error, ''), duration_ms, created_at 
		FROM llm_logs 
		ORDER BY created_at DESC 
		LIMIT 100
	`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to query logs"})
		return
	}
	defer rows.Close()

	type LLMLog struct {
		ID         int    `json:"id"`
		System     string `json:"system"`
		Model      string `json:"model"`
		Prompt     string `json:"prompt"`
		Response   string `json:"response"`
		Error      string `json:"error"`
		DurationMs int    `json:"duration_ms"`
		CreatedAt  string `json:"created_at"`
	}

	var logs []LLMLog
	for rows.Next() {
		var l LLMLog
		if err := rows.Scan(&l.ID, &l.System, &l.Model, &l.Prompt, &l.Response, &l.Error, &l.DurationMs, &l.CreatedAt); err == nil {
			logs = append(logs, l)
		}
	}

	writeJSON(w, 200, map[string]interface{}{"logs": logs})
}
