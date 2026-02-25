package profile

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"clipfeed/auth"
	"clipfeed/crypto"
	"clipfeed/db"
	"clipfeed/httputil"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler holds dependencies for profile and cookie endpoints.
type Handler struct {
	DB           *db.CompatDB
	CookieSecret string
}

// HandleGetProfile returns the authenticated user's profile and preferences.
func (h *Handler) HandleGetProfile(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)

	var username, email, displayName, createdAt string
	var avatarURL *string
	var explorationRate, scoutThreshold, diversityMix, freshnessBias float64
	var topicWeightsJSON string
	var minClip, maxClip int
	var autoplay, dedupeSeen24h, trendingBoost, scoutAutoIngest int

	err := h.DB.QueryRowContext(r.Context(), `
		SELECT u.username, u.email, u.display_name, u.avatar_url, u.created_at,
		       COALESCE(p.exploration_rate, 0.3),
		       COALESCE(p.topic_weights, '{}'),
		       COALESCE(p.dedupe_seen_24h, 1),
		       COALESCE(p.min_clip_seconds, 5),
		       COALESCE(p.max_clip_seconds, 120),
		       COALESCE(p.autoplay, 1),
		       COALESCE(p.scout_threshold, 6.0),
		       COALESCE(p.scout_auto_ingest, 1),
		       COALESCE(p.diversity_mix, 0.5),
		       COALESCE(p.trending_boost, 1),
		       COALESCE(p.freshness_bias, 0.5)
		FROM users u
		LEFT JOIN user_preferences p ON u.id = p.user_id
		WHERE u.id = ?
	`, userID).Scan(&username, &email, &displayName, &avatarURL, &createdAt,
		&explorationRate, &topicWeightsJSON, &dedupeSeen24h, &minClip, &maxClip, &autoplay, &scoutThreshold,
		&scoutAutoIngest, &diversityMix, &trendingBoost, &freshnessBias)

	if err != nil {
		httputil.WriteJSON(w, 404, map[string]string{"error": "user not found"})
		return
	}

	var topicWeights map[string]interface{}
	json.Unmarshal([]byte(topicWeightsJSON), &topicWeights)
	if topicWeights == nil {
		topicWeights = make(map[string]interface{})
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"id": userID, "username": username, "email": email,
		"display_name": displayName, "avatar_url": avatarURL,
		"created_at": createdAt,
		"preferences": map[string]interface{}{
			"exploration_rate":  explorationRate,
			"topic_weights":     topicWeights,
			"dedupe_seen_24h":   dedupeSeen24h == 1,
			"min_clip_seconds":  minClip,
			"max_clip_seconds":  maxClip,
			"autoplay":          autoplay == 1,
			"scout_threshold":   scoutThreshold,
			"scout_auto_ingest": scoutAutoIngest == 1,
			"diversity_mix":     diversityMix,
			"trending_boost":    trendingBoost == 1,
			"freshness_bias":    freshnessBias,
		},
	})
}

