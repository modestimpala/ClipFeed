package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

func scanClips(rows *sql.Rows) []map[string]interface{} {
	var clips []map[string]interface{}
	for rows.Next() {
		var id, title, description, thumbnailKey, topicsJSON, tagsJSON, createdAt, sourceID string
		var duration, score float64
		var transcriptLength, fileSizeBytes, ageHours float64
		var channelName, platform, sourceURL *string

		rows.Scan(&id, &title, &description, &duration,
			&thumbnailKey, &topicsJSON, &tagsJSON, &score,
			&createdAt, &channelName, &platform, &sourceURL,
			&sourceID, &transcriptLength, &fileSizeBytes, &ageHours)

		var topics, tags []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		json.Unmarshal([]byte(tagsJSON), &tags)

		clips = append(clips, map[string]interface{}{
			"id": id, "title": title, "description": description,
			"duration_seconds": duration, "thumbnail_key": thumbnailKey,
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

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
