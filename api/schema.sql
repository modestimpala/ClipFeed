-- SQLite schema for ClipFeed (WAL mode set via PRAGMA at runtime)

CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    username        TEXT UNIQUE NOT NULL,
    email           TEXT UNIQUE NOT NULL,
    password_hash   TEXT NOT NULL,
    display_name    TEXT,
    avatar_url      TEXT,
    created_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS user_preferences (
    user_id             TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    exploration_rate    REAL DEFAULT 0.3,
    topic_weights       TEXT DEFAULT '{}',
    dedupe_seen_24h     INTEGER DEFAULT 1,
    min_clip_seconds    INTEGER DEFAULT 5,
    max_clip_seconds    INTEGER DEFAULT 120,
    autoplay            INTEGER DEFAULT 1,
    nsfw_filter         INTEGER DEFAULT 1,
    scout_threshold     REAL DEFAULT 6.0,
    updated_at          TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS sources (
    id              TEXT PRIMARY KEY,
    url             TEXT NOT NULL,
    platform        TEXT NOT NULL,
    external_id     TEXT,
    title           TEXT,
    description     TEXT,
    duration_seconds REAL,
    thumbnail_url   TEXT,
    channel_name    TEXT,
    channel_id      TEXT,
    metadata        TEXT DEFAULT '{}',
    status          TEXT DEFAULT 'pending',
    submitted_by    TEXT REFERENCES users(id),
    created_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(platform, external_id)
);

CREATE TABLE IF NOT EXISTS clips (
    id              TEXT PRIMARY KEY,
    source_id       TEXT REFERENCES sources(id) ON DELETE SET NULL,
    title           TEXT,
    description     TEXT,
    duration_seconds REAL NOT NULL,
    start_time      REAL,
    end_time        REAL,
    storage_key     TEXT NOT NULL,
    thumbnail_key   TEXT,
    hls_key         TEXT,
    width           INTEGER,
    height          INTEGER,
    file_size_bytes INTEGER,
    transcript      TEXT,
    language        TEXT,
    topics          TEXT DEFAULT '[]',
    tags            TEXT DEFAULT '[]',
    content_score   REAL DEFAULT 0.5,
    expires_at      TEXT,
    is_protected    INTEGER DEFAULT 0,
    status          TEXT DEFAULT 'processing',
    created_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_clips_status ON clips(status);
CREATE INDEX IF NOT EXISTS idx_clips_expires ON clips(expires_at)
    WHERE expires_at IS NOT NULL AND is_protected = 0;
CREATE INDEX IF NOT EXISTS idx_clips_score ON clips(content_score DESC);

-- FTS5 full-text search (replaces Meilisearch)
CREATE VIRTUAL TABLE IF NOT EXISTS clips_fts USING fts5(
    clip_id UNINDEXED,
    title,
    transcript,
    platform UNINDEXED,
    channel_name UNINDEXED
);

CREATE TABLE IF NOT EXISTS interactions (
    id                     TEXT PRIMARY KEY,
    user_id                TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id                TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    action                 TEXT NOT NULL,
    watch_duration_seconds REAL,
    watch_percentage       REAL,
    created_at             TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_interactions_user ON interactions(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_interactions_clip ON interactions(clip_id);
CREATE INDEX IF NOT EXISTS idx_interactions_action ON interactions(user_id, action);

CREATE TABLE IF NOT EXISTS saved_clips (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id    TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (user_id, clip_id)
);

CREATE TABLE IF NOT EXISTS collections (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    description TEXT,
    is_public   INTEGER DEFAULT 0,
    created_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS collection_clips (
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    clip_id       TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    position      INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (collection_id, clip_id)
);

CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,
    source_id    TEXT REFERENCES sources(id),
    job_type     TEXT NOT NULL,
    status       TEXT DEFAULT 'queued',
    priority     INTEGER DEFAULT 5,
    payload      TEXT DEFAULT '{}',
    result       TEXT DEFAULT '{}',
    error        TEXT,
    attempts     INTEGER DEFAULT 0,
    max_attempts INTEGER DEFAULT 3,
    run_after    TEXT,
    locked_at    TEXT,
    started_at   TEXT,
    completed_at TEXT,
    created_at   TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status, priority DESC, created_at ASC);

CREATE TABLE IF NOT EXISTS platform_cookies (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform   TEXT NOT NULL,
    cookie_str TEXT NOT NULL,
    is_active  INTEGER DEFAULT 1,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(user_id, platform)
);

CREATE INDEX IF NOT EXISTS idx_platform_cookies_user ON platform_cookies(user_id, platform);

-- Protect clip when saved
CREATE TRIGGER IF NOT EXISTS trg_protect_saved
    AFTER INSERT ON saved_clips FOR EACH ROW
BEGIN
    UPDATE clips SET is_protected = 1 WHERE id = NEW.clip_id;
END;

-- Unprotect clip when last save is removed
CREATE TRIGGER IF NOT EXISTS trg_check_unprotect
    AFTER DELETE ON saved_clips FOR EACH ROW
BEGIN
    UPDATE clips SET is_protected = 0
    WHERE id = OLD.clip_id
      AND NOT EXISTS (SELECT 1 FROM saved_clips WHERE clip_id = OLD.clip_id);
END;

-- --- Topic Graph ---

CREATE TABLE IF NOT EXISTS topics (
    id          TEXT PRIMARY KEY,
    name        TEXT UNIQUE NOT NULL,
    slug        TEXT UNIQUE NOT NULL,
    path        TEXT NOT NULL DEFAULT '',
    parent_id   TEXT REFERENCES topics(id) ON DELETE SET NULL,
    depth       INTEGER NOT NULL DEFAULT 0,
    clip_count  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_topics_path ON topics(path);
CREATE INDEX IF NOT EXISTS idx_topics_parent ON topics(parent_id);

CREATE TABLE IF NOT EXISTS topic_edges (
    source_id   TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    target_id   TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    relation    TEXT NOT NULL DEFAULT 'related_to',
    weight      REAL NOT NULL DEFAULT 1.0,
    created_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (source_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_topic_edges_target ON topic_edges(target_id);

CREATE TABLE IF NOT EXISTS clip_topics (
    clip_id     TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    topic_id    TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    confidence  REAL NOT NULL DEFAULT 1.0,
    source      TEXT NOT NULL DEFAULT 'keybert',
    created_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (clip_id, topic_id)
);

CREATE INDEX IF NOT EXISTS idx_clip_topics_topic ON clip_topics(topic_id);

CREATE TABLE IF NOT EXISTS user_topic_affinities (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    topic_id    TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    weight      REAL NOT NULL DEFAULT 1.0,
    source      TEXT NOT NULL DEFAULT 'explicit',
    updated_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (user_id, topic_id)
);

-- Keep clip_count on topics accurate
CREATE TRIGGER IF NOT EXISTS trg_clip_topic_inc
    AFTER INSERT ON clip_topics FOR EACH ROW
BEGIN
    UPDATE topics SET clip_count = clip_count + 1 WHERE id = NEW.topic_id;
END;

CREATE TRIGGER IF NOT EXISTS trg_clip_topic_dec
    AFTER DELETE ON clip_topics FOR EACH ROW
BEGIN
    UPDATE topics SET clip_count = clip_count - 1 WHERE id = OLD.topic_id;
END;

-- --- Embeddings ---

CREATE TABLE IF NOT EXISTS clip_embeddings (
    clip_id          TEXT PRIMARY KEY REFERENCES clips(id) ON DELETE CASCADE,
    text_embedding   BLOB,
    visual_embedding BLOB,
    model_version    TEXT NOT NULL DEFAULT '',
    created_at       TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- --- User Profile Embeddings ---

CREATE TABLE IF NOT EXISTS user_embeddings (
    user_id          TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    text_embedding   BLOB,
    interaction_count INTEGER NOT NULL DEFAULT 0,
    updated_at       TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- --- Saved Filters ---

CREATE TABLE IF NOT EXISTS saved_filters (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    query       TEXT NOT NULL,
    is_default  INTEGER DEFAULT 0,
    created_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_saved_filters_user ON saved_filters(user_id);

-- --- Content Scout ---

CREATE TABLE IF NOT EXISTS scout_sources (
    id                   TEXT PRIMARY KEY,
    user_id              TEXT REFERENCES users(id),
    source_type          TEXT NOT NULL,
    platform             TEXT NOT NULL,
    identifier           TEXT NOT NULL,
    is_active            INTEGER DEFAULT 1,
    last_checked         TEXT,
    check_interval_hours INTEGER DEFAULT 24,
    force_check          INTEGER DEFAULT 0,
    created_at           TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(platform, identifier)
);

CREATE TABLE IF NOT EXISTS scout_candidates (
    id              TEXT PRIMARY KEY,
    scout_source_id TEXT REFERENCES scout_sources(id),
    url             TEXT NOT NULL,
    platform        TEXT NOT NULL,
    external_id     TEXT NOT NULL,
    title           TEXT,
    channel_name    TEXT,
    duration_seconds REAL,
    llm_score       REAL,
    status          TEXT DEFAULT 'pending',
    created_at      TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(platform, external_id)
);

CREATE INDEX IF NOT EXISTS idx_scout_candidates_status ON scout_candidates(status);

-- --- Clip Summaries (LLM-generated, cached) ---

CREATE TABLE IF NOT EXISTS clip_summaries (
    clip_id    TEXT PRIMARY KEY REFERENCES clips(id) ON DELETE CASCADE,
    summary    TEXT NOT NULL,
    model      TEXT NOT NULL,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
