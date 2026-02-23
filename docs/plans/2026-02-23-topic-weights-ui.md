# Topic Weights UI — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a transparent topic-weight control UI that reads library-wide topics, lets users set per-topic boost sliders, and integrates those weights into the feed ranking algorithm.

**Architecture:** New `GET /api/topics` endpoint aggregates topics from all clips. `GET /api/me` is extended to return preferences (including `topic_weights`). The feed handler applies a per-clip topic boost multiplier in Go after the SQL query. The frontend adds a "Topic Preferences" section to SettingsScreen with per-topic sliders.

**Tech Stack:** Go (chi router, SQLite), React (Vite), existing CSS token system.

---

### Task 1: Add `GET /api/topics` endpoint

**Files:**
- Modify: `api/main.go`

**Step 1: Add the handler function**

Add after `handleGetProfile` (after line 726), before `handleUpdatePreferences`:

```go
func (a *App) handleGetTopics(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT topics FROM clips WHERE status = 'ready' AND topics != '[]'
	`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to fetch topics"})
		return
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var topicsJSON string
		rows.Scan(&topicsJSON)
		var topics []string
		json.Unmarshal([]byte(topicsJSON), &topics)
		for _, t := range topics {
			counts[t]++
		}
	}

	type topicEntry struct {
		Name      string `json:"name"`
		ClipCount int    `json:"clip_count"`
	}
	var result []topicEntry
	for name, count := range counts {
		result = append(result, topicEntry{Name: name, ClipCount: count})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ClipCount > result[j].ClipCount
	})
	if len(result) > 20 {
		result = result[:20]
	}

	writeJSON(w, 200, map[string]interface{}{"topics": result})
}
```

**Step 2: Add `"sort"` to the imports**

Add `"sort"` to the import block at the top of `api/main.go` (line 2–27), alongside the other stdlib imports.

**Step 3: Register the route**

Add the route at line 145 (after the `r.Get("/api/search", ...)` line):

```go
r.Get("/api/topics", app.handleGetTopics)
```

**Step 4: Verify it compiles**

Run: `cd api && go build ./...`
Expected: no errors.

**Step 5: Commit**

```bash
git add api/main.go
git commit -m "feat(api): add GET /api/topics endpoint for topic aggregation"
```

---

### Task 2: Update `GET /api/me` to return preferences

**Files:**
- Modify: `api/main.go` (lines 705–726, `handleGetProfile`)

**Step 1: Update the handler to join preferences**

Replace the existing `handleGetProfile` function with:

```go
func (a *App) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	var username, email, displayName, createdAt string
	var avatarURL *string
	var explorationRate float64
	var topicWeightsJSON string
	var minClip, maxClip int
	var autoplay int

	err := a.db.QueryRowContext(r.Context(), `
		SELECT u.username, u.email, u.display_name, u.avatar_url, u.created_at,
		       COALESCE(p.exploration_rate, 0.3),
		       COALESCE(p.topic_weights, '{}'),
		       COALESCE(p.min_clip_seconds, 5),
		       COALESCE(p.max_clip_seconds, 120),
		       COALESCE(p.autoplay, 1)
		FROM users u
		LEFT JOIN user_preferences p ON u.id = p.user_id
		WHERE u.id = ?
	`, userID).Scan(&username, &email, &displayName, &avatarURL, &createdAt,
		&explorationRate, &topicWeightsJSON, &minClip, &maxClip, &autoplay)

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "user not found"})
		return
	}

	var topicWeights map[string]interface{}
	json.Unmarshal([]byte(topicWeightsJSON), &topicWeights)
	if topicWeights == nil {
		topicWeights = make(map[string]interface{})
	}

	writeJSON(w, 200, map[string]interface{}{
		"id": userID, "username": username, "email": email,
		"display_name": displayName, "avatar_url": avatarURL,
		"created_at": createdAt,
		"preferences": map[string]interface{}{
			"exploration_rate": explorationRate,
			"topic_weights":   topicWeights,
			"min_clip_seconds": minClip,
			"max_clip_seconds": maxClip,
			"autoplay":         autoplay == 1,
		},
	})
}
```

**Step 2: Verify it compiles**

Run: `cd api && go build ./...`
Expected: no errors.

**Step 3: Commit**

```bash
git add api/main.go
git commit -m "feat(api): return preferences (including topic_weights) from GET /api/me"
```

---

### Task 3: Add topic boost to feed ranking

**Files:**
- Modify: `api/main.go` (lines 382–437, `handleFeed`)

**Step 1: Replace the `handleFeed` function**

The key change: for authenticated users, also fetch `topic_weights` from preferences; after scanning clips, compute a per-clip topic boost and re-sort.

```go
func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	limit := 20

	var rows *sql.Rows
	var err error
	var topicWeights map[string]float64

	if userID != "" {
		var topicWeightsJSON string
		a.db.QueryRowContext(r.Context(),
			`SELECT COALESCE(topic_weights, '{}') FROM user_preferences WHERE user_id = ?`,
			userID,
		).Scan(&topicWeightsJSON)
		json.Unmarshal([]byte(topicWeightsJSON), &topicWeights)

		rows, err = a.db.QueryContext(r.Context(), `
			WITH prefs AS (
				SELECT exploration_rate, min_clip_seconds, max_clip_seconds
				FROM user_preferences WHERE user_id = ?
			),
			seen AS (
				SELECT clip_id FROM interactions
				WHERE user_id = ? AND created_at > datetime('now', '-24 hours')
			)
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			  AND c.id NOT IN (SELECT clip_id FROM seen)
			  AND c.duration_seconds >= COALESCE((SELECT min_clip_seconds FROM prefs), 5)
			  AND c.duration_seconds <= COALESCE((SELECT max_clip_seconds FROM prefs), 120)
			ORDER BY
			    (c.content_score * (1.0 - COALESCE((SELECT exploration_rate FROM prefs), 0.3)))
			    + (CAST(ABS(RANDOM()) AS REAL) / 9223372036854775807.0
			       * COALESCE((SELECT exploration_rate FROM prefs), 0.3))
			    DESC
			LIMIT ?
		`, userID, userID, limit)
	} else {
		rows, err = a.db.QueryContext(r.Context(), `
			SELECT c.id, c.title, c.description, c.duration_seconds,
			       c.thumbnail_key, c.topics, c.tags, c.content_score,
			       c.created_at, s.channel_name, s.platform
			FROM clips c
			LEFT JOIN sources s ON c.source_id = s.id
			WHERE c.status = 'ready'
			ORDER BY (c.content_score * 0.7)
			    + (CAST(ABS(RANDOM()) AS REAL) / 9223372036854775807.0 * 0.3) DESC
			LIMIT ?
		`, limit)
	}

	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to fetch feed"})
		return
	}
	defer rows.Close()

	clips := scanClips(rows)

	if len(topicWeights) > 0 {
		for i, clip := range clips {
			topics, _ := clip["topics"].([]string)
			boost := computeTopicBoost(topics, topicWeights)
			clips[i]["_score"] = boost
		}
		sort.SliceStable(clips, func(i, j int) bool {
			si, _ := clips[i]["_score"].(float64)
			sj, _ := clips[j]["_score"].(float64)
			return si > sj
		})
		for _, clip := range clips {
			delete(clip, "_score")
		}
	}

	writeJSON(w, 200, map[string]interface{}{"clips": clips, "count": len(clips)})
}

