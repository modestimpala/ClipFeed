-- Postgres schema for ClipFeed
-- Applied idempotently at startup (CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS).

-- Helper: current UTC time as ISO 8601 text matching the SQLite format.
CREATE OR REPLACE FUNCTION iso_now() RETURNS TEXT AS $$
    SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"');
$$ LANGUAGE SQL STABLE;

CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    username        TEXT UNIQUE NOT NULL,
    email           TEXT UNIQUE NOT NULL,
    password_hash   TEXT NOT NULL,
    display_name    TEXT,
    avatar_url      TEXT,
    created_at      TEXT DEFAULT (iso_now()),
    updated_at      TEXT DEFAULT (iso_now())
);

CREATE TABLE IF NOT EXISTS user_preferences (
    user_id             TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    exploration_rate    REAL DEFAULT 0.3,
    topic_weights       TEXT DEFAULT '{}',
    min_clip_seconds    INTEGER DEFAULT 5,
    max_clip_seconds    INTEGER DEFAULT 120,
    autoplay            INTEGER DEFAULT 1,
    nsfw_filter         INTEGER DEFAULT 1,
    diversity_mix       REAL DEFAULT 0.5,
    trending_boost      INTEGER DEFAULT 1,
    freshness_bias      REAL DEFAULT 0.5,
    updated_at          TEXT DEFAULT (iso_now())
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
    created_at      TEXT DEFAULT (iso_now()),
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
    created_at      TEXT DEFAULT (iso_now())
);

CREATE INDEX IF NOT EXISTS idx_clips_status ON clips(status);
CREATE INDEX IF NOT EXISTS idx_clips_expires ON clips(expires_at)
    WHERE expires_at IS NOT NULL AND is_protected = 0;
CREATE INDEX IF NOT EXISTS idx_clips_score ON clips(content_score DESC);

-- Full-text search via tsvector (replaces SQLite FTS5).
CREATE TABLE IF NOT EXISTS clips_fts (
    clip_id      TEXT PRIMARY KEY REFERENCES clips(id) ON DELETE CASCADE,
    title        TEXT,
    transcript   TEXT,
    platform     TEXT,
    channel_name TEXT,
    tsv          tsvector
);

CREATE INDEX IF NOT EXISTS idx_clips_fts_tsv ON clips_fts USING GIN(tsv);

-- Auto-maintain tsvector on insert/update.
CREATE OR REPLACE FUNCTION fn_clips_fts_update_tsv() RETURNS TRIGGER AS $$
BEGIN
    NEW.tsv := to_tsvector('english',
        COALESCE(NEW.title, '') || ' ' ||
        COALESCE(NEW.transcript, '') || ' ' ||
        COALESCE(NEW.channel_name, ''));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_clips_fts_tsv ON clips_fts;
CREATE TRIGGER trg_clips_fts_tsv
    BEFORE INSERT OR UPDATE ON clips_fts
    FOR EACH ROW EXECUTE FUNCTION fn_clips_fts_update_tsv();

CREATE TABLE IF NOT EXISTS interactions (
    id                     TEXT PRIMARY KEY,
    user_id                TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id                TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    action                 TEXT NOT NULL,
    watch_duration_seconds REAL,
    watch_percentage       REAL,
    created_at             TEXT DEFAULT (iso_now())
);

