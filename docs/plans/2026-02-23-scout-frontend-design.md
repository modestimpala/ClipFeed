# Scout Frontend Design

**Date:** 2026-02-23
**Status:** Approved

## Overview

Add a Scout configuration and activity screen to the frontend. The scout worker is hands-off — it polls configured sources, scores candidates via Ollama, and auto-ingests anything above the user's threshold. The UI covers configuration and visibility, not a manual review workflow.

## Navigation

- Settings screen gets a "Content Scout →" tappable row that pushes a `ScoutScreen`.
- Sub-screen state is managed locally inside `SettingsScreen` (`subscreen` useState) — no changes to App.jsx or the bottom nav.
- `ScoutScreen` has a back-chevron header and two tabs: **Config** and **Candidates**, driven by local `activeTab` state.

## Config Tab

Three sections, top to bottom:

**Auto-ingest Threshold**
- Slider: "Auto-ingest if score ≥ X / 10", range 0–10, step 0.5.
- Saves to `user_preferences.scout_threshold` via the existing `PUT /api/me/preferences` endpoint.
- Loaded from `GET /api/me` on mount alongside other preferences.

**Sources List**
- One card per source: source type badge (Channel / Playlist / Hashtag), identifier (truncated), check interval inline dropdown (6h / 12h / 24h / 48h), active toggle, delete button.
- Muted "Last checked X ago" line per card.
- Interval change calls a `PATCH /api/scout/sources/{id}` endpoint.
- Delete calls `DELETE /api/scout/sources/{id}`.
- Empty state prompts to add the first source.

**Add Source Form**
- Collapsed behind an "+ Add Source" button; expands inline on tap.
- Fields: source type select (Channel / Playlist / Hashtag), identifier input (URL or search term), interval select (6h / 12h / 24h / 48h).
- Platform is always YouTube — not exposed as a field.
- On submit: `POST /api/scout/sources`, collapses form, refreshes list.

## Candidates Tab

Toggle pill (Ingested | Rejected) switches between two lists (last 50 each, server-limited).

**Ingested**
- Read-only list. Each row: title (truncated), channel name, LLM score badge (green), time ago.
- Tapping a row opens the source URL.

**Rejected**
- Same card layout; score badge muted/red.
- "Ingest anyway" button calls `POST /api/scout/candidates/{id}/approve`, optimistically removes the card.
- No confirmation dialog.

Both lists show a simple empty state message.

## Backend Changes

| Change | Detail |
|--------|--------|
| `user_preferences` column | `ALTER TABLE user_preferences ADD COLUMN scout_threshold REAL DEFAULT 6.0` — applied as a one-off migration, not in schema.sql |
| Preferences API | No changes needed — `scout_threshold` rides along with existing JSON blob in `PUT /api/me/preferences` and `GET /api/me` |
| Scout worker | Read `scout_threshold` from `user_preferences` per source's `user_id` during `evaluate_candidates`; fall back to `LLM_THRESHOLD` env var if NULL |
| Delete source endpoint | Add `DELETE /api/scout/sources/{id}` scoped to authenticated user |
| Update source endpoint | Add `PATCH /api/scout/sources/{id}` for interval and active toggle updates |

## New Frontend Files

```
web/src/features/settings/components/ScoutScreen.jsx
web/src/features/settings/components/ScoutSourceCard.jsx
web/src/features/settings/components/AddScoutSourceForm.jsx
web/src/features/settings/components/ScoutCandidateList.jsx
web/src/features/settings/scout.css
```

## New API Client Methods

```js
getScoutSources()
createScoutSource({ source_type, identifier, check_interval_hours })
updateScoutSource(id, { is_active, check_interval_hours })
deleteScoutSource(id)
getScoutCandidates(status)   // status: 'ingested' | 'rejected'
approveCandidate(id)
```
