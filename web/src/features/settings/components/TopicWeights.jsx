import React, { useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

const COLLAPSED_COUNT = 8;

export function TopicWeights({ currentWeights, onWeightsChange }) {
  const [topics, setTopics] = useState(null);
  const [weights, setWeights] = useState(currentWeights || {});
  const [error, setError] = useState(null);
  const [expanded, setExpanded] = useState(false);
  const [search, setSearch] = useState('');
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

  // Sort: customized topics first, then by clip count desc
  const sortedTopics = useMemo(() => {
    if (!topics) return [];
    return [...topics].sort((a, b) => {
      const aCustom = (weights[a.name] ?? 1.0) !== 1.0 ? 1 : 0;
      const bCustom = (weights[b.name] ?? 1.0) !== 1.0 ? 1 : 0;
      if (aCustom !== bCustom) return bCustom - aCustom;
      return (b.clip_count || 0) - (a.clip_count || 0);
    });
  }, [topics, weights]);

  // Filter by search
  const filtered = useMemo(() => {
    if (!search.trim()) return sortedTopics;
    const q = search.toLowerCase();
    return sortedTopics.filter((t) => t.name.toLowerCase().includes(q));
  }, [sortedTopics, search]);

  const customCount = useMemo(
    () => Object.values(weights).filter((v) => v !== 1.0).length,
    [weights]
  );

  const displayTopics = expanded || search ? filtered : filtered.slice(0, COLLAPSED_COUNT);
  const hasMore = !search && filtered.length > COLLAPSED_COUNT;

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

  function handleResetAll() {
    const next = {};
    setWeights(next);
    api.updatePreferences({ topic_weights: next })
      .then(() => onWeightsChange?.(next))
      .catch(console.error);
  }

  function formatLabel(val) {
    if (val === 0) return 'Hidden';
    if (val === 1) return 'Normal';
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
        No topics available yet -- add some clips first.
      </div>
    );
  }

  return (
    <div className="topic-weights-container">
      {/* Summary + actions bar */}
      <div className="topic-weights-toolbar">
        <span className="topic-weights-summary">
          {topics.length} topics{customCount > 0 && <> · <span className="topic-weights-custom">{customCount} customized</span></>}
        </span>
        {customCount > 0 && (
          <button className="topic-weights-reset" onClick={handleResetAll}>
            Reset all
          </button>
        )}
      </div>

      {/* Search -- show when expanded or many topics */}
      {(expanded || topics.length > COLLAPSED_COUNT) && (
        <div className="topic-search-wrap">
          <svg className="topic-search-icon" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <input
            className="topic-search-input"
            type="text"
            placeholder="Filter topics…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          {search && (
            <button className="topic-search-clear" onClick={() => setSearch('')}>×</button>
          )}
        </div>
      )}

      {/* Topic list */}
      <div className="topic-weights-list">
        {displayTopics.map((t) => {
          const val = weights[t.name] ?? 1.0;
          const isCustom = val !== 1.0;
          return (
            <div key={t.name} className={`topic-weight-row ${isCustom ? 'customized' : ''}`}>
              <div className="topic-weight-header">
                <span className="topic-weight-name">
                  {t.name}
                  <span className="topic-weight-count">{t.clip_count}</span>
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

      {/* Show more/less */}
      {hasMore && (
        <button
          className="topic-weights-toggle"
          onClick={() => setExpanded((p) => !p)}
        >
          {expanded ? 'Show less' : `Show all ${filtered.length} topics`}
        </button>
      )}

      {/* Search no-results */}
      {search && displayTopics.length === 0 && (
        <div className="topic-weights-empty">No topics matching "{search}"</div>
      )}
    </div>
  );
}
