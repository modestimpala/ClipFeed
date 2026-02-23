# Topic Weights UI — Design Document

**Date:** 2026-02-23

## Problem

ClipFeed's core promise is a transparent, user-controllable algorithm. The SQLite schema has a `topic_weights` JSON column on `user_preferences` and the Python ingestion worker extracts KeyBERT topics per clip — but nothing connects these two pieces. The feed query ignores topic weights entirely, and the frontend has no UI for viewing or adjusting them.

## Decision Record

| Decision | Choice | Rationale |
|---|---|---|
| Topic source | All clips in the library | Gives new users something to tune immediately without needing interaction history |
| Ranking strategy | Boost/penalize multiplier | Intuitive: 1.0 = neutral, >1 = show more, <1 = show less, 0 = hide |
| UI placement | New section in existing SettingsScreen | Keeps all algorithm controls in one place |

## Architecture

### New API: `GET /api/topics`

Public endpoint (no auth required). Scans all `ready` clips, parses each clip's `topics` JSON array, aggregates by frequency, returns the top 20.

Response:
```json
{
  "topics": [
    {"name": "technology", "clip_count": 42},
    {"name": "cooking", "clip_count": 31}
  ]
}
```

### Updated API: `GET /api/me`

Currently returns only user profile fields. Updated to join `user_preferences` and include preferences in the response — specifically `topic_weights`, `exploration_rate`, `min_clip_seconds`, `max_clip_seconds`, and `autoplay`.

### Updated Feed: Topic Boost

Current formula:
```
score = content_score * (1 - exploration_rate) + random * exploration_rate
```

Updated: after scanning clips, compute a topic boost per clip from the user's `topic_weights`. For each clip topic that appears in the user's weights, average those weights. Topics not in the user's map default to 1.0. Multiply the SQL-ranked score by this boost, then re-sort in Go.

```
topic_boost = avg(user_weight[t] for t in clip.topics if t in user_weights) or 1.0
final_score = sql_score * topic_boost
```

The boost is computed in Go (not SQL) because parsing JSON arrays in SQLite for this kind of cross-row matching is fragile and hard to maintain.

### Frontend: Topic Sliders in SettingsScreen

- On mount: fetch `GET /api/topics` + `GET /api/me` in parallel
- Render a slider per topic: range 0–2, step 0.05, default 1.0
- Visual labels: 0 = "Hide", 1.0 = "Neutral", 2.0 = "Boost"
- Debounce saves (500ms) via `PUT /api/me/preferences`
- Empty state: "No topics yet" when no clips exist

## Out of Scope

- Per-clip topic display in the feed
- Auto-learning weights from behavior
- Topic search/filter
- Server-side caching of topic aggregation
