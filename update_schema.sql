CREATE TABLE IF NOT EXISTS llm_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    system      TEXT NOT NULL,
    model       TEXT NOT NULL,
    prompt      TEXT NOT NULL,
    response    TEXT,
    error       TEXT,
    duration_ms INTEGER,
    created_at  TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Add scout_auto_ingest preference (idempotent via error-ignore in app)
ALTER TABLE user_preferences ADD COLUMN scout_auto_ingest INTEGER DEFAULT 1;
