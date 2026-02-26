ALTER TABLE scout_candidates ADD COLUMN IF NOT EXISTS description TEXT;
ALTER TABLE scout_candidates ADD COLUMN IF NOT EXISTS view_count BIGINT;
ALTER TABLE scout_candidates ADD COLUMN IF NOT EXISTS upload_date TEXT;
