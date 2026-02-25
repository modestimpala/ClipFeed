import React, { useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { showToast } from '../../../shared/ui/toast';

const INTERVAL_OPTIONS = [6, 12, 24, 48];

export function AddScoutSourceForm({ onCreated }) {
  const [open, setOpen] = useState(false);
  const [sourceType, setSourceType] = useState('channel');
  const [identifier, setIdentifier] = useState('');
  const [interval, setInterval] = useState(24);
  const [submitting, setSubmitting] = useState(false);

  function handleSubmit(e) {
    e.preventDefault();
    if (!identifier.trim() || submitting) return;
    setSubmitting(true);
    api.createScoutSource({
      source_type: sourceType,
      identifier: identifier.trim(),
      check_interval_hours: interval,
    })
      .then(() => {
        setIdentifier('');
        setSourceType('channel');
        setInterval(24);
        setOpen(false);
        onCreated();
      })
      .catch(() => showToast('Failed to add source'))
      .finally(() => setSubmitting(false));
  }

  if (!open) {
    return (
      <button className="scout-add-btn" onClick={() => setOpen(true)}>
        + Add Source
      </button>
    );
  }

  return (
    <form className="scout-add-form" onSubmit={handleSubmit}>
      <div className="scout-add-form-row">
        <label className="scout-form-label">Type</label>
        <select className="scout-form-select" value={sourceType} onChange={(e) => setSourceType(e.target.value)}>
          <option value="channel">Channel</option>
          <option value="playlist">Playlist</option>
          <option value="hashtag">Hashtag</option>
        </select>
      </div>

      <div className="scout-add-form-row">
        <label className="scout-form-label">Identifier</label>
        <input
          className="scout-form-input"
          type="text"
          placeholder="URL or search term"
          value={identifier}
          onChange={(e) => setIdentifier(e.target.value)}
        />
      </div>

      <div className="scout-add-form-row">
        <label className="scout-form-label">Check every</label>
        <select className="scout-form-select" value={interval} onChange={(e) => setInterval(parseInt(e.target.value, 10))}>
          {INTERVAL_OPTIONS.map((h) => (
            <option key={h} value={h}>{h}h</option>
          ))}
        </select>
      </div>

      <div className="scout-add-form-actions">
        <button type="submit" className="scout-submit-btn" disabled={!identifier.trim() || submitting}>
          {submitting ? 'Adding…' : 'Add'}
        </button>
        <button type="button" className="scout-cancel-btn" onClick={() => setOpen(false)}>Cancel</button>
      </div>
    </form>
  );
}
