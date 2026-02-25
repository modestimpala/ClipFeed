package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
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

	// Batched counts: database, content, queue stats in a single query
	var totalUsers, totalInteractions int
	var dbSizeMB float64
	var readyClips, processingClips, failedClips, expiredClips, evictedClips int
	var totalBytes int64
	var queuedJobs, runningJobs, completeJobs, failedJobs int
	var rejectedJobs int

	if err := a.db.QueryRow(fmt.Sprintf(`
		SELECT
			(SELECT COUNT(*) FROM users),
			(SELECT COUNT(*) FROM interactions),
			%s,
			(SELECT COUNT(*) FROM clips WHERE status = 'ready'),
			(SELECT COUNT(*) FROM clips WHERE status = 'processing'),
			(SELECT COUNT(*) FROM clips WHERE status = 'failed'),
			(SELECT COUNT(*) FROM clips WHERE status = 'expired'),
			(SELECT COUNT(*) FROM clips WHERE status = 'evicted'),
			(SELECT COALESCE(SUM(file_size_bytes), 0) FROM clips WHERE status = 'ready'),
			(SELECT COUNT(*) FROM jobs WHERE status = 'queued'),
			(SELECT COUNT(*) FROM jobs WHERE status = 'running'),
			(SELECT COUNT(*) FROM jobs WHERE status = 'complete'),
			(SELECT COUNT(*) FROM jobs WHERE status = 'failed'),
			(SELECT COUNT(*) FROM jobs WHERE status = 'rejected')
	`, a.db.DBSizeExpr())).Scan(&totalUsers, &totalInteractions, &dbSizeMB,
		&readyClips, &processingClips, &failedClips, &expiredClips, &evictedClips, &totalBytes,
		&queuedJobs, &runningJobs, &completeJobs, &failedJobs, &rejectedJobs); err != nil {
		log.Printf("admin status: stats query failed: %v", err)
	}

	stats["database"] = map[string]interface{}{
		"total_users":        totalUsers,
		"total_interactions": totalInteractions,
		"size_mb":            dbSizeMB,
	}

	stats["content"] = map[string]interface{}{
		"ready":      readyClips,
		"processing": processingClips,
		"failed":     failedClips,
		"expired":    expiredClips,
		"evicted":    evictedClips,
		"storage_gb": float64(totalBytes) / (1024 * 1024 * 1024),
	}

	stats["queue"] = map[string]interface{}{
		"queued":   queuedJobs,
		"running":  runningJobs,
		"complete": completeJobs,
		"failed":   failedJobs,
		"rejected": rejectedJobs,
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
		if err := rows.Err(); err != nil {
			log.Printf("fetchDailyStats: rows iteration error: %v", err)
		}
		return res
	}

	stats["graphs"] = map[string]interface{}{
		"interactions_7d": fetchDailyStats(fmt.Sprintf(`
			SELECT %s as d, COUNT(*) 
			FROM interactions 
			WHERE created_at >= %s 
			GROUP BY d ORDER BY d ASC`, a.db.DateOfExpr("created_at"), a.db.DateExpr("-7 days"))),
		"clips_7d": fetchDailyStats(fmt.Sprintf(`
			SELECT %s as d, COUNT(*) 
			FROM clips 
			WHERE created_at >= %s AND status = 'ready'
			GROUP BY d ORDER BY d ASC`, a.db.DateOfExpr("created_at"), a.db.DateExpr("-7 days"))),
	}

	// LLM / AI Stats — batched into one query
	var totalSummaries, evaluatedCandidates, approvedCandidates int
	var avgScore float64
	
	if err := a.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM clip_summaries),
			(SELECT COUNT(*) FROM scout_candidates WHERE llm_score IS NOT NULL),
			(SELECT COUNT(*) FROM scout_candidates WHERE status = 'ingested'),
			(SELECT COALESCE(AVG(llm_score), 0) FROM scout_candidates WHERE llm_score IS NOT NULL)
	`).Scan(&totalSummaries, &evaluatedCandidates, &approvedCandidates, &avgScore); err != nil {
		log.Printf("admin status: AI stats query failed: %v", err)
	}

	stats["ai"] = map[string]interface{}{
		"clip_summaries":      totalSummaries,
		"scout_evaluated":     evaluatedCandidates,
		"scout_approved":      approvedCandidates,
		"avg_scout_llm_score": avgScore,
	}

	// Recent failed jobs with error details
	type FailedJob struct {
		ID        string  `json:"id"`
		Title     *string `json:"title"`
		URL       *string `json:"url"`
		Error     *string `json:"error"`
		Attempts  int     `json:"attempts"`
		FailedAt  *string `json:"failed_at"`
	}

	failedRows, err := a.db.Query(`
		SELECT j.id, s.title, s.url, j.error, j.attempts, j.completed_at
		FROM jobs j
		LEFT JOIN sources s ON j.source_id = s.id
		WHERE j.status = 'failed'
		ORDER BY COALESCE(j.completed_at, j.created_at) DESC
		LIMIT 10
	`)
	var recentFailed []FailedJob
	if err == nil {
		defer failedRows.Close()
		for failedRows.Next() {
			var fj FailedJob
			if err := failedRows.Scan(&fj.ID, &fj.Title, &fj.URL, &fj.Error, &fj.Attempts, &fj.FailedAt); err == nil {
				recentFailed = append(recentFailed, fj)
			}
		}
	}
	stats["recent_failures"] = recentFailed

	// Auto-purge: delete failed jobs older than 48 hours (they can be re-ingested naturally)
	// Also purge rejected jobs (validation failures like duration exceeded)
	purged, _ := a.db.Exec(fmt.Sprintf(`
		DELETE FROM jobs
		WHERE (status = 'failed' AND attempts >= max_attempts
		        AND %s)
		   OR (status = 'rejected'
		        AND %s)
	`, a.db.PurgeDatetimeComparison("COALESCE(completed_at, created_at)", "-48 hours"),
		a.db.PurgeDatetimeComparison("COALESCE(completed_at, created_at)", "-24 hours")))
	if purged != nil {
		if n, _ := purged.RowsAffected(); n > 0 {
			log.Printf("admin status: auto-purged %d stale failed jobs", n)
		}
	}

	writeJSON(w, 200, stats)
}

func (a *App) handleClearFailedJobs(w http.ResponseWriter, r *http.Request) {
	// Reset associated sources back to pending so they can be re-ingested naturally
	_, err := a.db.Exec(`
		UPDATE sources SET status = 'pending'
		WHERE id IN (
			SELECT source_id FROM jobs
			WHERE status = 'failed' AND source_id IS NOT NULL
		)
	`)
	if err != nil {
		log.Printf("admin clear-failed: source reset error: %v", err)
	}

	result, err := a.db.Exec(`DELETE FROM jobs WHERE status = 'failed'`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to clear jobs"})
		return
	}
	cleared, _ := result.RowsAffected()
	log.Printf("admin: cleared %d failed jobs", cleared)
	writeJSON(w, 200, map[string]interface{}{"cleared": cleared})
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