func computeTopicBoost(clipTopics []string, weights map[string]float64) float64 {
	if len(clipTopics) == 0 {
		return 1.0
	}
	sum := 0.0
	count := 0
	for _, t := range clipTopics {
		if w, ok := weights[t]; ok {
			sum += w
			count++
		}
	}
	if count == 0 {
		return 1.0
	}
	return sum / float64(count)
}
```

**Step 2: Verify it compiles**

Run: `cd api && go build ./...`
Expected: no errors.

**Step 3: Commit**

```bash
git add api/main.go
git commit -m "feat(api): apply topic_weights boost to feed ranking"
```

---

### Task 4: Add `getTopics` to the frontend API client

**Files:**
- Modify: `web/src/shared/api/clipfeedApi.js`

**Step 1: Add the method**

Add after the `getProfile` line (line 32):

```js
getTopics: () => request('GET', '/topics'),
```

**Step 2: Commit**

```bash
git add web/src/shared/api/clipfeedApi.js
git commit -m "feat(web): add getTopics API method"
```

---

### Task 5: Build TopicWeights component

**Files:**
- Create: `web/src/features/settings/components/TopicWeights.jsx`

**Step 1: Create the component**

```jsx
import React, { useEffect, useRef, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

export function TopicWeights({ currentWeights, onWeightsChange }) {
  const [topics, setTopics] = useState(null);
  const [weights, setWeights] = useState(currentWeights || {});
  const [error, setError] = useState(null);
  const saveTimer = useRef(null);

  useEffect(() => {
    api.getTopics()
      .then((data) => setTopics(data.topics || []))
      .catch(() => setError('Failed to load topics'));
  }, []);

  useEffect(() => {
    setWeights(currentWeights || {});
  }, [currentWeights]);

  function handleSlider(topicName, value) {
    const next = { ...weights, [topicName]: value };
    setWeights(next);

    if (saveTimer.current) clearTimeout(saveTimer.current);
    saveTimer.current = setTimeout(() => {
      api.updatePreferences({ topic_weights: next })
        .then(() => onWeightsChange?.(next))
        .catch(console.error);
    }, 500);
  }

  function formatLabel(val) {
    if (val === 0) return 'Hide';
    if (val === 1) return 'Neutral';
    if (val === 2) return 'Max';
    return `${val.toFixed(1)}×`;
  }

  function sliderColor(val) {
    if (val < 0.5) return 'var(--accent)';
    if (val > 1.5) return 'var(--success)';
    return 'var(--text-dim)';
  }

  if (error) {
    return <div className="topic-weights-empty">{error}</div>;
  }

  if (topics === null) {
    return <div className="topic-weights-empty">Loading topics…</div>;
  }

  if (topics.length === 0) {
    return (
      <div className="topic-weights-empty">
        No topics available yet — add some clips first.
      </div>
    );
  }

  return (
    <div className="topic-weights-list">
      {topics.map((t) => {
        const val = weights[t.name] ?? 1.0;
        return (
          <div key={t.name} className="topic-weight-row">
            <div className="topic-weight-header">
              <span className="topic-weight-name">
                {t.name}
                <span className="topic-weight-count">{t.clip_count} clips</span>
              </span>
              <span className="topic-weight-value" style={{ color: sliderColor(val) }}>
                {formatLabel(val)}
              </span>
            </div>
            <input
              type="range"
              min="0"
              max="2"
              step="0.05"
              value={val}
              style={{ accentColor: sliderColor(val) }}
              onChange={(e) => handleSlider(t.name, parseFloat(e.target.value))}
            />
          </div>
        );
      })}
    </div>
  );
}
```

**Step 2: Commit**

```bash
git add web/src/features/settings/components/TopicWeights.jsx
git commit -m "feat(web): add TopicWeights component with per-topic sliders"
```

---

### Task 6: Integrate TopicWeights into SettingsScreen

**Files:**
- Modify: `web/src/features/settings/components/SettingsScreen.jsx`

**Step 1: Update imports and state**

Add import at line 2 (after the api import):

```jsx
import { TopicWeights } from './TopicWeights';
```

**Step 2: Update `useEffect` to load preferences from profile**

Replace the existing `useEffect` (lines 13–15) with:

```jsx
useEffect(() => {
  api.getProfile()
    .then((data) => {
      if (data.preferences) {
        setPrefs((prev) => ({ ...prev, ...data.preferences }));
      }
    })
    .catch(() => {});
}, []);
```

**Step 3: Add `topic_weights` to initial state**

Update the `useState` default (line 6–11) to include `topic_weights`:

```jsx
const [prefs, setPrefs] = useState({
  exploration_rate: 0.3,
  min_clip_seconds: 5,
  max_clip_seconds: 120,
  autoplay: true,
  topic_weights: {},
});
```

**Step 4: Add the TopicWeights section**

Insert a new `settings-section` after the "Feed Tuning" section's closing `</div>` (after line 78, before the CookieSection):

```jsx
<div className="settings-section">
  <h3>Topic Preferences</h3>
  <TopicWeights
    currentWeights={prefs.topic_weights}
    onWeightsChange={(tw) => setPrefs((prev) => ({ ...prev, topic_weights: tw }))}
  />
</div>
```

**Step 5: Verify build**

Run: `cd web && npm run build`
Expected: no errors.

**Step 6: Commit**

```bash
git add web/src/features/settings/components/SettingsScreen.jsx
git commit -m "feat(web): integrate TopicWeights section into SettingsScreen"
```

---

### Task 7: Add CSS for topic weights

**Files:**
- Modify: `web/src/features/settings/settings.css`

**Step 1: Append styles**

Add at the end of the file:

```css
.topic-weights-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.topic-weights-empty {
  padding: 20px 16px;
  text-align: center;
  color: var(--text-dim);
  font-size: 14px;
  background: var(--bg-elevated);
  border-radius: var(--radius-sm);
}

.topic-weight-row {
  padding: 12px 16px;
  background: var(--bg-elevated);
  border-radius: var(--radius-sm);
}

.topic-weight-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 8px;
}

.topic-weight-name {
  font-size: 14px;
  font-weight: 500;
  display: flex;
  align-items: center;
  gap: 8px;
}

.topic-weight-count {
  font-size: 11px;
  color: var(--text-muted);
  font-weight: 400;
}

.topic-weight-value {
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 600;
  min-width: 52px;
  text-align: right;
}
```

**Step 2: Verify build**

Run: `cd web && npm run build`
Expected: no errors.

**Step 3: Commit**

```bash
git add web/src/features/settings/settings.css
git commit -m "feat(web): add topic weights CSS styles"
```

---

### Verification

After all tasks, verify the full stack:

```bash
cd api && go build ./...
cd ../web && npm run build
```

Both commands should succeed with zero errors.
