package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

func scanClips(rows *sql.Rows) []map[string]interface{} {
	clips := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, title, createdAt, sourceID string
		var description, thumbnailKey, topicsJSON, tagsJSON sql.NullString
		var duration, score float64
		var transcriptLength, fileSizeBytes, ageHours float64
		var channelName, platform, sourceURL *string

		if err := rows.Scan(&id, &title, &description, &duration,
			&thumbnailKey, &topicsJSON, &tagsJSON, &score,
			&createdAt, &channelName, &platform, &sourceURL,
			&sourceID, &transcriptLength, &fileSizeBytes, &ageHours); err != nil {
			continue
		}

		var topics, tags []string
		topicsRaw := topicsJSON.String
		if topicsRaw == "" {
			topicsRaw = "[]"
		}
		tagsRaw := tagsJSON.String
		if tagsRaw == "" {
			tagsRaw = "[]"
		}
		json.Unmarshal([]byte(topicsRaw), &topics)
		json.Unmarshal([]byte(tagsRaw), &tags)

		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "description": description.String,
			"duration_seconds": duration, "thumbnail_key": thumbnailKey.String,
			"topics": topics, "tags": tags, "content_score": score,
			"created_at": createdAt, "channel_name": channelName,
			"platform": platform, "source_url": sourceURL,
			"_source_id": sourceID,
			"_transcript_length": transcriptLength,
			"_file_size_bytes": fileSizeBytes,
			"_age_hours": ageHours,
		})
	}
	return clips
}

// thumbnailURL builds the browser-facing URL for a MinIO object.
// path = "/storage/{bucket}/{key}" which nginx rewrites to /{bucket}/{key}
// and MinIO resolves as bucket + object-key.
func thumbnailURL(bucket, key string) string {
	if key == "" {
		return ""
	}
	return "/storage/" + bucket + "/" + key
}

// addThumbnailURLs enriches clip maps with a thumbnail_url field.
func addThumbnailURLs(clips []map[string]interface{}, bucket string) {
	for _, clip := range clips {
		if key, ok := clip["thumbnail_key"].(string); ok && key != "" {
			clip["thumbnail_url"] = thumbnailURL(bucket, key)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
