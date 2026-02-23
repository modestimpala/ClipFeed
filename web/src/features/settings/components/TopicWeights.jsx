import React, { useEffect, useRef, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

export function TopicWeights({ currentWeights, onWeightsChange }) {
  const [topics, setTopics] = useState(null);
  const [weights, setWeights] = useState(currentWeights || {});
  const [error, setError] = useState(null);
  const saveTimer = useRef(null);

  useEffect(() => {
    api.getTopics()
      .then((data) => setTopics(data.topics || []))
      .catch(() => setError('Failed to load topics'));
    return () => {
      if (saveTimer.current) clearTimeout(saveTimer.current);
    };
  }, []);

  useEffect(() => {
    setWeights(currentWeights || {});
  }, [currentWeights]);

  function handleSlider(topicName, value) {
    const next = { ...weights, [topicName]: value };
    setWeights(next);

    if (saveTimer.current) clearTimeout(saveTimer.current);
    saveTimer.current = setTimeout(() => {
      api.updatePreferences({ topic_weights: next })
        .then(() => onWeightsChange?.(next))
        .catch(console.error);
    }, 500);
  }

  function formatLabel(val) {
    if (val === 0) return 'Hide';
    if (val === 1) return 'Neutral';
    if (val === 2) return 'Max';
    return `${val.toFixed(1)}×`;
  }

  function sliderColor(val) {
    if (val < 0.5) return 'var(--accent)';
    if (val > 1.5) return 'var(--success)';
    return 'var(--text-dim)';
  }

  if (error) {
    return <div className="topic-weights-empty">{error}</div>;
  }

  if (topics === null) {
    return <div className="topic-weights-empty">Loading topics…</div>;
  }

  if (topics.length === 0) {
    return (
      <div className="topic-weights-empty">
        No topics available yet — add some clips first.
      </div>
    );
  }

  return (
    <div className="topic-weights-list">
      {topics.map((t) => {
        const val = weights[t.name] ?? 1.0;
        return (
          <div key={t.name} className="topic-weight-row">
            <div className="topic-weight-header">
              <span className="topic-weight-name">
                {t.name}
                <span className="topic-weight-count">{t.clip_count} clips</span>
              </span>
              <span className="topic-weight-value" style={{ color: sliderColor(val) }}>
                {formatLabel(val)}
              </span>
            </div>
            <input
              type="range"
              min="0"
              max="2"
              step="0.05"
              value={val}
              style={{ accentColor: sliderColor(val) }}
              onChange={(e) => handleSlider(t.name, parseFloat(e.target.value))}
            />
          </div>
        );
      })}
    </div>
  );
}