CREATE INDEX IF NOT EXISTS idx_interactions_user ON interactions(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_interactions_clip ON interactions(clip_id);
CREATE INDEX IF NOT EXISTS idx_interactions_action ON interactions(user_id, action);
CREATE INDEX IF NOT EXISTS idx_interactions_user_clip_created ON interactions(user_id, clip_id, created_at DESC);

CREATE TABLE IF NOT EXISTS saved_clips (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id    TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    created_at TEXT DEFAULT (iso_now()),
    PRIMARY KEY (user_id, clip_id)
);

CREATE TABLE IF NOT EXISTS collections (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    description TEXT,
    is_public   INTEGER DEFAULT 0,
    created_at  TEXT DEFAULT (iso_now())
);

CREATE TABLE IF NOT EXISTS collection_clips (
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    clip_id       TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    position      INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT DEFAULT (iso_now()),
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
    created_at   TEXT DEFAULT (iso_now())
);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status, priority DESC, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(status, run_after, priority DESC, created_at ASC);

CREATE TABLE IF NOT EXISTS platform_cookies (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform   TEXT NOT NULL,
    cookie_str TEXT NOT NULL,
    is_active  INTEGER DEFAULT 1,
    created_at TEXT DEFAULT (iso_now()),
    updated_at TEXT DEFAULT (iso_now()),
    UNIQUE(user_id, platform)
);

CREATE INDEX IF NOT EXISTS idx_platform_cookies_user ON platform_cookies(user_id, platform);

-- Protect clip when saved (Postgres trigger syntax).
CREATE OR REPLACE FUNCTION fn_protect_saved() RETURNS TRIGGER AS $$
BEGIN
    UPDATE clips SET is_protected = 1 WHERE id = NEW.clip_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_protect_saved ON saved_clips;
CREATE TRIGGER trg_protect_saved
    AFTER INSERT ON saved_clips
    FOR EACH ROW EXECUTE FUNCTION fn_protect_saved();

-- Unprotect clip when last save is removed.
CREATE OR REPLACE FUNCTION fn_check_unprotect() RETURNS TRIGGER AS $$
BEGIN
    UPDATE clips SET is_protected = 0
    WHERE id = OLD.clip_id
      AND NOT EXISTS (SELECT 1 FROM saved_clips WHERE clip_id = OLD.clip_id);
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_check_unprotect ON saved_clips;
CREATE TRIGGER trg_check_unprotect
    AFTER DELETE ON saved_clips
    FOR EACH ROW EXECUTE FUNCTION fn_check_unprotect();

-- --- Topic Graph ---

CREATE TABLE IF NOT EXISTS topics (
    id          TEXT PRIMARY KEY,
    name        TEXT UNIQUE NOT NULL,
    slug        TEXT UNIQUE NOT NULL,
    path        TEXT NOT NULL DEFAULT '',
    parent_id   TEXT REFERENCES topics(id) ON DELETE SET NULL,
    depth       INTEGER NOT NULL DEFAULT 0,
    clip_count  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT DEFAULT (iso_now())
);

CREATE INDEX IF NOT EXISTS idx_topics_path ON topics(path);
CREATE INDEX IF NOT EXISTS idx_topics_parent ON topics(parent_id);

CREATE TABLE IF NOT EXISTS topic_edges (
    source_id   TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    target_id   TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    relation    TEXT NOT NULL DEFAULT 'related_to',
    weight      REAL NOT NULL DEFAULT 1.0,
    created_at  TEXT DEFAULT (iso_now()),
    PRIMARY KEY (source_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_topic_edges_target ON topic_edges(target_id);

CREATE TABLE IF NOT EXISTS clip_topics (
    clip_id     TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    topic_id    TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    confidence  REAL NOT NULL DEFAULT 1.0,
    source      TEXT NOT NULL DEFAULT 'keybert',
    created_at  TEXT DEFAULT (iso_now()),
    PRIMARY KEY (clip_id, topic_id)
);

CREATE INDEX IF NOT EXISTS idx_clip_topics_topic ON clip_topics(topic_id);
CREATE INDEX IF NOT EXISTS idx_clip_topics_clip ON clip_topics(clip_id);

-- Keep clip_count accurate on topics.
CREATE OR REPLACE FUNCTION fn_clip_topic_count() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE topics SET clip_count = clip_count + 1 WHERE id = NEW.topic_id;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE topics SET clip_count = clip_count - 1 WHERE id = OLD.topic_id;
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_clip_topic_inc ON clip_topics;
CREATE TRIGGER trg_clip_topic_inc
    AFTER INSERT ON clip_topics
    FOR EACH ROW EXECUTE FUNCTION fn_clip_topic_count();

DROP TRIGGER IF EXISTS trg_clip_topic_dec ON clip_topics;
CREATE TRIGGER trg_clip_topic_dec
    AFTER DELETE ON clip_topics
    FOR EACH ROW EXECUTE FUNCTION fn_clip_topic_count();

CREATE TABLE IF NOT EXISTS user_topic_affinities (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    topic_id    TEXT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    weight      REAL NOT NULL DEFAULT 1.0,
    source      TEXT NOT NULL DEFAULT 'explicit',
    updated_at  TEXT DEFAULT (iso_now()),
    PRIMARY KEY (user_id, topic_id)
);

-- --- Embeddings ---

CREATE TABLE IF NOT EXISTS clip_embeddings (
    clip_id          TEXT PRIMARY KEY REFERENCES clips(id) ON DELETE CASCADE,
    text_embedding   BYTEA,
    visual_embedding BYTEA,
    model_version    TEXT NOT NULL DEFAULT '',
    created_at       TEXT DEFAULT (iso_now())
);

-- --- User Profile Embeddings ---

CREATE TABLE IF NOT EXISTS user_embeddings (
    user_id          TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    text_embedding   BYTEA,
    interaction_count INTEGER NOT NULL DEFAULT 0,
    updated_at       TEXT DEFAULT (iso_now())
);

-- --- Saved Filters ---

CREATE TABLE IF NOT EXISTS saved_filters (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    query       TEXT NOT NULL,
    is_default  INTEGER DEFAULT 0,
    created_at  TEXT DEFAULT (iso_now())
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
    created_at           TEXT DEFAULT (iso_now()),
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
    created_at      TEXT DEFAULT (iso_now()),
    UNIQUE(platform, external_id)
);

CREATE INDEX IF NOT EXISTS idx_scout_candidates_status ON scout_candidates(status);

-- --- Clip Summaries (LLM-generated, cached) ---

CREATE TABLE IF NOT EXISTS clip_summaries (
    clip_id    TEXT PRIMARY KEY REFERENCES clips(id) ON DELETE CASCADE,
    summary    TEXT NOT NULL,
    model      TEXT NOT NULL,
    created_at TEXT DEFAULT (iso_now())
);

-- --- LLM Logs ---

CREATE TABLE IF NOT EXISTS llm_logs (
    id          SERIAL PRIMARY KEY,
    system      TEXT NOT NULL,
    model       TEXT NOT NULL,
    prompt      TEXT NOT NULL,
    response    TEXT,
    error       TEXT,
    duration_ms INTEGER,
    created_at  TEXT DEFAULT (iso_now())
);
CREATE INDEX IF NOT EXISTS idx_llm_logs_created ON llm_logs(created_at DESC);

-- --- Performance indexes ---

CREATE INDEX IF NOT EXISTS idx_interactions_clip_created ON interactions(clip_id, created_at);
CREATE INDEX IF NOT EXISTS idx_scout_candidates_source ON scout_candidates(scout_source_id);
CREATE INDEX IF NOT EXISTS idx_jobs_source ON jobs(source_id);
