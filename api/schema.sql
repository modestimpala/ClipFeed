-- ClipFeed SQLite Schema
-- Idempotent: all CREATE statements use IF NOT EXISTS
-- Replaces separate Postgres migration files

-- ============================================================
-- TABLE: users
-- ============================================================
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name  TEXT,
    avatar_url    TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- ============================================================
-- TABLE: user_preferences
-- ============================================================
CREATE TABLE IF NOT EXISTS user_preferences (
    user_id              TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    exploration_rate     REAL    NOT NULL DEFAULT 0.3,
    topic_weights        TEXT    NOT NULL DEFAULT '{}',
    min_clip_seconds     INTEGER NOT NULL DEFAULT 5,
    max_clip_seconds     INTEGER NOT NULL DEFAULT 120,
    autoplay             INTEGER NOT NULL DEFAULT 1,
    nsfw_filter          INTEGER NOT NULL DEFAULT 1,
    updated_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- ============================================================
-- TABLE: sources
-- ============================================================
CREATE TABLE IF NOT EXISTS sources (
    id               TEXT PRIMARY KEY,
    url              TEXT NOT NULL,
    platform         TEXT NOT NULL,
    external_id      TEXT NOT NULL,
    title            TEXT,
    description      TEXT,
    duration_seconds REAL,
    thumbnail_url    TEXT,
    channel_name     TEXT,
    channel_id       TEXT,
    metadata         TEXT NOT NULL DEFAULT '{}',
    status           TEXT NOT NULL DEFAULT 'pending',
    submitted_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(platform, external_id)
);

-- ============================================================
-- TABLE: clips
-- ============================================================
CREATE TABLE IF NOT EXISTS clips (
    id                TEXT PRIMARY KEY,
    source_id         TEXT REFERENCES sources(id) ON DELETE SET NULL,
    title             TEXT,
    description       TEXT,
    duration_seconds  REAL    NOT NULL,
    start_time        REAL,
    end_time          REAL,
    storage_key       TEXT    NOT NULL,
    thumbnail_key     TEXT,
    hls_key           TEXT,
    width             INTEGER,
    height            INTEGER,
    file_size_bytes   INTEGER,
    transcript        TEXT,
    language          TEXT,
    topics            TEXT    NOT NULL DEFAULT '[]',
    tags              TEXT    NOT NULL DEFAULT '[]',
    content_score     REAL    NOT NULL DEFAULT 0.5,
    expires_at        TEXT,
    is_protected      INTEGER NOT NULL DEFAULT 0,
    status            TEXT    NOT NULL DEFAULT 'processing',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- ============================================================
-- TABLE: interactions
-- ============================================================
CREATE TABLE IF NOT EXISTS interactions (
    id                     TEXT PRIMARY KEY,
    user_id                TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id                TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    action                 TEXT NOT NULL,
    watch_duration_seconds REAL,
    watch_percentage       REAL,
    created_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- ============================================================
-- TABLE: saved_clips
-- ============================================================
CREATE TABLE IF NOT EXISTS saved_clips (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id    TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (user_id, clip_id)
);

-- ============================================================
-- TABLE: collections
-- ============================================================
CREATE TABLE IF NOT EXISTS collections (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    description TEXT,
    is_public   INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- ============================================================
-- TABLE: collection_clips
-- ============================================================
CREATE TABLE IF NOT EXISTS collection_clips (
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    clip_id       TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    position      INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (collection_id, clip_id)
);

-- ============================================================
-- TABLE: jobs
-- ============================================================
CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,
    source_id    TEXT REFERENCES sources(id) ON DELETE SET NULL,
    job_type     TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'queued',
    priority     INTEGER NOT NULL DEFAULT 5,
    payload      TEXT    NOT NULL DEFAULT '{}',
    result       TEXT    NOT NULL DEFAULT '{}',
    error        TEXT,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    locked_at    TEXT,
    started_at   TEXT,
    completed_at TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- ============================================================
-- TABLE: platform_cookies
-- ============================================================
CREATE TABLE IF NOT EXISTS platform_cookies (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform   TEXT NOT NULL,
    cookie_str TEXT NOT NULL,
    is_active  INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(user_id, platform)
);

-- ============================================================
-- INDEXES
-- ============================================================
CREATE INDEX IF NOT EXISTS idx_clips_status
    ON clips(status);

CREATE INDEX IF NOT EXISTS idx_clips_expires
    ON clips(expires_at)
    WHERE expires_at IS NOT NULL AND is_protected = 0;

CREATE INDEX IF NOT EXISTS idx_clips_score
    ON clips(content_score DESC);

CREATE INDEX IF NOT EXISTS idx_interactions_user
    ON interactions(user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_interactions_clip
    ON interactions(clip_id);

CREATE INDEX IF NOT EXISTS idx_interactions_action
    ON interactions(user_id, action);

CREATE INDEX IF NOT EXISTS idx_jobs_status
    ON jobs(status, priority DESC, created_at ASC);

CREATE INDEX IF NOT EXISTS idx_platform_cookies_user
    ON platform_cookies(user_id, platform);

CREATE INDEX IF NOT EXISTS idx_saved_clips_clip
    ON saved_clips(clip_id);

-- ============================================================
-- FTS5 VIRTUAL TABLE (replaces Meilisearch)
-- ============================================================
CREATE VIRTUAL TABLE IF NOT EXISTS clips_fts USING fts5(
    clip_id      UNINDEXED,
    title,
    transcript,
    platform     UNINDEXED,
    channel_name UNINDEXED
);

-- ============================================================
-- TRIGGERS: saved clip protection
-- ============================================================

-- Protect clip when it is saved by a user
CREATE TRIGGER IF NOT EXISTS trg_protect_saved
    AFTER INSERT ON saved_clips FOR EACH ROW
BEGIN
    UPDATE clips SET is_protected = 1 WHERE id = NEW.clip_id;
END;

-- Unprotect clip when the last save referencing it is removed
CREATE TRIGGER IF NOT EXISTS trg_check_unprotect
    AFTER DELETE ON saved_clips FOR EACH ROW
BEGIN
    UPDATE clips SET is_protected = 0
    WHERE id = OLD.clip_id
      AND NOT EXISTS (SELECT 1 FROM saved_clips WHERE clip_id = OLD.clip_id);
END;

-- Remove orphaned FTS entry when a clip is deleted
CREATE TRIGGER IF NOT EXISTS trg_clips_fts_delete
    AFTER DELETE ON clips FOR EACH ROW
BEGIN
    DELETE FROM clips_fts WHERE clip_id = OLD.id;
END;
