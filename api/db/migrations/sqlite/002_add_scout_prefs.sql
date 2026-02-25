-- Add scout and dedupe preferences
ALTER TABLE user_preferences ADD COLUMN scout_threshold REAL DEFAULT 6.0;
ALTER TABLE user_preferences ADD COLUMN dedupe_seen_24h INTEGER DEFAULT 1;
ALTER TABLE user_preferences ADD COLUMN scout_auto_ingest INTEGER DEFAULT 1;
ALTER TABLE scout_sources ADD COLUMN force_check INTEGER DEFAULT 0;
