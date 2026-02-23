import React, { useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

const INTERVAL_OPTIONS = [6, 12, 24, 48];

function timeAgo(iso) {
  if (!iso) return 'Never';
  const diff = (Date.now() - new Date(iso).getTime()) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

const TYPE_LABELS = { channel: 'Channel', playlist: 'Playlist', hashtag: 'Hashtag' };

export function ScoutSourceCard({ source, onUpdate, onDelete, onTrigger }) {
  const [checking, setChecking] = useState(false);
  const isChecking = checking || source.force_check;

  function handleTrigger() {
    setChecking(true);
    api.triggerScoutSource(source.id)
      .then(() => { if (onTrigger) onTrigger(); })
      .catch(() => {})
      .finally(() => setTimeout(() => setChecking(false), 2000));
  }

  const counts = source.candidates || {};

  return (
    <div className="scout-source-card">
      <div className="scout-source-header">
        <span className="scout-source-badge">{TYPE_LABELS[source.source_type] || source.source_type}</span>
        <span className="scout-source-id" title={source.identifier}>{source.identifier}</span>
      </div>

      {(counts.pending > 0 || counts.ingested > 0 || counts.rejected > 0) && (
        <div className="scout-source-stats">
          {counts.pending > 0 && <span className="scout-stat pending">{counts.pending} pending</span>}
          {counts.ingested > 0 && <span className="scout-stat ingested">{counts.ingested} ingested</span>}
          {counts.rejected > 0 && <span className="scout-stat rejected">{counts.rejected} rejected</span>}
        </div>
      )}

      <div className="scout-source-controls">
        <select
          className="scout-interval-select"
          value={source.check_interval_hours}
          onChange={(e) => onUpdate(source.id, { check_interval_hours: parseInt(e.target.value, 10) })}
        >
          {INTERVAL_OPTIONS.map((h) => (
            <option key={h} value={h}>Every {h}h</option>
          ))}
        </select>

        <button
          className={`scout-toggle-btn ${source.is_active ? 'active' : ''}`}
          onClick={() => onUpdate(source.id, { is_active: !source.is_active })}
        >
          {source.is_active ? 'Active' : 'Paused'}
        </button>

        <button
          className={`scout-check-btn ${isChecking ? 'checking' : ''}`}
          onClick={handleTrigger}
          disabled={isChecking}
        >
          {isChecking ? 'Checking…' : 'Check Now'}
        </button>

        <button className="scout-delete-btn" onClick={() => onDelete(source.id)}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <polyline points="3 6 5 6 21 6" /><path d="M19 6l-1 14a2 2 0 01-2 2H8a2 2 0 01-2-2L5 6" /><path d="M10 11v6" /><path d="M14 11v6" />
          </svg>
        </button>
      </div>

      <div className="scout-source-meta">Last checked {timeAgo(source.last_checked)}</div>
    </div>
  );
}