// HandleUpdatePreferences updates the user's feed/scout preferences.
func (h *Handler) HandleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	httputil.MaxBody(r, httputil.DefaultBodyLimit)

	var prefs map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&prefs); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	for _, key := range []string{"exploration_rate", "diversity_mix", "freshness_bias"} {
		if v, ok := prefs[key]; ok && v != nil {
			var f float64
			switch vt := v.(type) {
			case float64:
				f = vt
			case json.Number:
				var err error
				f, err = vt.Float64()
				if err != nil {
					httputil.WriteJSON(w, 400, map[string]string{"error": key + " must be a number between 0 and 1"})
					return
				}
			default:
				httputil.WriteJSON(w, 400, map[string]string{"error": key + " must be a number between 0 and 1"})
				return
			}
			if f < 0 || f > 1 {
				httputil.WriteJSON(w, 400, map[string]string{"error": key + " must be between 0 and 1"})
				return
			}
		}
	}

	topicWeights, _ := json.Marshal(prefs["topic_weights"])

	_, err := h.DB.ExecContext(r.Context(), fmt.Sprintf(`
		INSERT INTO user_preferences (user_id, exploration_rate, topic_weights, dedupe_seen_24h, min_clip_seconds, max_clip_seconds, autoplay, scout_threshold, scout_auto_ingest, diversity_mix, trending_boost, freshness_bias)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			exploration_rate  = COALESCE(excluded.exploration_rate,  user_preferences.exploration_rate),
			topic_weights     = COALESCE(excluded.topic_weights,     user_preferences.topic_weights),
			dedupe_seen_24h   = COALESCE(excluded.dedupe_seen_24h,   user_preferences.dedupe_seen_24h),
			min_clip_seconds  = COALESCE(excluded.min_clip_seconds,  user_preferences.min_clip_seconds),
			max_clip_seconds  = COALESCE(excluded.max_clip_seconds,  user_preferences.max_clip_seconds),
			autoplay          = COALESCE(excluded.autoplay,          user_preferences.autoplay),
			scout_threshold   = COALESCE(excluded.scout_threshold,   user_preferences.scout_threshold),
			scout_auto_ingest = COALESCE(excluded.scout_auto_ingest, user_preferences.scout_auto_ingest),
			diversity_mix     = COALESCE(excluded.diversity_mix,     user_preferences.diversity_mix),
			trending_boost    = COALESCE(excluded.trending_boost,    user_preferences.trending_boost),
			freshness_bias    = COALESCE(excluded.freshness_bias,    user_preferences.freshness_bias),
			updated_at        = %s
	`, h.DB.NowUTC()), userID,
		prefs["exploration_rate"],
		string(topicWeights),
		prefs["dedupe_seen_24h"],
		prefs["min_clip_seconds"],
		prefs["max_clip_seconds"],
		prefs["autoplay"],
		prefs["scout_threshold"],
		prefs["scout_auto_ingest"],
		prefs["diversity_mix"],
		prefs["trending_boost"],
		prefs["freshness_bias"],
	)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to update preferences"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "updated"})
}

// ValidPlatforms lists supported cookie platforms.
var ValidPlatforms = map[string]bool{
	"youtube": true, "tiktok": true, "instagram": true, "twitter": true,
}

// HandleSetCookie stores an encrypted platform cookie.
func (h *Handler) HandleSetCookie(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	platform := chi.URLParam(r, "platform")

	if !ValidPlatforms[platform] {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid platform (tiktok, instagram, twitter)"})
		return
	}

	var req struct {
		CookieStr string `json:"cookie_str"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CookieStr == "" {
		httputil.WriteJSON(w, 400, map[string]string{"error": "cookie_str required"})
		return
	}

	encrypted, err := crypto.EncryptCookie(req.CookieStr, h.CookieSecret)
	if err != nil {
		log.Printf("cookie encryption failed: %v", err)
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to save cookie"})
		return
	}

	cookieID := uuid.New().String()
	_, err = h.DB.ExecContext(r.Context(), fmt.Sprintf(`
		INSERT INTO platform_cookies (id, user_id, platform, cookie_str, is_active, updated_at)
		VALUES (?, ?, ?, ?, 1, %s)
		ON CONFLICT(user_id, platform) DO UPDATE SET
			cookie_str = excluded.cookie_str,
			is_active  = 1,
			updated_at = %s
	`, h.DB.NowUTC(), h.DB.NowUTC()), cookieID, userID, platform, encrypted)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to save cookie"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "saved", "platform": platform})
}

// HandleDeleteCookie deactivates a platform cookie.
func (h *Handler) HandleDeleteCookie(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)
	platform := chi.URLParam(r, "platform")

	if !ValidPlatforms[platform] {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid platform"})
		return
	}

	if _, err := h.DB.ExecContext(r.Context(),
		fmt.Sprintf(`UPDATE platform_cookies SET is_active = 0, updated_at = %s
		 WHERE user_id = ? AND platform = ?`, h.DB.NowUTC()),
		userID, platform); err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to remove cookie"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"status": "removed", "platform": platform})
}

// HandleListCookieStatus returns active/inactive status per platform.
func (h *Handler) HandleListCookieStatus(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(auth.UserIDKey).(string)

	statuses := map[string]map[string]interface{}{}
	for platform := range ValidPlatforms {
		statuses[platform] = map[string]interface{}{
			"saved":      false,
			"updated_at": nil,
		}
	}

	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT platform, updated_at FROM platform_cookies WHERE user_id = ? AND is_active = 1`,
		userID,
	)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to list cookie status"})
		return
	}
	defer rows.Close()

	for rows.Next() {
		var platform, updatedAt string
		if rows.Scan(&platform, &updatedAt) != nil {
			continue
		}
		if _, ok := statuses[platform]; ok {
			statuses[platform] = map[string]interface{}{
				"saved":      true,
				"updated_at": updatedAt,
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("handleListCookieStatus: rows iteration error: %v", err)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"platforms": statuses})
}
