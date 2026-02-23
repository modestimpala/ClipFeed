-- Content scoring function
-- Weights: watch_pct_avg*0.35 + like_rate*0.25 + save_rate*0.20 + complete_rate*0.15
--          - skip_rate*0.30 - dislike_rate*0.15, clamped to [0.0, 1.0]
-- Only updates clips with >= 5 views (cold-start clips stay at 0.5)

CREATE OR REPLACE FUNCTION update_content_scores()
RETURNS INTEGER AS $$
DECLARE updated_count INTEGER;
BEGIN
  WITH stats AS (
    SELECT
      clip_id,
      COUNT(*) FILTER (WHERE action='view') AS view_count,
      AVG(watch_percentage) FILTER (WHERE action='view' AND watch_percentage IS NOT NULL) AS watch_pct_avg,
      COUNT(*) FILTER (WHERE action='like')::REAL   / NULLIF(COUNT(*) FILTER (WHERE action='view'),0) AS like_rate,
      COUNT(*) FILTER (WHERE action='save')::REAL   / NULLIF(COUNT(*) FILTER (WHERE action='view'),0) AS save_rate,
      COUNT(*) FILTER (WHERE action='watch_full')::REAL / NULLIF(COUNT(*) FILTER (WHERE action='view'),0) AS complete_rate,
      COUNT(*) FILTER (WHERE action='skip')::REAL   / NULLIF(COUNT(*) FILTER (WHERE action='view'),0) AS skip_rate,
      COUNT(*) FILTER (WHERE action='dislike')::REAL / NULLIF(COUNT(*) FILTER (WHERE action='view'),0) AS dislike_rate
    FROM interactions GROUP BY clip_id
    HAVING COUNT(*) FILTER (WHERE action='view') >= 5
  ),
  scores AS (
    SELECT clip_id, GREATEST(0.0, LEAST(1.0,
        COALESCE(watch_pct_avg,0.5)*0.35
      + COALESCE(like_rate,0)*0.25
      + COALESCE(save_rate,0)*0.20
      + COALESCE(complete_rate,0)*0.15
      - COALESCE(skip_rate,0)*0.30
      - COALESCE(dislike_rate,0)*0.15
    )) AS new_score FROM stats
  )
  UPDATE clips SET content_score = scores.new_score
  FROM scores WHERE clips.id = scores.clip_id AND clips.status = 'ready';
  GET DIAGNOSTICS updated_count = ROW_COUNT;
  RETURN updated_count;
END;
$$ LANGUAGE plpgsql;
