import React, { useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

export function IngestModal({ onClose }) {
  const [url, setUrl] = useState('');
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState(null);

  async function handleSubmit(e) {
    e.preventDefault();
    if (!url.trim()) return;
    setLoading(true);
    try {
      const data = await api.ingest(url);
      setResult(data);
      setUrl('');
    } catch (err) {
      setResult({ error: err.error || 'Failed to submit' });
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="modal-overlay" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal-sheet">
        <div className="modal-handle" />
        <div className="modal-title">Add Content</div>
        <div className="ingest-platforms">
          <span className="platform-tag">YouTube</span>
          <span className="platform-tag">Vimeo</span>
          <span className="platform-tag">TikTok</span>
          <span className="platform-tag">Instagram</span>
          <span className="platform-tag">X/Twitter</span>
          <span className="platform-tag">Direct URL</span>
        </div>
        <form onSubmit={handleSubmit}>
          <input
            className="ingest-input"
            placeholder="Paste video URL..."
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            autoFocus
          />
          <button className="ingest-submit" type="submit" disabled={loading}>
            {loading ? 'Submitting...' : 'Process Video'}
          </button>
        </form>
        {result && !result.error && (
          <div style={{ marginTop: 16, color: 'var(--success)', fontSize: 14 }}>
            Queued for processing (Job: {result.job_id?.slice(0, 8)}...)
          </div>
        )}
        {result?.error && (
          <div style={{ marginTop: 16, color: 'var(--accent)', fontSize: 14 }}>
            {result.error}
          </div>
        )}
      </div>
    </div>
  );
}
