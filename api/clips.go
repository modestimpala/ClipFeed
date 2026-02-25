package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (a *App) handleGetClip(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var id, title, description, thumbnailKey, topicsJSON, tagsJSON, status, createdAt string
	var duration, score float64
	var width, height, fileSize *int64
	var channelName, platform, sourceURL *string

	err := a.db.QueryRowContext(r.Context(), `
		SELECT c.id, c.title, c.description, c.duration_seconds,
		       c.thumbnail_key, c.topics, c.tags, c.content_score,
		       c.status, c.created_at, c.width, c.height, c.file_size_bytes,
		       s.channel_name, s.platform, s.url
		FROM clips c
		LEFT JOIN sources s ON c.source_id = s.id
		WHERE c.id = ?
	`, clipID).Scan(&id, &title, &description, &duration,
		&thumbnailKey, &topicsJSON, &tagsJSON, &score,
		&status, &createdAt, &width, &height, &fileSize,
		&channelName, &platform, &sourceURL)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	var topics, tags []string
	json.Unmarshal([]byte(topicsJSON), &topics)
	json.Unmarshal([]byte(tagsJSON), &tags)

	writeJSON(w, 200, map[string]interface{}{
		"id": id, "title": title, "description": description,
		"duration_seconds": duration, "thumbnail_key": thumbnailKey,
		"thumbnail_url": thumbnailURL(a.cfg.MinioBucket, thumbnailKey),
		"topics": topics, "tags": tags, "content_score": score,
		"status": status, "created_at": createdAt,
		"width": width, "height": height, "file_size_bytes": fileSize,
		"channel_name": channelName, "platform": platform,
		"source_url": sourceURL,
	})
}

