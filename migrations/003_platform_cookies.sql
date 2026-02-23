-- Platform cookie storage for authenticated scraping
CREATE TABLE platform_cookies (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform    VARCHAR(32) NOT NULL,
    cookie_str  TEXT NOT NULL,
    is_active   BOOLEAN DEFAULT true,
    created_at  TIMESTAMPTZ DEFAULT now(),
    updated_at  TIMESTAMPTZ DEFAULT now(),
    UNIQUE(user_id, platform)
);

CREATE INDEX idx_platform_cookies_user ON platform_cookies(user_id, platform);
