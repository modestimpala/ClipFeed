-- ClipFeed Schema

-- Users and authentication
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username        VARCHAR(32) UNIQUE NOT NULL,
    email           VARCHAR(255) UNIQUE NOT NULL,
    password_hash   TEXT NOT NULL,
    display_name    VARCHAR(64),
    avatar_url      TEXT,
    created_at      TIMESTAMPTZ DEFAULT now(),
    updated_at      TIMESTAMPTZ DEFAULT now()
);

-- Algorithm preferences per user
CREATE TABLE user_preferences (
    user_id             UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    exploration_rate    REAL DEFAULT 0.3,       -- 0.0 = comfort zone, 1.0 = all discovery
    topic_weights       JSONB DEFAULT '{}',     -- {"cooking": 0.8, "tech": 0.5}
    min_clip_seconds    INT DEFAULT 5,
    max_clip_seconds    INT DEFAULT 120,
    autoplay            BOOLEAN DEFAULT true,
    nsfw_filter         BOOLEAN DEFAULT true,
    updated_at          TIMESTAMPTZ DEFAULT now()
);

-- Source videos (the original long-form content)
CREATE TABLE sources (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url             TEXT NOT NULL,
    platform        VARCHAR(32) NOT NULL,       -- youtube, vimeo, tiktok, instagram, direct
    external_id     VARCHAR(255),               -- platform-specific ID
    title           TEXT,
    description     TEXT,
    duration_seconds INT,
    thumbnail_url   TEXT,
    channel_name    VARCHAR(255),
    channel_id      VARCHAR(255),
    metadata        JSONB DEFAULT '{}',         -- raw metadata from yt-dlp
    status          VARCHAR(32) DEFAULT 'pending', -- pending, downloading, processing, complete, failed
    submitted_by    UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ DEFAULT now(),
    UNIQUE(platform, external_id)
);

-- Clips (the short-form segments)
CREATE TABLE clips (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id       UUID REFERENCES sources(id) ON DELETE SET NULL,
    title           TEXT,
    description     TEXT,
    duration_seconds REAL NOT NULL,
    start_time      REAL,                       -- offset in source video
    end_time        REAL,
    storage_key     TEXT NOT NULL,               -- MinIO object key
    thumbnail_key   TEXT,
    hls_key         TEXT,                        -- HLS manifest key
    width           INT,
    height          INT,
    file_size_bytes BIGINT,
    transcript      TEXT,                        -- from Whisper
    language        VARCHAR(8),
    topics          TEXT[] DEFAULT '{}',
    tags            TEXT[] DEFAULT '{}',
    content_score   REAL DEFAULT 0.5,           -- predicted engagement (0-1)
    expires_at      TIMESTAMPTZ,                -- lifecycle management
    is_protected    BOOLEAN DEFAULT false,       -- protected from auto-deletion
    status          VARCHAR(32) DEFAULT 'processing',
    created_at      TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX idx_clips_topics ON clips USING GIN(topics);
CREATE INDEX idx_clips_tags ON clips USING GIN(tags);
CREATE INDEX idx_clips_status ON clips(status);
CREATE INDEX idx_clips_expires ON clips(expires_at) WHERE expires_at IS NOT NULL AND NOT is_protected;
CREATE INDEX idx_clips_score ON clips(content_score DESC);

-- User interactions (drives the algorithm)
CREATE TABLE interactions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id     UUID NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    action      VARCHAR(32) NOT NULL,           -- view, like, dislike, save, share, skip, watch_full
    watch_duration_seconds REAL,
    watch_percentage REAL,
    created_at  TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX idx_interactions_user ON interactions(user_id, created_at DESC);
CREATE INDEX idx_interactions_clip ON interactions(clip_id);
CREATE INDEX idx_interactions_action ON interactions(user_id, action);

-- Saved/favorited clips (protected from lifecycle cleanup)
CREATE TABLE saved_clips (
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    clip_id     UUID NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (user_id, clip_id)
);

-- Collections (user-created playlists)
CREATE TABLE collections (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       VARCHAR(128) NOT NULL,
    description TEXT,
    is_public   BOOLEAN DEFAULT false,
    created_at  TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE collection_clips (
    collection_id UUID NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    clip_id       UUID NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
    position      INT NOT NULL DEFAULT 0,
    added_at      TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (collection_id, clip_id)
);

-- Ingestion job queue (tracked in DB alongside Redis for persistence)
CREATE TABLE jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id       UUID REFERENCES sources(id),
    job_type        VARCHAR(32) NOT NULL,       -- download, split, transcode, transcribe, analyze
    status          VARCHAR(32) DEFAULT 'queued',
    priority        INT DEFAULT 5,
    payload         JSONB DEFAULT '{}',
    result          JSONB DEFAULT '{}',
    error           TEXT,
    attempts        INT DEFAULT 0,
    max_attempts    INT DEFAULT 3,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX idx_jobs_status ON jobs(status, priority DESC);

-- Trigger to auto-protect clips when saved
CREATE OR REPLACE FUNCTION protect_saved_clip()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE clips SET is_protected = true WHERE id = NEW.clip_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_protect_saved
    AFTER INSERT ON saved_clips
    FOR EACH ROW EXECUTE FUNCTION protect_saved_clip();

-- Trigger to check if clip should be unprotected when unsaved
CREATE OR REPLACE FUNCTION check_unprotect_clip()
RETURNS TRIGGER AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM saved_clips WHERE clip_id = OLD.clip_id) THEN
        UPDATE clips SET is_protected = false WHERE id = OLD.clip_id;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_check_unprotect
    AFTER DELETE ON saved_clips
    FOR EACH ROW EXECUTE FUNCTION check_unprotect_clip();