func (a *App) handleStreamClip(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var storageKey string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT storage_key FROM clips WHERE id = ? AND status = 'ready'`,
		clipID).Scan(&storageKey)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	presignedURL, err := a.minio.PresignedGetObject(r.Context(),
		a.cfg.MinioBucket, storageKey, 2*time.Hour, nil)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to generate stream URL"})
		return
	}
	streamURL, err := buildBrowserStreamURL(presignedURL.String())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to build stream URL"})
		return
	}

	writeJSON(w, 200, map[string]string{"url": streamURL})
}

func buildBrowserStreamURL(presigned string) (string, error) {
	u, err := url.Parse(presigned)
	if err != nil || u.Path == "" {
		return "", fmt.Errorf("invalid presigned URL")
	}

	streamPath := "/storage" + u.EscapedPath()
	if u.RawQuery != "" {
		streamPath += "?" + u.RawQuery
	}
	return streamPath, nil
}

// --- Interactions ---

type InteractionRequest struct {
	Action          string  `json:"action"`
	WatchDuration   float64 `json:"watch_duration_seconds"`
	WatchPercentage float64 `json:"watch_percentage"`
}

func (a *App) handleInteraction(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	clipID := chi.URLParam(r, "id")

	var req InteractionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	validActions := map[string]bool{
		"view": true, "like": true, "dislike": true,
		"save": true, "share": true, "skip": true, "watch_full": true,
	}
	if !validActions[req.Action] {
		writeJSON(w, 400, map[string]string{"error": "invalid action"})
		return
	}

	// Verify clip exists before inserting interaction
	var exists int
	if err := a.db.QueryRowContext(r.Context(), `SELECT 1 FROM clips WHERE id = ?`, clipID).Scan(&exists); err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}

	interactionID := uuid.New().String()
	_, err := a.db.ExecContext(r.Context(), `
		INSERT INTO interactions (id, user_id, clip_id, action, watch_duration_seconds, watch_percentage)
		VALUES (?, ?, ?, ?, ?, ?)
	`, interactionID, userID, clipID, req.Action, req.WatchDuration, req.WatchPercentage)

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to record interaction"})
		return
	}

	writeJSON(w, 200, map[string]string{"status": "recorded"})
}

// --- Clip Summary (LLM) ---

func (a *App) handleClipSummary(w http.ResponseWriter, r *http.Request) {
	clipID := chi.URLParam(r, "id")

	var summary, model string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT summary, model FROM clip_summaries WHERE clip_id = ?`, clipID,
	).Scan(&summary, &model)
	if err == nil {
		log.Printf("[LLM] Summary cache hit for clip %s (model=%s)", clipID, model)
		writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": summary, "model": model, "cached": true})
		return
	}

	var transcript string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(transcript, '') FROM clips WHERE id = ?`, clipID,
	).Scan(&transcript)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "clip not found"})
		return
	}
	if transcript == "" {
		log.Printf("[LLM] No transcript for clip %s — skipping summary", clipID)
		writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": "", "model": "", "cached": false})
		return
	}

	prompt := fmt.Sprintf("Summarize this video transcript in 1-2 sentences:\n\n%s", transcript)
	if runes := []rune(prompt); len(runes) > 4000 {
		prompt = string(runes[:4000])
	}

	log.Printf("[LLM] Generating summary for clip %s (transcript_len=%d)", clipID, len(transcript))
	start := time.Now()
	summaryText, modelName, err := generateSummaryWithLLM(prompt)
	durationMs := time.Since(start).Milliseconds()
	
	if err != nil {
		log.Printf("[LLM] Summary generation FAILED for clip %s: %v", clipID, err)
		a.db.ExecContext(r.Context(),
			`INSERT INTO llm_logs (system, model, prompt, error, duration_ms) VALUES (?, ?, ?, ?, ?)`,
			"summary", modelName, prompt, err.Error(), durationMs)
		writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": "", "error": "LLM unavailable"})
		return
	}

	log.Printf("[LLM] Summary generated for clip %s: model=%s summary_len=%d", clipID, modelName, len(summaryText))

	a.db.ExecContext(r.Context(),
		`INSERT INTO llm_logs (system, model, prompt, response, duration_ms) VALUES (?, ?, ?, ?, ?)`,
		"summary", modelName, prompt, summaryText, durationMs)

	if summaryText != "" {
		if _, err := a.db.ExecContext(r.Context(),
			`INSERT OR REPLACE INTO clip_summaries (clip_id, summary, model) VALUES (?, ?, ?)`,
			clipID, summaryText, modelName); err != nil {
			log.Printf("[LLM] Failed to cache summary for clip %s: %v", clipID, err)
		}
	}

	writeJSON(w, 200, map[string]interface{}{"clip_id": clipID, "summary": summaryText, "cached": false})
}

func generateSummaryWithLLM(prompt string) (string, string, error) {
	provider := strings.ToLower(strings.TrimSpace(getEnv("LLM_PROVIDER", "ollama")))
	model := strings.TrimSpace(getEnv("LLM_MODEL", ""))
	if model == "" {
		model = getEnv("OLLAMA_MODEL", "llama3.2:3b")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(getEnv("LLM_BASE_URL", "")), "/")
	if baseURL == "" {
		if provider == "" || provider == "ollama" {
			baseURL = strings.TrimRight(getEnv("LLM_URL", "http://llm:11434"), "/")
		} else if provider == "anthropic" {
			baseURL = "https://api.anthropic.com/v1"
		} else {
			baseURL = "https://api.openai.com/v1"
		}
	}

	log.Printf("[LLM] Summary request: provider=%s model=%s base_url=%s prompt_len=%d", provider, model, baseURL, len(prompt))

	start := time.Now()
	client := &http.Client{Timeout: 60 * time.Second}
	if provider == "" || provider == "ollama" {
		reqBody, _ := json.Marshal(map[string]interface{}{
			"model": model,
			"prompt": prompt,
			"stream": false,
		})

		endpoint := baseURL + "/api/generate"
		log.Printf("[LLM] POST %s (model=%s)", endpoint, model)
		resp, err := client.Post(endpoint, "application/json", strings.NewReader(string(reqBody)))
		if err != nil {
			log.Printf("[LLM] Request FAILED: %v (elapsed=%v)", err, time.Since(start))
			return "", model, err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			log.Printf("[LLM] Request returned error status=%d (elapsed=%v)", resp.StatusCode, time.Since(start))
			return "", model, fmt.Errorf("llm request failed: status=%d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[LLM] Response read FAILED: %v", err)
			return "", model, err
		}
		var result struct {
			Response string `json:"response"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			log.Printf("[LLM] Response parse FAILED: %v", err)
			return "", model, err
		}
		text := strings.TrimSpace(result.Response)
		log.Printf("[LLM] Ollama response: model=%s elapsed=%v response_len=%d", model, time.Since(start), len(text))
		return text, model, nil
	}

	apiKey := strings.TrimSpace(getEnv("LLM_API_KEY", ""))
	if apiKey == "" {
		log.Printf("[LLM] No API key configured for provider=%s", provider)
		return "", model, fmt.Errorf("missing API key")
	}

	if provider == "anthropic" {
		anthropicVersion := strings.TrimSpace(getEnv("ANTHROPIC_VERSION", "2023-06-01"))
		if anthropicVersion == "" {
			anthropicVersion = "2023-06-01"
		}

		reqBody, _ := json.Marshal(map[string]interface{}{
			"model": model,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
			"max_tokens": 180,
			"temperature": 0.2,
		})

		endpoint := baseURL + "/messages"
		log.Printf("[LLM] POST %s (model=%s, anthropic_version=%s)", endpoint, model, anthropicVersion)
		req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(reqBody)))
		if err != nil {
			return "", model, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[LLM] Anthropic request FAILED: %v (elapsed=%v)", err, time.Since(start))
			return "", model, err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			log.Printf("[LLM] Anthropic returned error status=%d (elapsed=%v)", resp.StatusCode, time.Since(start))
			return "", model, fmt.Errorf("llm request failed: status=%d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[LLM] Anthropic response read FAILED: %v", err)
			return "", model, err
		}
		var result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			log.Printf("[LLM] Anthropic response parse FAILED: %v", err)
			return "", model, err
		}

		parts := make([]string, 0, len(result.Content))
		for _, part := range result.Content {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				parts = append(parts, strings.TrimSpace(part.Text))
			}
		}

		text := strings.Join(parts, " ")
		log.Printf("[LLM] Anthropic response: model=%s elapsed=%v response_len=%d", model, time.Since(start), len(text))
		return text, model, nil
	}

	// OpenAI-compatible provider
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model": model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 180,
		"temperature": 0.2,
	})

	endpoint := baseURL + "/chat/completions"
	log.Printf("[LLM] POST %s (model=%s)", endpoint, model)
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(reqBody)))
	if err != nil {
		return "", model, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[LLM] OpenAI request FAILED: %v (elapsed=%v)", err, time.Since(start))
		return "", model, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[LLM] OpenAI returned error status=%d (elapsed=%v)", resp.StatusCode, time.Since(start))
		return "", model, fmt.Errorf("llm request failed: status=%d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[LLM] OpenAI response read FAILED: %v", err)
		return "", model, err
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("[LLM] OpenAI response parse FAILED: %v", err)
		return "", model, err
	}
	if len(result.Choices) == 0 {
		log.Printf("[LLM] OpenAI returned 0 choices (elapsed=%v)", time.Since(start))
		return "", model, nil
	}

	var contentText string
	if err := json.Unmarshal(result.Choices[0].Message.Content, &contentText); err == nil {
		return strings.TrimSpace(contentText), model, nil
	}

	var contentParts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(result.Choices[0].Message.Content, &contentParts); err != nil {
		return "", model, nil
	}

	parts := make([]string, 0, len(contentParts))
	for _, p := range contentParts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			parts = append(parts, strings.TrimSpace(p.Text))
		}
	}

	return strings.Join(parts, " "), model, nil
}
