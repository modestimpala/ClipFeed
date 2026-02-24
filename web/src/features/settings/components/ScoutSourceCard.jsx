import React, { useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';
import { timeAgo } from '../../../shared/utils/formatters';

const INTERVAL_OPTIONS = [6, 12, 24, 48];

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
          <Icons.Trash />
        </button>
      </div>

      <div className="scout-source-meta">Last checked {timeAgo(source.last_checked) || 'Never'}</div>
    </div>
  );
}
